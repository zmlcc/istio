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

package eureka

import (
	"fmt"

	"istio.io/istio/pilot/pkg/model"
	"istio.io/istio/pkg/log"
)

// Convert Eureka applications to services. If provided, only convert applications in the hostnames whitelist,
// otherwise convert all.
func convertServices(apps []*application, hostnames map[model.Hostname]bool) map[model.Hostname]*model.Service {
	services := make(map[model.Hostname]*model.Service)
	for _, app := range apps {
		for _, instance := range app.Instances {
			hostname := model.Hostname(instance.Hostname)
			if len(hostnames) > 0 && !hostnames[hostname] {
				continue
			}

			if instance.Status != statusUp {
				continue
			}

			ports := convertPorts(instance)
			if len(ports) == 0 {
				continue
			}

			service := services[hostname]
			if service == nil {
				service = &model.Service{
					Hostname:     model.Hostname(instance.Hostname),
					Address:      "::0",
					Ports:        make(model.PortList, 0),
					ExternalName: "",
					MeshExternal: false,
					Resolution:   model.ClientSideLB,
				}
				services[hostname] = service
			}

			protocol := convertProtocol(instance.Metadata)
			for _, port := range ports {
				if servicePort, exists := service.Ports.GetByPort(port.Port); exists {
					if servicePort.Protocol != protocol {
						log.Warnf(
							"invalid Eureka config: "+
								"%s:%d has conflicting protocol definitions %s, %s",
							instance.Hostname, servicePort.Port,
							servicePort.Protocol, protocol)
					}
					continue
				}

				service.Ports = append(service.Ports, port)
			}
		}
	}
	return services
}

// Convert Eureka applications to service instances. The services argument must contain a map of hostnames to
// services. Only service instances with a corresponding service are converted.
func convertServiceInstances(services map[model.Hostname]*model.Service, apps []*application) []*model.ServiceInstance {
	out := make([]*model.ServiceInstance, 0)
	for _, app := range apps {
		for _, instance := range app.Instances {
			hostname := model.Hostname(instance.Hostname)
			if services[hostname] == nil {
				continue
			}

			if instance.Status != statusUp {
				continue
			}

			for _, port := range convertPorts(instance) {
				out = append(out, &model.ServiceInstance{
					Endpoint: model.NetworkEndpoint{
						Address:     instance.IPAddress,
						Port:        port.Port,
						ServicePort: port,
					},
					Service: services[hostname],
					Labels:  convertLabels(instance.Metadata),
				})
			}
		}
	}
	return out
}

func convertPorts(instance *instance) model.PortList {
	out := make(model.PortList, 0, 2) // Eureka instances have 0..2 enabled ports
	protocol := convertProtocol(instance.Metadata)
	for _, port := range []port{instance.Port, instance.SecurePort} {
		if !port.Enabled {
			continue
		}

		out = append(out, &model.Port{
			Name:     fmt.Sprint(port.Port),
			Port:     port.Port,
			Protocol: protocol,
		})
	}
	return out
}

const protocolMetadata = "istio.protocol" // metadata key for port protocol

func convertProtocol(md metadata) model.Protocol {
	name := md[protocolMetadata]

	if md != nil {
		protocol := model.ParseProtocol(name)
		if protocol == model.ProtocolUnsupported {
			log.Warnf("unsupported protocol value: %s", name)
		} else {
			return protocol
		}
	}
	return model.ProtocolTCP // default protocol
}

func convertLabels(metadata metadata) model.Labels {
	labels := make(model.Labels)
	for k, v := range metadata {
		labels[k] = v
	}

	// filter out special labels
	delete(labels, protocolMetadata)
	delete(labels, "@class")

	return labels
}
