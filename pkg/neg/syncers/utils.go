/*
Copyright 2018 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package syncers

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"google.golang.org/api/compute/v1"
	apiv1 "k8s.io/api/core/v1"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/ingress-gce/pkg/flags"
	negtypes "k8s.io/ingress-gce/pkg/neg/types"
	"k8s.io/ingress-gce/pkg/utils"
	"k8s.io/klog"
)

const (
	MAX_NETWORK_ENDPOINTS_PER_BATCH = 500
	// For each NEG, only retries 15 times to process it.
	// This is a convention in kube-controller-manager.
	maxRetries                          = 15
	minRetryDelay                       = 5 * time.Second
	maxRetryDelay                       = 600 * time.Second
	separator                           = "||"
	negIPPortNetworkEndpointType        = "GCE_VM_IP_PORT"
	negPrivateIPPortNetworkEdnpointType = "NON_GCP_PRIVATE_IP_PORT"
)

// encodeEndpoint encodes ip and instance into a single string
func encodeEndpoint(ip, instance, port string) string {
	return strings.Join([]string{ip, instance, port}, separator)
}

// decodeEndpoint decodes ip and instance from an encoded string
func decodeEndpoint(str string) (string, string, string) {
	strs := strings.Split(str, separator)
	return strs[0], strs[1], strs[2]
}

// calculateDifference determines what endpoints needs to be added and removed in order to move current state to target state.
func calculateDifference(targetMap, currentMap map[string]sets.String) (map[string]sets.String, map[string]sets.String) {
	addSet := map[string]sets.String{}
	removeSet := map[string]sets.String{}
	for zone, endpointSet := range targetMap {
		diff := endpointSet.Difference(currentMap[zone])
		if len(diff) > 0 {
			addSet[zone] = diff
		}
	}

	for zone, endpointSet := range currentMap {
		diff := endpointSet.Difference(targetMap[zone])
		if len(diff) > 0 {
			removeSet[zone] = diff
		}
	}
	return addSet, removeSet
}

// calculateNetworkEndpointDifference determines what endpoints needs to be added and removed in order to move current state to target state.
func calculateNetworkEndpointDifference(targetMap, currentMap map[string]negtypes.NetworkEndpointSet) (map[string]negtypes.NetworkEndpointSet, map[string]negtypes.NetworkEndpointSet) {
	addSet := map[string]negtypes.NetworkEndpointSet{}
	removeSet := map[string]negtypes.NetworkEndpointSet{}
	for zone, endpointSet := range targetMap {
		diff := endpointSet.Difference(currentMap[zone])
		if len(diff) > 0 {
			addSet[zone] = diff
		}
	}

	for zone, endpointSet := range currentMap {
		diff := endpointSet.Difference(targetMap[zone])
		if len(diff) > 0 {
			removeSet[zone] = diff
		}
	}
	return addSet, removeSet
}

// getService retrieves service object from serviceLister based on the input Namespace and Name
func getService(serviceLister cache.Indexer, namespace, name string) *apiv1.Service {
	if serviceLister == nil {
		return nil
	}
	service, exists, err := serviceLister.GetByKey(utils.ServiceKeyFunc(namespace, name))
	if exists && err == nil {
		return service.(*apiv1.Service)
	}
	if err != nil {
		klog.Errorf("Failed to retrieve service %s/%s from store: %v", namespace, name, err)
	}
	return nil
}

// ensureNetworkEndpointGroup ensures corresponding NEG is configured correctly in the specified zone.
func ensureNetworkEndpointGroup(svcNamespace, svcName, negName, zone, negServicePortName string, cloud negtypes.NetworkEndpointGroupCloud, serviceLister cache.Indexer, recorder record.EventRecorder) error {
	neg, err := cloud.GetNetworkEndpointGroup(negName, zone)
	if err != nil {
		// Most likely to be caused by non-existed NEG
		klog.V(4).Infof("Error while retriving %q in zone %q: %v", negName, zone, err)
	}

	needToCreate := false
	if neg == nil {
		needToCreate = true
	} else if !utils.EqualResourceIDs(neg.Network, cloud.NetworkURL()) ||
		!utils.EqualResourceIDs(neg.Subnetwork, cloud.SubnetworkURL()) {
		needToCreate = true
		klog.V(2).Infof("NEG %q in %q does not match network and subnetwork of the cluster. Deleting NEG.", negName, zone)
		err = cloud.DeleteNetworkEndpointGroup(negName, zone)
		if err != nil {
			return err
		} else {
			if recorder != nil && serviceLister != nil {
				if svc := getService(serviceLister, svcNamespace, svcName); svc != nil {
					recorder.Eventf(svc, apiv1.EventTypeNormal, "Delete", "Deleted NEG %q for %s in %q.", negName, negServicePortName, zone)
				}
			}
		}
	}

	if needToCreate {
		klog.V(2).Infof("Creating NEG %q for %s in %q.", negName, negServicePortName, zone)
		networkEndpointType := negIPPortNetworkEndpointType
		subnetwork := cloud.SubnetworkURL()
		if flags.F.CreateHybridNeg {
			networkEndpointType = negPrivateIPPortNetworkEdnpointType
			subnetwork = ""
		}
		err = cloud.CreateNetworkEndpointGroup(&compute.NetworkEndpointGroup{
			Name:                negName,
			NetworkEndpointType: networkEndpointType,
			Network:             cloud.NetworkURL(),
			Subnetwork:          subnetwork,
		}, zone)
		if err != nil {
			return err
		} else {
			if recorder != nil && serviceLister != nil {
				if svc := getService(serviceLister, svcNamespace, svcName); svc != nil {
					recorder.Eventf(svc, apiv1.EventTypeNormal, "Create", "Created NEG %q for %s in %q.", negName, negServicePortName, zone)
				}
			}
		}
	}
	return nil
}

// toZoneNetworkEndpointMap translates addresses in endpoints object and Istio:DestinationRule subset into zone and endpoints map
func toZoneNetworkEndpointMap(endpoints *apiv1.Endpoints, zoneGetter negtypes.ZoneGetter, targetPort string, podLister cache.Indexer, subsetLables string) (map[string]negtypes.NetworkEndpointSet, negtypes.EndpointPodMap, error) {
	zoneNetworkEndpointMap := map[string]negtypes.NetworkEndpointSet{}
	networkEndpointPodMap := negtypes.EndpointPodMap{}
	if endpoints == nil {
		klog.Errorf("Endpoint object is nil")
		return zoneNetworkEndpointMap, networkEndpointPodMap, nil
	}
	targetPortNum, _ := strconv.Atoi(targetPort)
	for _, subset := range endpoints.Subsets {
		matchPort := ""
		// service spec allows target Port to be a named Port.
		// support both explicit Port and named Port.
		for _, port := range subset.Ports {
			if targetPortNum != 0 {
				// TargetPort is int
				if int(port.Port) == targetPortNum {
					matchPort = targetPort
				}
			} else {
				// TargetPort is string
				if port.Name == targetPort {
					matchPort = strconv.Itoa(int(port.Port))
				}
			}
			if len(matchPort) > 0 {
				break
			}
		}

		// subset does not contain target Port
		if len(matchPort) == 0 {
			continue
		}

		// processAddressFunc adds the qualified endpoints from the input list into the endpointSet group by zone
		processAddressFunc := func(addresses []v1.EndpointAddress, includeAllEndpoints bool) error {
			for _, address := range addresses {
				// Apply the selector if Istio:DestinationRule subset labels provided.
				if subsetLables != "" {
					if address.TargetRef == nil || address.TargetRef.Kind != "Pod" {
						klog.V(2).Infof("Endpoint %q in Endpoints %s/%s does not have a Pod as the TargetRef object. Skipping", address.IP, endpoints.Namespace, endpoints.Name)
						continue
					}
					// Skip if the endpoint's pod not matching the subset lables.
					if !shouldPodBeInDestinationRuleSubset(podLister, address.TargetRef.Namespace, address.TargetRef.Name, subsetLables) {
						continue
					}
				}
				if address.NodeName == nil {
					klog.V(2).Infof("Endpoint %q in Endpoints %s/%s does not have an associated node. Skipping", address.IP, endpoints.Namespace, endpoints.Name)
					continue
				}
				if address.TargetRef == nil {
					klog.V(2).Infof("Endpoint %q in Endpoints %s/%s does not have an associated pod. Skipping", address.IP, endpoints.Namespace, endpoints.Name)
					continue
				}
				zone, err := zoneGetter.GetZoneForNode(*address.NodeName)
				if err != nil {
					return fmt.Errorf("failed to retrieve associated zone of node %q: %v", *address.NodeName, err)
				}
				if zoneNetworkEndpointMap[zone] == nil {
					zoneNetworkEndpointMap[zone] = negtypes.NewNetworkEndpointSet()
				}

				if includeAllEndpoints || shouldPodBeInNeg(podLister, address.TargetRef.Namespace, address.TargetRef.Name) {
					networkEndpoint := negtypes.NetworkEndpoint{IP: address.IP, Port: matchPort, Node: *address.NodeName}
					zoneNetworkEndpointMap[zone].Insert(networkEndpoint)
					networkEndpointPodMap[networkEndpoint] = types.NamespacedName{Namespace: address.TargetRef.Namespace, Name: address.TargetRef.Name}
				}
			}
			return nil
		}

		if err := processAddressFunc(subset.Addresses, true); err != nil {
			return nil, nil, err
		}
		if err := processAddressFunc(subset.NotReadyAddresses, false); err != nil {
			return nil, nil, err
		}
	}
	return zoneNetworkEndpointMap, networkEndpointPodMap, nil
}

// retrieveExistingZoneNetworkEndpointMap lists existing network endpoints in the neg and return the zone and endpoints map
func retrieveExistingZoneNetworkEndpointMap(negName string, zoneGetter negtypes.ZoneGetter, cloud negtypes.NetworkEndpointGroupCloud) (map[string]negtypes.NetworkEndpointSet, error) {
	zones, err := zoneGetter.ListZones()
	if err != nil {
		return nil, err
	}

	zoneNetworkEndpointMap := map[string]negtypes.NetworkEndpointSet{}
	for _, zone := range zones {
		zoneNetworkEndpointMap[zone] = negtypes.NewNetworkEndpointSet()
		networkEndpointsWithHealthStatus, err := cloud.ListNetworkEndpoints(negName, zone, false)
		if err != nil {
			return nil, err
		}
		for _, ne := range networkEndpointsWithHealthStatus {
			zoneNetworkEndpointMap[zone].Insert(negtypes.NetworkEndpoint{IP: ne.NetworkEndpoint.IpAddress, Node: ne.NetworkEndpoint.Instance, Port: strconv.FormatInt(ne.NetworkEndpoint.Port, 10)})
		}
	}
	return zoneNetworkEndpointMap, nil
}

// makeEndpointBatch return a batch of endpoint from the input and remove the endpoints from input set
// The return map has the encoded endpoint as key and GCE network endpoint object as value
func makeEndpointBatch(endpoints negtypes.NetworkEndpointSet) (map[negtypes.NetworkEndpoint]*compute.NetworkEndpoint, error) {
	endpointBatch := map[negtypes.NetworkEndpoint]*compute.NetworkEndpoint{}

	for i := 0; i < MAX_NETWORK_ENDPOINTS_PER_BATCH; i++ {
		networkEndpoint, ok := endpoints.PopAny()
		if !ok {
			break
		}

		portNum, err := strconv.Atoi(networkEndpoint.Port)
		if err != nil {
			return nil, fmt.Errorf("failed to decode endpoint port %v: %v", networkEndpoint, err)
		}

		endpointBatch[networkEndpoint] = &compute.NetworkEndpoint{
			IpAddress: networkEndpoint.IP,
			Port:      int64(portNum),
		}

		if !flags.F.CreateHybridNeg {
			endpointBatch[networkEndpoint].Instance = networkEndpoint.Node
		}
	}
	return endpointBatch, nil
}

func keyFunc(namespace, name string) string {
	return fmt.Sprintf("%s/%s", namespace, name)
}

// shouldPodBeInNeg returns true if pod is not in graceful termination state
func shouldPodBeInNeg(podLister cache.Indexer, namespace, name string) bool {
	if podLister == nil {
		return false
	}
	key := keyFunc(namespace, name)
	obj, exists, err := podLister.GetByKey(key)
	if err != nil {
		klog.Errorf("Failed to retrieve pod %s from pod lister: %v", key, err)
		return false
	}
	if !exists {
		return false
	}
	pod, ok := obj.(*v1.Pod)
	if !ok {
		klog.Errorf("Failed to convert obj %s to v1.Pod. The object type is %T", key, obj)
		return false
	}

	// if pod has DeletionTimestamp, that means pod is in graceful termination state.
	if pod.DeletionTimestamp != nil {
		return false
	}
	return true
}

// shouldPodBeInDestinationRuleSubset return ture if pod match the DestinationRule subset lables.
func shouldPodBeInDestinationRuleSubset(podLister cache.Indexer, namespace, name string, subsetLables string) bool {
	if podLister == nil {
		return false
	}
	key := keyFunc(namespace, name)
	obj, exists, err := podLister.GetByKey(key)
	if err != nil {
		klog.Errorf("Failed to retrieve pod %s from pod lister: %v", key, err)
		return false
	}
	if !exists {
		return false
	}
	pod, ok := obj.(*v1.Pod)
	if !ok {
		klog.Errorf("Failed to convert obj %s to v1.Pod. The object type is %T", key, obj)
		return false
	}

	selector, err := labels.Parse(subsetLables)
	if err != nil {
		klog.Errorf("Failed to parse the subset selectors.")
		return false
	}
	return selector.Matches(labels.Set(pod.Labels))
}
