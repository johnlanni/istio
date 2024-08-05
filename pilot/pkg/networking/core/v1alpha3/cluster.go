// Copyright Istio Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package v1alpha3

import (
	"fmt"
	"math"
	"net"
	"strconv"
	"strings"

	cluster "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	core "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	endpoint "github.com/envoyproxy/go-control-plane/envoy/config/endpoint/v3"
	internalupstream "github.com/envoyproxy/go-control-plane/envoy/extensions/transport_sockets/internal_upstream/v3"
	discovery "github.com/envoyproxy/go-control-plane/envoy/service/discovery/v3"
	metadata "github.com/envoyproxy/go-control-plane/envoy/type/metadata/v3"
	xdstype "github.com/envoyproxy/go-control-plane/envoy/type/v3"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/structpb"
	wrappers "google.golang.org/protobuf/types/known/wrapperspb"

	meshconfig "istio.io/api/mesh/v1alpha1"
	networking "istio.io/api/networking/v1alpha3"
	"istio.io/istio/pilot/pkg/features"
	"istio.io/istio/pilot/pkg/model"
	"istio.io/istio/pilot/pkg/networking/core/v1alpha3/envoyfilter"
	"istio.io/istio/pilot/pkg/networking/core/v1alpha3/loadbalancer"
	"istio.io/istio/pilot/pkg/networking/telemetry"
	"istio.io/istio/pilot/pkg/networking/util"
	"istio.io/istio/pilot/pkg/serviceregistry/provider"
	"istio.io/istio/pilot/pkg/util/protoconv"
	xdsfilters "istio.io/istio/pilot/pkg/xds/filters"
	alifeatures "istio.io/istio/pkg/ali/features"
	"istio.io/istio/pkg/config/host"
	"istio.io/istio/pkg/config/protocol"
	"istio.io/istio/pkg/config/schema/kind"
	"istio.io/istio/pkg/log"
	"istio.io/istio/pkg/security"
	"istio.io/istio/pkg/slices"
	netutil "istio.io/istio/pkg/util/net"
	"istio.io/istio/pkg/util/sets"
)

// deltaConfigTypes are used to detect changes and trigger delta calculations. When config updates has ONLY entries
// in this map, then delta calculation is triggered.
var deltaConfigTypes = sets.New(kind.ServiceEntry.String(), kind.DestinationRule.String())

const TransportSocketInternalUpstream = "envoy.transport_sockets.internal_upstream"

// getDefaultCircuitBreakerThresholds returns a copy of the default circuit breaker thresholds for the given traffic direction.
func getDefaultCircuitBreakerThresholds() *cluster.CircuitBreakers_Thresholds {
	// Modified by ingress
	return &cluster.CircuitBreakers_Thresholds{
		// DefaultMaxRetries specifies the default for the Envoy circuit breaker parameter max_retries. This
		// defines the maximum number of parallel retries a given Envoy will allow to the upstream cluster. Envoy defaults
		// this value to 3, however that has shown to be insufficient during periods of pod churn (e.g. rolling updates),
		// where multiple endpoints in a cluster are terminated. In these scenarios the circuit breaker can kick
		// in before Pilot is able to deliver an updated endpoint list to Envoy, leading to client-facing 503s.
		MaxRetries:         &wrappers.UInt32Value{Value: math.MaxUint32},
		MaxRequests:        &wrappers.UInt32Value{Value: uint32(alifeatures.DefaultUpstreamConcurrencyThreshold)},
		MaxConnections:     &wrappers.UInt32Value{Value: uint32(alifeatures.DefaultUpstreamConcurrencyThreshold)},
		MaxPendingRequests: &wrappers.UInt32Value{Value: uint32(alifeatures.DefaultUpstreamConcurrencyThreshold)},
		TrackRemaining:     true,
	}
	// End modified by ingress
}

// BuildClusters returns the list of clusters for the given proxy. This is the CDS output
// For outbound: Cluster for each service/subset hostname or cidr with SNI set to service hostname
// Cluster type based on resolution
// For inbound (sidecar only): Cluster for each inbound endpoint port and for each service port
func (configgen *ConfigGeneratorImpl) BuildClusters(proxy *model.Proxy, req *model.PushRequest) ([]*discovery.Resource, model.XdsLogDetails) {
	// In Sotw, we care about all services.
	var services []*model.Service
	if features.FilterGatewayClusterConfig && proxy.Type == model.Router {
		services = req.Push.GatewayServices(proxy)
	} else {
		services = proxy.SidecarScope.Services()
	}
	return configgen.buildClusters(proxy, req, services)
}

// BuildDeltaClusters generates the deltas (add and delete) for a given proxy. Currently, only service changes are reflected with deltas.
// Otherwise, we fall back onto generating everything.
func (configgen *ConfigGeneratorImpl) BuildDeltaClusters(proxy *model.Proxy, updates *model.PushRequest,
	watched *model.WatchedResource,
) ([]*discovery.Resource, []string, model.XdsLogDetails, bool) {
	// if we can't use delta, fall back to generate all
	if !shouldUseDelta(updates) {
		cl, lg := configgen.BuildClusters(proxy, updates)
		return cl, nil, lg, false
	}

	var deletedClusters []string
	var services []*model.Service
	// Holds clusters per service, keyed by hostname.
	serviceClusters := make(map[string]sets.String)
	// Holds service ports, keyed by hostname.Inner map port and its cluster name.
	// This is mainly used when service is updated and a port has been removed.
	servicePortClusters := make(map[string]map[int]string)
	// Holds subset clusters per service, keyed by hostname.
	subsetClusters := make(map[string]sets.String)

	for _, cluster := range watched.ResourceNames {
		// WatchedResources.ResourceNames will contain the names of the clusters it is subscribed to. We can
		// check with the name of our service (cluster names are in the format outbound|<port>|<subset>|<hostname>).
		_, subset, svcHost, port := model.ParseSubsetKey(cluster)
		if subset == "" {
			sets.InsertOrNew(serviceClusters, string(svcHost), cluster)
		} else {
			sets.InsertOrNew(subsetClusters, string(svcHost), cluster)
		}
		if servicePortClusters[string(svcHost)] == nil {
			servicePortClusters[string(svcHost)] = make(map[int]string)
		}
		servicePortClusters[string(svcHost)][port] = cluster
	}

	for key := range updates.ConfigsUpdated {
		// deleted clusters for this config.
		var deleted []string
		var svcs []*model.Service
		switch key.Kind {
		case kind.ServiceEntry:
			svcs, deleted = configgen.deltaFromServices(key, proxy, updates.Push, serviceClusters,
				servicePortClusters, subsetClusters)
		case kind.DestinationRule:
			svcs, deleted = configgen.deltaFromDestinationRules(key, proxy, subsetClusters)
		}
		services = append(services, svcs...)
		deletedClusters = append(deletedClusters, deleted...)
	}
	clusters, log := configgen.buildClusters(proxy, updates, services)
	// DeletedClusters contains list of all subset clusters for the deleted DR or updated DR.
	// When clusters are rebuilt, it rebuilts the subset clusters as well. So, we know what
	// subset clusters are really needed. So if deleted cluster is not rebuilt, then it is really deleted.
	builtClusters := sets.New[string]()
	for _, c := range clusters {
		builtClusters.Insert(c.Name)
	}
	finalDeletedClusters := slices.FilterInPlace(deletedClusters, func(cluster string) bool {
		return !builtClusters.Contains(cluster)
	})
	return clusters, finalDeletedClusters, log, true
}

// deltaFromServices computes the delta clusters from the updated services.
func (configgen *ConfigGeneratorImpl) deltaFromServices(key model.ConfigKey, proxy *model.Proxy, push *model.PushContext,
	serviceClusters map[string]sets.String, servicePortClusters map[string]map[int]string, subsetClusters map[string]sets.String,
) ([]*model.Service, []string) {
	var deletedClusters []string
	var services []*model.Service
	service := push.ServiceForHostname(proxy, host.Name(key.Name))
	// push.ServiceForHostname will return nil if the proxy doesn't care about the service OR it was deleted.
	// we can cross-reference with WatchedResources to figure out which services were deleted.
	if service == nil {
		// We assume a service was deleted and delete all clusters for that service.
		deletedClusters = append(deletedClusters, serviceClusters[key.Name].UnsortedList()...)
		deletedClusters = append(deletedClusters, subsetClusters[key.Name].UnsortedList()...)
	} else {
		// Service exists. If the service update has port change, we need to the corresponding port clusters.
		services = append(services, service)
		for port, cluster := range servicePortClusters[service.Hostname.String()] {
			// if this service port is removed, we can conclude that it is a removed cluster.
			if _, exists := service.Ports.GetByPort(port); !exists {
				deletedClusters = append(deletedClusters, cluster)
			}
		}
	}
	return services, deletedClusters
}

// deltaFromDestinationRules computes the delta clusters from the updated destination rules.
func (configgen *ConfigGeneratorImpl) deltaFromDestinationRules(updatedDr model.ConfigKey, proxy *model.Proxy,
	subsetClusters map[string]sets.String,
) ([]*model.Service, []string) {
	var deletedClusters []string
	var services []*model.Service
	cfg := proxy.SidecarScope.DestinationRuleByName(updatedDr.Name, updatedDr.Namespace)
	if cfg == nil {
		// Destinationrule was deleted. Find matching services from previous destinationrule.
		prevCfg := proxy.PrevSidecarScope.DestinationRuleByName(updatedDr.Name, updatedDr.Namespace)
		if prevCfg == nil {
			log.Debugf("Prev DestinationRule form PrevSidecarScope is missing for %s/%s", updatedDr.Namespace, updatedDr.Name)
			return nil, nil
		}
		dr := prevCfg.Spec.(*networking.DestinationRule)
		services = append(services, proxy.SidecarScope.ServicesForHostname(host.Name(dr.Host))...)
	} else {
		dr := cfg.Spec.(*networking.DestinationRule)
		// Destinationrule was updated. Find matching services from updated destinationrule.
		services = append(services, proxy.SidecarScope.ServicesForHostname(host.Name(dr.Host))...)
		// Check if destination rule host is changed, if yes, then we need to add previous host matching services.
		prevCfg := proxy.PrevSidecarScope.DestinationRuleByName(updatedDr.Name, updatedDr.Namespace)
		if prevCfg != nil {
			prevDr := prevCfg.Spec.(*networking.DestinationRule)
			if dr.Host != prevDr.Host {
				services = append(services, proxy.SidecarScope.ServicesForHostname(host.Name(prevDr.Host))...)
			}
		}
	}

	// Remove all matched service subsets. When we rebuild clusters, we will rebuild the subset clusters as well.
	// We can reconcile the actual subsets that are needed when we rebuild the clusters.
	for _, matchedSvc := range services {
		if subsetClusters[matchedSvc.Hostname.String()] != nil {
			deletedClusters = append(deletedClusters, subsetClusters[matchedSvc.Hostname.String()].UnsortedList()...)
		}
	}
	return services, deletedClusters
}

// buildClusters builds clusters for the proxy with the services passed.
func (configgen *ConfigGeneratorImpl) buildClusters(proxy *model.Proxy, req *model.PushRequest,
	services []*model.Service,
) ([]*discovery.Resource, model.XdsLogDetails) {
	clusters := make([]*cluster.Cluster, 0)
	resources := model.Resources{}
	envoyFilterPatches := req.Push.EnvoyFilters(proxy)
	cb := NewClusterBuilder(proxy, req, configgen.Cache)
	instances := proxy.ServiceInstances
	cacheStats := cacheStats{}
	switch proxy.Type {
	case model.SidecarProxy:
		// Setup outbound clusters
		outboundPatcher := clusterPatcher{efw: envoyFilterPatches, pctx: networking.EnvoyFilter_SIDECAR_OUTBOUND}
		ob, cs := configgen.buildOutboundClusters(cb, proxy, outboundPatcher, services)
		cacheStats = cacheStats.merge(cs)
		resources = append(resources, ob...)
		// Add a blackhole and passthrough cluster for catching traffic to unresolved routes
		clusters = outboundPatcher.conditionallyAppend(clusters, nil, cb.buildBlackHoleCluster(), cb.buildDefaultPassthroughCluster())
		clusters = append(clusters, outboundPatcher.insertedClusters()...)
		// Setup inbound clusters
		inboundPatcher := clusterPatcher{efw: envoyFilterPatches, pctx: networking.EnvoyFilter_SIDECAR_INBOUND}
		clusters = append(clusters, configgen.buildInboundClusters(cb, proxy, instances, inboundPatcher)...)
		if proxy.EnableHBONE() {
			clusters = append(clusters, configgen.buildInboundHBONEClusters())
		}
		// Pass through clusters for inbound traffic. These cluster bind loopback-ish src address to access node local service.
		clusters = inboundPatcher.conditionallyAppend(clusters, nil, cb.buildInboundPassthroughClusters()...)
		clusters = append(clusters, inboundPatcher.insertedClusters()...)
	case model.Waypoint:
		_, svcs := findWaypointResources(proxy, req.Push)
		// Waypoint proxies do not need outbound clusters in most cases, unless we have a route pointing to something
		outboundPatcher := clusterPatcher{efw: envoyFilterPatches, pctx: networking.EnvoyFilter_SIDECAR_OUTBOUND}
		ob, cs := configgen.buildOutboundClusters(cb, proxy, outboundPatcher, filterWaypointOutboundServices(req.Push.ServicesAttachedToMesh(), svcs, services))
		cacheStats = cacheStats.merge(cs)
		resources = append(resources, ob...)
		// Setup inbound clusters
		inboundPatcher := clusterPatcher{efw: envoyFilterPatches, pctx: networking.EnvoyFilter_SIDECAR_INBOUND}
		clusters = append(clusters, configgen.buildWaypointInboundClusters(cb, proxy, req.Push, svcs)...)
		clusters = append(clusters, inboundPatcher.insertedClusters()...)
	default: // Gateways
		patcher := clusterPatcher{efw: envoyFilterPatches, pctx: networking.EnvoyFilter_GATEWAY}
		ob, cs := configgen.buildOutboundClusters(cb, proxy, patcher, services)
		cacheStats = cacheStats.merge(cs)
		resources = append(resources, ob...)
		// Gateways do not require the default passthrough cluster as they do not have original dst listeners.
		clusters = patcher.conditionallyAppend(clusters, nil, cb.buildBlackHoleCluster())
		if proxy.Type == model.Router && proxy.MergedGateway != nil && proxy.MergedGateway.ContainsAutoPassthroughGateways {
			clusters = append(clusters, configgen.buildOutboundSniDnatClusters(proxy, req, patcher)...)
		}
		clusters = append(clusters, patcher.insertedClusters()...)
	}

	// OutboundTunnel cluster is needed for sidecar and gateway.
	if proxy.EnableHBONE() {
		clusters = append(clusters, cb.buildConnectOriginate(proxy, req.Push, nil))
	}

	// if credential socket exists, create a cluster for it
	if proxy.Metadata != nil && proxy.Metadata.Raw[security.CredentialMetaDataName] == "true" {
		clusters = append(clusters, cb.buildExternalSDSCluster(security.CredentialNameSocketPath))
	}
	for _, c := range clusters {
		resources = append(resources, &discovery.Resource{Name: c.Name, Resource: protoconv.MessageToAny(c)})
	}
	resources = cb.normalizeClusters(resources)

	if cacheStats.empty() {
		return resources, model.DefaultXdsLogDetails
	}
	return resources, model.XdsLogDetails{AdditionalInfo: fmt.Sprintf("cached:%v/%v", cacheStats.hits, cacheStats.hits+cacheStats.miss)}
}

func shouldUseDelta(updates *model.PushRequest) bool {
	return updates != nil && deltaAwareConfigTypes(updates.ConfigsUpdated) && len(updates.ConfigsUpdated) > 0
}

// deltaAwareConfigTypes returns true if all updated configs are delta enabled.
func deltaAwareConfigTypes(cfgs sets.Set[model.ConfigKey]) bool {
	for k := range cfgs {
		if !deltaConfigTypes.Contains(k.Kind.String()) {
			return false
		}
	}
	return true
}

// buildOutboundClusters generates all outbound (including subsets) clusters for a given proxy.
func (configgen *ConfigGeneratorImpl) buildOutboundClusters(cb *ClusterBuilder, proxy *model.Proxy, cp clusterPatcher,
	services []*model.Service,
) ([]*discovery.Resource, cacheStats) {
	resources := make([]*discovery.Resource, 0)
	efKeys := cp.efw.KeysApplyingTo(networking.EnvoyFilter_CLUSTER)
	hit, miss := 0, 0
	for _, service := range services {
		for _, port := range service.Ports {
			if port.Protocol == protocol.UDP {
				continue
			}
			clusterKey := buildClusterKey(service, port, cb, proxy, efKeys)
			cached, allFound := cb.getAllCachedSubsetClusters(*clusterKey)
			if allFound && !features.EnableUnsafeAssertions {
				hit += len(cached)
				resources = append(resources, cached...)
				continue
			}
			miss += len(cached)

			// We have a cache miss, so we will re-generate the cluster and later store it in the cache.
			lbEndpoints := cb.buildLocalityLbEndpoints(clusterKey.proxyView, service, port.Port, nil)

			// create default cluster
			discoveryType := convertResolution(cb.proxyType, service)
			defaultCluster := cb.buildDefaultCluster(clusterKey.clusterName, discoveryType, lbEndpoints, model.TrafficDirectionOutbound, port, service, nil)
			if defaultCluster == nil {
				continue
			}

			// if the service uses persistent sessions, override status allows
			// DRAINING endpoints to be kept as 'UNHEALTHY' coarse status in envoy.
			// Will not be used for normal traffic, only when explicit override.
			if service.Attributes.Labels[features.PersistentSessionLabel] != "" {
				// Default is UNKNOWN, HEALTHY, DEGRADED. Without this change, Envoy will drop endpoints with any other
				// status received in EDS. With this setting, the DRAINING and UNHEALTHY endpoints are kept - both marked
				// as UNHEALTHY ('coarse state'), which is what will show in config dumps.
				// DRAINING/UNHEALTHY will not be used normally for new requests. They will be used if cookie/header
				// selects them.
				defaultCluster.cluster.CommonLbConfig.OverrideHostStatus = &core.HealthStatusSet{
					Statuses: []core.HealthStatus{
						core.HealthStatus_HEALTHY,
						core.HealthStatus_DRAINING, core.HealthStatus_UNKNOWN, core.HealthStatus_DEGRADED,
					},
				}
			}

			// If stat name is configured, build the alternate stats name.
			if len(cb.req.Push.Mesh.OutboundClusterStatName) != 0 {
				defaultCluster.cluster.AltStatName = telemetry.BuildStatPrefix(cb.req.Push.Mesh.OutboundClusterStatName,
					string(service.Hostname), "", port, 0, &service.Attributes)
			}

			subsetClusters := cb.applyDestinationRule(defaultCluster, DefaultClusterMode, service, port,
				clusterKey.proxyView, clusterKey.destinationRule.GetRule(), clusterKey.serviceAccounts)

			if patched := cp.patch(nil, defaultCluster.build()); patched != nil {
				resources = append(resources, patched)
				if features.EnableCDSCaching {
					cb.cache.Add(clusterKey, cb.req, patched)
				}
			}
			for _, ss := range subsetClusters {
				if patched := cp.patch(nil, ss); patched != nil {
					resources = append(resources, patched)
					if features.EnableCDSCaching {
						nk := *clusterKey
						nk.clusterName = ss.Name
						cb.cache.Add(&nk, cb.req, patched)
					}
				}
			}
		}
	}

	return resources, cacheStats{hits: hit, miss: miss}
}

type clusterPatcher struct {
	efw  *model.EnvoyFilterWrapper
	pctx networking.EnvoyFilter_PatchContext
}

func (p clusterPatcher) patch(hosts []host.Name, c *cluster.Cluster) *discovery.Resource {
	cluster := p.doPatch(hosts, c)
	if cluster == nil {
		return nil
	}
	return &discovery.Resource{Name: cluster.Name, Resource: protoconv.MessageToAny(cluster)}
}

func (p clusterPatcher) doPatch(hosts []host.Name, c *cluster.Cluster) *cluster.Cluster {
	if !envoyfilter.ShouldKeepCluster(p.pctx, p.efw, c, hosts) {
		return nil
	}
	return envoyfilter.ApplyClusterMerge(p.pctx, p.efw, c, hosts)
}

func (p clusterPatcher) conditionallyAppend(l []*cluster.Cluster, hosts []host.Name, clusters ...*cluster.Cluster) []*cluster.Cluster {
	if !p.hasPatches() {
		return append(l, clusters...)
	}
	for _, c := range clusters {
		if patched := p.doPatch(hosts, c); patched != nil {
			l = append(l, patched)
		}
	}
	return l
}

func (p clusterPatcher) insertedClusters() []*cluster.Cluster {
	return envoyfilter.InsertedClusters(p.pctx, p.efw)
}

func (p clusterPatcher) hasPatches() bool {
	return p.efw != nil && len(p.efw.Patches[networking.EnvoyFilter_CLUSTER]) > 0
}

// SniDnat clusters do not have any TLS setting, as they simply forward traffic to upstream
// All SniDnat clusters are internal services in the mesh.
// TODO enable cache - there is no blockers here, skipped to simplify the original caching implementation
func (configgen *ConfigGeneratorImpl) buildOutboundSniDnatClusters(proxy *model.Proxy, req *model.PushRequest,
	cp clusterPatcher,
) []*cluster.Cluster {
	clusters := make([]*cluster.Cluster, 0)
	cb := NewClusterBuilder(proxy, req, nil)

	proxyView := proxy.GetView()

	for _, service := range proxy.SidecarScope.Services() {
		if service.MeshExternal {
			continue
		}

		destRule := proxy.SidecarScope.DestinationRule(model.TrafficDirectionOutbound, proxy, service.Hostname).GetRule()
		for _, port := range service.Ports {
			if port.Protocol == protocol.UDP {
				continue
			}
			lbEndpoints := cb.buildLocalityLbEndpoints(proxyView, service, port.Port, nil)

			// create default cluster
			discoveryType := convertResolution(cb.proxyType, service)

			clusterName := model.BuildDNSSrvSubsetKey(model.TrafficDirectionOutbound, "",
				service.Hostname, port.Port)
			defaultCluster := cb.buildDefaultCluster(clusterName, discoveryType, lbEndpoints, model.TrafficDirectionOutbound, port, service, nil)
			if defaultCluster == nil {
				continue
			}
			subsetClusters := cb.applyDestinationRule(defaultCluster, SniDnatClusterMode, service, port, proxyView, destRule, nil)
			clusters = cp.conditionallyAppend(clusters, nil, defaultCluster.build())
			clusters = cp.conditionallyAppend(clusters, nil, subsetClusters...)
		}
	}

	return clusters
}

func buildInboundLocalityLbEndpoints(bind string, port uint32) []*endpoint.LocalityLbEndpoints {
	if bind == "" {
		return nil
	}
	address := util.BuildAddress(bind, port)
	lbEndpoint := &endpoint.LbEndpoint{
		HostIdentifier: &endpoint.LbEndpoint_Endpoint{
			Endpoint: &endpoint.Endpoint{
				Address: address,
			},
		},
	}
	return []*endpoint.LocalityLbEndpoints{
		{
			LbEndpoints: []*endpoint.LbEndpoint{lbEndpoint},
		},
	}
}

func (configgen *ConfigGeneratorImpl) buildClustersFromServiceInstances(cb *ClusterBuilder, proxy *model.Proxy,
	instances []*model.ServiceInstance, cp clusterPatcher,
	enableSidecarServiceInboundListenerMerge bool,
) []*cluster.Cluster {
	clusters := make([]*cluster.Cluster, 0)
	_, actualLocalHosts := getWildcardsAndLocalHost(proxy.GetIPMode())
	clustersToBuild := make(map[int][]*model.ServiceInstance)

	ingressPortListSet := sets.New[int]()
	sidecarScope := proxy.SidecarScope
	if sidecarScope.HasIngressListener() {
		ingressPortListSet = getSidecarIngressPortList(proxy)
	}
	for _, instance := range instances {
		// For service instances with the same port,
		// we still need to capture all the instances on this port, as its required to populate telemetry metadata
		// The first instance will be used as the "primary" instance; this means if we have an conflicts between
		// Services the first one wins
		ep := int(instance.Endpoint.EndpointPort)
		clustersToBuild[ep] = append(clustersToBuild[ep], instance)
	}

	bind := actualLocalHosts[0]
	if features.EnableInboundPassthrough {
		bind = ""
	}
	// For each workload port, we will construct a cluster
	for epPort, instances := range clustersToBuild {
		if enableSidecarServiceInboundListenerMerge && sidecarScope.HasIngressListener() &&
			ingressPortListSet.Contains(int(instances[0].Endpoint.EndpointPort)) {
			// here if port is declared in service and sidecar ingress both, we continue to take the one on sidecar + other service ports
			// e.g. 1,2, 3 in service and 3,4 in sidecar ingress,
			// this will still generate listeners for 1,2,3,4 where 3 is picked from sidecar ingress
			// port present in sidecarIngress listener so let sidecar take precedence
			continue
		}
		services := slices.Map(instances, func(e *model.ServiceInstance) ServiceTarget {
			return ServiceTarget{
				Service: e.Service,
				Port: ServiceInstancePort{
					ServicePort: e.ServicePort,
					TargetPort:  e.Endpoint.EndpointPort,
				},
			}
		})
		localCluster := cb.buildInboundClusterForPortOrUDS(epPort, bind, proxy, services[0], services)
		// If inbound cluster match has service, we should see if it matches with any host name across all instances.
		hosts := make([]host.Name, 0, len(instances))
		for _, si := range instances {
			hosts = append(hosts, si.Service.Hostname)
		}
		clusters = cp.conditionallyAppend(clusters, hosts, localCluster.build())
	}
	return clusters
}

func (configgen *ConfigGeneratorImpl) buildInboundClusters(cb *ClusterBuilder, proxy *model.Proxy, instances []*model.ServiceInstance,
	cp clusterPatcher,
) []*cluster.Cluster {
	clusters := make([]*cluster.Cluster, 0)

	// The inbound clusters for a node depends on whether the node has a SidecarScope with inbound listeners
	// or not. If the node has a sidecarscope with ingress listeners, we only return clusters corresponding
	// to those listeners i.e. clusters made out of the defaultEndpoint field.
	// If the node has no sidecarScope and has interception mode set to NONE, then we should skip the inbound
	// clusters, because there would be no corresponding inbound listeners
	sidecarScope := proxy.SidecarScope
	noneMode := proxy.GetInterceptionMode() == model.InterceptionNone

	_, actualLocalHosts := getWildcardsAndLocalHost(proxy.GetIPMode())
	// No user supplied sidecar scope or the user supplied one has no ingress listeners
	if !sidecarScope.HasIngressListener() {
		// We should not create inbound listeners in NONE mode based on the service instances
		// Doing so will prevent the workloads from starting as they would be listening on the same port
		// Users are required to provide the sidecar config to define the inbound listeners
		if noneMode {
			return nil
		}
		clusters = configgen.buildClustersFromServiceInstances(cb, proxy, instances, cp, false)
		return clusters
	}

	if features.EnableSidecarServiceInboundListenerMerge {
		// only allow to merge inbound listeners if sidecar has ingress listener and pilot has env EnableSidecarServiceInboundListenerMerge set
		clusters = configgen.buildClustersFromServiceInstances(cb, proxy, instances, cp, true)
	}
	for _, ingressListener := range sidecarScope.Sidecar.Ingress {
		// LDS would have setup the inbound clusters
		// as inbound|portNumber|portName|Hostname[or]SidecarScopeID
		listenPort := &model.Port{
			Port:     int(ingressListener.Port.Number),
			Protocol: protocol.Parse(ingressListener.Port.Protocol),
			Name:     ingressListener.Port.Name,
		}

		// Set up the endpoint. By default, we set this empty which will use ORIGINAL_DST passthrough.
		// This can be overridden by ingress.defaultEndpoint.
		// * 127.0.0.1: send to localhost
		// * 0.0.0.0: send to INSTANCE_IP
		// * unix:///...: send to configured unix domain socket
		endpointAddress := ""
		port := 0
		if strings.HasPrefix(ingressListener.DefaultEndpoint, model.UnixAddressPrefix) {
			// this is a UDS endpoint. assign it as is
			endpointAddress = ingressListener.DefaultEndpoint
		} else if len(ingressListener.DefaultEndpoint) > 0 {
			// parse the ip, port. Validation guarantees presence of :
			hostIP, hostPort, hostErr := net.SplitHostPort(ingressListener.DefaultEndpoint)
			if hostPort == "" || hostErr != nil {
				continue
			}
			var err error
			if port, err = strconv.Atoi(hostPort); err != nil {
				continue
			}
			if hostIP == model.PodIPAddressPrefix {
				for _, proxyIPAddr := range cb.proxyIPAddresses {
					if netutil.IsIPv4Address(proxyIPAddr) {
						endpointAddress = proxyIPAddr
						break
					}
				}
				// if there is no any IPv4 address in proxyIPAddresses
				if endpointAddress == "" {
					endpointAddress = model.LocalhostAddressPrefix
				}
			} else if hostIP == model.PodIPv6AddressPrefix {
				for _, proxyIPAddr := range cb.proxyIPAddresses {
					if netutil.IsIPv6Address(proxyIPAddr) {
						endpointAddress = proxyIPAddr
						break
					}
				}
				// if there is no any IPv6 address in proxyIPAddresses
				if endpointAddress == "" {
					endpointAddress = model.LocalhostIPv6AddressPrefix
				}
			} else if hostIP == model.LocalhostAddressPrefix {
				// prefer 127.0.0.1 to ::1, but if given no option choose ::1
				ipV6EndpointAddress := ""
				for _, host := range actualLocalHosts {
					if netutil.IsIPv4Address(host) {
						endpointAddress = host
						break
					}
					if netutil.IsIPv6Address(host) {
						ipV6EndpointAddress = host
					}
				}
				if endpointAddress == "" {
					endpointAddress = ipV6EndpointAddress
				}
			} else if hostIP == model.LocalhostIPv6AddressPrefix {
				// prefer ::1 to 127.0.0.1, but if given no option choose 127.0.0.1
				ipV4EndpointAddress := ""
				for _, host := range actualLocalHosts {
					if netutil.IsIPv6Address(host) {
						endpointAddress = host
						break
					}
					if netutil.IsIPv4Address(host) {
						ipV4EndpointAddress = host
					}
				}
				if endpointAddress == "" {
					endpointAddress = ipV4EndpointAddress
				}
			}
		}
		// Find the service instance that corresponds to this ingress listener by looking
		// for a service instance that matches this ingress port as this will allow us
		// to generate the right cluster name that LDS expects inbound|portNumber|portName|Hostname
		svc := findOrCreateService(instances, ingressListener, sidecarScope.Name, sidecarScope.Namespace)
		endpoint := ServiceTarget{
			Service: svc,
			Port: ServiceInstancePort{
				ServicePort: listenPort,
				TargetPort:  uint32(port),
			},
		}
		localCluster := cb.buildInboundClusterForPortOrUDS(int(ingressListener.Port.Number), endpointAddress, proxy, endpoint, nil)
		clusters = cp.conditionallyAppend(clusters, []host.Name{endpoint.Service.Hostname}, localCluster.build())
	}
	return clusters
}

func findOrCreateService(instances []*model.ServiceInstance,
	ingressListener *networking.IstioIngressListener, sidecar string, sidecarns string,
) *model.Service {
	for _, realInstance := range instances {
		if realInstance.Endpoint.EndpointPort == ingressListener.Port.Number {
			return realInstance.Service
		}
	}
	// We didn't find a matching instance. Create a dummy one because we need the right
	// params to generate the right cluster name i.e. inbound|portNumber|portName|SidecarScopeID - which is uniformly generated by LDS/CDS.
	return &model.Service{
		Hostname: host.Name(sidecar + "." + sidecarns),
		Attributes: model.ServiceAttributes{
			Name: sidecar,
			// This will ensure that the right AuthN policies are selected
			Namespace: sidecarns,
		},
	}
}

func convertResolution(proxyType model.NodeType, service *model.Service) cluster.Cluster_DiscoveryType {
	switch service.Resolution {
	case model.ClientSideLB:
		return cluster.Cluster_EDS
	case model.DNSLB:
		return cluster.Cluster_STRICT_DNS
	case model.DNSRoundRobinLB:
		return cluster.Cluster_LOGICAL_DNS
	case model.Passthrough:
		// Gateways cannot use passthrough clusters. So fallback to EDS
		if proxyType == model.Router {
			return cluster.Cluster_EDS
		}
		if service.Attributes.ServiceRegistry == provider.Kubernetes && features.EnableEDSForHeadless {
			return cluster.Cluster_EDS
		}
		return cluster.Cluster_ORIGINAL_DST
	default:
		return cluster.Cluster_EDS
	}
}

// SelectTrafficPolicyComponents returns the components of TrafficPolicy that should be used for given port.
func selectTrafficPolicyComponents(policy *networking.TrafficPolicy) (
	*networking.ConnectionPoolSettings, *networking.OutlierDetection, *networking.LoadBalancerSettings, *networking.ClientTLSSettings,
) {
	if policy == nil {
		return nil, nil, nil, nil
	}
	connectionPool := policy.ConnectionPool
	outlierDetection := policy.OutlierDetection
	loadBalancer := policy.LoadBalancer
	tls := policy.Tls

	// Check if CA Certificate should be System CA Certificate
	if features.VerifyCertAtClient && tls != nil && tls.CaCertificates == "" {
		tls.CaCertificates = "system"
	}

	return connectionPool, outlierDetection, loadBalancer, tls
}

// ClusterMode defines whether the cluster is being built for SNI-DNATing (sni passthrough) or not
type ClusterMode string

const (
	// SniDnatClusterMode indicates cluster is being built for SNI dnat mode
	SniDnatClusterMode ClusterMode = "sni-dnat"
	// DefaultClusterMode indicates usual cluster with mTLS et al
	DefaultClusterMode ClusterMode = "outbound"
)

type buildClusterOpts struct {
	mesh             *meshconfig.MeshConfig
	mutable          *MutableCluster
	policy           *networking.TrafficPolicy
	port             *model.Port
	serviceAccounts  []string
	serviceInstances []*model.ServiceInstance
	// Used for traffic across multiple network clusters
	// the east-west gateway in a remote cluster will use this value to route
	// traffic to the appropriate service
	istioMtlsSni    string
	clusterMode     ClusterMode
	direction       model.TrafficDirection
	meshExternal    bool
	serviceMTLSMode model.MutualTLSMode
	// Indicates the service registry of the cluster being built.
	serviceRegistry provider.ID
	// Indicates if the destionationRule has a workloadSelector
	isDrWithSelector bool
}

func applyTCPKeepalive(mesh *meshconfig.MeshConfig, c *cluster.Cluster, tcp *networking.ConnectionPoolSettings_TCPSettings) {
	// Apply mesh wide TCP keepalive if available.
	setKeepAliveSettings(c, mesh.TcpKeepalive)

	// Apply/Override individual attributes with DestinationRule TCP keepalive if set.
	if tcp != nil {
		setKeepAliveSettings(c, tcp.TcpKeepalive)
	}
}

func setKeepAliveSettings(c *cluster.Cluster, keepalive *networking.ConnectionPoolSettings_TCPSettings_TcpKeepalive) {
	if keepalive == nil {
		return
	}
	// Start with empty tcp_keepalive, which would set SO_KEEPALIVE on the socket with OS default values.
	if c.UpstreamConnectionOptions == nil {
		c.UpstreamConnectionOptions = &cluster.UpstreamConnectionOptions{
			TcpKeepalive: &core.TcpKeepalive{},
		}
	}
	if keepalive.Probes > 0 {
		c.UpstreamConnectionOptions.TcpKeepalive.KeepaliveProbes = &wrappers.UInt32Value{Value: keepalive.Probes}
	}

	if keepalive.Time != nil {
		c.UpstreamConnectionOptions.TcpKeepalive.KeepaliveTime = &wrappers.UInt32Value{Value: uint32(keepalive.Time.Seconds)}
	}

	if keepalive.Interval != nil {
		c.UpstreamConnectionOptions.TcpKeepalive.KeepaliveInterval = &wrappers.UInt32Value{Value: uint32(keepalive.Interval.Seconds)}
	}
}

// FIXME: there isn't a way to distinguish between unset values and zero values
func applyOutlierDetection(c *cluster.Cluster, outlier *networking.OutlierDetection) {
	if outlier == nil {
		return
	}

	out := &cluster.OutlierDetection{}

	// SuccessRate based outlier detection should be disabled.
	out.EnforcingSuccessRate = &wrappers.UInt32Value{Value: 0}

	if e := outlier.Consecutive_5XxErrors; e != nil {
		v := e.GetValue()

		out.Consecutive_5Xx = &wrappers.UInt32Value{Value: v}

		if v > 0 {
			v = 100
		}
		out.EnforcingConsecutive_5Xx = &wrappers.UInt32Value{Value: v}
	}
	if e := outlier.ConsecutiveGatewayErrors; e != nil {
		v := e.GetValue()

		out.ConsecutiveGatewayFailure = &wrappers.UInt32Value{Value: v}

		if v > 0 {
			v = 100
		}
		out.EnforcingConsecutiveGatewayFailure = &wrappers.UInt32Value{Value: v}
	}

	if outlier.Interval != nil {
		out.Interval = outlier.Interval
	}
	if outlier.BaseEjectionTime != nil {
		out.BaseEjectionTime = outlier.BaseEjectionTime
	}
	if outlier.MaxEjectionPercent > 0 {
		out.MaxEjectionPercent = &wrappers.UInt32Value{Value: uint32(outlier.MaxEjectionPercent)}
	}

	if outlier.SplitExternalLocalOriginErrors {
		out.SplitExternalLocalOriginErrors = true
		if outlier.ConsecutiveLocalOriginFailures.GetValue() > 0 {
			out.ConsecutiveLocalOriginFailure = &wrappers.UInt32Value{Value: outlier.ConsecutiveLocalOriginFailures.Value}
			out.EnforcingConsecutiveLocalOriginFailure = &wrappers.UInt32Value{Value: 100}
		}
		// SuccessRate based outlier detection should be disabled.
		out.EnforcingLocalOriginSuccessRate = &wrappers.UInt32Value{Value: 0}
	}

	c.OutlierDetection = out

	// Disable panic threshold by default as its not typically applicable in k8s environments
	// with few pods per service.
	// To do so, set the healthy_panic_threshold field even if its value is 0 (defaults to 50 in Envoy).
	// FIXME: we can't distinguish between it being unset or being explicitly set to 0
	minHealthPercent := outlier.MinHealthPercent
	if minHealthPercent >= 0 {
		// When we are sending unhealthy endpoints, we should disable Panic Threshold. Otherwise
		// Envoy will send traffic to "Unready" pods when the percentage of healthy hosts fall
		// below minimum health percentage.
		if features.SendUnhealthyEndpoints.Load() {
			minHealthPercent = 0
		}
		c.CommonLbConfig.HealthyPanicThreshold = &xdstype.Percent{Value: float64(minHealthPercent)}
	}
}

func defaultLBAlgorithm() cluster.Cluster_LbPolicy {
	return cluster.Cluster_LEAST_REQUEST
}

func applyLoadBalancer(c *cluster.Cluster, lb *networking.LoadBalancerSettings, port *model.Port,
	locality *core.Locality, proxyLabels map[string]string, meshConfig *meshconfig.MeshConfig,
) {
	// Disable panic threshold when SendUnhealthyEndpoints is enabled as enabling it "may" send traffic to unready
	// end points when load balancer is in panic mode.
	if features.SendUnhealthyEndpoints.Load() {
		c.CommonLbConfig.HealthyPanicThreshold = &xdstype.Percent{Value: 0}
	}
	localityLbSetting := loadbalancer.GetLocalityLbSetting(meshConfig.GetLocalityLbSetting(), lb.GetLocalityLbSetting())
	if localityLbSetting != nil {
		c.CommonLbConfig.LocalityConfigSpecifier = &cluster.Cluster_CommonLbConfig_LocalityWeightedLbConfig_{
			LocalityWeightedLbConfig: &cluster.Cluster_CommonLbConfig_LocalityWeightedLbConfig{},
		}
	}
	// Use locality lb settings from load balancer settings if present, else use mesh wide locality lb settings
	applyLocalityLBSetting(locality, proxyLabels, c, localityLbSetting)

	if c.GetType() == cluster.Cluster_ORIGINAL_DST {
		c.LbPolicy = cluster.Cluster_CLUSTER_PROVIDED
		return
	}

	// Redis protocol must be defaulted with MAGLEV to benefit from client side sharding.
	if features.EnableRedisFilter && port != nil && port.Protocol == protocol.Redis {
		c.LbPolicy = cluster.Cluster_MAGLEV
		return
	}

	// DO not do if else here. since lb.GetSimple returns a enum value (not pointer).
	switch lb.GetSimple() {
	// nolint: staticcheck
	case networking.LoadBalancerSettings_LEAST_CONN, networking.LoadBalancerSettings_LEAST_REQUEST:
		applyLeastRequestLoadBalancer(c, lb)
	case networking.LoadBalancerSettings_RANDOM:
		c.LbPolicy = cluster.Cluster_RANDOM
	case networking.LoadBalancerSettings_ROUND_ROBIN:
		applyRoundRobinLoadBalancer(c, lb)
	case networking.LoadBalancerSettings_PASSTHROUGH:
		c.LbPolicy = cluster.Cluster_CLUSTER_PROVIDED
		c.ClusterDiscoveryType = &cluster.Cluster_Type{Type: cluster.Cluster_ORIGINAL_DST}
		// Wipe out any LoadAssignment, if set. This can occur when we have a STATIC Service but PASSTHROUGH traffic policy
		c.LoadAssignment = nil
	default:
		applySimpleDefaultLoadBalancer(c, lb)
	}

	ApplyRingHashLoadBalancer(c, lb)
}

// applySimpleDefaultLoadBalancer will set the DefaultLBPolicy and create an LbConfig if used in LoadBalancerSettings
func applySimpleDefaultLoadBalancer(c *cluster.Cluster, loadbalancer *networking.LoadBalancerSettings) {
	c.LbPolicy = defaultLBAlgorithm()
	switch c.LbPolicy {
	case cluster.Cluster_ROUND_ROBIN:
		applyRoundRobinLoadBalancer(c, loadbalancer)
	case cluster.Cluster_LEAST_REQUEST:
		applyLeastRequestLoadBalancer(c, loadbalancer)
	}
}

// applyRoundRobinLoadBalancer will set the LbPolicy and create an LbConfig for ROUND_ROBIN if used in LoadBalancerSettings
func applyRoundRobinLoadBalancer(c *cluster.Cluster, loadbalancer *networking.LoadBalancerSettings) {
	c.LbPolicy = cluster.Cluster_ROUND_ROBIN

	if loadbalancer.GetWarmupDurationSecs() != nil {
		c.LbConfig = &cluster.Cluster_RoundRobinLbConfig_{
			RoundRobinLbConfig: &cluster.Cluster_RoundRobinLbConfig{
				SlowStartConfig: setSlowStartConfig(loadbalancer.GetWarmupDurationSecs()),
			},
		}
	}
}

// applyLeastRequestLoadBalancer will set the LbPolicy and create an LbConfig for LEAST_REQUEST if used in LoadBalancerSettings
func applyLeastRequestLoadBalancer(c *cluster.Cluster, loadbalancer *networking.LoadBalancerSettings) {
	c.LbPolicy = cluster.Cluster_LEAST_REQUEST

	if loadbalancer.GetWarmupDurationSecs() != nil {
		c.LbConfig = &cluster.Cluster_LeastRequestLbConfig_{
			LeastRequestLbConfig: &cluster.Cluster_LeastRequestLbConfig{
				SlowStartConfig: setSlowStartConfig(loadbalancer.GetWarmupDurationSecs()),
			},
		}
	}
}

// setSlowStartConfig will set the warmupDurationSecs for LEAST_REQUEST and ROUND_ROBIN if provided in DestinationRule
func setSlowStartConfig(dur *durationpb.Duration) *cluster.Cluster_SlowStartConfig {
	return &cluster.Cluster_SlowStartConfig{
		SlowStartWindow: dur,
	}
}

// ApplyRingHashLoadBalancer will set the LbPolicy and create an LbConfig for RING_HASH if  used in LoadBalancerSettings
func ApplyRingHashLoadBalancer(c *cluster.Cluster, lb *networking.LoadBalancerSettings) {
	consistentHash := lb.GetConsistentHash()
	if consistentHash == nil {
		return
	}

	switch {
	case consistentHash.GetMaglev() != nil:
		c.LbPolicy = cluster.Cluster_MAGLEV
		if consistentHash.GetMaglev().TableSize != 0 {
			c.LbConfig = &cluster.Cluster_MaglevLbConfig_{
				MaglevLbConfig: &cluster.Cluster_MaglevLbConfig{
					TableSize: &wrappers.UInt64Value{Value: consistentHash.GetMaglev().TableSize},
				},
			}
		}
	case consistentHash.GetRingHash() != nil:
		c.LbPolicy = cluster.Cluster_RING_HASH
		if consistentHash.GetRingHash().MinimumRingSize != 0 {
			c.LbConfig = &cluster.Cluster_RingHashLbConfig_{
				RingHashLbConfig: &cluster.Cluster_RingHashLbConfig{
					MinimumRingSize: &wrappers.UInt64Value{Value: consistentHash.GetRingHash().MinimumRingSize},
				},
			}
		}
	default:
		// Check the deprecated MinimumRingSize.
		// TODO: MinimumRingSize is an int, and zero could potentially
		// be a valid value unable to distinguish between set and unset
		// case currently.
		// 1024 is the default value for envoy.
		minRingSize := &wrappers.UInt64Value{Value: 1024}

		if consistentHash.MinimumRingSize != 0 { // nolint: staticcheck
			minRingSize = &wrappers.UInt64Value{Value: consistentHash.GetMinimumRingSize()} // nolint: staticcheck
		}
		c.LbPolicy = cluster.Cluster_RING_HASH
		c.LbConfig = &cluster.Cluster_RingHashLbConfig_{
			RingHashLbConfig: &cluster.Cluster_RingHashLbConfig{
				MinimumRingSize: minRingSize,
			},
		}
	}
}

func applyLocalityLBSetting(locality *core.Locality, proxyLabels map[string]string, cluster *cluster.Cluster,
	localityLB *networking.LocalityLoadBalancerSetting,
) {
	// Failover should only be applied with outlier detection, or traffic will never failover.
	enabledFailover := cluster.OutlierDetection != nil
	if cluster.LoadAssignment != nil {
		// TODO: enable failoverPriority for `STRICT_DNS` cluster type
		loadbalancer.ApplyLocalityLBSetting(cluster.LoadAssignment, nil, locality, proxyLabels, localityLB, enabledFailover)
	}
}

func addTelemetryMetadata(opts buildClusterOpts, service *model.Service, direction model.TrafficDirection, inboundServices []ServiceTarget) {
	if !features.EnableTelemetryLabel {
		return
	}
	if opts.mutable.cluster == nil {
		return
	}
	if direction == model.TrafficDirectionInbound && (len(inboundServices) == 0 || opts.port == nil) {
		// At inbound, port and local service instance has to be provided
		return
	}
	if direction == model.TrafficDirectionOutbound && service == nil {
		// At outbound, the service corresponding to the cluster has to be provided.
		return
	}

	im := getOrCreateIstioMetadata(opts.mutable.cluster)

	// Add services field into istio metadata
	im.Fields["services"] = &structpb.Value{
		Kind: &structpb.Value_ListValue{
			ListValue: &structpb.ListValue{
				Values: []*structpb.Value{},
			},
		},
	}

	svcMetaList := im.Fields["services"].GetListValue()

	// Add service related metadata. This will be consumed by telemetry v2 filter for metric labels.
	if direction == model.TrafficDirectionInbound {
		// For inbound cluster, add all services on the cluster port
		have := make(map[host.Name]bool)
		for _, svc := range inboundServices {
			if svc.Port.Port != opts.port.Port {
				// If the service port is different from the port of the cluster that is being built,
				// skip adding telemetry metadata for the service to the cluster.
				continue
			}
			if _, ok := have[svc.Service.Hostname]; ok {
				// Skip adding metadata for instance with the same host name.
				// This could happen when a service has multiple IPs.
				continue
			}
			svcMetaList.Values = append(svcMetaList.Values, buildServiceMetadata(svc.Service))
			have[svc.Service.Hostname] = true
		}
	} else if direction == model.TrafficDirectionOutbound {
		// For outbound cluster, add telemetry metadata based on the service that the cluster is built for.
		svcMetaList.Values = append(svcMetaList.Values, buildServiceMetadata(service))
	}
}

// Build a struct which contains service metadata and will be added into cluster label.
func buildServiceMetadata(svc *model.Service) *structpb.Value {
	return &structpb.Value{
		Kind: &structpb.Value_StructValue{
			StructValue: &structpb.Struct{
				Fields: map[string]*structpb.Value{
					// service fqdn
					"host": {
						Kind: &structpb.Value_StringValue{
							StringValue: string(svc.Hostname),
						},
					},
					// short name of the service
					"name": {
						Kind: &structpb.Value_StringValue{
							StringValue: svc.Attributes.Name,
						},
					},
					// namespace of the service
					"namespace": {
						Kind: &structpb.Value_StringValue{
							StringValue: svc.Attributes.Namespace,
						},
					},
				},
			},
		},
	}
}

func getOrCreateIstioMetadata(cluster *cluster.Cluster) *structpb.Struct {
	if cluster.Metadata == nil {
		cluster.Metadata = &core.Metadata{
			FilterMetadata: map[string]*structpb.Struct{},
		}
	}
	// Create Istio metadata if does not exist yet
	if _, ok := cluster.Metadata.FilterMetadata[util.IstioMetadataKey]; !ok {
		cluster.Metadata.FilterMetadata[util.IstioMetadataKey] = &structpb.Struct{
			Fields: map[string]*structpb.Value{},
		}
	}
	return cluster.Metadata.FilterMetadata[util.IstioMetadataKey]
}

var HboneOrPlaintextSocket = []*cluster.Cluster_TransportSocketMatch{
	hboneTransportSocket,
	defaultTransportSocketMatch(),
}

var InternalUpstreamSocket = &core.TransportSocket{
	Name: TransportSocketInternalUpstream,
	ConfigType: &core.TransportSocket_TypedConfig{TypedConfig: protoconv.MessageToAny(&internalupstream.InternalUpstreamTransport{
		PassthroughMetadata: []*internalupstream.InternalUpstreamTransport_MetadataValueSource{
			{
				Kind: &metadata.MetadataKind{Kind: &metadata.MetadataKind_Host_{}},
				Name: "tunnel",
			},
			{
				Kind: &metadata.MetadataKind{Kind: &metadata.MetadataKind_Cluster_{
					Cluster: &metadata.MetadataKind_Cluster{},
				}},
				Name: "istio",
			},
			{
				Kind: &metadata.MetadataKind{Kind: &metadata.MetadataKind_Host_{
					Host: &metadata.MetadataKind_Host{},
				}},
				Name: "istio",
			},
		},
		TransportSocket: xdsfilters.RawBufferTransportSocket,
	})},
}
