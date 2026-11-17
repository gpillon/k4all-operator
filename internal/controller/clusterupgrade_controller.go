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
	"encoding/json"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	k4allv1alpha1 "github.com/gpillon/k4all-operator/api/v1alpha1"
	"github.com/gpillon/k4all-operator/internal/clusterupgrade"
	"github.com/gpillon/k4all-operator/internal/upgradegraph"
)

// GraphSource returns the upgrade graph published at a URL.
type GraphSource interface {
	Get(ctx context.Context, url string) (*upgradegraph.Graph, error)
}

// ClusterUpgradeReconciler runs the ClusterUpgrade named k4all: it decides the
// next step with clusterupgrade.Decide and applies it to the Nodes. It is
// separate from the ReleaseReconciler, which pauses while a node runs a hop.
type ClusterUpgradeReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// APIReader lists the pods of a node without caching every pod.
	APIReader client.Reader
	Graphs    GraphSource
	// Self is the operator's own pod: it is never evicted, so the drain of
	// the node it runs on cannot stop it.
	Self types.NamespacedName
	// Now is time.Now unless a test sets it.
	Now func() time.Time
}

// +kubebuilder:rbac:groups=k4all.magesgate.com,resources=clusterupgrades,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=k4all.magesgate.com,resources=clusterupgrades/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=k4all.magesgate.com,resources=clusterupgrades/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=pods/eviction,verbs=create

// Reconcile moves the ClusterUpgrade one step on.
func (r *ClusterUpgradeReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	if req.Name != k4allv1alpha1.ClusterUpgradeName {
		return ctrl.Result{}, nil
	}
	cu := &k4allv1alpha1.ClusterUpgrade{}
	if err := r.Get(ctx, req.NamespacedName, cu); apierrors.IsNotFound(err) {
		cu = &k4allv1alpha1.ClusterUpgrade{ObjectMeta: metav1.ObjectMeta{Name: k4allv1alpha1.ClusterUpgradeName}}
		if err := r.Create(ctx, cu); err != nil {
			return ctrl.Result{}, fmt.Errorf("create ClusterUpgrade %s: %w", cu.Name, err)
		}
	} else if err != nil {
		return ctrl.Result{}, err
	}

	nodeList := &corev1.NodeList{}
	if err := r.List(ctx, nodeList); err != nil {
		return ctrl.Result{}, fmt.Errorf("list nodes: %w", err)
	}
	in := clusterupgrade.Input{
		Spec:       cu.Spec,
		Previous:   cu.Status,
		PodsLeft:   map[string]int{},
		GraphURL:   cu.Spec.GraphURL,
		Now:        r.now(),
		Generation: cu.Generation,
	}
	if in.GraphURL == "" {
		in.GraphURL = k4allv1alpha1.DefaultUpgradeGraphURL
	}
	_, in.Retry = cu.Annotations[k4allv1alpha1.ClusterUpgradeRetryAnnotation]
	in.Graph, in.GraphErr = r.Graphs.Get(ctx, in.GraphURL)
	for i := range nodeList.Items {
		n := &nodeList.Items[i]
		in.Nodes = append(in.Nodes, clusterupgrade.Node{
			Name: n.Name, Annotations: n.Annotations, Ready: nodeReady(n), Unschedulable: n.Spec.Unschedulable,
		})
		if n.Annotations[k4allv1alpha1.NodeUpgradeCordonedAnnotation] != "" {
			pods, err := r.podsToEvict(ctx, n.Name)
			if err != nil {
				return ctrl.Result{}, err
			}
			in.PodsLeft[n.Name] = len(pods)
		}
	}

	status, actions := clusterupgrade.Decide(in)
	// Actions first: the status must never claim a step the Nodes do not show.
	for _, a := range actions {
		log.Info("cluster upgrade step", "action", a.Kind, "node", a.Node, "release", a.Release)
		if err := r.run(ctx, a); err != nil {
			return ctrl.Result{}, fmt.Errorf("%s %s: %w", a.Kind, a.Node, err)
		}
	}

	base := cu.DeepCopy()
	status.ObservedGeneration = cu.Generation
	cu.Status = status
	if err := r.Status().Patch(ctx, cu, client.MergeFrom(base)); err != nil {
		return ctrl.Result{}, fmt.Errorf("patch ClusterUpgrade status: %w", err)
	}
	if in.Retry {
		patch := []byte(`{"metadata":{"annotations":{"` + k4allv1alpha1.ClusterUpgradeRetryAnnotation + `":null}}}`)
		if err := r.Patch(ctx, cu, client.RawPatch(types.MergePatchType, patch)); err != nil {
			return ctrl.Result{}, fmt.Errorf("remove the retry annotation: %w", err)
		}
	}
	return ctrl.Result{RequeueAfter: requeueAfter(status)}, nil
}

// requeueAfter is how soon to look again. Node changes trigger a reconcile
// anyway; the drain and the graph need polling.
func requeueAfter(status k4allv1alpha1.ClusterUpgradeStatus) time.Duration {
	for _, n := range status.Nodes {
		if n.Phase == k4allv1alpha1.NodeUpgradeDraining {
			return 10 * time.Second
		}
	}
	if status.Phase == k4allv1alpha1.ClusterUpgradeProgressing {
		return 30 * time.Second
	}
	return 10 * time.Minute
}

func (r *ClusterUpgradeReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func nodeReady(n *corev1.Node) bool {
	for _, c := range n.Status.Conditions {
		if c.Type == corev1.NodeReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

// podsToEvict lists the pods the drain moves off nodeName: not the
// DaemonSet ones (they would come back), not the static ones, not finished
// ones, not the operator itself. Terminating pods count, as they are still
// there.
func (r *ClusterUpgradeReconciler) podsToEvict(ctx context.Context, nodeName string) ([]corev1.Pod, error) {
	pods := &corev1.PodList{}
	if err := r.APIReader.List(ctx, pods, client.MatchingFields{"spec.nodeName": nodeName}); err != nil {
		return nil, fmt.Errorf("list pods on %s: %w", nodeName, err)
	}
	out := make([]corev1.Pod, 0, len(pods.Items))
	for _, p := range pods.Items {
		if p.Spec.NodeName != nodeName || // the fake client ignores field selectors
			p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed ||
			(p.Namespace == r.Self.Namespace && p.Name == r.Self.Name) {
			continue
		}
		if _, mirror := p.Annotations[corev1.MirrorPodAnnotationKey]; mirror {
			continue
		}
		if ownedByDaemonSet(p) {
			continue
		}
		out = append(out, p)
	}
	return out, nil
}

func ownedByDaemonSet(p corev1.Pod) bool {
	for _, o := range p.OwnerReferences {
		if o.Kind == "DaemonSet" {
			return true
		}
	}
	return false
}

// run applies one action of Decide to its Node.
func (r *ClusterUpgradeReconciler) run(ctx context.Context, a clusterupgrade.Action) error {
	switch a.Kind {
	case clusterupgrade.Cordon:
		node := &corev1.Node{}
		if err := r.Get(ctx, types.NamespacedName{Name: a.Node}, node); err != nil {
			return err
		}
		// A node the admin had cordoned keeps its cordon after the hop.
		mark := k4allv1alpha1.NodeCordonedByOperator
		if node.Spec.Unschedulable {
			mark = k4allv1alpha1.NodeCordonedAlready
		}
		return r.patchNode(ctx, a.Node, map[string]interface{}{
			"metadata": map[string]interface{}{"annotations": map[string]interface{}{
				k4allv1alpha1.NodeUpgradeCordonedAnnotation: mark,
			}},
			"spec": map[string]interface{}{"unschedulable": true},
		})
	case clusterupgrade.Evict:
		return r.evict(ctx, a.Node)
	case clusterupgrade.Uncordon:
		// From here a cordon on the node is the admin's: Finish leaves it.
		return r.patchNode(ctx, a.Node, map[string]interface{}{
			"metadata": map[string]interface{}{"annotations": map[string]interface{}{
				k4allv1alpha1.NodeUpgradeCordonedAnnotation: k4allv1alpha1.NodeCordonReleased,
			}},
			"spec": map[string]interface{}{"unschedulable": false},
		})
	case clusterupgrade.SetTarget:
		return r.patchNode(ctx, a.Node, map[string]interface{}{
			"metadata": map[string]interface{}{"annotations": map[string]interface{}{
				k4allv1alpha1.NodeUpgradeTargetAnnotation: a.Release,
			}},
		})
	case clusterupgrade.Finish:
		node := &corev1.Node{}
		if err := r.Get(ctx, types.NamespacedName{Name: a.Node}, node); err != nil {
			return err
		}
		patch := map[string]interface{}{
			"metadata": map[string]interface{}{"annotations": map[string]interface{}{
				k4allv1alpha1.NodeUpgradeCordonedAnnotation: nil,
				k4allv1alpha1.NodeUpgradeTargetAnnotation:   nil,
				k4allv1alpha1.NodeUpgradeDoneAnnotation:     nil,
			}},
		}
		if node.Annotations[k4allv1alpha1.NodeUpgradeCordonedAnnotation] == k4allv1alpha1.NodeCordonedByOperator {
			patch["spec"] = map[string]interface{}{"unschedulable": false}
		}
		return r.patchNode(ctx, a.Node, patch)
	}
	return fmt.Errorf("unknown action %q", a.Kind)
}

func (r *ClusterUpgradeReconciler) patchNode(ctx context.Context, name string, patch map[string]interface{}) error {
	data, err := json.Marshal(patch)
	if err != nil {
		return err
	}
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}}
	return r.Patch(ctx, node, client.RawPatch(types.MergePatchType, data))
}

// evict asks the API server to evict each pod the drain moves. A
// PodDisruptionBudget refusing it (429) is retried at the next reconcile,
// until the drain timeout.
func (r *ClusterUpgradeReconciler) evict(ctx context.Context, nodeName string) error {
	log := logf.FromContext(ctx)
	pods, err := r.podsToEvict(ctx, nodeName)
	if err != nil {
		return err
	}
	for i := range pods {
		p := &pods[i]
		if p.DeletionTimestamp != nil {
			continue
		}
		eviction := &policyv1.Eviction{ObjectMeta: metav1.ObjectMeta{Name: p.Name, Namespace: p.Namespace}}
		err := r.SubResource("eviction").Create(ctx, p, eviction)
		switch {
		case err == nil, apierrors.IsNotFound(err):
		case apierrors.IsTooManyRequests(err):
			log.Info("eviction refused for now", "pod", client.ObjectKeyFromObject(p), "reason", err.Error())
		default:
			return fmt.Errorf("evict %s/%s: %w", p.Namespace, p.Name, err)
		}
	}
	return nil
}

// SetupWithManager sets up the controller with the Manager. Every Node
// change reaches the ClusterUpgrade, as k4all-upgrade reports through the
// Node annotations.
func (r *ClusterUpgradeReconciler) SetupWithManager(mgr ctrl.Manager) error {
	toUpgrade := handler.EnqueueRequestsFromMapFunc(func(context.Context, client.Object) []reconcile.Request {
		return []reconcile.Request{{NamespacedName: types.NamespacedName{Name: k4allv1alpha1.ClusterUpgradeName}}}
	})
	return ctrl.NewControllerManagedBy(mgr).
		For(&k4allv1alpha1.ClusterUpgrade{}).
		Watches(&corev1.Node{}, toUpgrade).
		Named("clusterupgrade").
		Complete(r)
}
