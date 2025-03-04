// SPDX-License-Identifier: Apache-2.0
// Copyright 2018-2021 Authors of Cilium

package k8s

import (
	"net"

	"github.com/cilium/cilium/pkg/datapath"
	"github.com/cilium/cilium/pkg/ip"
	slim_corev1 "github.com/cilium/cilium/pkg/k8s/slim/k8s/api/core/v1"
	slim_discovery_v1 "github.com/cilium/cilium/pkg/k8s/slim/k8s/api/discovery/v1"
	slim_discovery_v1beta1 "github.com/cilium/cilium/pkg/k8s/slim/k8s/api/discovery/v1beta1"
	"github.com/cilium/cilium/pkg/loadbalancer"
	"github.com/cilium/cilium/pkg/lock"
	"github.com/cilium/cilium/pkg/logging/logfields"
	"github.com/cilium/cilium/pkg/option"
	serviceStore "github.com/cilium/cilium/pkg/service/store"
	core_v1 "k8s.io/api/core/v1"

	"github.com/davecgh/go-spew/spew"
	"github.com/sirupsen/logrus"
)

// CacheAction is the type of action that was performed on the cache
type CacheAction int

const (
	// UpdateService reflects that the service was updated or added
	UpdateService CacheAction = iota

	// DeleteService reflects that the service was deleted
	DeleteService
)

// Used to implement the topology aware hints.
const LabelTopologyZone = "topology.kubernetes.io/zone"

// String returns the cache action as a string
func (c CacheAction) String() string {
	switch c {
	case UpdateService:
		return "service-updated"
	case DeleteService:
		return "service-deleted"
	default:
		return "unknown"
	}
}

// ServiceEvent is emitted via the Events channel of ServiceCache and describes
// the change that occurred in the cache
type ServiceEvent struct {
	// Action is the action that was performed in the cache
	Action CacheAction

	// ID is the identified of the service
	ID ServiceID

	// Service is the service structure
	Service *Service

	// OldService is the service structure
	OldService *Service

	// Endpoints is the endpoints structured correlated with the service
	Endpoints *Endpoints

	// SWG provides a mechanism to detect if a service was synchronized with
	// the datapath.
	SWG *lock.StoppableWaitGroup
}

// ServiceCache is a list of services correlated with the matching endpoints.
// The Events member will receive events as services.
type ServiceCache struct {
	Events chan ServiceEvent

	// mutex protects the maps below including the concurrent access of each
	// value.
	mutex    lock.RWMutex
	services map[ServiceID]*Service
	// endpoints maps a service to a map of EndpointSlices. In case the cluster
	// is still using the v1.Endpoints, the key used in the internal map of
	// EndpointSlices is the v1.Endpoint name.
	endpoints map[ServiceID]*EndpointSlices

	// externalEndpoints is a list of additional service backends derived from source other than the local cluster
	externalEndpoints map[ServiceID]externalEndpoints

	nodeAddressing datapath.NodeAddressing

	selfNodeZoneLabel string
}

// NewServiceCache returns a new ServiceCache
func NewServiceCache(nodeAddressing datapath.NodeAddressing) ServiceCache {
	return ServiceCache{
		services:          map[ServiceID]*Service{},
		endpoints:         map[ServiceID]*EndpointSlices{},
		externalEndpoints: map[ServiceID]externalEndpoints{},
		Events:            make(chan ServiceEvent, option.Config.K8sServiceCacheSize),
		nodeAddressing:    nodeAddressing,
	}
}

// GetServiceIP returns a random L3n4Addr that is backing the given Service ID.
// The returned IP is with external scope since its string representation might
// be used for net Dialer.
func (s *ServiceCache) GetServiceIP(svcID ServiceID) *loadbalancer.L3n4Addr {
	s.mutex.RLock()
	defer s.mutex.RUnlock()
	svc := s.services[svcID]
	if svc == nil || len(svc.FrontendIPs) == 0 || len(svc.Ports) == 0 {
		return nil
	}

	feIP := ip.GetIPFromListByFamily(svc.FrontendIPs, option.Config.EnableIPv4)
	if feIP == nil {
		return nil
	}

	for _, port := range svc.Ports {
		return loadbalancer.NewL3n4Addr(port.Protocol, feIP, port.Port,
			loadbalancer.ScopeExternal)
	}
	return nil
}

// GetServiceFrontendIP returns the frontend IP (aka clusterIP) for the given service with type.
func (s *ServiceCache) GetServiceFrontendIP(svcID ServiceID, svcType loadbalancer.SVCType) net.IP {
	s.mutex.RLock()
	defer s.mutex.RUnlock()
	svc := s.services[svcID]
	if svc == nil || svc.Type != svcType || len(svc.FrontendIPs) == 0 {
		return nil
	}

	return ip.GetIPFromListByFamily(svc.FrontendIPs, option.Config.EnableIPv4)
}

// GetServiceAddrsWithType returns a map of all the ports and slice of L3n4Addr that are backing the
// given Service ID with given type. It also returns the number of frontend IPs associated with the service.
// Note: The returned IPs are with External scope.
func (s *ServiceCache) GetServiceAddrsWithType(svcID ServiceID,
	svcType loadbalancer.SVCType) (map[loadbalancer.FEPortName][]*loadbalancer.L3n4Addr, int) {
	s.mutex.RLock()
	defer s.mutex.RUnlock()
	svc := s.services[svcID]
	if svc == nil || svc.Type != svcType || len(svc.FrontendIPs) == 0 {
		return nil, 0
	}

	addrsByPort := make(map[loadbalancer.FEPortName][]*loadbalancer.L3n4Addr)
	for pName, l4Addr := range svc.Ports {
		addrs := make([]*loadbalancer.L3n4Addr, 0, len(svc.FrontendIPs))
		for _, feIP := range svc.FrontendIPs {
			if isValidServiceFrontendIP(feIP) {
				addrs = append(addrs, loadbalancer.NewL3n4Addr(l4Addr.Protocol, feIP, l4Addr.Port, loadbalancer.ScopeExternal))
			}
		}

		addrsByPort[pName] = addrs
	}

	return addrsByPort, len(svc.FrontendIPs)
}

// GetEndpointsOfService returns all the endpoints that correlate with a
// service given a ServiceID.
func (s *ServiceCache) GetEndpointsOfService(svcID ServiceID) *Endpoints {
	s.mutex.RLock()
	defer s.mutex.RUnlock()
	eps, ok := s.endpoints[svcID]
	if !ok {
		return nil
	}
	return eps.GetEndpoints()
}

// GetNodeAddressing returns the registered node addresses to this service cache.
func (s *ServiceCache) GetNodeAddressing() datapath.NodeAddressing {
	return s.nodeAddressing
}

// UpdateService parses a Kubernetes service and adds or updates it in the
// ServiceCache. Returns the ServiceID unless the Kubernetes service could not
// be parsed and a bool to indicate whether the service was changed in the
// cache or not.
func (s *ServiceCache) UpdateService(k8sSvc *slim_corev1.Service, swg *lock.StoppableWaitGroup) ServiceID {
	svcID, newService := ParseService(k8sSvc, s.nodeAddressing)
	if newService == nil {
		return svcID
	}

	s.mutex.Lock()
	defer s.mutex.Unlock()

	oldService, ok := s.services[svcID]
	if ok {
		if oldService.DeepEqual(newService) {
			return svcID
		}
	}

	s.services[svcID] = newService

	// Check if the corresponding Endpoints resource is already available
	endpoints, serviceReady := s.correlateEndpoints(svcID)
	if serviceReady {
		swg.Add()
		s.Events <- ServiceEvent{
			Action:     UpdateService,
			ID:         svcID,
			Service:    newService,
			OldService: oldService,
			Endpoints:  endpoints,
			SWG:        swg,
		}
	}

	return svcID
}

func (s *ServiceCache) EnsureService(svcID ServiceID, swg *lock.StoppableWaitGroup) bool {
	s.mutex.RLock()
	defer s.mutex.RUnlock()
	if svc, found := s.services[svcID]; found {
		if endpoints, serviceReady := s.correlateEndpoints(svcID); serviceReady {
			swg.Add()
			s.Events <- ServiceEvent{
				Action:     UpdateService,
				ID:         svcID,
				Service:    svc,
				OldService: svc,
				Endpoints:  endpoints,
				SWG:        swg,
			}
			return true
		}
	}
	return false
}

// DeleteService parses a Kubernetes service and removes it from the
// ServiceCache
func (s *ServiceCache) DeleteService(k8sSvc *slim_corev1.Service, swg *lock.StoppableWaitGroup) {
	svcID := ParseServiceID(k8sSvc)

	s.mutex.Lock()
	defer s.mutex.Unlock()

	oldService, serviceOK := s.services[svcID]
	endpoints, _ := s.correlateEndpoints(svcID)
	delete(s.services, svcID)

	if serviceOK {
		swg.Add()
		s.Events <- ServiceEvent{
			Action:    DeleteService,
			ID:        svcID,
			Service:   oldService,
			Endpoints: endpoints,
			SWG:       swg,
		}
	}
}

func (s *ServiceCache) updateEndpoints(esID EndpointSliceID, newEndpoints *Endpoints, swg *lock.StoppableWaitGroup) (ServiceID, *Endpoints) {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	eps, ok := s.endpoints[esID.ServiceID]
	if ok {
		if eps.epSlices[esID.EndpointSliceName].DeepEqual(newEndpoints) {
			return esID.ServiceID, newEndpoints
		}
	} else {
		eps = newEndpointsSlices()
		s.endpoints[esID.ServiceID] = eps
	}

	eps.Upsert(esID.EndpointSliceName, newEndpoints)

	// Check if the corresponding Endpoints resource is already available
	svc, ok := s.services[esID.ServiceID]
	endpoints, serviceReady := s.correlateEndpoints(esID.ServiceID)
	if ok && serviceReady {
		swg.Add()
		s.Events <- ServiceEvent{
			Action:    UpdateService,
			ID:        esID.ServiceID,
			Service:   svc,
			Endpoints: endpoints,
			SWG:       swg,
		}
	}

	return esID.ServiceID, endpoints
}

// UpdateEndpoints parses a Kubernetes endpoints and adds or updates it in the
// ServiceCache. Returns the ServiceID unless the Kubernetes endpoints could not
// be parsed and a bool to indicate whether the endpoints was changed in the
// cache or not.
func (s *ServiceCache) UpdateEndpoints(k8sEndpoints *slim_corev1.Endpoints, swg *lock.StoppableWaitGroup) (ServiceID, *Endpoints) {
	svcID, newEndpoints := ParseEndpoints(k8sEndpoints)
	epSliceID := EndpointSliceID{
		ServiceID:         svcID,
		EndpointSliceName: k8sEndpoints.GetName(),
	}
	return s.updateEndpoints(epSliceID, newEndpoints, swg)
}

func (s *ServiceCache) UpdateEndpointSlicesV1(epSlice *slim_discovery_v1.EndpointSlice, swg *lock.StoppableWaitGroup) (ServiceID, *Endpoints) {
	svcID, newEndpoints := ParseEndpointSliceV1(epSlice)

	return s.updateEndpoints(svcID, newEndpoints, swg)
}

func (s *ServiceCache) UpdateEndpointSlicesV1Beta1(epSlice *slim_discovery_v1beta1.EndpointSlice, swg *lock.StoppableWaitGroup) (ServiceID, *Endpoints) {
	svcID, newEndpoints := ParseEndpointSliceV1Beta1(epSlice)

	return s.updateEndpoints(svcID, newEndpoints, swg)
}

func (s *ServiceCache) deleteEndpoints(svcID EndpointSliceID, swg *lock.StoppableWaitGroup) ServiceID {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	svc, serviceOK := s.services[svcID.ServiceID]
	isEmpty := s.endpoints[svcID.ServiceID].Delete(svcID.EndpointSliceName)
	if isEmpty {
		delete(s.endpoints, svcID.ServiceID)
	}
	endpoints, _ := s.correlateEndpoints(svcID.ServiceID)

	if serviceOK {
		swg.Add()
		event := ServiceEvent{
			Action:    UpdateService,
			ID:        svcID.ServiceID,
			Service:   svc,
			Endpoints: endpoints,
			SWG:       swg,
		}

		s.Events <- event
	}

	return svcID.ServiceID
}

// DeleteEndpoints parses a Kubernetes endpoints and removes it from the
// ServiceCache
func (s *ServiceCache) DeleteEndpoints(k8sEndpoints *slim_corev1.Endpoints, swg *lock.StoppableWaitGroup) ServiceID {
	svcID := ParseEndpointsID(k8sEndpoints)
	epSliceID := EndpointSliceID{
		ServiceID:         svcID,
		EndpointSliceName: k8sEndpoints.GetName(),
	}
	return s.deleteEndpoints(epSliceID, swg)
}

func (s *ServiceCache) DeleteEndpointSlices(epSlice endpointSlice, swg *lock.StoppableWaitGroup) ServiceID {
	svcID := ParseEndpointSliceID(epSlice)

	return s.deleteEndpoints(svcID, swg)
}

// FrontendList is the list of all k8s service frontends
type FrontendList map[string]struct{}

// LooseMatch returns true if the provided frontend is found in the
// FrontendList. If the frontend has a protocol value set, it only matches a
// k8s service with a matching protocol. If no protocol is set, any k8s service
// matching frontend IP and port is considered a match, regardless of protocol.
func (l FrontendList) LooseMatch(frontend loadbalancer.L3n4Addr) (exists bool) {
	switch frontend.Protocol {
	case loadbalancer.NONE:
		for _, protocol := range loadbalancer.AllProtocols {
			frontend.Protocol = protocol
			_, exists = l[frontend.StringWithProtocol()]
			if exists {
				return
			}
		}

	// If the protocol is set, perform an exact match
	default:
		_, exists = l[frontend.StringWithProtocol()]
	}
	return
}

// UniqueServiceFrontends returns all externally scoped services known to
// the service cache as a map, indexed by the string representation of a
// loadbalancer.L3n4Addr. This helper is only used in unit tests.
func (s *ServiceCache) UniqueServiceFrontends() FrontendList {
	uniqueFrontends := FrontendList{}

	s.mutex.RLock()
	defer s.mutex.RUnlock()

	for _, svc := range s.services {
		for _, feIP := range svc.FrontendIPs {
			for _, p := range svc.Ports {
				address := loadbalancer.L3n4Addr{
					IP:     feIP,
					L4Addr: *p,
					Scope:  loadbalancer.ScopeExternal,
				}
				uniqueFrontends[address.StringWithProtocol()] = struct{}{}
			}
		}

		for _, nodePortFEs := range svc.NodePorts {
			for _, fe := range nodePortFEs {
				if fe.Scope == loadbalancer.ScopeExternal {
					uniqueFrontends[fe.StringWithProtocol()] = struct{}{}
				}
			}
		}
	}

	return uniqueFrontends
}

// filterEndpoints filters local endpoints by using k8s service heuristics.
// For now it only implements the topology aware hints.
func (s *ServiceCache) filterEndpoints(localEndpoints *Endpoints, svc *Service) *Endpoints {
	if !option.Config.EnableServiceTopology || svc == nil || !svc.TopologyAware {
		return localEndpoints
	}

	if s.selfNodeZoneLabel == "" {
		// The node doesn't have the zone label set, so we cannot filter endpoints
		// by zone. Therefore, return all endpoints.
		return localEndpoints
	}

	if svc.TrafficPolicy == loadbalancer.SVCTrafficPolicyLocal {
		// According to https://kubernetes.io/docs/concepts/services-networking/topology-aware-hints/#constraints:
		// """
		// Topology Aware Hints are not used when either externalTrafficPolicy or
		// internalTrafficPolicy is set to Local on a Service.
		// """
		return localEndpoints
	}

	filteredEndpoints := &Endpoints{Backends: map[string]*Backend{}}

	for key, backend := range localEndpoints.Backends {
		if len(backend.HintsForZones) == 0 {
			return localEndpoints
		}

		for _, hint := range backend.HintsForZones {
			if hint == s.selfNodeZoneLabel {
				filteredEndpoints.Backends[key] = backend
				break
			}
		}
	}

	if len(filteredEndpoints.Backends) == 0 {
		// Fallback to all endpoints if there is no any which could match
		// the zone. Otherwise, the node will start dropping requests to
		// the service.
		return localEndpoints
	}

	return filteredEndpoints
}

// correlateEndpoints builds a combined Endpoints of the local endpoints and
// all external endpoints if the service is marked as a global service. Also
// returns a boolean that indicates whether the service is ready to be plumbed,
// this is true if:
// A local endpoints resource is present. Regardless whether the
//    endpoints resource contains actual backends or not.
// OR Remote endpoints exist which correlate to the service.
func (s *ServiceCache) correlateEndpoints(id ServiceID) (*Endpoints, bool) {
	endpoints := newEndpoints()

	localEndpoints := s.endpoints[id].GetEndpoints()
	svc, svcFound := s.services[id]

	hasLocalEndpoints := localEndpoints != nil
	if hasLocalEndpoints {
		localEndpoints = s.filterEndpoints(localEndpoints, svc)

		for ip, e := range localEndpoints.Backends {
			endpoints.Backends[ip] = e
		}
	}

	if svcFound && svc.IncludeExternal {
		externalEndpoints, hasExternalEndpoints := s.externalEndpoints[id]
		if hasExternalEndpoints {
			// remote cluster endpoints already contain all Endpoints from all
			// EndpointSlices so no need to search the endpoints of a particular
			// EndpointSlice.
			for clusterName, remoteClusterEndpoints := range externalEndpoints.endpoints {
				for ip, e := range remoteClusterEndpoints.Backends {
					if _, ok := endpoints.Backends[ip]; ok {
						log.WithFields(logrus.Fields{
							logfields.K8sSvcName:   id.Name,
							logfields.K8sNamespace: id.Namespace,
							logfields.IPAddr:       ip,
							"cluster":              clusterName,
						}).Warning("Conflicting service backend IP")
					} else {
						endpoints.Backends[ip] = e
					}
				}
			}
		}
	}

	// Report the service as ready if a local endpoints object exists or if
	// external endpoints have have been identified
	return endpoints, hasLocalEndpoints || len(endpoints.Backends) > 0
}

// MergeExternalServiceUpdate merges a cluster service of a remote cluster into
// the local service cache. The service endpoints are stored as external endpoints
// and are correlated on demand with local services via correlateEndpoints().
func (s *ServiceCache) MergeExternalServiceUpdate(service *serviceStore.ClusterService, swg *lock.StoppableWaitGroup) {
	// Ignore updates of own cluster
	if service.Cluster == option.Config.ClusterName {
		return
	}

	s.mutex.Lock()
	defer s.mutex.Unlock()

	s.mergeServiceUpdateLocked(service, nil, swg)
}

func (s *ServiceCache) mergeServiceUpdateLocked(service *serviceStore.ClusterService, oldService *Service, swg *lock.StoppableWaitGroup) {
	id := ServiceID{Name: service.Name, Namespace: service.Namespace}
	scopedLog := log.WithFields(logrus.Fields{logfields.ServiceName: service.String()})

	externalEndpoints, ok := s.externalEndpoints[id]
	if !ok {
		externalEndpoints = newExternalEndpoints()
		s.externalEndpoints[id] = externalEndpoints
	}

	scopedLog.Debugf("Updating backends to %+v", service.Backends)
	backends := map[string]*Backend{}
	for ipString, portConfig := range service.Backends {
		backends[ipString] = &Backend{Ports: portConfig}
	}
	externalEndpoints.endpoints[service.Cluster] = &Endpoints{
		Backends: backends,
	}

	svc, ok := s.services[id]

	endpoints, serviceReady := s.correlateEndpoints(id)

	// Only send event notification if service is shared and ready.
	// External endpoints are still tracked but correlation will not happen
	// until the service is marked as shared.
	if ok && svc.Shared && serviceReady {
		swg.Add()
		s.Events <- ServiceEvent{
			Action:     UpdateService,
			ID:         id,
			Service:    svc,
			OldService: oldService,
			Endpoints:  endpoints,
			SWG:        swg,
		}
	}
}

// MergeExternalServiceDelete merges the deletion of a cluster service in a
// remote cluster into the local service cache. The service endpoints are
// stored as external endpoints and are correlated on demand with local
// services via correlateEndpoints().
func (s *ServiceCache) MergeExternalServiceDelete(service *serviceStore.ClusterService, swg *lock.StoppableWaitGroup) {
	scopedLog := log.WithFields(logrus.Fields{logfields.ServiceName: service.String()})
	id := ServiceID{Name: service.Name, Namespace: service.Namespace}

	// Ignore updates of own cluster
	if service.Cluster == option.Config.ClusterName {
		scopedLog.Debug("Not merging external service. Own cluster")
		return
	}

	s.mutex.Lock()
	defer s.mutex.Unlock()

	externalEndpoints, ok := s.externalEndpoints[id]
	if ok {
		scopedLog.Debug("Deleting external endpoints")

		delete(externalEndpoints.endpoints, service.Cluster)

		svc, ok := s.services[id]

		endpoints, serviceReady := s.correlateEndpoints(id)

		// Only send event notification if service is shared. External
		// endpoints are still tracked but correlation will not happen
		// until the service is marked as shared.
		if ok && svc.Shared {
			swg.Add()
			event := ServiceEvent{
				Action:    UpdateService,
				ID:        id,
				Service:   svc,
				Endpoints: endpoints,
				SWG:       swg,
			}

			if !serviceReady {
				event.Action = DeleteService
			}

			s.Events <- event
		}
	} else {
		scopedLog.Debug("Received delete event for non-existing endpoints")
	}
}

// MergeClusterServiceUpdate merges a cluster service of a local cluster into
// the local service cache. The service endpoints are stored as external endpoints
// and are correlated on demand with local services via correlateEndpoints().
// Local service is created and/or updated if needed.
func (s *ServiceCache) MergeClusterServiceUpdate(service *serviceStore.ClusterService, swg *lock.StoppableWaitGroup) {
	scopedLog := log.WithFields(logrus.Fields{logfields.ServiceName: service.String()})
	id := ServiceID{Name: service.Name, Namespace: service.Namespace}

	s.mutex.Lock()
	defer s.mutex.Unlock()

	var oldService *Service
	svc, ok := s.services[id]
	if !ok || !svc.EqualsClusterService(service) {
		oldService = svc
		svc = ParseClusterService(service)
		s.services[id] = svc
		scopedLog.Debugf("Added new service %v", svc)
	}
	s.mergeServiceUpdateLocked(service, oldService, swg)
}

// MergeClusterServiceDelete merges the deletion of a cluster service in a
// remote cluster into the local service cache, deleting the local service.
func (s *ServiceCache) MergeClusterServiceDelete(service *serviceStore.ClusterService, swg *lock.StoppableWaitGroup) {
	scopedLog := log.WithFields(logrus.Fields{logfields.ServiceName: service.String()})
	id := ServiceID{Name: service.Name, Namespace: service.Namespace}

	s.mutex.Lock()
	defer s.mutex.Unlock()

	externalEndpoints, ok := s.externalEndpoints[id]
	if ok {
		scopedLog.Debug("Deleting cluster endpoints")
		delete(externalEndpoints.endpoints, service.Cluster)
	}

	svc, ok := s.services[id]
	endpoints, _ := s.correlateEndpoints(id)
	delete(s.services, id)

	if ok {
		swg.Add()
		s.Events <- ServiceEvent{
			Action:    DeleteService,
			ID:        id,
			Service:   svc,
			Endpoints: endpoints,
			SWG:       swg,
		}
	}
}

// DebugStatus implements debug.StatusObject to provide debug status collection
// ability
func (s *ServiceCache) DebugStatus() string {
	s.mutex.RLock()
	str := spew.Sdump(s)
	s.mutex.RUnlock()
	return str
}

// Implementation of subscriber.Node

func (s *ServiceCache) OnAddNode(node *core_v1.Node, swg *lock.StoppableWaitGroup) error {
	s.updateSelfNodeLabels(node.GetLabels(), swg)

	return nil
}

func (s *ServiceCache) OnUpdateNode(oldNode, newNode *core_v1.Node,
	swg *lock.StoppableWaitGroup) error {

	s.updateSelfNodeLabels(newNode.GetLabels(), swg)

	return nil
}

func (s *ServiceCache) OnDeleteNode(node *core_v1.Node,
	swg *lock.StoppableWaitGroup) error {

	return nil
}

func (s *ServiceCache) updateSelfNodeLabels(labels map[string]string,
	swg *lock.StoppableWaitGroup) {

	if !option.Config.EnableServiceTopology {
		return
	}

	s.mutex.Lock()
	defer s.mutex.Unlock()

	zone := labels[LabelTopologyZone]

	if s.selfNodeZoneLabel == zone {
		return
	}

	s.selfNodeZoneLabel = zone

	for id, svc := range s.services {
		if !svc.TopologyAware {
			continue
		}

		if endpoints, ready := s.correlateEndpoints(id); ready {
			swg.Add()
			s.Events <- ServiceEvent{
				Action:     UpdateService,
				ID:         id,
				Service:    svc,
				OldService: svc,
				Endpoints:  endpoints,
				SWG:        swg,
			}
		}
	}
}
