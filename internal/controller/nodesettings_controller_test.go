package controller

import (
	"context"
	"strings"
	"testing"

	k4allv1alpha1 "github.com/gpillon/k4all-operator/api/v1alpha1"
	"github.com/gpillon/k4all-operator/internal/nodesettings"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// kubeadmKubeletConfig is data.kubelet of a k4all cluster's kubelet-config.
const kubeadmKubeletConfig = `apiVersion: kubelet.config.k8s.io/v1beta1
cgroupDriver: systemd
clusterDNS:
- 10.96.0.10
clusterDomain: cluster.local
kind: KubeletConfiguration
maxPods: 250
`

type settingsTestbed struct {
	t      *testing.T
	c      client.Client
	r      *NodeSettingsReconciler
	config string
}

// settingsNode is a Ready node on k4all v2.7.0 with 8 CPUs and 32 GiB.
func settingsNode(name, role string) *corev1.Node {
	n := upgradeNode(name, role, "v2.7.0")
	n.Status.Capacity = corev1.ResourceList{
		corev1.ResourceCPU:    resource.MustParse("8"),
		corev1.ResourceMemory: resource.MustParse("32Gi"),
	}
	return n
}

func newSettingsTestbed(t *testing.T, nodes ...*corev1.Node) *settingsTestbed {
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
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: "kube-system", Name: "kubelet-config"},
		Data:       map[string]string{"kubelet": kubeadmKubeletConfig},
	}
	if err := c.Create(ctx, cm); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Delete(context.Background(), cm) })
	config := &k4allv1alpha1.ClusterConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "k4all-cluster-config"},
		Spec: k4allv1alpha1.ClusterConfigSpec{
			Networking: k4allv1alpha1.NetworkingConfig{CNI: k4allv1alpha1.CNIConfig{Type: calicoCNI}},
		},
	}
	createClusterConfig(t, c, config)
	return &settingsTestbed{t: t, c: c, config: config.Name, r: &NodeSettingsReconciler{
		Client:    c,
		Scheme:    c.Scheme(),
		APIReader: c,
		Self:      types.NamespacedName{Namespace: "default", Name: "k4all-operator"},
		Lock:      &NodeLock{},
	}}
}

func (b *settingsTestbed) setNodes(nodes *k4allv1alpha1.NodesConfig) {
	b.t.Helper()
	cc := b.clusterConfig()
	cc.Spec.Nodes = nodes
	if err := b.c.Update(context.Background(), cc); err != nil {
		b.t.Fatal(err)
	}
}

func (b *settingsTestbed) clusterConfig() *k4allv1alpha1.ClusterConfig {
	b.t.Helper()
	cc := &k4allv1alpha1.ClusterConfig{}
	if err := b.c.Get(context.Background(), types.NamespacedName{Name: b.config}, cc); err != nil {
		b.t.Fatal(err)
	}
	return cc
}

func (b *settingsTestbed) reconcile() *k4allv1alpha1.ClusterConfig {
	b.t.Helper()
	if _, err := b.r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: b.config}}); err != nil {
		b.t.Fatalf("Reconcile: %v", err)
	}
	return b.clusterConfig()
}

func (b *settingsTestbed) node(name string) *corev1.Node {
	b.t.Helper()
	n := &corev1.Node{}
	if err := b.c.Get(context.Background(), types.NamespacedName{Name: name}, n); err != nil {
		b.t.Fatal(err)
	}
	return n
}

func (b *settingsTestbed) kubeletConfig() *corev1.ConfigMap {
	b.t.Helper()
	cm := &corev1.ConfigMap{}
	if err := b.c.Get(context.Background(), types.NamespacedName{Namespace: "kube-system", Name: "kubelet-config"}, cm); err != nil {
		b.t.Fatal(err)
	}
	return cm
}

// agentApplies is the node's k4all-upgrade agent reporting hash applied.
func (b *settingsTestbed) agentApplies(name, hash string) {
	b.t.Helper()
	n := b.node(name)
	n.Annotations[nodesettings.AppliedAnnotation] = hash
	if err := b.c.Update(context.Background(), n); err != nil {
		b.t.Fatal(err)
	}
}

func perNodePhase(cc *k4allv1alpha1.ClusterConfig, name string) string {
	if cc.Status.Nodes == nil {
		return ""
	}
	for _, n := range cc.Status.Nodes.PerNode {
		if n.Name == name {
			return n.Phase
		}
	}
	return ""
}

func TestNodeSettings_RollsHugepagesOutOneNodeAtATime(t *testing.T) {
	b := newSettingsTestbed(t, settingsNode("ns-cp", "bootstrap"), settingsNode("ns-w", "worker"))
	b.setNodes(&k4allv1alpha1.NodesConfig{Hugepages: map[string]int32{"1Gi": 2}})
	hash := nodesettings.FromSpec(&k4allv1alpha1.NodesConfig{Hugepages: map[string]int32{"1Gi": 2}}).Hash()

	cc := b.reconcile()
	cm := b.kubeletConfig()
	rec, err := nodesettings.ParseRecord(cm.Annotations[nodesettings.ConfigMapAnnotation])
	if err != nil || rec.Hash != hash || rec.Hugepages["1Gi"] != 2 {
		t.Fatalf("kubelet-config record = %+v, %v", rec, err)
	}
	if cm.Data["kubelet"] != kubeadmKubeletConfig {
		t.Errorf("data.kubelet rewritten for hugepages:\n%s", cm.Data["kubelet"])
	}
	w := b.node("ns-w")
	if !w.Spec.Unschedulable || w.Labels[drainingLabel] != drainingOn || w.Annotations[nodesettings.StepAnnotation] == "" {
		t.Fatalf("ns-w unschedulable=%v labels=%v annotations=%v, want cordoned for its step", w.Spec.Unschedulable, w.Labels, w.Annotations)
	}
	if b.node("ns-cp").Spec.Unschedulable {
		t.Fatalf("two nodes cordoned at once")
	}
	if cc.Status.Nodes == nil || cc.Status.Nodes.SettingsHash != hash || cc.Status.Nodes.Rollout.Phase != k4allv1alpha1.NodeRolloutRolling ||
		cc.Status.Nodes.Rollout.CurrentNode != "ns-w" || perNodePhase(cc, "ns-w") != nodesettings.PhaseDraining {
		t.Errorf("status.nodes = %+v", cc.Status.Nodes)
	}

	b.reconcile() // nothing to evict: the target
	if got := b.node("ns-w").Annotations[nodesettings.TargetAnnotation]; got != hash {
		t.Fatalf("node-settings-target = %q, want %s", got, hash)
	}
	b.agentApplies("ns-w", hash)
	cc = b.reconcile() // finish
	w = b.node("ns-w")
	if w.Spec.Unschedulable || w.Labels[drainingLabel] != "" || w.Annotations[nodesettings.StepAnnotation] != "" {
		t.Fatalf("ns-w not finished: unschedulable=%v labels=%v annotations=%v", w.Spec.Unschedulable, w.Labels, w.Annotations)
	}
	if perNodePhase(cc, "ns-w") != nodesettings.PhaseDone {
		t.Errorf("ns-w phase = %s, want Done", perNodePhase(cc, "ns-w"))
	}

	b.reconcile() // the bootstrap node
	if !b.node("ns-cp").Spec.Unschedulable {
		t.Fatalf("ns-cp not started after ns-w")
	}
	b.reconcile()
	b.agentApplies("ns-cp", hash)
	b.reconcile()
	cc = b.reconcile()
	cond := meta.FindStatusCondition(cc.Status.Conditions, k4allv1alpha1.ConditionNodeSettingsRollout)
	if cc.Status.Nodes.Rollout.Phase != k4allv1alpha1.NodeRolloutIdle || cond == nil || cond.Status != metav1.ConditionTrue {
		t.Errorf("rollout = %+v, condition %+v, want Idle and True", cc.Status.Nodes.Rollout, cond)
	}
	if b.node("ns-cp").Spec.Unschedulable {
		t.Errorf("ns-cp still cordoned")
	}
}

// Only the kubelet fields the ClusterConfig sets are k4all's; clearing one
// removes what k4all wrote, and only that.
func TestNodeSettings_OwnsOnlyTheKubeletFieldsItSets(t *testing.T) {
	b := newSettingsTestbed(t, settingsNode("ns-only", "bootstrap"))
	spec := &k4allv1alpha1.NodesConfig{Kubelet: &k4allv1alpha1.KubeletSettings{TopologyManagerScope: "pod"}}
	b.setNodes(spec)

	b.reconcile()
	data := b.kubeletConfig().Data["kubelet"]
	for _, want := range []string{`topologyManagerScope: "pod"`, "maxPods: 250", "cgroupDriver: systemd", "- 10.96.0.10"} {
		if !strings.Contains(data, want) {
			t.Errorf("kubelet-config lacks %q:\n%s", want, data)
		}
	}
	hash := nodesettings.FromSpec(spec).Hash()
	if n := b.node("ns-only"); !n.Spec.Unschedulable || n.Annotations[nodesettings.TargetAnnotation] != hash {
		t.Fatalf("single node: unschedulable=%v target=%q, want cordoned with the target at once",
			n.Spec.Unschedulable, n.Annotations[nodesettings.TargetAnnotation])
	}
	b.agentApplies("ns-only", hash)
	b.reconcile()

	b.setNodes(nil)
	b.reconcile()
	data = b.kubeletConfig().Data["kubelet"]
	if strings.Contains(data, "topologyManagerScope") || !strings.Contains(data, "maxPods: 250") {
		t.Errorf("after clearing, kubelet-config:\n%s", data)
	}
}

// D12: no step starts, and kubelet-config is not written, while a
// ClusterUpgrade runs.
func TestNodeSettings_WaitsForTheClusterUpgrade(t *testing.T) {
	b := newSettingsTestbed(t, settingsNode("ns-cp", "bootstrap"), settingsNode("ns-w", "worker"))
	cu := &k4allv1alpha1.ClusterUpgrade{ObjectMeta: metav1.ObjectMeta{Name: k4allv1alpha1.ClusterUpgradeName},
		Spec: k4allv1alpha1.ClusterUpgradeSpec{TargetRelease: "v2.7.1"}}
	if err := b.c.Create(context.Background(), cu); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = b.c.Delete(context.Background(), cu) })
	cu.Status.Phase = k4allv1alpha1.ClusterUpgradeProgressing
	if err := b.c.Status().Update(context.Background(), cu); err != nil {
		t.Fatal(err)
	}
	b.setNodes(&k4allv1alpha1.NodesConfig{IOMMU: ptrTo(true)})

	cc := b.reconcile()
	if _, ok := b.kubeletConfig().Annotations[nodesettings.ConfigMapAnnotation]; ok {
		t.Errorf("kubelet-config written during a ClusterUpgrade")
	}
	if b.node("ns-w").Spec.Unschedulable || b.node("ns-cp").Spec.Unschedulable {
		t.Errorf("a node cordoned during a ClusterUpgrade")
	}
	cond := meta.FindStatusCondition(cc.Status.Conditions, k4allv1alpha1.ConditionNodeSettingsRollout)
	if cond == nil || cond.Reason != "UpgradeInProgress" || !strings.Contains(cond.Message, "v2.7.1") {
		t.Errorf("condition = %+v", cond)
	}
}

// The rollout writes its condition and status.nodes, and leaves the rest of
// the ClusterConfig status alone.
func TestNodeSettings_KeepsTheOtherConditions(t *testing.T) {
	b := newSettingsTestbed(t, settingsNode("ns-only", "bootstrap"))
	cc := b.clusterConfig()
	meta.SetStatusCondition(&cc.Status.Conditions, metav1.Condition{Type: "Reconciled", Status: metav1.ConditionTrue, Reason: "Synced"})
	cc.Status.ActiveCNI = calicoCNI
	if err := b.c.Status().Update(context.Background(), cc); err != nil {
		t.Fatal(err)
	}
	b.setNodes(&k4allv1alpha1.NodesConfig{IOMMU: ptrTo(true)})

	cc = b.reconcile()
	if meta.FindStatusCondition(cc.Status.Conditions, "Reconciled") == nil || cc.Status.ActiveCNI != calicoCNI {
		t.Errorf("status = %+v, want Reconciled and activeCNI kept", cc.Status)
	}
	if meta.FindStatusCondition(cc.Status.Conditions, k4allv1alpha1.ConditionNodeSettingsRollout) == nil {
		t.Errorf("no %s condition", k4allv1alpha1.ConditionNodeSettingsRollout)
	}
}

// k4all#97 in the rollout: the operator's node goes last, and the operator
// moves off it before its settings.
func TestNodeSettings_OperatorsNodeGoesLastAndTheOperatorMoves(t *testing.T) {
	b := newSettingsTestbed(t, settingsNode("ns-cp", "bootstrap"), settingsNode("ns-w", "worker"))
	ub := &upgradeTestbed{t: t, c: b.c}
	self := ub.operatorPod("k4all-operator", "ns-w")
	spec := &k4allv1alpha1.NodesConfig{Hugepages: map[string]int32{"2Mi": 512}}
	hash := nodesettings.FromSpec(spec).Hash()
	b.setNodes(spec)

	b.reconcile()
	if !b.node("ns-cp").Spec.Unschedulable || b.node("ns-w").Spec.Unschedulable {
		t.Fatalf("the bootstrap node goes first when the operator runs on the worker")
	}
	b.reconcile()
	b.agentApplies("ns-cp", hash)
	b.reconcile() // finish ns-cp
	b.reconcile() // start ns-w
	b.reconcile() // drained but for the operator: it leaves
	if !ub.evicted(self) {
		t.Fatalf("the operator's pod was not evicted from its node")
	}
	if got := b.node("ns-w").Annotations[nodesettings.TargetAnnotation]; got != "" {
		t.Errorf("node-settings-target = %q before the operator left", got)
	}
}

func ptrTo[T any](v T) *T { return &v }

// k4all#99 in the rollout: a step that drains does not cordon a node that
// runs a pod that declares no controller, and names it; once the admin
// deletes the pod, the step starts.
func TestNodeSettings_DoesNotCordonANodeThatRunsABarePod(t *testing.T) {
	b := newSettingsTestbed(t, settingsNode("ns-cp", "bootstrap"), settingsNode("ns-w", "worker"))
	ctx := context.Background()
	debug := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "debug-x"},
		Spec:       corev1.PodSpec{NodeName: "ns-w", Containers: []corev1.Container{{Name: "c", Image: "busybox"}}},
	}
	if err := b.c.Create(ctx, debug); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = b.c.Delete(context.Background(), debug, client.GracePeriodSeconds(0)) })
	b.setNodes(&k4allv1alpha1.NodesConfig{Hugepages: map[string]int32{"1Gi": 2}})

	cc := b.reconcile()
	if w := b.node("ns-w"); w.Spec.Unschedulable || w.Annotations[nodesettings.StepAnnotation] != "" {
		t.Fatalf("ns-w unschedulable=%v annotations=%v, want it left alone", w.Spec.Unschedulable, w.Annotations)
	}
	if b.node("ns-cp").Spec.Unschedulable {
		t.Fatalf("ns-cp cordoned while the rollout is blocked")
	}
	cond := meta.FindStatusCondition(cc.Status.Conditions, k4allv1alpha1.ConditionNodeSettingsRollout)
	want := "ns-w runs pods that declare no controller: default/debug-x (a drain would delete them"
	if cond == nil || cond.Reason != "DrainBlocked" || !strings.Contains(cc.Status.Nodes.Rollout.Message, want) {
		t.Errorf("condition %+v, rollout %+v, want DrainBlocked naming debug-x", cond, cc.Status.Nodes.Rollout)
	}

	if err := b.c.Delete(ctx, debug, client.GracePeriodSeconds(0)); err != nil {
		t.Fatal(err)
	}
	b.reconcile()
	if !b.node("ns-w").Spec.Unschedulable {
		t.Errorf("ns-w not cordoned once the bare pod is gone")
	}
}

func TestNodePatch_UncordonsOnlyWhatK4allCordoned(t *testing.T) {
	step := &nodesettings.Step{Hash: "h", Class: nodesettings.DrainReboot, Cordon: nodesettings.CordonAlready}
	patch := nodePatch(nodesettings.Action{Kind: nodesettings.Finish, Node: "n", Step: step})
	if _, ok := patch["spec"]; ok {
		t.Errorf("finish of an admin-cordoned node patches spec: %v", patch)
	}
	start := nodePatch(nodesettings.Action{Kind: nodesettings.Start, Node: "n", Step: step})
	if _, ok := start["spec"]; ok {
		t.Errorf("start on an admin-cordoned node patches spec: %v", start)
	}
	if labels := start["metadata"].(map[string]interface{})["labels"]; labels.(map[string]interface{})[drainingLabel] != drainingOn {
		t.Errorf("start labels = %v, want draining=true", labels)
	}
	restart := &nodesettings.Step{Hash: "h", Class: nodesettings.Restart, Cordon: nodesettings.CordonNone}
	if p := nodePatch(nodesettings.Action{Kind: nodesettings.Start, Node: "n", Step: restart}); p["spec"] != nil || p["metadata"].(map[string]interface{})["labels"] != nil {
		t.Errorf("a restart step cordons or labels: %v", p)
	}
	letGo := nodePatch(nodesettings.Action{Kind: nodesettings.LetGo, Node: "n", Step: &nodesettings.Step{Class: nodesettings.DrainRestart, Cordon: nodesettings.CordonByK4all}})
	ann := letGo["metadata"].(map[string]interface{})["annotations"].(map[string]interface{})
	if v, ok := ann[nodesettings.TargetAnnotation]; !ok || v != nil || letGo["spec"].(map[string]interface{})["unschedulable"] != false {
		t.Errorf("let go = %v, want the target removed and the node uncordoned", letGo)
	}
}

// Clearing a kubelet field puts back what kubelet-config had before k4all:
// kubeadm's maxPods 250, not the kubelet's 110. The nodes take it with a
// restart.
func TestNodeSettings_ClearingPutsBackWhatKubeadmHad(t *testing.T) {
	b := newSettingsTestbed(t, settingsNode("ns-only", "bootstrap"))
	spec := &k4allv1alpha1.NodesConfig{Kubelet: &k4allv1alpha1.KubeletSettings{MaxPods: ptrTo(int32(300))}}
	b.setNodes(spec)
	b.reconcile()
	if data := b.kubeletConfig().Data["kubelet"]; !strings.Contains(data, "maxPods: 300") {
		t.Fatalf("kubelet-config:\n%s", data)
	}
	b.agentApplies("ns-only", nodesettings.FromSpec(spec).Hash())
	b.reconcile()

	b.setNodes(nil)
	b.reconcile()
	if data := b.kubeletConfig().Data["kubelet"]; !strings.Contains(data, "maxPods: 250") || strings.Contains(data, "300") {
		t.Errorf("after clearing, kubelet-config:\n%s", data)
	}
	n := b.node("ns-only")
	if n.Annotations[nodesettings.TargetAnnotation] != (nodesettings.Settings{}).Hash() || n.Spec.Unschedulable {
		t.Errorf("node: target %q unschedulable %v, want the restart to the cleared settings, no cordon",
			n.Annotations[nodesettings.TargetAnnotation], n.Spec.Unschedulable)
	}
}
