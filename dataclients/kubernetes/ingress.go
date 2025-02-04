package kubernetes

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	log "github.com/sirupsen/logrus"
	"github.com/zalando/skipper/dataclients/kubernetes/definitions"
	"github.com/zalando/skipper/eskip"
	"github.com/zalando/skipper/predicates"
)

const (
	ingressRouteIDPrefix                = "kube"
	backendWeightsAnnotationKey         = "zalando.org/backend-weights"
	ratelimitAnnotationKey              = "zalando.org/ratelimit"
	skipperfilterAnnotationKey          = "zalando.org/skipper-filter"
	skipperpredicateAnnotationKey       = "zalando.org/skipper-predicate"
	skipperRoutesAnnotationKey          = "zalando.org/skipper-routes"
	skipperLoadBalancerAnnotationKey    = "zalando.org/skipper-loadbalancer"
	skipperBackendProtocolAnnotationKey = "zalando.org/skipper-backend-protocol"
	pathModeAnnotationKey               = "zalando.org/skipper-ingress-path-mode"
	ingressOriginName                   = "ingress"
)

type ingressContext struct {
	state               *clusterState
	ingress             *definitions.IngressItem
	logger              *log.Entry
	annotationFilters   []*eskip.Filter
	annotationPredicate string
	extraRoutes         []*eskip.Route
	backendWeights      map[string]float64
	pathMode            PathMode
	redirect            *redirectInfo
	hostRoutes          map[string][]*eskip.Route
	defaultFilters      defaultFilters
}

type ingress struct {
	provideHTTPSRedirect     bool
	httpsRedirectCode        int
	pathMode                 PathMode
	kubernetesEnableEastWest bool
	kubernetesEastWestDomain string
	eastWestRangeDomains     []string
	eastWestRangePredicates  []*eskip.Predicate
	allowedExternalNames     []*regexp.Regexp
}

var nonWord = regexp.MustCompile(`\W`)

var errNotAllowedExternalName = errors.New("ingress with not allowed external name service")

func (ic *ingressContext) addHostRoute(host string, route *eskip.Route) {
	ic.hostRoutes[host] = append(ic.hostRoutes[host], route)
}

func newIngress(o Options) *ingress {
	return &ingress{
		provideHTTPSRedirect:     o.ProvideHTTPSRedirect,
		httpsRedirectCode:        o.HTTPSRedirectCode,
		pathMode:                 o.PathMode,
		kubernetesEnableEastWest: o.KubernetesEnableEastWest,
		kubernetesEastWestDomain: o.KubernetesEastWestDomain,
		eastWestRangeDomains:     o.KubernetesEastWestRangeDomains,
		eastWestRangePredicates:  o.KubernetesEastWestRangePredicates,
		allowedExternalNames:     o.AllowedExternalNames,
	}
}

func getLoadBalancerAlgorithm(m *definitions.Metadata) string {
	algorithm := defaultLoadBalancerAlgorithm
	if algorithmAnnotationValue, ok := m.Annotations[skipperLoadBalancerAnnotationKey]; ok {
		algorithm = algorithmAnnotationValue
	}

	return algorithm
}

// TODO: find a nicer way to autogenerate route IDs
func routeID(namespace, name, host, path, backend string) string {
	namespace = nonWord.ReplaceAllString(namespace, "_")
	name = nonWord.ReplaceAllString(name, "_")
	host = nonWord.ReplaceAllString(host, "_")
	path = nonWord.ReplaceAllString(path, "_")
	backend = nonWord.ReplaceAllString(backend, "_")
	return fmt.Sprintf("%s_%s__%s__%s__%s__%s", ingressRouteIDPrefix, namespace, name, host, path, backend)
}

// routeIDForCustom generates a route id for a custom route of an ingress
// resource.
func routeIDForCustom(namespace, name, id, host string, index int) string {
	name = name + "_" + id + "_" + strconv.Itoa(index)
	return routeID(namespace, name, host, "", "")
}

func setPath(m PathMode, r *eskip.Route, p string) {
	if p == "" {
		return
	}

	switch m {
	case PathPrefix:
		r.Predicates = append(r.Predicates, &eskip.Predicate{
			Name: "PathSubtree",
			Args: []interface{}{p},
		})
	case PathRegexp:
		r.PathRegexps = []string{p}
	default:
		if p == "/" {
			r.PathRegexps = []string{"^/"}
		} else {
			r.PathRegexps = []string{"^(" + p + ")"}
		}
	}
}

func externalNameRoute(
	ns, name, idHost string,
	hostRegexps []string,
	svc *service,
	servicePort *servicePort,
	allowedNames []*regexp.Regexp,
) (*eskip.Route, error) {
	if !isExternalDomainAllowed(allowedNames, svc.Spec.ExternalName) {
		return nil, fmt.Errorf("%w: %s", errNotAllowedExternalName, svc.Spec.ExternalName)
	}

	scheme := "https"
	if n, _ := servicePort.TargetPort.Number(); n != 443 {
		scheme = "http"
	}

	u := fmt.Sprintf("%s://%s:%s", scheme, svc.Spec.ExternalName, servicePort.TargetPort)
	f, err := eskip.ParseFilters(fmt.Sprintf(`setRequestHeader("Host", "%s")`, svc.Spec.ExternalName))
	if err != nil {
		return nil, err
	}

	return &eskip.Route{
		Id:          routeID(ns, name, idHost, "", svc.Spec.ExternalName),
		BackendType: eskip.NetworkBackend,
		Backend:     u,
		Filters:     f,
		HostRegexps: hostRegexps,
	}, nil
}

func convertPathRule(
	state *clusterState,
	metadata *definitions.Metadata,
	host string,
	prule *definitions.PathRule,
	pathMode PathMode,
	allowedExternalNames []*regexp.Regexp,
) (*eskip.Route, error) {

	ns := metadata.Namespace
	name := metadata.Name

	if prule.Backend == nil {
		return nil, fmt.Errorf("invalid path rule, missing backend in: %s/%s/%s", ns, name, host)
	}

	var (
		eps []string
		err error
		svc *service
	)

	var hostRegexp []string
	if host != "" {
		hostRegexp = []string{createHostRx(host)}
	}
	svcPort := prule.Backend.ServicePort
	svcName := prule.Backend.ServiceName

	svc, err = state.getService(ns, svcName)
	if err != nil {
		log.Errorf("convertPathRule: Failed to get service %s, %s, %s", ns, svcName, svcPort)
		return nil, err
	}

	servicePort, err := svc.getServicePort(svcPort)
	if err != nil {
		// service definition is wrong or no pods
		err = nil
		if len(eps) > 0 {
			// should never happen
			log.Errorf("convertPathRule: Failed to find target port for service %s, but %d endpoints exist. Kubernetes has inconsistent data", svcName, len(eps))
		}
	} else if svc.Spec.Type == "ExternalName" {
		return externalNameRoute(ns, name, host, hostRegexp, svc, servicePort, allowedExternalNames)
	} else {
		protocol := "http"
		if p, ok := metadata.Annotations[skipperBackendProtocolAnnotationKey]; ok {
			protocol = p
		}

		eps = state.getEndpointsByService(ns, svcName, protocol, servicePort)
		log.Debugf("convertPathRule: Found %d endpoints %s for %s", len(eps), servicePort, svcName)
	}
	if len(eps) == 0 {
		// add shunt route https://github.com/zalando/skipper/issues/1525
		log.Debugf("convertPathRule: add shuntroute to return 502 for ingress %s/%s service %s with %d endpoints", ns, name, svcName, len(eps))
		r := &eskip.Route{
			Id:          routeID(ns, name, host, prule.Path, svcName),
			HostRegexps: hostRegexp,
		}
		setPath(pathMode, r, prule.Path)
		setTraffic(r, svcName, prule.Backend.Traffic, prule.Backend.NoopCount)
		shuntRoute(r)
		return r, nil
	}

	log.Debugf("convertPathRule: %d routes for %s/%s/%s", len(eps), ns, svcName, svcPort)
	if len(eps) == 1 {
		r := &eskip.Route{
			Id:          routeID(ns, name, host, prule.Path, svcName),
			Backend:     eps[0],
			HostRegexps: hostRegexp,
		}

		setPath(pathMode, r, prule.Path)
		setTraffic(r, svcName, prule.Backend.Traffic, prule.Backend.NoopCount)
		return r, nil
	}

	r := &eskip.Route{
		Id:          routeID(ns, name, host, prule.Path, prule.Backend.ServiceName),
		BackendType: eskip.LBBackend,
		LBEndpoints: eps,
		LBAlgorithm: getLoadBalancerAlgorithm(metadata),
		HostRegexps: hostRegexp,
	}
	setPath(pathMode, r, prule.Path)
	setTraffic(r, svcName, prule.Backend.Traffic, prule.Backend.NoopCount)
	return r, nil
}

func setTraffic(r *eskip.Route, svcName string, weight float64, noopCount int) {
	// add traffic predicate if traffic weight is between 0.0 and 1.0
	if 0.0 < weight && weight < 1.0 {
		r.Predicates = append([]*eskip.Predicate{{
			Name: predicates.TrafficName,
			Args: []interface{}{weight},
		}}, r.Predicates...)
		log.Debugf("Traffic weight %.2f for backend '%s'", weight, svcName)
	}
	for i := 0; i < noopCount; i++ {
		r.Predicates = append([]*eskip.Predicate{{
			Name: predicates.TrueName,
			Args: []interface{}{},
		}}, r.Predicates...)
	}
}

func applyAnnotationPredicates(m PathMode, r *eskip.Route, annotation string) error {
	if annotation == "" {
		return nil
	}

	predicates, err := eskip.ParsePredicates(annotation)
	if err != nil {
		return err
	}

	// to avoid conflict, give precedence for those predicates that come
	// from annotations
	if m == PathPrefix {
		for _, p := range predicates {
			if p.Name != "Path" && p.Name != "PathSubtree" {
				continue
			}

			r.Path = ""
			for i, p := range r.Predicates {
				if p.Name != "PathSubtree" && p.Name != "Path" {
					continue
				}

				copy(r.Predicates[i:], r.Predicates[i+1:])
				r.Predicates[len(r.Predicates)-1] = nil
				r.Predicates = r.Predicates[:len(r.Predicates)-1]
				break
			}
		}
	}

	r.Predicates = append(r.Predicates, predicates...)
	return nil
}

func (ing *ingress) addEndpointsRule(ic ingressContext, host string, prule *definitions.PathRule) error {
	meta := ic.ingress.Metadata
	endpointsRoute, err := convertPathRule(
		ic.state,
		meta,
		host,
		prule,
		ic.pathMode,
		ing.allowedExternalNames,
	)
	if err != nil {
		// if the service is not found the route should be removed
		if err == errServiceNotFound || err == errResourceNotFound {
			return nil
		}

		// TODO: this error checking should not really be used, and the error handling of the ingress
		// problems should be refactored such that a single ingress's error doesn't block the
		// processing of the independent ingresses.
		if errors.Is(err, errNotAllowedExternalName) {
			log.Infof("Not allowed external name: %v", err)
			return nil
		}

		// Ingress status field does not support errors
		return fmt.Errorf("error while getting service: %v", err)
	}

	// safe prepend, see: https://play.golang.org/p/zg5aGKJpRyK
	filters := make([]*eskip.Filter, len(endpointsRoute.Filters)+len(ic.annotationFilters))
	copy(filters, ic.annotationFilters)
	copy(filters[len(ic.annotationFilters):], endpointsRoute.Filters)
	endpointsRoute.Filters = filters

	// add pre-configured default filters
	df, err := ic.defaultFilters.getNamed(meta.Namespace, prule.Backend.ServiceName)
	if err != nil {
		ic.logger.Errorf("Failed to retrieve default filters: %v.", err)
	} else {
		// it's safe to prepend, because type defaultFilters copies the slice during get()
		endpointsRoute.Filters = append(df, endpointsRoute.Filters...)
	}

	err = applyAnnotationPredicates(ic.pathMode, endpointsRoute, ic.annotationPredicate)
	if err != nil {
		ic.logger.Errorf("failed to apply annotation predicates: %v", err)
	}
	ic.addHostRoute(host, endpointsRoute)

	redirect := ic.redirect
	ewRangeMatch := false
	for _, s := range ing.eastWestRangeDomains {
		if strings.HasSuffix(host, s) {
			ewRangeMatch = true
			break
		}
	}
	if !(ewRangeMatch || strings.HasSuffix(host, ing.kubernetesEastWestDomain) && ing.kubernetesEastWestDomain != "") {
		switch {
		case redirect.ignore:
			// no redirect
		case redirect.enable:
			ic.addHostRoute(host, createIngressEnableHTTPSRedirect(endpointsRoute, redirect.code))
			redirect.setHost(host)
		case redirect.disable:
			ic.addHostRoute(host, createIngressDisableHTTPSRedirect(endpointsRoute))
			redirect.setHostDisabled(host)
		case redirect.defaultEnabled:
			ic.addHostRoute(host, createIngressEnableHTTPSRedirect(endpointsRoute, redirect.code))
			redirect.setHost(host)
		}
	}

	if ing.kubernetesEnableEastWest {
		ewRoute := createEastWestRouteIng(ing.kubernetesEastWestDomain, meta.Name, meta.Namespace, endpointsRoute)
		ewHost := fmt.Sprintf("%s.%s.%s", meta.Name, meta.Namespace, ing.kubernetesEastWestDomain)
		ic.addHostRoute(ewHost, ewRoute)
	}
	return nil
}

func addExtraRoutes(ic ingressContext, ruleHost, path, eastWestDomain string, enableEastWest bool) {
	hosts := []string{createHostRx(ruleHost)}
	name := ic.ingress.Metadata.Name
	ns := ic.ingress.Metadata.Namespace

	// add extra routes from optional annotation
	for extraIndex, r := range ic.extraRoutes {
		route := *r
		route.HostRegexps = hosts
		route.Id = routeIDForCustom(
			ns,
			name,
			route.Id,
			ruleHost+strings.Replace(path, "/", "_", -1),
			extraIndex)
		setPath(ic.pathMode, &route, path)
		if n := countPathRoutes(&route); n <= 1 {
			ic.addHostRoute(ruleHost, &route)
			ic.redirect.updateHost(ruleHost)
		} else {
			log.Errorf("Failed to add route having %d path routes: %v", n, r)
		}
		if enableEastWest {
			ewRoute := createEastWestRouteIng(eastWestDomain, name, ns, &route)
			ewHost := fmt.Sprintf("%s.%s.%s", name, ns, eastWestDomain)
			ic.addHostRoute(ewHost, ewRoute)
		}
	}
}

// computeBackendWeights computes and sets the backend traffic weights on the
// rule backends.
// The traffic is calculated based on the following rules:
//
// * if no weight is defined for a backend it will get weight 0.
// * if no weights are specified for all backends of a path, then traffic will
//   be distributed equally.
//
// Each traffic weight is relative to the number of backends per path. If there
// are multiple backends per path the weight will be relative to the number of
// remaining backends for the path e.g. if the weight is specified as
//
//      backend-1: 0.2
//      backend-2: 0.6
//      backend-3: 0.2
//
// then the weight will be calculated to:
//
//      backend-1: 0.2
//      backend-2: 0.75
//      backend-3: 1.0
//
// where for a weight of 1.0 no Traffic predicate will be generated.
func computeBackendWeights(backendWeights map[string]float64, rule *definitions.Rule) {
	type pathInfo struct {
		sum          float64
		lastActive   *definitions.Backend
		count        int
		weightsCount int
	}

	// get backend weight sum and count of backends for all paths
	pathInfos := make(map[string]*pathInfo)
	for _, path := range rule.Http.Paths {
		sc, ok := pathInfos[path.Path]
		if !ok {
			sc = &pathInfo{}
			pathInfos[path.Path] = sc
		}

		if weight, ok := backendWeights[path.Backend.ServiceName]; ok {
			sc.sum += weight
			if weight > 0 {
				sc.lastActive = path.Backend
				sc.weightsCount++
			}
		} else {
			sc.count++
		}
	}

	// calculate traffic weight for each backend
	for _, path := range rule.Http.Paths {
		if sc, ok := pathInfos[path.Path]; ok {
			if weight, ok := backendWeights[path.Backend.ServiceName]; ok {
				// force a weight of 1.0 for the last backend with a non-zero weight to avoid rounding issues
				if sc.lastActive == path.Backend {
					path.Backend.Traffic = 1.0
					continue
				}

				path.Backend.Traffic = weight / sc.sum
				// subtract weight from the sum in order to
				// give subsequent backends a higher relative
				// weight.
				sc.sum -= weight

				// noops are required to make sure that routes are in order selected by
				// routing tree
				if sc.weightsCount > 2 {
					path.Backend.NoopCount = sc.weightsCount - 2
				}
				sc.weightsCount--
			} else if sc.sum == 0 && sc.count > 0 {
				path.Backend.Traffic = 1.0 / float64(sc.count)
			}
			// reduce count by one in order to give subsequent
			// backends for the path a higher relative weight.
			sc.count--
		}
	}
}

// TODO: default filters not applied to 'extra' routes from the custom route annotations. Is it on purpose?
// https://github.com/zalando/skipper/issues/1287
func (ing *ingress) addSpecRule(ic ingressContext, ru *definitions.Rule) error {
	if ru.Http == nil {
		ic.logger.Warn("invalid ingress item: rule missing http definitions")
		return nil
	}
	// update Traffic field for each backend
	computeBackendWeights(ic.backendWeights, ru)
	for _, prule := range ru.Http.Paths {
		addExtraRoutes(ic, ru.Host, prule.Path, ing.kubernetesEastWestDomain, ing.kubernetesEnableEastWest)
		if prule.Backend.Traffic > 0 {
			err := ing.addEndpointsRule(ic, ru.Host, prule)
			if err != nil {
				return err
			}
		}
	}
	return nil
}

// converts the default backend if any
func (ing *ingress) convertDefaultBackend(
	state *clusterState,
	i *definitions.IngressItem,
) (*eskip.Route, bool, error) {
	// the usage of the default backend depends on what we want
	// we can generate a hostname out of it based on shared rules
	// and instructions in annotations, if there are no rules defined

	// this is a flaw in the ingress API design, because it is not on the hosts' level, but the spec
	// tells to match if no rule matches. This means that there is no matching rule on this ingress
	// and if there are multiple ingress items, then there is a race between them.
	if i.Spec.DefaultBackend == nil {
		return nil, false, nil
	}

	var (
		eps     []string
		err     error
		ns      = i.Metadata.Namespace
		name    = i.Metadata.Name
		svcName = i.Spec.DefaultBackend.ServiceName
		svcPort = i.Spec.DefaultBackend.ServicePort
	)

	svc, err := state.getService(ns, svcName)
	if err != nil {
		log.Errorf("convertDefaultBackend: Failed to get service %s, %s, %s", ns, svcName, svcPort)
		return nil, false, err
	}

	servicePort, err := svc.getServicePort(svcPort)
	if err != nil {
		log.Errorf("convertDefaultBackend: Failed to find target port %v, %s, for ingress %s/%s and service %s add shuntroute: %v", svc.Spec.Ports, svcPort, ns, name, svcName, err)
		err = nil
	} else if svc.Spec.Type == "ExternalName" {
		r, err := externalNameRoute(ns, name, "default", nil, svc, servicePort, ing.allowedExternalNames)
		return r, err == nil, err
	} else {
		log.Debugf("convertDefaultBackend: Found target port %v, for service %s", servicePort.TargetPort, svcName)
		protocol := "http"
		if p, ok := i.Metadata.Annotations[skipperBackendProtocolAnnotationKey]; ok {
			protocol = p
		}

		eps = state.getEndpointsByService(
			ns,
			svcName,
			protocol,
			servicePort,
		)
		log.Debugf("convertDefaultBackend: Found %d endpoints for %s: %v", len(eps), svcName, err)
	}

	if len(eps) == 0 {
		// add shunt route https://github.com/zalando/skipper/issues/1525
		log.Debugf("convertDefaultBackend: add shuntroute to return 502 for ingress %s/%s service %s with %d endpoints", ns, name, svcName, len(eps))
		r := &eskip.Route{
			Id: routeID(ns, name, "", "", ""),
		}
		shuntRoute(r)
		return r, true, nil
	} else if len(eps) == 1 {
		return &eskip.Route{
			Id:      routeID(ns, name, "", "", ""),
			Backend: eps[0],
		}, true, nil
	}

	return &eskip.Route{
		Id:          routeID(ns, name, "", "", ""),
		BackendType: eskip.LBBackend,
		LBEndpoints: eps,
		LBAlgorithm: getLoadBalancerAlgorithm(i.Metadata),
	}, true, nil
}

func countPathRoutes(r *eskip.Route) int {
	i := 0
	for _, p := range r.Predicates {
		if p.Name == "PathSubtree" || p.Name == "Path" {
			i++
		}
	}
	if r.Path != "" {
		i++
	}
	return i
}

// parse filter and ratelimit annotation
func annotationFilter(i *definitions.IngressItem, logger *log.Entry) []*eskip.Filter {
	var annotationFilter string
	if ratelimitAnnotationValue, ok := i.Metadata.Annotations[ratelimitAnnotationKey]; ok {
		annotationFilter = ratelimitAnnotationValue
	}
	if val, ok := i.Metadata.Annotations[skipperfilterAnnotationKey]; ok {
		if annotationFilter != "" {
			annotationFilter += " -> "
		}
		annotationFilter += val
	}

	if annotationFilter != "" {
		annotationFilters, err := eskip.ParseFilters(annotationFilter)
		if err == nil {
			return annotationFilters
		}
		logger.Errorf("Can not parse annotation filters: %v", err)
	}
	return nil
}

// parse predicate annotation
func annotationPredicate(i *definitions.IngressItem) string {
	var annotationPredicate string
	if val, ok := i.Metadata.Annotations[skipperpredicateAnnotationKey]; ok {
		annotationPredicate = val
	}
	return annotationPredicate
}

// parse routes annotation
func extraRoutes(i *definitions.IngressItem, logger *log.Entry) []*eskip.Route {
	var extraRoutes []*eskip.Route
	annotationRoutes := i.Metadata.Annotations[skipperRoutesAnnotationKey]
	if annotationRoutes != "" {
		var err error
		extraRoutes, err = eskip.Parse(annotationRoutes)
		if err != nil {
			logger.Errorf("failed to parse routes from %s, skipping: %v", skipperRoutesAnnotationKey, err)
		}
	}
	return extraRoutes
}

// parse backend-weights annotation if it exists
func backendWeights(i *definitions.IngressItem, logger *log.Entry) map[string]float64 {
	var backendWeights map[string]float64
	if backends, ok := i.Metadata.Annotations[backendWeightsAnnotationKey]; ok {
		err := json.Unmarshal([]byte(backends), &backendWeights)
		if err != nil {
			logger.Errorf("error while parsing backend-weights annotation: %v", err)
		}
	}
	return backendWeights
}

// parse pathmode from annotation or fallback to global default
func pathMode(i *definitions.IngressItem, globalDefault PathMode) PathMode {
	pathMode := globalDefault

	if pathModeString, ok := i.Metadata.Annotations[pathModeAnnotationKey]; ok {
		if p, err := ParsePathMode(pathModeString); err != nil {
			log.Errorf("Failed to get path mode for ingress %s/%s: %v", i.Metadata.Namespace, i.Metadata.Name, err)
		} else {
			log.Debugf("Set pathMode to %s", p)
			pathMode = p
		}
	}
	return pathMode
}

func (ing *ingress) ingressRoute(
	i *definitions.IngressItem,
	redirect *redirectInfo,
	state *clusterState,
	hostRoutes map[string][]*eskip.Route,
	df defaultFilters,
) (*eskip.Route, error) {
	if i.Metadata == nil || i.Metadata.Namespace == "" || i.Metadata.Name == "" || i.Spec == nil {
		log.Error("invalid ingress item: missing Metadata or Spec")
		return nil, nil
	}
	logger := log.WithFields(log.Fields{
		"ingress": fmt.Sprintf("%s/%s", i.Metadata.Namespace, i.Metadata.Name),
	})
	redirect.initCurrent(i.Metadata)
	ic := ingressContext{
		state:               state,
		ingress:             i,
		logger:              logger,
		annotationFilters:   annotationFilter(i, logger),
		annotationPredicate: annotationPredicate(i),
		extraRoutes:         extraRoutes(i, logger),
		backendWeights:      backendWeights(i, logger),
		pathMode:            pathMode(i, ing.pathMode),
		redirect:            redirect,
		hostRoutes:          hostRoutes,
		defaultFilters:      df,
	}

	var route *eskip.Route
	if r, ok, err := ing.convertDefaultBackend(state, i); ok {
		route = r
	} else if err != nil {
		ic.logger.Errorf("error while converting default backend: %v", err)
	}
	for _, rule := range i.Spec.Rules {
		err := ing.addSpecRule(ic, rule)
		if err != nil {
			return nil, err
		}
	}
	return route, nil
}

func (ing *ingress) addCatchAllRoutes(host string, r *eskip.Route, redirect *redirectInfo) []*eskip.Route {
	catchAll := &eskip.Route{
		Id:          routeID("", "catchall", host, "", ""),
		HostRegexps: r.HostRegexps,
		BackendType: eskip.ShuntBackend,
	}
	routes := []*eskip.Route{catchAll}
	if ing.kubernetesEnableEastWest {
		if ew := createEastWestRouteIng(ing.kubernetesEastWestDomain, r.Name, r.Namespace, catchAll); ew != nil {
			routes = append(routes, ew)
		}
	}
	if code, ok := redirect.setHostCode[host]; ok {
		routes = append(routes, createIngressEnableHTTPSRedirect(catchAll, code))
	}
	if redirect.disableHost[host] {
		routes = append(routes, createIngressDisableHTTPSRedirect(catchAll))
	}

	return routes
}

// hasCatchAllRoutes returns true if one of the routes in the list has a catchAll
// path expression.
//
// TODO: this should also consider path types exact and subtree
func hasCatchAllRoutes(routes []*eskip.Route) bool {
	for _, route := range routes {
		if len(route.PathRegexps) == 0 {
			return true
		}

		for _, exp := range route.PathRegexps {
			if exp == "^/" {
				return true
			}
		}
	}

	return false
}

// convert logs if an invalid found, but proceeds with the
// valid ones.  Reporting failures in Ingress status is not possible,
// because Ingress status field is v1beta1.LoadBalancerIngress that only
// supports IP and Hostname as string.
func (ing *ingress) convert(state *clusterState, df defaultFilters) ([]*eskip.Route, error) {
	var ewIngInfo map[string][]string // r.Id -> {namespace, name}
	if ing.kubernetesEnableEastWest {
		ewIngInfo = make(map[string][]string)
	}
	routes := make([]*eskip.Route, 0, len(state.ingresses))
	hostRoutes := make(map[string][]*eskip.Route)
	redirect := createRedirectInfo(ing.provideHTTPSRedirect, ing.httpsRedirectCode)
	for _, i := range state.ingresses {
		r, err := ing.ingressRoute(i, redirect, state, hostRoutes, df)
		if err != nil {
			return nil, err
		}
		if r != nil {
			routes = append(routes, r)
			if ing.kubernetesEnableEastWest {
				ewIngInfo[r.Id] = []string{i.Metadata.Namespace, i.Metadata.Name}
			}
		}
	}

	for host, rs := range hostRoutes {
		if len(rs) == 0 {
			continue
		}

		applyEastWestRange(ing.eastWestRangeDomains, ing.eastWestRangePredicates, host, rs)
		routes = append(routes, rs...)

		// if routes were configured, but there is no catchall route
		// defined for the host name, create a route which returns 404
		if !hasCatchAllRoutes(rs) {
			routes = append(routes, ing.addCatchAllRoutes(host, rs[0], redirect)...)
		}
	}

	if ing.kubernetesEnableEastWest && len(routes) > 0 && len(ewIngInfo) > 0 {
		ewroutes := make([]*eskip.Route, 0, len(routes))
		for _, r := range routes {
			if v, ok := ewIngInfo[r.Id]; ok {
				ewroutes = append(ewroutes, createEastWestRouteIng(ing.kubernetesEastWestDomain, v[0], v[1], r))
			}
		}
		l := len(routes)
		routes = append(routes, ewroutes...)
		log.Infof("enabled east west routes: %d %d %d %d", l, len(routes), len(ewroutes), len(hostRoutes))
	}

	return routes, nil
}
