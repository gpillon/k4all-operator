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
	"strings"

	"github.com/go-logr/logr"
	k4allv1alpha1 "github.com/gpillon/k4all-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// NodeConfigReconciler watches Node objects and ensures a matching
// NodeConfig CR exists for each node, populated with observed state.
type NodeConfigReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	Log    logr.Logger
}

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
	return ctrl.Result{}, nil
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
