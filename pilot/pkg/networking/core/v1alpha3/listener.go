// Copyright 2017 Istio Authors
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
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"time"

	xdsapi "github.com/envoyproxy/go-control-plane/envoy/api/v2"
	"github.com/envoyproxy/go-control-plane/envoy/api/v2/auth"
	"github.com/envoyproxy/go-control-plane/envoy/api/v2/core"
	"github.com/envoyproxy/go-control-plane/envoy/api/v2/listener"
	accesslog "github.com/envoyproxy/go-control-plane/envoy/config/filter/accesslog/v2"
	http_conn "github.com/envoyproxy/go-control-plane/envoy/config/filter/network/http_connection_manager/v2"
	tcp_proxy "github.com/envoyproxy/go-control-plane/envoy/config/filter/network/tcp_proxy/v2"
	xdsutil "github.com/envoyproxy/go-control-plane/pkg/util"
	google_protobuf "github.com/gogo/protobuf/types"
	"github.com/prometheus/client_golang/prometheus"

	meshconfig "istio.io/api/mesh/v1alpha1"
	"istio.io/istio/pilot/pkg/model"
	"istio.io/istio/pilot/pkg/networking/plugin"
	"istio.io/istio/pilot/pkg/networking/util"
	"istio.io/istio/pkg/log"
)

const (
	fileAccessLog = "envoy.file_access_log"

	envoyHTTPConnectionManager = "envoy.http_connection_manager"

	// HTTPStatPrefix indicates envoy stat prefix for http listeners
	HTTPStatPrefix = "http"

	// RDSName is the name of route-discovery-service (RDS) cluster
	RDSName = "rds"

	// RDSHttpProxy is the special name for HTTP PROXY route
	RDSHttpProxy = "http_proxy"

	// VirtualListenerName is the name for traffic capture listener
	VirtualListenerName = "virtual"

	// WildcardAddress binds to all IP addresses
	// WildcardAddress = "0.0.0.0"
	WildcardAddress = "::0"

	// LocalhostAddress for local binding
	// LocalhostAddress = "127.0.0.1"
	LocalhostAddress = "::1"
)

var (
	// Very verbose output in the logs - full LDS response logged for each sidecar.
	// Use /debug/ldsz instead.
	verboseDebug = os.Getenv("PILOT_DUMP_ALPHA3") != ""

	// TODO: gauge should be reset on refresh, not the best way to represent errors but better
	// than nothing.
	// TODO: add dimensions - namespace of rule, service, rule name
	conflictingOutbound = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "pilot_conf_out_listeners",
		Help: "Number of conflicting listeners.",
	})
	invalidOutboundListeners = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "pilot_invalid_out_listeners",
		Help: "Number of invalid outbound listeners.",
	})
)

func init() {
	prometheus.MustRegister(conflictingOutbound)
	prometheus.MustRegister(invalidOutboundListeners)
}

// ListenersALPNProtocols denotes the the list of ALPN protocols that the listener
// should expose
var ListenersALPNProtocols = []string{"h2", "http/1.1"}

// BuildListeners produces a list of listeners and referenced clusters for all proxies
func (configgen *ConfigGeneratorImpl) BuildListeners(env model.Environment, node model.Proxy) ([]*xdsapi.Listener, error) {
	switch node.Type {
	case model.Sidecar:
		return configgen.buildSidecarListeners(env, node)
	case model.Router, model.Ingress:
		// TODO: add listeners for other protocols too
		return configgen.buildGatewayListeners(env, node)
	}
	return nil, nil
}

// buildSidecarListeners produces a list of listeners for sidecar proxies
func (configgen *ConfigGeneratorImpl) buildSidecarListeners(env model.Environment, node model.Proxy) ([]*xdsapi.Listener, error) {

	mesh := env.Mesh
	managementPorts := env.ManagementPorts(node.IPAddress)

	proxyInstances, err := env.GetProxyServiceInstances(&node)
	if err != nil {
		return nil, err
	}

	services, err := env.Services()
	if err != nil {
		return nil, err
	}

	// ensure services are ordered to simplify generation logic
	sort.Slice(services, func(i, j int) bool { return services[i].Hostname < services[j].Hostname })

	listeners := make([]*xdsapi.Listener, 0)

	if mesh.ProxyListenPort > 0 {
		inbound := configgen.buildSidecarInboundListeners(env, node, proxyInstances)
		outbound := configgen.buildSidecarOutboundListeners(env, node, proxyInstances, services)

		listeners = append(listeners, inbound...)
		listeners = append(listeners, outbound...)

		mgmtListeners := buildMgmtPortListeners(managementPorts, node.IPAddress)
		// If management listener port and service port are same, bad things happen
		// when running in kubernetes, as the probes stop responding. So, append
		// non overlapping listeners only.
		for i := range mgmtListeners {
			m := mgmtListeners[i]
			l := util.GetByAddress(listeners, m.Address.String())
			if l != nil {
				log.Warnf("Omitting listener for management address %s (%s) due to collision with service listener %s (%s)",
					m.Name, m.Address, l.Name, l.Address)
				continue
			}
			listeners = append(listeners, m)
		}

		// We need a dummy filter to fill in the filter stack for orig_dst listener
		// TODO: Move to Listener filters and set up original dst filter there.
		dummyTCPProxy := &tcp_proxy.TcpProxy{
			StatPrefix: util.BlackHoleCluster,
			Cluster:    util.BlackHoleCluster,
		}

		var transparent *google_protobuf.BoolValue
		if mode := node.Metadata["INTERCEPTION_MODE"]; mode == "TPROXY" {
			transparent = &google_protobuf.BoolValue{true}
		}

		// add an extra listener that binds to the port that is the recipient of the iptables redirect
		listeners = append(listeners, &xdsapi.Listener{
			Name:           VirtualListenerName,
			Address:        util.BuildAddress(WildcardAddress, uint32(mesh.ProxyListenPort)),
			Transparent:    transparent,
			UseOriginalDst: &google_protobuf.BoolValue{true},
			FilterChains: []listener.FilterChain{
				{
					Filters: []listener.Filter{
						{
							Name:   xdsutil.TCPProxy,
							Config: util.MessageToStruct(dummyTCPProxy),
						},
					},
				},
			},
		})
	}

	// enable HTTP PROXY port if necessary; this will add an RDS route for this port
	if mesh.ProxyHttpPort > 0 {
		useRemoteAddress := false
		traceOperation := http_conn.EGRESS
		listenAddress := LocalhostAddress

		if node.Type == model.Router {
			useRemoteAddress = true
			traceOperation = http_conn.INGRESS
			listenAddress = WildcardAddress
		}

		opts := buildListenerOpts{
			env:            env,
			proxy:          node,
			proxyInstances: proxyInstances,
			ip:             listenAddress,
			port:           int(mesh.ProxyHttpPort),
			protocol:       model.ProtocolHTTP,
			filterChainOpts: []*filterChainOpts{{
				httpOpts: &httpListenerOpts{
					routeConfig: configgen.BuildSidecarOutboundHTTPRouteConfig(env, node, proxyInstances,
						services, RDSHttpProxy),
					//rds:              RDSHttpProxy,
					useRemoteAddress: useRemoteAddress,
					direction:        traceOperation,
					connectionManager: &http_conn.HttpConnectionManager{
						HttpProtocolOptions: &core.Http1ProtocolOptions{
							AllowAbsoluteUrl: &google_protobuf.BoolValue{
								Value: true,
							},
						},
					},
				},
			}},
			bindToPort: true,
		}
		l := buildListener(opts)
		if err := marshalFilters(l, opts, []plugin.FilterChain{{}}); err != nil {
			log.Warna("buildSidecarListeners ", err.Error())
		} else {
			listeners = append(listeners, l)
		}
		// TODO: need inbound listeners in HTTP_PROXY case, with dedicated ingress listener.
	}

	return listeners, nil
}

// buildSidecarInboundListeners creates listeners for the server-side (inbound)
// configuration for co-located service proxyInstances.
func (configgen *ConfigGeneratorImpl) buildSidecarInboundListeners(env model.Environment, node model.Proxy,
	proxyInstances []*model.ServiceInstance) []*xdsapi.Listener {

	var listeners []*xdsapi.Listener
	listenerMap := make(map[string]*xdsapi.Listener)
	// inbound connections/requests are redirected to the endpoint address but appear to be sent
	// to the service address.
	for _, instance := range proxyInstances {
		endpoint := instance.Endpoint
		protocol := endpoint.ServicePort.Protocol

		// Local service instances can be accessed through one of three
		// addresses: localhost, endpoint IP, and service
		// VIP. Localhost bypasses the proxy and doesn't need any TCP
		// route config. Endpoint IP is handled below and Service IP is handled
		// by outbound routes.
		// Traffic sent to our service VIP is redirected by remote
		// services' kubeproxy to our specific endpoint IP.
		var listenerType plugin.ListenerType
		listenerOpts := buildListenerOpts{
			env:            env,
			proxy:          node,
			proxyInstances: proxyInstances,
			ip:             endpoint.Address,
			port:           endpoint.Port,
			protocol:       protocol,
		}

		listenerMapKey := fmt.Sprintf("%s:%d", endpoint.Address, endpoint.Port)
		if l, exists := listenerMap[listenerMapKey]; exists {
			log.Warnf("Conflicting inbound listeners on %s: previous listener %s", listenerMapKey, l.Name)
			// Skip building listener for the same ip port
			continue
		}
		listenerType = plugin.ModelProtocolToListenerType(protocol)
		switch listenerType {
		case plugin.ListenerTypeHTTP:
			listenerOpts.filterChainOpts = []*filterChainOpts{{
				httpOpts: &httpListenerOpts{
					routeConfig:      configgen.buildSidecarInboundHTTPRouteConfig(env, node, instance),
					rds:              "",
					useRemoteAddress: false,
					direction:        http_conn.INGRESS,
				}},
			}
		case plugin.ListenerTypeTCP:
			listenerOpts.filterChainOpts = []*filterChainOpts{{
				networkFilters: buildInboundNetworkFilters(instance),
			}}

		default:
			log.Warnf("Unsupported inbound protocol %v for port %#v", protocol, instance.Endpoint.ServicePort)
			continue
		}

		// call plugins
		l := buildListener(listenerOpts)
		mutable := &plugin.MutableObjects{
			Listener:     l,
			FilterChains: make([]plugin.FilterChain, len(l.FilterChains)),
		}
		for _, p := range configgen.Plugins {
			params := &plugin.InputParams{
				ListenerType:    listenerType,
				Env:             &env,
				Node:            &node,
				ProxyInstances:  proxyInstances,
				ServiceInstance: instance,
			}
			if err := p.OnInboundListener(params, mutable); err != nil {
				log.Warn(err.Error())
			}
		}
		// Filters are serialized one time into an opaque struct once we have the complete list.
		if err := marshalFilters(mutable.Listener, listenerOpts, mutable.FilterChains); err != nil {
			log.Warna("buildSidecarInboundListeners ", err.Error())
		} else {
			listeners = append(listeners, mutable.Listener)
			listenerMap[listenerMapKey] = mutable.Listener
		}
	}
	return listeners
}

// buildSidecarOutboundListeners generates http and tcp listeners for outbound connections from the service instance
// TODO(github.com/istio/pilot/issues/237)
//
// Sharing tcp_proxy and http_connection_manager filters on the same port for
// different destination services doesn't work with Envoy (yet). When the
// tcp_proxy filter's route matching fails for the http service the connection
// is closed without falling back to the http_connection_manager.
//
// Temporary workaround is to add a listener for each service IP that requires
// TCP routing
//
// Connections to the ports of non-load balanced services are directed to
// the connection's original destination. This avoids costly queries of instance
// IPs and ports, but requires that ports of non-load balanced service be unique.
func (configgen *ConfigGeneratorImpl) buildSidecarOutboundListeners(env model.Environment, node model.Proxy,
	proxyInstances []*model.ServiceInstance, services []*model.Service) []*xdsapi.Listener {

	var tcpListeners, httpListeners []*xdsapi.Listener
	var currentListener *xdsapi.Listener
	listenerTypeMap := make(map[string]model.Protocol)
	listenerMap := make(map[string]*xdsapi.Listener)
	for _, service := range services {
		for _, servicePort := range service.Ports {
			clusterName := model.BuildSubsetKey(model.TrafficDirectionOutbound, "",
				service.Hostname, servicePort.Port)

			listenAddress := WildcardAddress
			var addresses []string
			var listenerMapKey string
			listenerOpts := buildListenerOpts{
				env:            env,
				proxy:          node,
				proxyInstances: proxyInstances,
				ip:             WildcardAddress,
				port:           servicePort.Port,
				protocol:       servicePort.Protocol,
			}

			currentListener = nil

			switch plugin.ModelProtocolToListenerType(servicePort.Protocol) {
			case plugin.ListenerTypeHTTP:
				listenerMapKey = fmt.Sprintf("%s:%d", listenAddress, servicePort.Port)
				if l, exists := listenerMap[listenerMapKey]; exists {
					if !listenerTypeMap[listenerMapKey].IsHTTP() {
						conflictingOutbound.Add(1)
						log.Warnf("buildSidecarOutboundListeners: listener conflict (%v current and new %v) on %s, destination:%s, current Listener: (%s %v)",
							servicePort.Protocol, listenerTypeMap[listenerMapKey], listenerMapKey, clusterName, l.Name, l)
					}
					// Skip building listener for the same http port
					continue
				}

				operation := http_conn.EGRESS
				useRemoteAddress := false

				if node.Type == model.Router {
					// if this is in Router mode, then use ingress style trace operation, and remote address settings
					useRemoteAddress = true
					operation = http_conn.INGRESS
				}

				listenerOpts.protocol = servicePort.Protocol
				listenerOpts.filterChainOpts = []*filterChainOpts{{
					httpOpts: &httpListenerOpts{
						//rds:              fmt.Sprintf("%d", servicePort.Port),
						routeConfig: configgen.BuildSidecarOutboundHTTPRouteConfig(
							env, node, proxyInstances, services, fmt.Sprintf("%d", servicePort.Port)),
						useRemoteAddress: useRemoteAddress,
						direction:        operation,
					},
				}}
			case plugin.ListenerTypeTCP:
				if service.Resolution != model.Passthrough {
					listenAddress = service.GetServiceAddressForProxy(&node)
					addresses = []string{listenAddress}
				}

				listenerMapKey = fmt.Sprintf("%s:%d", listenAddress, servicePort.Port)
				var exists bool
				if currentListener, exists = listenerMap[listenerMapKey]; exists {
					// Check if this is HTTPS port collision for external service. If so, we can use SNI to differentiate
					// Internal TCP services will never hit this issue because they are bound by specific IP_port, while
					// external service listeners are typically bound to 0.0.0.0
					if !listenerTypeMap[listenerMapKey].IsTCP() || servicePort.Protocol != model.ProtocolHTTPS || !service.MeshExternal {
						conflictingOutbound.Add(1)
						log.Warnf("buildSidecarOutboundListeners: listener conflict (%v current and new %v) on %s, destination:%s, current Listener: (%s %v)",
							servicePort.Protocol, listenerTypeMap[listenerMapKey], listenerMapKey, clusterName, currentListener.Name, currentListener)
						continue
					}
				}
				filterChainOption := &filterChainOpts{
					networkFilters: buildOutboundNetworkFilters(clusterName, addresses, servicePort),
				}

				// TODO (@rshriram): This is not sufficient. There are other TCP protocols that use SNI, that need to be tackled.
				// Set SNI hosts for External services only. It may or may not work for internal services.
				// TODO (@rshriram): We need an explicit option to enable/disable SNI for a given service
				if servicePort.Protocol == model.ProtocolHTTPS && service.MeshExternal {
					filterChainOption.sniHosts = []string{service.Hostname.String()}
				}

				listenerOpts.filterChainOpts = []*filterChainOpts{filterChainOption}
			default:
				// UDP or other protocols: no need to log, it's too noisy
				continue
			}

			// Even if we have a non empty current listener, lets build the new listener with the filter chains
			// In the end, we will merge the filter chains

			// call plugins
			listenerOpts.ip = listenAddress
			l := buildListener(listenerOpts)
			mutable := &plugin.MutableObjects{
				Listener:     l,
				FilterChains: make([]plugin.FilterChain, len(l.FilterChains)),
			}

			for _, p := range configgen.Plugins {
				params := &plugin.InputParams{
					ListenerType:   plugin.ModelProtocolToListenerType(servicePort.Protocol),
					Env:            &env,
					Node:           &node,
					ProxyInstances: proxyInstances,
					Service:        service,
				}

				if err := p.OnOutboundListener(params, mutable); err != nil {
					log.Warn(err.Error())
				}
			}

			// Filters are serialized one time into an opaque struct once we have the complete list.
			if err := marshalFilters(mutable.Listener, listenerOpts, mutable.FilterChains); err != nil {
				log.Warna("buildSidecarOutboundListeners: ", err.Error())
				continue
			}

			if currentListener != nil {
				// merge the newly built listener with the existing listener
				newFilterChains := make([]listener.FilterChain, 0, len(currentListener.FilterChains)+len(mutable.Listener.FilterChains))
				newFilterChains = append(newFilterChains, currentListener.FilterChains...)
				newFilterChains = append(newFilterChains, mutable.Listener.FilterChains...)
				currentListener.FilterChains = newFilterChains
			} else {
				listenerMap[listenerMapKey] = mutable.Listener
				listenerTypeMap[listenerMapKey] = servicePort.Protocol
			}

			if log.DebugEnabled() && len(mutable.Listener.FilterChains) > 1 || currentListener != nil {
				var numChains int
				if currentListener != nil {
					numChains = len(currentListener.FilterChains)
				} else {
					numChains = len(mutable.Listener.FilterChains)
				}
				log.Debugf("buildSidecarOutboundListeners: multiple filter chain listener %s with %d chains", mutable.Listener.Name, numChains)
			}
		}
	}

	for name, l := range listenerMap {
		ltype := listenerTypeMap[name]
		if err := l.Validate(); err != nil {
			log.Warnf("buildSidecarOutboundListeners: error validating listener %s (type %v): %v", name, ltype, err)
			invalidOutboundListeners.Add(1)
			continue
		}
		if ltype.IsTCP() {
			tcpListeners = append(tcpListeners, l)
		} else {
			httpListeners = append(httpListeners, l)
		}
	}

	return append(tcpListeners, httpListeners...)
}

// buildMgmtPortListeners creates inbound TCP only listeners for the management ports on
// server (inbound). Management port listeners are slightly different from standard Inbound listeners
// in that, they do not have mixer filters nor do they have inbound auth.
// N.B. If a given management port is same as the service instance's endpoint port
// the pod will fail to start in Kubernetes, because the mixer service tries to
// lookup the service associated with the Pod. Since the pod is yet to be started
// and hence not bound to the service), the service lookup fails causing the mixer
// to fail the health check call. This results in a vicious cycle, where kubernetes
// restarts the unhealthy pod after successive failed health checks, and the mixer
// continues to reject the health checks as there is no service associated with
// the pod.
// So, if a user wants to use kubernetes probes with Istio, she should ensure
// that the health check ports are distinct from the service ports.
func buildMgmtPortListeners(managementPorts model.PortList, managementIP string) []*xdsapi.Listener {
	listeners := make([]*xdsapi.Listener, 0, len(managementPorts))

	if managementIP == "" {
		// managementIP = "127.0.0.1"
		managementIP = "::1"
	}

	// assumes that inbound connections/requests are sent to the endpoint address
	for _, mPort := range managementPorts {
		switch mPort.Protocol {
		case model.ProtocolHTTP, model.ProtocolHTTP2, model.ProtocolGRPC, model.ProtocolTCP,
			model.ProtocolHTTPS, model.ProtocolMongo, model.ProtocolRedis:

			instance := &model.ServiceInstance{
				Endpoint: model.NetworkEndpoint{
					Address:     managementIP,
					Port:        mPort.Port,
					ServicePort: mPort,
				},
				Service: &model.Service{
					Hostname: ManagementClusterHostname,
				},
			}
			listenerOpts := buildListenerOpts{
				ip:       managementIP,
				port:     mPort.Port,
				protocol: model.ProtocolTCP,
				filterChainOpts: []*filterChainOpts{{
					networkFilters: buildInboundNetworkFilters(instance),
				}},
			}
			l := buildListener(listenerOpts)
			// TODO: should we call plugins for the admin port listeners too? We do everywhere else we contruct listeners.
			if err := marshalFilters(l, listenerOpts, []plugin.FilterChain{{}}); err != nil {
				log.Warna("buildMgmtPortListeners ", err.Error())
			} else {
				listeners = append(listeners, l)
			}
		default:
			log.Warnf("Unsupported inbound protocol %v for management port %#v",
				mPort.Protocol, mPort)
		}
	}

	return listeners
}

// httpListenerOpts are options for an HTTP listener
type httpListenerOpts struct {
	//nolint: maligned
	routeConfig      *xdsapi.RouteConfiguration
	rds              string
	useRemoteAddress bool
	direction        http_conn.HttpConnectionManager_Tracing_OperationName
	// If set, use this as a basis
	connectionManager *http_conn.HttpConnectionManager
}

// filterChainOpts describes a filter chain: a set of filters with the same TLS context
type filterChainOpts struct {
	sniHosts       []string
	tlsContext     *auth.DownstreamTlsContext
	httpOpts       *httpListenerOpts
	networkFilters []listener.Filter
}

// buildListenerOpts are the options required to build a Listener
type buildListenerOpts struct {
	// nolint: maligned
	env             model.Environment
	proxy           model.Proxy
	proxyInstances  []*model.ServiceInstance
	ip              string
	port            int
	protocol        model.Protocol
	bindToPort      bool
	filterChainOpts []*filterChainOpts
}

func buildHTTPConnectionManager(mesh *meshconfig.MeshConfig, httpOpts *httpListenerOpts, httpFilters []*http_conn.HttpFilter) *http_conn.HttpConnectionManager {
	filters := append(httpFilters,
		&http_conn.HttpFilter{Name: xdsutil.CORS},
		&http_conn.HttpFilter{Name: xdsutil.Fault},
		&http_conn.HttpFilter{Name: xdsutil.Router},
	)

	refresh := time.Duration(mesh.RdsRefreshDelay.Seconds) * time.Second
	if refresh == 0 {
		// envoy crashes if 0. Will go away once we move to v2
		refresh = 5 * time.Second
	}

	if httpOpts.connectionManager == nil {
		httpOpts.connectionManager = &http_conn.HttpConnectionManager{}
	}

	connectionManager := httpOpts.connectionManager
	connectionManager.CodecType = http_conn.AUTO
	connectionManager.AccessLog = []*accesslog.AccessLog{
		{
			Config: nil,
		},
	}
	connectionManager.HttpFilters = filters
	connectionManager.StatPrefix = HTTPStatPrefix
	connectionManager.UseRemoteAddress = &google_protobuf.BoolValue{httpOpts.useRemoteAddress}

	// not enabled yet
	if httpOpts.rds != "" {
		rds := &http_conn.HttpConnectionManager_Rds{
			Rds: &http_conn.Rds{
				RouteConfigName: httpOpts.rds,
				ConfigSource: core.ConfigSource{
					ConfigSourceSpecifier: &core.ConfigSource_ApiConfigSource{
						ApiConfigSource: &core.ApiConfigSource{
							ApiType:      core.ApiConfigSource_REST_LEGACY,
							ClusterNames: []string{RDSName},
							RefreshDelay: &refresh,
						},
					},
				},
			},
		}
		connectionManager.RouteSpecifier = rds
	} else {
		connectionManager.RouteSpecifier = &http_conn.HttpConnectionManager_RouteConfig{RouteConfig: httpOpts.routeConfig}
	}

	if connectionManager.RouteSpecifier == nil {
		connectionManager.RouteSpecifier = &http_conn.HttpConnectionManager_RouteConfig{
			RouteConfig: httpOpts.routeConfig,
		}
	}

	if mesh.AccessLogFile != "" {
		fl := &accesslog.FileAccessLog{
			Path: mesh.AccessLogFile,
		}

		connectionManager.AccessLog = []*accesslog.AccessLog{
			{
				Config: util.MessageToStruct(fl),
				Name:   fileAccessLog,
			},
		}
	}

	if mesh.EnableTracing {
		connectionManager.Tracing = &http_conn.HttpConnectionManager_Tracing{
			OperationName: httpOpts.direction,
		}
		connectionManager.GenerateRequestId = &google_protobuf.BoolValue{true}
	}

	if verboseDebug {
		connectionManagerJSON, _ := json.MarshalIndent(connectionManager, "  ", "  ")
		log.Infof("LDS: %s \n", string(connectionManagerJSON))
	}
	return connectionManager
}

// buildListener builds and initializes a Listener proto based on the provided opts. It does not set any filters.
func buildListener(opts buildListenerOpts) *xdsapi.Listener {
	filterChains := make([]listener.FilterChain, 0, len(opts.filterChainOpts))
	for _, chain := range opts.filterChainOpts {
		var match *listener.FilterChainMatch

		if len(chain.sniHosts) > 0 {
			fullWildcardFound := false
			for _, h := range chain.sniHosts {
				if h == "*" {
					fullWildcardFound = true
					// If we have a host with *, it effectively means match anything, i.e.
					// no SNI based matching for this host.
					break
				}
			}
			if !fullWildcardFound {
				match = &listener.FilterChainMatch{SniDomains: chain.sniHosts}
			}
		}
		filterChains = append(filterChains, listener.FilterChain{
			FilterChainMatch: match,
			TlsContext:       chain.tlsContext,
		})
	}

	var deprecatedV1 *xdsapi.Listener_DeprecatedV1
	if !opts.bindToPort {
		deprecatedV1 = &xdsapi.Listener_DeprecatedV1{
			BindToPort: boolFalse,
		}
	}

	return &xdsapi.Listener{
		Name:         fmt.Sprintf("%s_%d", opts.ip, opts.port),
		Address:      util.BuildAddress(opts.ip, uint32(opts.port)),
		FilterChains: filterChains,
		DeprecatedV1: deprecatedV1,
	}
}

// marshalFilters adds the provided TCP and HTTP filters to the provided Listener and serializes them.
//
// TODO: should we change this from []plugins.FilterChains to [][]listener.Filter, [][]*http_conn.HttpFilter?
// TODO: given how tightly tied listener.FilterChains, opts.filterChainOpts, and mutable.FilterChains are to eachother
// we should encapsulate them some way to ensure they remain consistent (mainly that in each an index refers to the same
// chain)
func marshalFilters(l *xdsapi.Listener, opts buildListenerOpts, chains []plugin.FilterChain) error {
	if len(opts.filterChainOpts) == 0 {
		return fmt.Errorf("must have more than 0 chains in listener: %#v", l)
	}

	for i, chain := range chains {
		opt := opts.filterChainOpts[i]
		// check that we either have all TCP or all HTTP chain, and not a mix
		// TODO: remove when Envoy supports port protocol multiplexing
		if (len(chain.TCP) > 0 || len(opt.networkFilters) > 0) && (len(chain.HTTP) > 0 || opt.httpOpts != nil) {
			return fmt.Errorf("listener %q filter chain %d cannot set both network(%#v) and HTTP(%#v) filter chains",
				l.Name, i, append(chain.TCP, opt.networkFilters...), chain.HTTP)
		}

		l.FilterChains[i].Filters = append(l.FilterChains[i].Filters, chain.TCP...)
		l.FilterChains[i].Filters = append(l.FilterChains[i].Filters, opt.networkFilters...)
		if log.DebugEnabled() {
			log.Debugf("attached %d network filters to listener %q filter chain %d", len(chain.TCP)+len(opt.networkFilters), l.Name, i)
		}

		if opt.httpOpts != nil {
			connectionManager := buildHTTPConnectionManager(opts.env.Mesh, opt.httpOpts, chain.HTTP)
			l.FilterChains[i].Filters = append(l.FilterChains[i].Filters, listener.Filter{
				Name:   envoyHTTPConnectionManager,
				Config: util.MessageToStruct(connectionManager),
			})
			log.Debugf("attached HTTP filter with %d http_filter options to listener %q filter chain %d", 1+len(chain.HTTP), l.Name, i)
		}
	}
	return nil
}
