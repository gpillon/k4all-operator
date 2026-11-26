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
	// Self is the operator's own pod: the drain never counts it, so the
	// drain of the node it runs on cannot stop it. That node goes last among
	// the nodes of its type, and once it is drained the operator evicts its
	// own pod, so the leader moves before the reboot (k4all#97).
	Self types.NamespacedName
	// Lock is shared with the node-settings rollout: one node at a time.
	Lock *NodeLock
	// Now is time.Now unless a test sets it.
	Now func() time.Time

	// selfEvictTried is the node whose drain evicted the operator's pod once.
	selfEvictTried string
}

// leavingRequeue is how soon a leader that evicted its own pod looks again,
// should its pod not be stopped.
const leavingRequeue = 10 * time.Second

// +kubebuilder:rbac:groups=k4all.magesgate.com,resources=clusterupgrades,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=k4all.magesgate.com,resources=clusterupgrades/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=k4all.magesgate.com,resources=clusterupgrades/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=pods/eviction,verbs=create
// +kubebuilder:rbac:groups=policy,resources=poddisruptionbudgets,verbs=get;list;watch

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

	in := clusterupgrade.Input{
		Spec:       cu.Spec,
		Previous:   cu.Status,
		Pods:       map[string][]clusterupgrade.Pod{},
		GraphURL:   cu.Spec.GraphURL,
		Generation: cu.Generation,
	}
	if in.GraphURL == "" {
		in.GraphURL = k4allv1alpha1.DefaultUpgradeGraphURL
	}
	_, in.Retry = cu.Annotations[k4allv1alpha1.ClusterUpgradeRetryAnnotation]
	in.Graph, in.GraphErr = r.Graphs.Get(ctx, in.GraphURL)

	unlock := r.Lock.lock()
	defer unlock()
	if r.Lock.left() {
		return ctrl.Result{RequeueAfter: leavingRequeue}, nil
	}
	in.Now = r.now()
	nodeList := &corev1.NodeList{}
	if err := r.nodeReader().List(ctx, nodeList); err != nil {
		return ctrl.Result{}, fmt.Errorf("list nodes: %w", err)
	}
	d := r.drainer()
	inFlight := ""
	for i := range nodeList.Items {
		n := &nodeList.Items[i]
		in.Nodes = append(in.Nodes, clusterupgrade.Node{
			Name: n.Name, Annotations: n.Annotations, Ready: nodeReady(n), Unschedulable: n.Spec.Unschedulable,
		})
		if n.Annotations[k4allv1alpha1.NodeUpgradeCordonedAnnotation] != "" {
			inFlight = n.Name
			pods, err := d.pods(ctx, n.Name)
			if err != nil {
				return ctrl.Result{}, err
			}
			in.Pods[n.Name] = pods
		}
	}
	if in.Self = d.self(ctx); in.Self != nil {
		in.SelfNode = in.Self.Node
	}
	if r.selfEvictTried != inFlight {
		r.selfEvictTried = ""
	}
	in.SelfEvictTried = inFlight != "" && r.selfEvictTried == inFlight

	pdbs, err := d.unhealthyPDBs(ctx)
	if err != nil {
		return ctrl.Result{}, err
	}
	in.UnhealthyPDBs = pdbs
	// Only an upgrade drains, and only with more than one node.
	if cu.Spec.TargetRelease != "" && len(nodeList.Items) > 1 {
		if in.Blockers, err = d.drainBlockers(ctx); err != nil {
			return ctrl.Result{}, err
		}
		// Read before the next cordon only: a node in flight moves first.
		if inFlight == "" {
			if in.BarePods, err = d.barePods(ctx); err != nil {
				return ctrl.Result{}, err
			}
		}
	}

	status, actions := clusterupgrade.Decide(in)
	// Actions first: the status must never claim a step the Nodes do not show.
	for _, a := range actions {
		log.Info("cluster upgrade step", "action", a.Kind, "node", a.Node, "release", a.Release)
		switch a.Kind {
		case clusterupgrade.Evict:
			refused, err := d.evict(ctx, a.Pods)
			if err != nil {
				return ctrl.Result{}, fmt.Errorf("%s %s: %w", a.Kind, a.Node, err)
			}
			clusterupgrade.NoteRefusedEvictions(&status, a.Node, refused)
		case clusterupgrade.EvictSelf:
			r.selfEvictTried = a.Node
			if evictSelf(ctx, d, a.Pods) {
				r.Lock.leave()
			}
		default:
			if err := r.run(ctx, a); err != nil {
				return ctrl.Result{}, fmt.Errorf("%s %s: %w", a.Kind, a.Node, err)
			}
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

// drainer drains with this reconciler's clients.
func (r *ClusterUpgradeReconciler) drainer() drainer {
	return drainer{Client: r.Client, APIReader: r.nodeReader(), Self: r.Self}
}

// nodeReader reads the Nodes past the cache (NodeLock).
func (r *ClusterUpgradeReconciler) nodeReader() client.Reader {
	if r.APIReader != nil {
		return r.APIReader
	}
	return r.Client
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
			"metadata": map[string]interface{}{
				"annotations": map[string]interface{}{k4allv1alpha1.NodeUpgradeCordonedAnnotation: mark},
				"labels":      map[string]interface{}{clusterupgrade.DrainingLabel: "true"},
			},
			"spec": map[string]interface{}{"unschedulable": true},
		})
	case clusterupgrade.Evict:
		_, err := r.drainer().evict(ctx, a.Pods)
		return err
	case clusterupgrade.Uncordon:
		// From here a cordon on the node is the admin's: Finish leaves it.
		return r.patchNode(ctx, a.Node, map[string]interface{}{
			"metadata": map[string]interface{}{
				"annotations": map[string]interface{}{k4allv1alpha1.NodeUpgradeCordonedAnnotation: k4allv1alpha1.NodeCordonReleased},
				"labels":      map[string]interface{}{clusterupgrade.DrainingLabel: nil},
			},
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
			"metadata": map[string]interface{}{
				"annotations": map[string]interface{}{
					k4allv1alpha1.NodeUpgradeCordonedAnnotation: nil,
					k4allv1alpha1.NodeUpgradeTargetAnnotation:   nil,
					k4allv1alpha1.NodeUpgradeDoneAnnotation:     nil,
				},
				// Also from a cordon that stays (the admin's): the drain is over.
				"labels": map[string]interface{}{clusterupgrade.DrainingLabel: nil},
			},
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
