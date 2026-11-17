package controller

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	k4allv1alpha1 "github.com/gpillon/k4all-operator/api/v1alpha1"
	"github.com/gpillon/k4all-operator/internal/upgradegraph"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// nextRelease is the release after v2.5.0 in testGraph.
const nextRelease = "v2.6.0"

// staticGraph serves the same upgrade graph for any URL.
type staticGraph struct{ graph *upgradegraph.Graph }

func (s staticGraph) Get(context.Context, string) (*upgradegraph.Graph, error) { return s.graph, nil }

func testGraph(t *testing.T) *upgradegraph.Graph {
	t.Helper()
	doc, _ := json.Marshal(map[string]interface{}{"releases": []map[string]interface{}{
		{"version": "v2.5.0", "kind": "minor", "minFrom": "v2.4.0", "kubernetes": "1.35.10", "crio": "1.35", "fedora": 45},
		{"version": nextRelease, "kind": "minor", "minFrom": "v2.5.0", "kubernetes": "1.35.10", "crio": "1.35", "fedora": 45},
	}})
	g, err := upgradegraph.Parse(doc)
	if err != nil {
		t.Fatal(err)
	}
	return g
}

// upgradeTestbed is an envtest cluster with the given nodes and a
// ClusterUpgradeReconciler on it. Everything is deleted at the end.
type upgradeTestbed struct {
	t *testing.T
	c client.Client
	r *ClusterUpgradeReconciler
}

func newUpgradeTestbed(t *testing.T, nodes ...*corev1.Node) *upgradeTestbed {
	t.Helper()
	c := envtestClient(t)
	ctx := context.Background()
	for _, n := range nodes {
		status := n.Status
		if err := c.Create(ctx, n); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = c.Delete(context.Background(), n) })
		n.Status = status
		if err := c.Status().Update(ctx, n); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		_ = c.Delete(context.Background(), &k4allv1alpha1.ClusterUpgrade{ObjectMeta: metav1.ObjectMeta{Name: k4allv1alpha1.ClusterUpgradeName}})
	})
	return &upgradeTestbed{t: t, c: c, r: &ClusterUpgradeReconciler{
		Client:    c,
		Scheme:    c.Scheme(),
		APIReader: c,
		Graphs:    staticGraph{testGraph(t)},
		Self:      types.NamespacedName{Namespace: "default", Name: "k4all-operator"},
	}}
}

func (b *upgradeTestbed) reconcile() k4allv1alpha1.ClusterUpgrade {
	b.t.Helper()
	ctx := context.Background()
	if _, err := b.r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: k4allv1alpha1.ClusterUpgradeName}}); err != nil {
		b.t.Fatalf("Reconcile: %v", err)
	}
	cu := k4allv1alpha1.ClusterUpgrade{}
	if err := b.c.Get(ctx, types.NamespacedName{Name: k4allv1alpha1.ClusterUpgradeName}, &cu); err != nil {
		b.t.Fatal(err)
	}
	return cu
}

// targetNextRelease sets spec.targetRelease to nextRelease.
func (b *upgradeTestbed) targetNextRelease() {
	b.t.Helper()
	cu := &k4allv1alpha1.ClusterUpgrade{}
	if err := b.c.Get(context.Background(), types.NamespacedName{Name: k4allv1alpha1.ClusterUpgradeName}, cu); err != nil {
		b.t.Fatal(err)
	}
	cu.Spec.TargetRelease = nextRelease
	if err := b.c.Update(context.Background(), cu); err != nil {
		b.t.Fatal(err)
	}
}

func (b *upgradeTestbed) node(name string) *corev1.Node {
	b.t.Helper()
	n := &corev1.Node{}
	if err := b.c.Get(context.Background(), types.NamespacedName{Name: name}, n); err != nil {
		b.t.Fatal(err)
	}
	return n
}

// pod creates a running pod bound to the node cu-cp, the one drained first.
func (b *upgradeTestbed) pod(name string, owners ...metav1.OwnerReference) *corev1.Pod {
	b.t.Helper()
	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: name, OwnerReferences: owners},
		Spec:       corev1.PodSpec{NodeName: "cu-cp", Containers: []corev1.Container{{Name: "c", Image: "busybox"}}},
	}
	if err := b.c.Create(context.Background(), p); err != nil {
		b.t.Fatal(err)
	}
	b.t.Cleanup(func() { _ = b.c.Delete(context.Background(), p, client.GracePeriodSeconds(0)) })
	return p
}

// evicted tells whether the pod was evicted (deleted, or terminating: envtest
// has no kubelet to finish the deletion).
func (b *upgradeTestbed) evicted(p *corev1.Pod) bool {
	b.t.Helper()
	got := &corev1.Pod{}
	err := b.c.Get(context.Background(), client.ObjectKeyFromObject(p), got)
	if apierrors.IsNotFound(err) {
		return true
	}
	if err != nil {
		b.t.Fatal(err)
	}
	return got.DeletionTimestamp != nil
}

// upgradeNode is a Ready node whose k4all-upgrade reported role and release.
func upgradeNode(name, role, release string, annotations ...string) *corev1.Node {
	n := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name, Annotations: map[string]string{
			k4allv1alpha1.NodeTypeAnnotation:    role,
			k4allv1alpha1.NodeReleaseAnnotation: release,
		}},
		Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{
			{Type: corev1.NodeReady, Status: corev1.ConditionTrue, LastHeartbeatTime: metav1.Now(), LastTransitionTime: metav1.Now()},
		}},
	}
	for i := 0; i+1 < len(annotations); i += 2 {
		n.Annotations[annotations[i]] = annotations[i+1]
	}
	return n
}

// The operator creates the only ClusterUpgrade, so the UI always finds the
// cluster's release and the releases it can move to.
func TestClusterUpgrade_CreatedWithTheClusterRelease(t *testing.T) {
	b := newUpgradeTestbed(t, upgradeNode("cu-created", "bootstrap", "v2.5.0"))

	cu := b.reconcile()

	if cu.Status.Phase != k4allv1alpha1.ClusterUpgradeIdle || cu.Status.CurrentRelease != "v2.5.0" {
		t.Errorf("status = %s at %q, want Idle at v2.5.0", cu.Status.Phase, cu.Status.CurrentRelease)
	}
	if len(cu.Status.AvailableReleases) != 1 || cu.Status.AvailableReleases[0].Version != nextRelease {
		t.Errorf("availableReleases = %+v, want v2.6.0", cu.Status.AvailableReleases)
	}
	if cu.Status.GraphURL != k4allv1alpha1.DefaultUpgradeGraphURL {
		t.Errorf("graphUrl = %q, want the default", cu.Status.GraphURL)
	}
}

func TestClusterUpgrade_DrainsTheNodeThenAsksForTheHop(t *testing.T) {
	b := newUpgradeTestbed(t,
		upgradeNode("cu-cp", "bootstrap", "v2.5.0"),
		upgradeNode("cu-w", "worker", "v2.5.0"))
	b.reconcile()
	app := b.pod("app")
	ds := b.pod("ds", metav1.OwnerReference{APIVersion: "apps/v1", Kind: "DaemonSet", Name: "ds", UID: "ds-uid"})
	self := b.pod("k4all-operator")
	b.targetNextRelease()

	b.reconcile()
	if n := b.node("cu-cp"); !n.Spec.Unschedulable || n.Annotations[k4allv1alpha1.NodeUpgradeCordonedAnnotation] != "true" {
		t.Fatalf("cu-cp unschedulable=%v cordoned=%q, want cordoned by the operator",
			n.Spec.Unschedulable, n.Annotations[k4allv1alpha1.NodeUpgradeCordonedAnnotation])
	}

	b.reconcile()
	if !b.evicted(app) {
		t.Errorf("pod app was not evicted")
	}
	if b.evicted(ds) || b.evicted(self) {
		t.Errorf("evicted a DaemonSet pod (%v) or the operator's own pod (%v)", b.evicted(ds), b.evicted(self))
	}
	if _, asked := b.node("cu-cp").Annotations[k4allv1alpha1.NodeUpgradeTargetAnnotation]; asked {
		t.Errorf("upgrade-target set while app is still terminating")
	}

	// The kubelet finishes the eviction.
	if err := b.c.Delete(context.Background(), app, client.GracePeriodSeconds(0)); client.IgnoreNotFound(err) != nil {
		t.Fatal(err)
	}
	cu := b.reconcile()
	if got := b.node("cu-cp").Annotations[k4allv1alpha1.NodeUpgradeTargetAnnotation]; got != nextRelease {
		t.Errorf("cu-cp upgrade-target = %q, want v2.6.0", got)
	}
	if cu.Status.Nodes[0].Phase != k4allv1alpha1.NodeUpgradeUpgrading {
		t.Errorf("cu-cp phase = %s, want Upgrading", cu.Status.Nodes[0].Phase)
	}
}

// A pod that tolerates the cordon is put back on the node by its controller
// (tigera-operator in the lab): the drain neither evicts it again nor waits
// for it, and names it.
func TestClusterUpgrade_DrainLeavesThePodsPutBackOnTheNode(t *testing.T) {
	b := newUpgradeTestbed(t,
		upgradeNode("cu-cp", "bootstrap", "v2.5.0"),
		upgradeNode("cu-w", "worker", "v2.5.0"))
	b.reconcile()
	app := b.pod("app")
	b.targetNextRelease()
	b.reconcile() // cordon

	time.Sleep(1100 * time.Millisecond) // creation times have a one-second resolution
	back := b.pod("app-back")
	b.reconcile()
	if !b.evicted(app) {
		t.Fatalf("pod app was not evicted")
	}
	if b.evicted(back) {
		t.Errorf("evicted app-back, put on the node after the drain started")
	}

	// The kubelet finishes the eviction of app.
	if err := b.c.Delete(context.Background(), app, client.GracePeriodSeconds(0)); client.IgnoreNotFound(err) != nil {
		t.Fatal(err)
	}
	cu := b.reconcile()
	if got := b.node("cu-cp").Annotations[k4allv1alpha1.NodeUpgradeTargetAnnotation]; got != nextRelease {
		t.Errorf("cu-cp upgrade-target = %q with only app-back left, want %s", got, nextRelease)
	}
	if msg := cu.Status.Nodes[0].Message; !strings.Contains(msg, "default/app-back") {
		t.Errorf("cu-cp message = %q, want app-back named", msg)
	}
}

// The drain names the PodDisruptionBudget that refuses an eviction.
func TestClusterUpgrade_DrainNamesThePDBThatRefuses(t *testing.T) {
	b := newUpgradeTestbed(t,
		upgradeNode("cu-cp", "bootstrap", "v2.5.0"),
		upgradeNode("cu-w", "worker", "v2.5.0"))
	b.reconcile()
	app := b.pod("app")
	app.Labels = map[string]string{"app": "pdb-guarded"}
	if err := b.c.Update(context.Background(), app); err != nil {
		t.Fatal(err)
	}
	// The eviction API ignores budgets for Pending pods: make it Running.
	app.Status = corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{
		{Type: corev1.PodReady, Status: corev1.ConditionTrue},
	}}
	if err := b.c.Status().Update(context.Background(), app); err != nil {
		t.Fatal(err)
	}
	minAvailable := intstr.FromInt32(1)
	pdb := &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "app-pdb"},
		Spec: policyv1.PodDisruptionBudgetSpec{
			MinAvailable: &minAvailable,
			Selector:     &metav1.LabelSelector{MatchLabels: map[string]string{"app": "pdb-guarded"}},
		},
	}
	if err := b.c.Create(context.Background(), pdb); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = b.c.Delete(context.Background(), pdb) })
	// What the disruption controller, absent from envtest, would report.
	pdb.Status = policyv1.PodDisruptionBudgetStatus{
		ObservedGeneration: pdb.Generation, DisruptionsAllowed: 0, CurrentHealthy: 0, DesiredHealthy: 1, ExpectedPods: 1,
	}
	if err := b.c.Status().Update(context.Background(), pdb); err != nil {
		t.Fatal(err)
	}
	b.targetNextRelease()
	b.reconcile() // cordon

	cu := b.reconcile()
	if b.evicted(app) {
		t.Fatalf("app was evicted despite its PodDisruptionBudget")
	}
	if msg := cu.Status.Nodes[0].Message; !strings.Contains(msg, "PDB default/app-pdb") {
		t.Errorf("cu-cp message = %q, want the PodDisruptionBudget named", msg)
	}
	if !strings.Contains(cu.Status.Message, "PDB default/app-pdb") {
		t.Errorf("message = %q, want the PodDisruptionBudget named", cu.Status.Message)
	}
}

func TestClusterUpgrade_FinishesTheNodeWhenTheHostIsDone(t *testing.T) {
	n := upgradeNode("cu-done", "bootstrap", nextRelease,
		k4allv1alpha1.NodeUpgradeCordonedAnnotation, "true",
		k4allv1alpha1.NodeUpgradeTargetAnnotation, nextRelease,
		k4allv1alpha1.NodeUpgradeDoneAnnotation, nextRelease)
	n.Spec.Unschedulable = true
	b := newUpgradeTestbed(t, n)
	b.reconcile()
	b.targetNextRelease()

	cu := b.reconcile()

	got := b.node("cu-done")
	if got.Spec.Unschedulable {
		t.Errorf("cu-done is still cordoned")
	}
	for _, key := range []string{k4allv1alpha1.NodeUpgradeCordonedAnnotation, k4allv1alpha1.NodeUpgradeTargetAnnotation, k4allv1alpha1.NodeUpgradeDoneAnnotation} {
		if v, ok := got.Annotations[key]; ok {
			t.Errorf("%s = %q left on the node", key, v)
		}
	}
	if cu.Status.Phase != k4allv1alpha1.ClusterUpgradeSucceeded {
		t.Errorf("phase = %s, want Succeeded", cu.Status.Phase)
	}
}

// A node the admin had cordoned stays cordoned after its hop.
func TestClusterUpgrade_UncordonsTheNodeAfterItsReboot(t *testing.T) {
	n := upgradeNode("cu-sync", "bootstrap", "v2.5.0",
		k4allv1alpha1.NodeUpgradeCordonedAnnotation, "true",
		k4allv1alpha1.NodeUpgradeTargetAnnotation, nextRelease,
		k4allv1alpha1.NodeUpgradePhaseAnnotation, "SYNC")
	n.Spec.Unschedulable = true
	b := newUpgradeTestbed(t, n)
	b.reconcile()
	b.targetNextRelease()

	b.reconcile()

	got := b.node("cu-sync")
	if got.Spec.Unschedulable {
		t.Fatalf("cu-sync is still cordoned during the SYNC")
	}
	if v := got.Annotations[k4allv1alpha1.NodeUpgradeCordonedAnnotation]; v != k4allv1alpha1.NodeCordonReleased {
		t.Errorf("upgrade-cordoned = %q, want %q", v, k4allv1alpha1.NodeCordonReleased)
	}
	if v := got.Annotations[k4allv1alpha1.NodeUpgradeTargetAnnotation]; v != nextRelease {
		t.Errorf("upgrade-target = %q, want %q until upgrade-done", v, nextRelease)
	}

	// A cordon set after the release is the admin's: the end of the hop keeps it.
	got.Spec.Unschedulable = true
	delete(got.Annotations, k4allv1alpha1.NodeUpgradePhaseAnnotation)
	got.Annotations[k4allv1alpha1.NodeReleaseAnnotation] = nextRelease
	got.Annotations[k4allv1alpha1.NodeUpgradeDoneAnnotation] = nextRelease
	if err := b.c.Update(context.Background(), got); err != nil {
		t.Fatal(err)
	}
	b.reconcile()

	got = b.node("cu-sync")
	if !got.Spec.Unschedulable {
		t.Errorf("the operator undid a cordon set after it released cu-sync")
	}
	if v, ok := got.Annotations[k4allv1alpha1.NodeUpgradeCordonedAnnotation]; ok {
		t.Errorf("upgrade-cordoned = %q left on the node", v)
	}
}

func TestClusterUpgrade_LeavesAnAdminCordonInPlace(t *testing.T) {
	n := upgradeNode("cu-admin", "bootstrap", "v2.5.0")
	n.Spec.Unschedulable = true
	b := newUpgradeTestbed(t, n)
	b.reconcile()
	b.targetNextRelease()
	b.reconcile() // cordon (already) + target: a single node is not drained

	got := b.node("cu-admin")
	got.Annotations[k4allv1alpha1.NodeReleaseAnnotation] = nextRelease
	got.Annotations[k4allv1alpha1.NodeUpgradeDoneAnnotation] = nextRelease
	if err := b.c.Update(context.Background(), got); err != nil {
		t.Fatal(err)
	}
	b.reconcile()

	if !b.node("cu-admin").Spec.Unschedulable {
		t.Errorf("the operator uncordoned a node the admin had cordoned")
	}
}

func TestClusterUpgrade_ConsumesTheRetryAnnotation(t *testing.T) {
	b := newUpgradeTestbed(t, upgradeNode("cu-retry", "bootstrap", "v2.5.0"))
	cu := b.reconcile()
	cu.Annotations = map[string]string{k4allv1alpha1.ClusterUpgradeRetryAnnotation: "1"}
	if err := b.c.Update(context.Background(), &cu); err != nil {
		t.Fatal(err)
	}

	cu = b.reconcile()
	if v, ok := cu.Annotations[k4allv1alpha1.ClusterUpgradeRetryAnnotation]; ok {
		t.Errorf("retry annotation %q left after the reconcile", v)
	}
}
