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

package route

import (
	"fmt"
	"net"
	"sync"
	"time"

	"strings"

	"github.com/golang/glog"
	"k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	coreinformers "k8s.io/client-go/informers/core/v1"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	v1core "k8s.io/client-go/kubernetes/typed/core/v1"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/cloud-provider-alibaba-cloud/cloud-controller-manager"
	v1node "k8s.io/kubernetes/pkg/api/v1/node"
	"k8s.io/kubernetes/pkg/cloudprovider"
	"k8s.io/kubernetes/pkg/controller"
	"k8s.io/kubernetes/pkg/util/metrics"
	nodeutil "k8s.io/kubernetes/pkg/util/node"
)

const (
	// Maximal number of concurrent CreateRoute API calls.
	// TODO: This should be per-provider.
	maxConcurrentRouteCreations int = 200
	// Maximum number of retries of route creations.
	maxRetries int = 5
	// Maximum number of retries of node status update.
	updateNodeStatusMaxRetries int = 3
)

type RouteController struct {
	routes           cloudprovider.Routes
	kubeClient       clientset.Interface
	clusterName      string
	clusterCIDR      *net.IPNet
	nodeLister       corelisters.NodeLister
	nodeListerSynced cache.InformerSynced
	broadcaster      record.EventBroadcaster
	recorder         record.EventRecorder
}

func New(routes cloudprovider.Routes, kubeClient clientset.Interface, nodeInformer coreinformers.NodeInformer, clusterName string, clusterCIDR *net.IPNet) *RouteController {
	if kubeClient != nil && kubeClient.CoreV1().RESTClient().GetRateLimiter() != nil {
		metrics.RegisterMetricAndTrackRateLimiterUsage("route_controller", kubeClient.CoreV1().RESTClient().GetRateLimiter())
	}

	if clusterCIDR == nil {
		glog.Fatal("RouteController: Must specify clusterCIDR.")
	}

	eventBroadcaster := record.NewBroadcaster()
	eventBroadcaster.StartLogging(glog.Infof)
	recorder := eventBroadcaster.NewRecorder(scheme.Scheme, v1.EventSource{Component: "route_controller"})

	rc := &RouteController{
		routes:           routes,
		kubeClient:       kubeClient,
		clusterName:      clusterName,
		clusterCIDR:      clusterCIDR,
		nodeLister:       nodeInformer.Lister(),
		nodeListerSynced: nodeInformer.Informer().HasSynced,
		broadcaster:      eventBroadcaster,
		recorder:         recorder,
	}

	return rc
}

func (rc *RouteController) Run(stopCh <-chan struct{}, syncPeriod time.Duration) {
	defer utilruntime.HandleCrash()

	glog.Info("Starting route controller")
	defer glog.Info("Shutting down route controller")

	if !controller.WaitForCacheSync("route", stopCh, rc.nodeListerSynced) {
		return
	}

	if rc.broadcaster != nil {
		rc.broadcaster.StartRecordingToSink(&v1core.EventSinkImpl{Interface: v1core.New(rc.kubeClient.CoreV1().RESTClient()).Events("")})
	}

	// TODO: If we do just the full Resync every 5 minutes (default value)
	// that means that we may wait up to 5 minutes before even starting
	// creating a route for it. This is bad.
	// We should have a watch on node and if we observe a new node (with CIDR?)
	// trigger reconciliation for that node.
	go wait.NonSlidingUntil(func() {
		if err := rc.reconcileNodeRoutes(); err != nil {
			glog.Errorf("Couldn't reconcile node routes: %v", err)
		}
	}, syncPeriod, stopCh)

	<-stopCh
}

func (rc *RouteController) reconcileNodeRoutes() error {
	routeList, err := rc.routes.ListRoutes(rc.clusterName)
	if err != nil {
		return fmt.Errorf("error listing routes: %v", err)
	}
	nodes, err := rc.nodeLister.List(labels.Everything())
	if err != nil {
		return fmt.Errorf("error listing nodes: %v", err)
	}
	return rc.reconcile(nodes, routeList)
}

func (rc *RouteController) reconcile(nodes []*v1.Node, routes []*cloudprovider.Route) error {
	// nodeCIDRs maps nodeName->nodeCIDR
	nodeCIDRs := make(map[types.NodeName]string)
	// routeMap maps routeTargetNode->route
	routeMap := make(map[types.NodeName]*cloudprovider.Route)
	for _, route := range routes {
		glog.V(5).Infof("ListRoutes: reconcile %+v", route)
		if route.TargetNode != "" {
			routeMap[route.TargetNode] = route
		}
	}

	wg := sync.WaitGroup{}

	// Aoxn: Alibaba Cloud does not support concurrent route operation
	rateLimiter := make(chan struct{}, 1)
	for _, route := range routes {
		if rc.isResponsibleForRoute(route) {
			nodesCidrRealContainsRoute := false
			for _, node := range nodes {
				if node.Spec.PodCIDR != "" {
					if realContains, err := alicloud.RealContainsCidr(node.Spec.PodCIDR, route.DestinationCIDR); err != nil {
						glog.Errorf("Could not judge relation between cidrs, will skip. node: %s, route: %s. error: %v", node.Spec.PodCIDR, route.DestinationCIDR, err)
						continue
					} else if realContains {
						nodesCidrRealContainsRoute = true
						break
					}
				}
			}

			// Check if this route is a blackhole, or applies to a node we know about & has an incorrect CIDR.
			if  route.Blackhole || nodesCidrRealContainsRoute {
				glog.Infof("responsible for: %t, %t", route.Blackhole, nodesCidrRealContainsRoute)
				wg.Add(1)
				// Delete the route.
				startTime := time.Now()
				// Aoxn: Alibaba Cloud does not support concurrent route operation
				//go func(route *cloudprovider.Route, startTime time.Time) {
				glog.Infof("Deleting route %s %s", route.Name, route.DestinationCIDR)
				if err := rc.routes.DeleteRoute(rc.clusterName, route); err != nil {
					glog.Errorf("Could not delete route %s %s after %v: %v", route.Name, route.DestinationCIDR, time.Now().Sub(startTime), err)
				} else {
					glog.Infof("Deleted route %s %s after %v", route.Name, route.DestinationCIDR, time.Now().Sub(startTime))
				}
				wg.Done()

				//}(route, time.Now())
			}
		}
	}
	wg.Wait()
	for _, node := range nodes {
		// Skip if the node hasn't been assigned a CIDR yet.
		if node.Spec.PodCIDR == "" {
			// UpdateNetworkUnavailable When PodCIDR is not allocated.
			rc.updateNetworkingCondition(types.NodeName(node.Name), false)
			continue
		}
		if node.Spec.ProviderID == "" {
			glog.Errorf("Node %s has no Provider ID, skip it", node.Name)
		}
		providerID := types.NodeName(node.Spec.ProviderID)
		// Check if we have a route for this node w/ the correct CIDR.
		r := routeMap[providerID]
		glog.V(5).Infof("Node: %s, r=%+v, node.Spec.PodCIDR=%s", node.Name, r, node.Spec.PodCIDR)
		if r == nil || r.DestinationCIDR != node.Spec.PodCIDR {
			// If not, create the route.
			route := &cloudprovider.Route{
				TargetNode:      providerID,
				DestinationCIDR: node.Spec.PodCIDR,
			}

			wg.Add(1)
			go func(node *v1.Node, route *cloudprovider.Route) {
				defer wg.Done()
				backoff := wait.Backoff{
					Duration: 4 * time.Second,
					Factor:   2,
					Jitter:   1,
					Steps:    8,
				}
				wait.ExponentialBackoff(backoff, func() (bool, error) {
					startTime := time.Now()
					nameHint := node.Name
					// Ensure that we don't have more than maxConcurrentRouteCreations
					// CreateRoute calls in flight.
					rateLimiter <- struct{}{}
					glog.Infof("Creating route for node %s %s with hint %s, throttled %v", node.Name, route.DestinationCIDR, nameHint, time.Now().Sub(startTime))
					err := rc.routes.CreateRoute(rc.clusterName, nameHint, route)
					if err != nil && strings.Contains(err.Error(), "please wait a moment and try again") {
						// Throttled, wait a second.
						glog.Infof("alicloud: throttle triggered. sleep for 10s before proceeding.")
						time.Sleep(5 * time.Second)
					}
					<-rateLimiter

					rc.updateNetworkingCondition(types.NodeName(node.Name), err == nil)
					if err != nil {
						msg := fmt.Sprintf("Could not create route %s %s for node %s after %v: %v", nameHint, route.DestinationCIDR, node.Name, time.Now().Sub(startTime), err)
						if rc.recorder != nil {
							rc.recorder.Eventf(
								&v1.ObjectReference{
									Kind:      "Node",
									Name:      node.Name,
									UID:       node.UID,
									Namespace: "",
								}, v1.EventTypeWarning, "FailedToCreateRoute", msg)
						}
						glog.Error(msg)

					} else {
						glog.Infof("Created route for node %s %s with hint %s after %v", node.Name, route.DestinationCIDR, nameHint, time.Now().Sub(startTime))
						return true, nil
					}
					return false, nil
				})
			}(node, route)
		} else {
			// Update condition only if it doesn't reflect the current state.
			_, condition := v1node.GetNodeCondition(&node.Status, v1.NodeNetworkUnavailable)
			if condition == nil || condition.Status != v1.ConditionFalse {
				rc.updateNetworkingCondition(types.NodeName(node.Name), true)
			}
		}
		nodeCIDRs[providerID] = node.Spec.PodCIDR
	}
	wg.Wait()
	return nil
}

func (rc *RouteController) updateNetworkingCondition(nodeName types.NodeName, routeCreated bool) error {
	var err error
	for i := 0; i < updateNodeStatusMaxRetries; i++ {
		// Patch could also fail, even though the chance is very slim. So we still do
		// patch in the retry loop.
		currentTime := metav1.Now()
		if routeCreated {
			err = nodeutil.SetNodeCondition(rc.kubeClient, nodeName, v1.NodeCondition{
				Type:               v1.NodeNetworkUnavailable,
				Status:             v1.ConditionFalse,
				Reason:             "RouteCreated",
				Message:            "RouteController created a route",
				LastTransitionTime: currentTime,
			})
		} else {
			err = nodeutil.SetNodeCondition(rc.kubeClient, nodeName, v1.NodeCondition{
				Type:               v1.NodeNetworkUnavailable,
				Status:             v1.ConditionTrue,
				Reason:             "NoRouteCreated",
				Message:            "RouteController failed to create a route",
				LastTransitionTime: currentTime,
			})
		}
		if err == nil {
			return nil
		}
		if !errors.IsConflict(err) {
			glog.Errorf("Error updating node %s: %v", nodeName, err)
			return err
		}
		glog.V(4).Infof("Error updating node %s, retrying: %v", nodeName, err)
	}
	glog.Errorf("Error updating node %s: %v", nodeName, err)
	return err
}

func (rc *RouteController) isResponsibleForRoute(route *cloudprovider.Route) bool {
	_, cidr, err := net.ParseCIDR(route.DestinationCIDR)
	if err != nil {
		glog.Errorf("Ignoring route %s, unparsable CIDR: %v", route.Name, err)
		return false
	}
	// Not responsible if this route's CIDR is not within our clusterCIDR
	lastIP := make([]byte, len(cidr.IP))
	for i := range lastIP {
		lastIP[i] = cidr.IP[i] | ^cidr.Mask[i]
	}
	if !rc.clusterCIDR.Contains(cidr.IP) || !rc.clusterCIDR.Contains(lastIP) {
		return false
	}
	return true
}
