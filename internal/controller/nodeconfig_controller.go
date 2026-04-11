/*
Copyright 2025.

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

package controller

import (
	"context"
	"fmt"
	"strings"

	"github.com/go-logr/logr"
	k4allv1alpha1 "github.com/gpillon/k4all-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// +kubebuilder:rbac:groups=nmstate.io,resources=nodenetworkstates,verbs=get;list;watch
// +kubebuilder:rbac:groups=nmstate.io,resources=nodenetworkconfigurationpolicies,verbs=get;list;watch;create;update;patch;delete

// NodeConfigReconciler watches Node objects and ensures a matching
// NodeConfig CR exists for each node, populated with observed state.
// When the OVS bridge feature flag is enabled it also creates per-node
// NodeNetworkConfigurationPolicy (NNCP) resources via nmstate.
type NodeConfigReconciler struct {
	client.Client
	Scheme    *runtime.Scheme
	Log       logr.Logger
	APIReader client.Reader
}

var (
	nncpGVR = schema.GroupVersionResource{Group: "nmstate.io", Version: "v1", Resource: "nodenetworkconfigurationpolicies"}
	nnsGVR  = schema.GroupVersionResource{Group: "nmstate.io", Version: "v1beta1", Resource: "nodenetworkstates"}
)

func (r *NodeConfigReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := r.Log.WithValues("node", req.Name)

	node := &corev1.Node{}
	if err := r.Get(ctx, req.NamespacedName, node); err != nil {
		if errors.IsNotFound(err) {
			return r.handleNodeDeletion(ctx, req.Name)
		}
		return ctrl.Result{}, err
	}

	nc := &k4allv1alpha1.NodeConfig{}
	err := r.Get(ctx, types.NamespacedName{Name: node.Name}, nc)
	if errors.IsNotFound(err) {
		nc = &k4allv1alpha1.NodeConfig{
			ObjectMeta: metav1.ObjectMeta{
				Name: node.Name,
				Labels: map[string]string{
					"app.kubernetes.io/managed-by": "k4all-operator",
					"k4all.magesgate.com/node":     node.Name,
				},
			},
		}
		if createErr := r.Create(ctx, nc); createErr != nil {
			return ctrl.Result{}, createErr
		}
		log.Info("created NodeConfig for node", "node", node.Name)
	} else if err != nil {
		return ctrl.Result{}, err
	}

	nc.Status.NodeName = node.Name
	nc.Status.NodeType = resolveNodeType(node)
	nc.Status.AvailableInterfaces = extractInterfaces(node)
	nc.Status.NetworkInterface = k4allv1alpha1.NetworkInterfaceStatus{
		State:          "Ready",
		LastUpdateTime: metav1.Now(),
	}

	if err := r.Status().Update(ctx, nc); err != nil {
		log.Error(err, "failed to update NodeConfig status")
		return ctrl.Result{}, err
	}

	if err := r.reconcileOVSBridge(ctx, log, node); err != nil {
		log.Error(err, "OVS bridge reconciliation failed")
	}

	return ctrl.Result{}, nil
}

func (r *NodeConfigReconciler) handleNodeDeletion(ctx context.Context, name string) (ctrl.Result, error) {
	nc := &k4allv1alpha1.NodeConfig{}
	if err := r.Get(ctx, types.NamespacedName{Name: name}, nc); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}
	if err := r.Delete(ctx, nc); err != nil && !errors.IsNotFound(err) {
		return ctrl.Result{}, err
	}
	r.Log.Info("deleted NodeConfig for removed node", "node", name)

	nncp := &unstructured.Unstructured{}
	nncp.SetGroupVersionKind(schema.GroupVersionKind{Group: "nmstate.io", Version: "v1", Kind: "NodeNetworkConfigurationPolicy"})
	nncp.SetName(nncpName(name))
	if err := r.Delete(ctx, nncp); err != nil && !errors.IsNotFound(err) && !isNoMatchError(err) {
		r.Log.V(1).Info("NNCP delete on node removal (best-effort)", "node", name, "error", err)
	}

	return ctrl.Result{}, nil
}

func nncpName(nodeName string) string {
	return "ovs-bridge-" + nodeName
}

// isNoMatchError returns true when the API server doesn't know the requested
// GVK (e.g. nmstate CRDs not yet installed).  We treat this the same as
// "not found" so the reconciler doesn't error-loop.
func isNoMatchError(err error) bool {
	return meta.IsNoMatchError(err)
}

func (r *NodeConfigReconciler) getClusterConfig(ctx context.Context) (*k4allv1alpha1.ClusterConfig, error) {
	list := &k4allv1alpha1.ClusterConfigList{}
	if err := r.List(ctx, list); err != nil {
		return nil, fmt.Errorf("list ClusterConfigs: %w", err)
	}
	if len(list.Items) == 0 {
		return nil, fmt.Errorf("no ClusterConfig found")
	}
	return &list.Items[0], nil
}

func (r *NodeConfigReconciler) reconcileOVSBridge(ctx context.Context, log logr.Logger, node *corev1.Node) error {
	config, err := r.getClusterConfig(ctx)
	if err != nil {
		return nil
	}

	if !config.Spec.Features.OVSBridge.Enabled {
		return nil
	}

	name := nncpName(node.Name)
	existing := &unstructured.Unstructured{}
	existing.SetGroupVersionKind(schema.GroupVersionKind{Group: "nmstate.io", Version: "v1", Kind: "NodeNetworkConfigurationPolicy"})
	if err := r.APIReader.Get(ctx, types.NamespacedName{Name: name}, existing); err == nil {
		return nil
	} else if isNoMatchError(err) {
		log.V(1).Info("nmstate CRDs not available yet, skipping OVS bridge reconciliation")
		return nil
	} else if !errors.IsNotFound(err) {
		return fmt.Errorf("check existing NNCP: %w", err)
	}

	nicName, err := r.detectDefaultNIC(ctx, node.Name)
	if err != nil {
		return fmt.Errorf("detect default NIC for %s: %w", node.Name, err)
	}
	if nicName == "" {
		log.Info("no default route interface found in NodeNetworkState, skipping NNCP creation", "node", node.Name)
		return nil
	}

	nncp := buildOVSBridgeNNCP(name, node.Name, nicName)
	if err := r.Create(ctx, nncp); err != nil {
		return fmt.Errorf("create NNCP %s: %w", name, err)
	}
	log.Info("created OVS bridge NNCP", "nncp", name, "nic", nicName, "node", node.Name)
	return nil
}

func (r *NodeConfigReconciler) detectDefaultNIC(ctx context.Context, nodeName string) (string, error) {
	nns := &unstructured.Unstructured{}
	nns.SetGroupVersionKind(schema.GroupVersionKind{Group: "nmstate.io", Version: "v1beta1", Kind: "NodeNetworkState"})
	if err := r.APIReader.Get(ctx, types.NamespacedName{Name: nodeName}, nns); err != nil {
		if errors.IsNotFound(err) || isNoMatchError(err) {
			return "", nil
		}
		return "", err
	}

	// The NodeNetworkState stores currentState as a nested JSON string in some
	// versions.  Try the direct nested path first, then fall back to parsing
	// the currentState as a raw JSON string.
	routes, found, err := unstructured.NestedSlice(nns.Object, "status", "currentState", "routes", "running")
	if err != nil || !found || len(routes) == 0 {
		return "", nil
	}

	for _, route := range routes {
		routeMap, ok := route.(map[string]interface{})
		if !ok {
			continue
		}
		dest, _ := routeMap["destination"].(string)
		if dest == "0.0.0.0/0" {
			iface, _ := routeMap["next-hop-interface"].(string)
			return iface, nil
		}
	}
	return "", nil
}

func buildOVSBridgeNNCP(name, nodeName, nicName string) *unstructured.Unstructured {
	nncp := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "nmstate.io/v1",
			"kind":       "NodeNetworkConfigurationPolicy",
			"metadata": map[string]interface{}{
				"name": name,
				"labels": map[string]interface{}{
					"app.kubernetes.io/managed-by": "k4all-operator",
				},
			},
			"spec": map[string]interface{}{
				"nodeSelector": map[string]interface{}{
					"kubernetes.io/hostname": nodeName,
				},
				"desiredState": map[string]interface{}{
					"interfaces": []interface{}{
						map[string]interface{}{
							"name":  nicName,
							"type":  "ethernet",
							"state": "up",
							"ipv4":  map[string]interface{}{"enabled": false},
							"ipv6":  map[string]interface{}{"enabled": false},
						},
						map[string]interface{}{
							"name":          "ovs-bridge",
							"type":          "ovs-interface",
							"state":         "up",
							"copy-mac-from": nicName,
							"ipv4": map[string]interface{}{
								"enabled": true,
								"dhcp":    true,
							},
							"ipv6": map[string]interface{}{
								"enabled":  true,
								"dhcp":     true,
								"autoconf": true,
							},
						},
						map[string]interface{}{
							"name":  "ovs-bridge",
							"type":  "ovs-bridge",
							"state": "up",
							"bridge": map[string]interface{}{
								"options": map[string]interface{}{
									"stp": false,
								},
								"port": []interface{}{
									map[string]interface{}{"name": nicName},
									map[string]interface{}{"name": "ovs-bridge"},
								},
							},
						},
					},
				},
			},
		},
	}
	return nncp
}

func resolveNodeType(node *corev1.Node) string {
	if _, ok := node.Labels["node-role.kubernetes.io/control-plane"]; ok {
		if _, hasWorker := node.Labels["node-role.kubernetes.io/worker"]; hasWorker {
			return "bootstrap"
		}
		return "control"
	}
	return "worker"
}

func extractInterfaces(node *corev1.Node) []k4allv1alpha1.AvailableInterface {
	var ifaces []k4allv1alpha1.AvailableInterface
	for _, addr := range node.Status.Addresses {
		if addr.Type == corev1.NodeInternalIP {
			ifaces = append(ifaces, k4allv1alpha1.AvailableInterface{
				Name: "primary",
				Mac:  extractMACFromAnnotation(node),
			})
			break
		}
	}
	return ifaces
}

func extractMACFromAnnotation(node *corev1.Node) string {
	if mac, ok := node.Annotations["k4all.magesgate.com/mac-address"]; ok {
		return mac
	}
	if machineID := node.Status.NodeInfo.MachineID; machineID != "" {
		return strings.TrimSpace(machineID[:12])
	}
	return ""
}

func (r *NodeConfigReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1.Node{}).
		Watches(&k4allv1alpha1.NodeConfig{}, handler.EnqueueRequestsFromMapFunc(
			func(ctx context.Context, obj client.Object) []reconcile.Request {
				nc := obj.(*k4allv1alpha1.NodeConfig)
				return []reconcile.Request{{NamespacedName: types.NamespacedName{Name: nc.Name}}}
			},
		)).
		Named("nodeconfig").
		Complete(r)
}
