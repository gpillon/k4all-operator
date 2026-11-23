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
	"errors"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
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

	nodeList := &corev1.NodeList{}
	if err := r.List(ctx, nodeList); err != nil {
		return ctrl.Result{}, fmt.Errorf("list nodes: %w", err)
	}
	in := clusterupgrade.Input{
		Spec:       cu.Spec,
		Previous:   cu.Status,
		Pods:       map[string][]clusterupgrade.Pod{},
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
			for _, p := range pods {
				in.Pods[n.Name] = append(in.Pods[n.Name], clusterupgrade.Pod{
					Namespace: p.Namespace, Name: p.Name, Created: p.CreationTimestamp.Time,
					Terminating: p.DeletionTimestamp != nil,
				})
			}
		}
	}

	pdbs, err := r.unhealthyPDBs(ctx)
	if err != nil {
		return ctrl.Result{}, err
	}
	in.UnhealthyPDBs = pdbs
	// Only an upgrade drains, and only with more than one node.
	if cu.Spec.TargetRelease != "" && len(nodeList.Items) > 1 {
		if in.Blockers, err = r.drainBlockers(ctx); err != nil {
			return ctrl.Result{}, err
		}
	}

	status, actions := clusterupgrade.Decide(in)
	// Actions first: the status must never claim a step the Nodes do not show.
	for _, a := range actions {
		log.Info("cluster upgrade step", "action", a.Kind, "node", a.Node, "release", a.Release)
		if a.Kind == clusterupgrade.Evict {
			refused, err := r.evict(ctx, a.Pods)
			if err != nil {
				return ctrl.Result{}, fmt.Errorf("%s %s: %w", a.Kind, a.Node, err)
			}
			clusterupgrade.NoteRefusedEvictions(&status, a.Node, refused)
			continue
		}
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
		if p.Spec.NodeName != nodeName { // the fake client ignores field selectors
			continue
		}
		if drainMoves(p, r.Self) {
			out = append(out, p)
		}
	}
	return out, nil
}

// drainMoves tells whether the drain moves p off its node: not a finished,
// static (mirror) or DaemonSet pod, nor the operator itself.
func drainMoves(p corev1.Pod, self types.NamespacedName) bool {
	if p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed ||
		(p.Namespace == self.Namespace && p.Name == self.Name) {
		return false
	}
	if _, mirror := p.Annotations[corev1.MirrorPodAnnotationKey]; mirror {
		return false
	}
	return !ownedByDaemonSet(p)
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
			"metadata": map[string]interface{}{
				"annotations": map[string]interface{}{k4allv1alpha1.NodeUpgradeCordonedAnnotation: mark},
				"labels":      map[string]interface{}{clusterupgrade.DrainingLabel: "true"},
			},
			"spec": map[string]interface{}{"unschedulable": true},
		})
	case clusterupgrade.Evict:
		_, err := r.evict(ctx, a.Pods)
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

// evict asks the API server to evict the pods Decide chose (namespace/name).
// It returns why evictions were refused: a PodDisruptionBudget refusing one
// (429) is retried at the next reconcile, until the drain timeout.
func (r *ClusterUpgradeReconciler) evict(ctx context.Context, pods []string) ([]string, error) {
	log := logf.FromContext(ctx)
	var refused []string
	for _, key := range pods {
		namespace, name, _ := strings.Cut(key, "/")
		p := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name}}
		eviction := &policyv1.Eviction{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace}}
		err := r.SubResource("eviction").Create(ctx, p, eviction)
		switch {
		case err == nil, apierrors.IsNotFound(err):
		case apierrors.IsTooManyRequests(err):
			why := refusal(namespace, err)
			log.Info("eviction refused for now", "pod", key, "reason", why)
			if !slices.Contains(refused, why) {
				refused = append(refused, why)
			}
		default:
			return refused, fmt.Errorf("evict %s: %w", key, err)
		}
	}
	return refused, nil
}

// disruptionBudgetCause is how the eviction API names the budget it applies,
// e.g. "The disruption budget calico-apiserver needs 1 healthy pods and has 0 currently".
var disruptionBudgetCause = regexp.MustCompile(`^The disruption budget (\S+) (.+)$`)

// refusal says why an eviction in namespace was refused, naming the
// PodDisruptionBudget when the API server does.
func refusal(namespace string, err error) string {
	var status apierrors.APIStatus
	if errors.As(err, &status) && status.Status().Details != nil {
		for _, c := range status.Status().Details.Causes {
			if m := disruptionBudgetCause.FindStringSubmatch(c.Message); m != nil {
				return fmt.Sprintf("PDB %s/%s %s", namespace, m[1], m[2])
			}
		}
	}
	return err.Error()
}

// unhealthyPDBs lists, sorted, the PodDisruptionBudgets with fewer healthy
// pods than they want, among the ones the disruption controller has seen.
func (r *ClusterUpgradeReconciler) unhealthyPDBs(ctx context.Context) ([]clusterupgrade.PDB, error) {
	list := &policyv1.PodDisruptionBudgetList{}
	if err := r.List(ctx, list); err != nil {
		return nil, fmt.Errorf("list PodDisruptionBudgets: %w", err)
	}
	out := make([]clusterupgrade.PDB, 0, len(list.Items))
	for _, p := range list.Items {
		if p.Status.ObservedGeneration < p.Generation || p.Status.CurrentHealthy >= p.Status.DesiredHealthy {
			continue
		}
		out = append(out, clusterupgrade.PDB{Namespace: p.Namespace, Name: p.Name,
			Healthy: p.Status.CurrentHealthy, Desired: p.Status.DesiredHealthy})
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Namespace+"/"+out[i].Name < out[j].Namespace+"/"+out[j].Name
	})
	return out, nil
}

// The labels of a CloudNativePG instance pod.
const (
	cnpgClusterLabel = "cnpg.io/cluster"
	cnpgRoleLabel    = "cnpg.io/instanceRole"
)

// drainBlockers lists, by node, the pods its drain could never evict
// (findBlockers). It reads the pods of the budgets that never allow a
// disruption, and the CloudNativePG replicas that could take over from a
// primary among them.
func (r *ClusterUpgradeReconciler) drainBlockers(ctx context.Context) (map[string][]clusterupgrade.Blocker, error) {
	list := &policyv1.PodDisruptionBudgetList{}
	if err := r.List(ctx, list); err != nil {
		return nil, fmt.Errorf("list PodDisruptionBudgets: %w", err)
	}
	stuck := make([]policyv1.PodDisruptionBudget, 0, len(list.Items))
	var pods []corev1.Pod
	seen := map[types.NamespacedName]bool{}
	add := func(namespace string, selector labels.Selector) error {
		got := &corev1.PodList{}
		if err := r.APIReader.List(ctx, got, client.InNamespace(namespace), client.MatchingLabelsSelector{Selector: selector}); err != nil {
			return fmt.Errorf("list pods in %s: %w", namespace, err)
		}
		for _, p := range got.Items {
			if key := client.ObjectKeyFromObject(&p); !seen[key] {
				seen[key] = true
				pods = append(pods, p)
			}
		}
		return nil
	}
	for _, p := range list.Items {
		// A null selector selects no pod (and as a list option, every pod).
		if p.Spec.Selector == nil || !neverAllows(p) {
			continue
		}
		selector, err := metav1.LabelSelectorAsSelector(p.Spec.Selector)
		if err != nil {
			continue
		}
		stuck = append(stuck, p)
		if err := add(p.Namespace, selector); err != nil {
			return nil, err
		}
	}
	clusters := map[types.NamespacedName]bool{}
	for _, p := range pods {
		if c := p.Labels[cnpgClusterLabel]; c != "" && p.Labels[cnpgRoleLabel] == "primary" {
			clusters[types.NamespacedName{Namespace: p.Namespace, Name: c}] = true
		}
	}
	for c := range clusters {
		if err := add(c.Namespace, labels.SelectorFromSet(labels.Set{cnpgClusterLabel: c.Name, cnpgRoleLabel: "replica"})); err != nil {
			return nil, err
		}
	}
	return findBlockers(stuck, pods, r.Self), nil
}

// neverAllows tells whether a PodDisruptionBudget allows no disruption
// although every pod it expects is healthy: waiting changes nothing. One
// short of healthy pods is not: its pods are starting (waitForPDBs, and the
// drain retries until its timeout). Nor is one the disruption controller has
// not seen yet.
func neverAllows(p policyv1.PodDisruptionBudget) bool {
	return p.Status.ObservedGeneration >= p.Generation && p.Status.DisruptionsAllowed == 0 &&
		p.Status.CurrentHealthy >= p.Status.ExpectedPods
}

// findBlockers lists, by node and sorted, the pods a drain of their node
// could never evict: a budget in pdbs that never allows a disruption selects
// them, and the eviction API applies it (a running pod, not terminating,
// that the drain moves). A CloudNativePG primary with a ready replica on
// another node is not one: CNPG switches it over when its node is cordoned.
func findBlockers(pdbs []policyv1.PodDisruptionBudget, pods []corev1.Pod, self types.NamespacedName) map[string][]clusterupgrade.Blocker {
	out := map[string][]clusterupgrade.Blocker{}
	for _, b := range pdbs {
		if !neverAllows(b) {
			continue
		}
		selector, err := metav1.LabelSelectorAsSelector(b.Spec.Selector)
		if err != nil {
			continue
		}
		for _, p := range pods {
			if p.Namespace != b.Namespace || !selector.Matches(labels.Set(p.Labels)) ||
				p.Status.Phase != corev1.PodRunning || p.DeletionTimestamp != nil ||
				!drainMoves(p, self) || cnpgSwitchesOver(p, pods) {
				continue
			}
			out[p.Spec.NodeName] = append(out[p.Spec.NodeName], clusterupgrade.Blocker{
				PDB: b.Namespace + "/" + b.Name, Pod: p.Namespace + "/" + p.Name})
		}
	}
	for _, blockers := range out {
		sort.Slice(blockers, func(i, j int) bool {
			return blockers[i].PDB+" "+blockers[i].Pod < blockers[j].PDB+" "+blockers[j].Pod
		})
	}
	return out
}

// cnpgSwitchesOver tells whether p is a CloudNativePG primary that CNPG
// moves off its node by itself: once the node is cordoned it switches over to
// a replica, here one ready on another node (CNPG docs, Kubernetes upgrade).
func cnpgSwitchesOver(p corev1.Pod, pods []corev1.Pod) bool {
	cluster := p.Labels[cnpgClusterLabel]
	if cluster == "" || p.Labels[cnpgRoleLabel] != "primary" {
		return false
	}
	for _, o := range pods {
		if o.Namespace == p.Namespace && o.Labels[cnpgClusterLabel] == cluster && o.Labels[cnpgRoleLabel] == "replica" &&
			o.Spec.NodeName != "" && o.Spec.NodeName != p.Spec.NodeName && o.DeletionTimestamp == nil &&
			o.Status.Phase == corev1.PodRunning && podReady(o) {
			return true
		}
	}
	return false
}

func podReady(p corev1.Pod) bool {
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
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
