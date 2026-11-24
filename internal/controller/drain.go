package controller

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/gpillon/k4all-operator/internal/drain"
)

// drainer reads and evicts the pods of a node the operator drains: for a
// ClusterUpgrade hop and for a node-settings step, with the same rules.
type drainer struct {
	client.Client
	// APIReader lists the pods of a node without caching every pod.
	APIReader client.Reader
	// Self is the operator's own pod: the drain never counts it.
	Self types.NamespacedName
}

// pods is podsToEvict as the drain package sees them.
func (d drainer) pods(ctx context.Context, nodeName string) ([]drain.Pod, error) {
	pods, err := d.podsToEvict(ctx, nodeName)
	if err != nil {
		return nil, err
	}
	out := make([]drain.Pod, 0, len(pods))
	for _, p := range pods {
		out = append(out, drain.Pod{Namespace: p.Namespace, Name: p.Name, Created: p.CreationTimestamp.Time,
			Terminating: p.DeletionTimestamp != nil})
	}
	return out, nil
}

// self is the operator's own pod and its node; nil when Self is not set or
// the pod cannot be read (it is then never evicted).
func (d drainer) self(ctx context.Context) *drain.Self {
	if d.Self.Name == "" {
		return nil
	}
	p := &corev1.Pod{}
	if err := d.APIReader.Get(ctx, d.Self, p); err != nil || p.Spec.NodeName == "" {
		return nil
	}
	return &drain.Self{Node: p.Spec.NodeName, Pod: drain.Pod{Namespace: p.Namespace, Name: p.Name,
		Created: p.CreationTimestamp.Time, Terminating: p.DeletionTimestamp != nil}}
}

// podsToEvict lists the pods the drain moves off nodeName: not the
// DaemonSet ones (they would come back), not the static ones, not finished
// ones, not the operator itself. Terminating pods count, as they are still
// there.
func (d drainer) podsToEvict(ctx context.Context, nodeName string) ([]corev1.Pod, error) {
	pods := &corev1.PodList{}
	if err := d.APIReader.List(ctx, pods, client.MatchingFields{"spec.nodeName": nodeName}); err != nil {
		return nil, fmt.Errorf("list pods on %s: %w", nodeName, err)
	}
	out := make([]corev1.Pod, 0, len(pods.Items))
	for _, p := range pods.Items {
		if p.Spec.NodeName != nodeName { // the fake client ignores field selectors
			continue
		}
		if drainMoves(p, d.Self) {
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

// evict asks the API server to evict the pods Decide chose (namespace/name).
// It returns why evictions were refused: a PodDisruptionBudget refusing one
// (429) is retried at the next reconcile, until the drain timeout.
func (d drainer) evict(ctx context.Context, pods []string) ([]string, error) {
	log := logf.FromContext(ctx)
	var refused []string
	for _, key := range pods {
		namespace, name, _ := strings.Cut(key, "/")
		p := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name}}
		eviction := &policyv1.Eviction{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace}}
		err := d.SubResource("eviction").Create(ctx, p, eviction)
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
func (d drainer) unhealthyPDBs(ctx context.Context) ([]drain.PDB, error) {
	list := &policyv1.PodDisruptionBudgetList{}
	if err := d.List(ctx, list); err != nil {
		return nil, fmt.Errorf("list PodDisruptionBudgets: %w", err)
	}
	out := make([]drain.PDB, 0, len(list.Items))
	for _, p := range list.Items {
		if p.Status.ObservedGeneration < p.Generation || p.Status.CurrentHealthy >= p.Status.DesiredHealthy {
			continue
		}
		out = append(out, drain.PDB{Namespace: p.Namespace, Name: p.Name,
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
func (d drainer) drainBlockers(ctx context.Context) (map[string][]drain.Blocker, error) {
	list := &policyv1.PodDisruptionBudgetList{}
	if err := d.List(ctx, list); err != nil {
		return nil, fmt.Errorf("list PodDisruptionBudgets: %w", err)
	}
	stuck := make([]policyv1.PodDisruptionBudget, 0, len(list.Items))
	var pods []corev1.Pod
	seen := map[types.NamespacedName]bool{}
	add := func(namespace string, selector labels.Selector) error {
		got := &corev1.PodList{}
		if err := d.APIReader.List(ctx, got, client.InNamespace(namespace), client.MatchingLabelsSelector{Selector: selector}); err != nil {
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
	return findBlockers(stuck, pods, d.Self), nil
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
func findBlockers(pdbs []policyv1.PodDisruptionBudget, pods []corev1.Pod, self types.NamespacedName) map[string][]drain.Blocker {
	out := map[string][]drain.Blocker{}
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
			out[p.Spec.NodeName] = append(out[p.Spec.NodeName], drain.Blocker{
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

// evictSelf evicts the operator's own pod (k4all#97) and tells whether it
// is on its way out. A refusal or an error is only logged: the drain goes
// on without it, and the operator goes down with the node, as before.
func evictSelf(ctx context.Context, d drainer, pods []string) bool {
	log := logf.FromContext(ctx)
	refused, err := d.evict(ctx, pods)
	switch {
	case err != nil:
		log.Error(err, "cannot evict the operator's own pod: it stays on the node")
		return false
	case len(refused) > 0:
		log.Info("the operator's own pod may not be evicted: it stays on the node", "reason", strings.Join(refused, "; "))
		return false
	}
	log.Info("evicted the operator's own pod: the next leader goes on", "pod", strings.Join(pods, ","))
	return true
}
