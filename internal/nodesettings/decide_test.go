package nodesettings

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	k4allv1alpha1 "github.com/gpillon/k4all-operator/api/v1alpha1"
	"github.com/gpillon/k4all-operator/internal/drain"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

var now = time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)

// w0 is the first worker, where most rollouts start.
const w0 = "w-0"

// node is a Ready node on k4all v2.7.0, with 8 CPUs and 32 GiB.
func node(name, role string) Node {
	return Node{Name: name, Ready: true, CPUs: 8, Memory: 32 * gib, Annotations: map[string]string{
		k4allv1alpha1.NodeTypeAnnotation:    role,
		k4allv1alpha1.NodeReleaseAnnotation: MinRelease,
	}}
}

func cluster() []Node {
	return []Node{node("cp-0", "bootstrap"), node("w-1", "worker"), node(w0, "worker")}
}

func hugepagesSpec(pages int32) *k4allv1alpha1.NodesConfig {
	return &k4allv1alpha1.NodesConfig{Hugepages: map[string]int32{"1Gi": pages}}
}

func maxPodsSpec(n int32) *k4allv1alpha1.NodesConfig {
	return &k4allv1alpha1.NodesConfig{Kubelet: &k4allv1alpha1.KubeletSettings{MaxPods: &n}}
}

func input(spec *k4allv1alpha1.NodesConfig, nodes ...Node) Input {
	return Input{Spec: spec, Nodes: nodes, KubeletConfig: kubeadmKubelet, Drain: drain.SettingsFrom(0, nil, false),
		Now: now, Generation: 3}
}

// rolled is the record of a ConfigMap that carries spec, as the operator
// writes it at the first step.
func rolled(spec *k4allv1alpha1.NodesConfig) *Record {
	s := FromSpec(spec)
	_, prior, _, _ := MergeKubelet(kubeadmKubelet, s.Kubelet, nil)
	return &Record{Hash: s.Hash(), Settings: s, Prior: prior}
}

func kinds(actions []Action) []string {
	out := make([]string, 0, len(actions))
	for _, a := range actions {
		out = append(out, string(a.Kind)+" "+a.Node)
	}
	return out
}

func expectKinds(t *testing.T, got []Action, want ...string) {
	t.Helper()
	if g := kinds(got); !reflect.DeepEqual(g, want) && (len(g) > 0 || len(want) > 0) {
		t.Errorf("actions = %v, want %v", g, want)
	}
}

func perNode(r Result) map[string]string {
	out := map[string]string{}
	for _, n := range r.Status.PerNode {
		out[n.Name] = n.Phase
	}
	return out
}

func expectRollout(t *testing.T, r Result, phase, reason string) {
	t.Helper()
	if r.Status == nil {
		t.Fatalf("no status")
	}
	if r.Status.Rollout.Phase != phase {
		t.Errorf("rollout phase = %s (%s), want %s", r.Status.Rollout.Phase, r.Status.Rollout.Message, phase)
	}
	if r.Condition == nil || r.Condition.Type != k4allv1alpha1.ConditionNodeSettingsRollout || r.Condition.Reason != reason {
		t.Errorf("condition = %+v, want reason %s", r.Condition, reason)
	}
	if r.Condition != nil && (r.Condition.Status == metav1.ConditionTrue) != (phase == k4allv1alpha1.NodeRolloutIdle) {
		t.Errorf("condition status = %s in %s", r.Condition.Status, phase)
	}
}

// inStep is n in its drain-reboot step for spec's settings, started at started.
func inStep(n Node, spec *k4allv1alpha1.NodesConfig, cordon string, started time.Time) Node {
	step := Step{Hash: FromSpec(spec).Hash(), Class: DrainReboot, Cordon: cordon, Started: started}
	n.Annotations[StepAnnotation] = step.Encode()
	if cordon != CordonNone {
		n.Unschedulable = true
	}
	return n
}

func TestDecide_NothingOwnedNothingReported(t *testing.T) {
	r := Decide(input(nil, cluster()...))
	if r.Status != nil || r.Condition != nil || len(r.Actions) != 0 {
		t.Errorf("result = %+v, want nothing", r)
	}
}

// The first step writes kubelet-config and starts the first worker.
func TestDecide_StartsWithAWorker(t *testing.T) {
	spec := hugepagesSpec(4)
	r := Decide(input(spec, cluster()...))

	expectKinds(t, r.Actions, "WriteConfig ", "Start w-0")
	expectRollout(t, r, k4allv1alpha1.NodeRolloutRolling, "Rolling")
	if r.Status.SettingsHash != FromSpec(spec).Hash() || r.Status.Rollout.CurrentNode != w0 {
		t.Errorf("status = %+v", r.Status)
	}
	if want := map[string]string{w0: PhaseDraining, "w-1": PhasePending, "cp-0": PhasePending}; !reflect.DeepEqual(perNode(r), want) {
		t.Errorf("per node = %v, want %v", perNode(r), want)
	}
	write := r.Actions[0]
	if write.Record == nil || write.Record.Hash != FromSpec(spec).Hash() || write.Record.Hugepages["1Gi"] != 4 {
		t.Errorf("record = %+v", write.Record)
	}
	if write.KubeletConfig != "" {
		t.Errorf("kubelet-config data rewritten for hugepages only:\n%s", write.KubeletConfig)
	}
	start := r.Actions[1].Step
	if start == nil || start.Class != DrainReboot || start.Cordon != CordonByK4all || !start.Started.Equal(now) {
		t.Errorf("step = %+v, want drain-reboot cordoned by k4all", start)
	}
}

// Workers, then control planes, then the bootstrap node; the node running
// the operator last (k4all#97).
func TestDecide_OrderPutsTheOperatorsNodeLast(t *testing.T) {
	in := input(hugepagesSpec(4), append(cluster(), node("cp-1", "control"))...)
	in.Self = &drain.Self{Node: w0, Pod: drain.Pod{Namespace: "k4all-operator-system", Name: "op"}}
	r := Decide(in)

	order := make([]string, 0, 4)
	for _, n := range r.Status.PerNode {
		order = append(order, n.Name)
	}
	if want := []string{"w-1", "cp-1", "cp-0", w0}; !reflect.DeepEqual(order, want) {
		t.Errorf("order = %v, want %v", order, want)
	}
	expectKinds(t, r.Actions, "WriteConfig ", "Start w-1")
}

// maxPods only: kubelet restarts, no drain, the target at once.
func TestDecide_RestartNeedsNoDrain(t *testing.T) {
	spec := maxPodsSpec(200)
	r := Decide(input(spec, cluster()...))

	expectKinds(t, r.Actions, "WriteConfig ", "Start w-0", "SetTarget w-0")
	if s := r.Actions[1].Step; s.Class != Restart || s.Cordon != CordonNone {
		t.Errorf("step = %+v, want restart without a cordon", s)
	}
	if !strings.Contains(r.Actions[0].KubeletConfig, "maxPods: 200") {
		t.Errorf("kubelet-config = %q, want maxPods 200", r.Actions[0].KubeletConfig)
	}
	if r.Actions[2].Hash != FromSpec(spec).Hash() || perNode(r)[w0] != PhaseApplying {
		t.Errorf("target %q, phase %s", r.Actions[2].Hash, perNode(r)[w0])
	}
}

func TestDecide_DrainsThenAsksForTheSettings(t *testing.T) {
	spec := hugepagesSpec(4)
	nodes := cluster()
	nodes[2] = inStep(nodes[2], spec, CordonByK4all, now.Add(-time.Minute))
	in := input(spec, nodes...)
	in.Record = rolled(spec)
	in.Pods = map[string][]drain.Pod{w0: {{Namespace: "app", Name: "web", Created: now.Add(-time.Hour)}}}

	r := Decide(in)
	expectKinds(t, r.Actions, "Evict w-0")
	if r.Actions[0].Pods[0] != "app/web" || perNode(r)[w0] != PhaseDraining {
		t.Errorf("evict %v, phase %s", r.Actions[0].Pods, perNode(r)[w0])
	}
	if !strings.Contains(r.Status.Rollout.Message, "w-0: draining, 1 pods left: app/web") {
		t.Errorf("message = %q", r.Status.Rollout.Message)
	}

	in.Pods = nil
	r = Decide(in)
	expectKinds(t, r.Actions, "SetTarget w-0")
	if perNode(r)[w0] != PhaseApplying {
		t.Errorf("phase = %s, want Applying", perNode(r)[w0])
	}
}

func applying(spec *k4allv1alpha1.NodesConfig, applied, message string) Input {
	nodes := cluster()
	nodes[2] = inStep(nodes[2], spec, CordonByK4all, now.Add(-10*time.Minute))
	nodes[2].Annotations[TargetAnnotation] = FromSpec(spec).Hash()
	if applied != "" {
		nodes[2].Annotations[AppliedAnnotation] = applied
	}
	if message != "" {
		nodes[2].Annotations[MessageAnnotation] = message
	}
	in := input(spec, nodes...)
	in.Record = rolled(spec)
	return in
}

func TestDecide_WaitsForTheNodeToApplyTheSettings(t *testing.T) {
	r := Decide(applying(hugepagesSpec(4), "", "waiting: rebooting for the kernel arguments"))
	expectKinds(t, r.Actions)
	expectRollout(t, r, k4allv1alpha1.NodeRolloutRolling, "Rolling")
	if got := r.Status.PerNode[0]; got.Name != w0 || got.Message != "waiting: rebooting for the kernel arguments" {
		t.Errorf("w-0 = %+v, want the agent's message", got)
	}
}

func TestDecide_NodeRefusingTheSettingsBlocksTheRollout(t *testing.T) {
	r := Decide(applying(hugepagesSpec(4), "", "hugepage kernel arguments set by hand: hugepagesz=1G hugepages=2"))
	expectKinds(t, r.Actions)
	expectRollout(t, r, k4allv1alpha1.NodeRolloutBlocked, "NodeRefused")
	if !strings.Contains(r.Status.Rollout.Message, "w-0 refuses the settings: hugepage kernel arguments set by hand") {
		t.Errorf("message = %q", r.Status.Rollout.Message)
	}
}

// A refusing node is let go when the settings change: its next step starts
// from the new ones.
func TestDecide_ChangedSettingsLetARefusingNodeGo(t *testing.T) {
	in := applying(hugepagesSpec(4), "", "hugepage kernel arguments set by hand")
	in.Spec = hugepagesSpec(2)
	r := Decide(in)
	expectKinds(t, r.Actions, "LetGo w-0")
	if a := r.Actions[0]; a.Step == nil || a.Step.Cordon != CordonByK4all || a.Target != "" {
		t.Errorf("let go = %+v, want the step to undo and the target removed", a)
	}
}

func TestDecide_FinishesTheNodeOnceApplied(t *testing.T) {
	spec := hugepagesSpec(4)
	in := applying(spec, FromSpec(spec).Hash(), "")
	r := Decide(in)
	expectKinds(t, r.Actions, "Finish w-0", "WriteConfig ")
	if r.Actions[1].Record.LastDone == nil || !r.Actions[1].Record.LastDone.Equal(now) || r.Actions[1].KubeletConfig != "" {
		t.Errorf("record = %+v, want only lastDone", r.Actions[1].Record)
	}
	if perNode(r)[w0] != PhaseDone {
		t.Errorf("w-0 = %s, want Done", perNode(r)[w0])
	}

	// Not Ready yet after its reboot: wait.
	in.Nodes[2].Ready = false
	expectKinds(t, Decide(in).Actions)
}

// After a node, the next waits for the PodDisruptionBudgets to settle.
func TestDecide_NextNodeWaitsForThePDBs(t *testing.T) {
	spec := hugepagesSpec(4)
	nodes := cluster()
	nodes[2].Annotations[TargetAnnotation] = FromSpec(spec).Hash()
	nodes[2].Annotations[AppliedAnnotation] = FromSpec(spec).Hash()
	in := input(spec, nodes...)
	in.Record = rolled(spec)
	done := now.Add(-time.Minute)
	in.Record.LastDone = &done
	in.UnhealthyPDBs = []drain.PDB{{Namespace: "db", Name: "pg", Healthy: 1, Desired: 2}}

	r := Decide(in)
	expectKinds(t, r.Actions)
	if r.Status.Rollout.Message != "waiting for PDB db/pg: 1/2 healthy" {
		t.Errorf("message = %q", r.Status.Rollout.Message)
	}
	in.UnhealthyPDBs = nil
	expectKinds(t, Decide(in).Actions, "Start w-1")
}

func TestDecide_IdleOnceEveryNodeHasTheSettings(t *testing.T) {
	spec := hugepagesSpec(4)
	nodes := cluster()
	for i := range nodes {
		nodes[i].Annotations[TargetAnnotation] = FromSpec(spec).Hash()
		nodes[i].Annotations[AppliedAnnotation] = FromSpec(spec).Hash()
	}
	in := input(spec, nodes...)
	in.Record = rolled(spec)
	r := Decide(in)
	expectKinds(t, r.Actions)
	expectRollout(t, r, k4allv1alpha1.NodeRolloutIdle, "RolledOut")
	if r.Condition.ObservedGeneration != 3 {
		t.Errorf("observedGeneration = %d", r.Condition.ObservedGeneration)
	}
}

func TestDecide_SingleNodeIsCordonedOnly(t *testing.T) {
	in := input(hugepagesSpec(4), node("cp-0", "bootstrap"))
	in.Pods = map[string][]drain.Pod{"cp-0": {{Namespace: "app", Name: "web", Created: now.Add(-time.Hour)}}}
	in.Self = &drain.Self{Node: "cp-0", Pod: drain.Pod{Namespace: "k4all-operator-system", Name: "op", Created: now.Add(-time.Hour)}}
	r := Decide(in)
	expectKinds(t, r.Actions, "WriteConfig ", "Start cp-0", "SetTarget cp-0")
	if r.Actions[1].Step.Cordon != CordonByK4all {
		t.Errorf("step = %+v, want cordoned", r.Actions[1].Step)
	}
}

// k4all#97: the drain of the operator's own node ends with the operator
// evicting itself, before the target, so the leader is elsewhere when the
// node reboots.
func TestDecide_OperatorLeavesItsNodeBeforeTheTarget(t *testing.T) {
	spec := hugepagesSpec(4)
	nodes := cluster()
	nodes[2] = inStep(nodes[2], spec, CordonByK4all, now.Add(-time.Minute))
	in := input(spec, nodes...)
	in.Record = rolled(spec)
	in.Self = &drain.Self{Node: w0, Pod: drain.Pod{Namespace: "k4all-operator-system", Name: "op", Created: now.Add(-time.Hour)}}

	r := Decide(in)
	expectKinds(t, r.Actions, "EvictSelf w-0")
	if r.Actions[0].Pods[0] != "k4all-operator-system/op" {
		t.Errorf("pods = %v", r.Actions[0].Pods)
	}
	in.SelfEvictTried = true
	expectKinds(t, Decide(in).Actions, "SetTarget w-0")
}

// D12: never during a ClusterUpgrade; kubelet-config is not written either.
func TestDecide_WaitsForTheClusterUpgrade(t *testing.T) {
	in := input(hugepagesSpec(4), cluster()...)
	in.UpgradeBusy = "the ClusterUpgrade to v2.7.1 is in progress"
	r := Decide(in)
	expectKinds(t, r.Actions)
	expectRollout(t, r, k4allv1alpha1.NodeRolloutBlocked, "UpgradeInProgress")
	if !strings.Contains(r.Status.Rollout.Message, "v2.7.1") {
		t.Errorf("message = %q", r.Status.Rollout.Message)
	}
}

// A step in progress finishes even if a ClusterUpgrade starts: the
// ClusterUpgrade waits for it (it cordons no node meanwhile).
func TestDecide_StepInProgressFinishesDuringAClusterUpgrade(t *testing.T) {
	spec := hugepagesSpec(4)
	in := applying(spec, FromSpec(spec).Hash(), "")
	in.UpgradeBusy = "the ClusterUpgrade to v2.7.1 is in progress"
	expectKinds(t, Decide(in).Actions, "Finish w-0", "WriteConfig ")
}

func TestDecide_PausedStopsBetweenNodes(t *testing.T) {
	spec := hugepagesSpec(4)
	spec.Rollout = &k4allv1alpha1.NodeRollout{Paused: true}
	r := Decide(input(spec, cluster()...))
	expectKinds(t, r.Actions)
	expectRollout(t, r, k4allv1alpha1.NodeRolloutPaused, "Paused")
	if !strings.Contains(r.Status.Rollout.Message, "3 nodes left") {
		t.Errorf("message = %q", r.Status.Rollout.Message)
	}

	in := applying(spec, FromSpec(spec).Hash(), "")
	expectKinds(t, Decide(in).Actions, "Finish w-0", "WriteConfig ")
}

// Nodes without the v2.7.0 agent are skipped, then rolled once upgraded.
func TestDecide_SkipsNodesOlderThanTheAgent(t *testing.T) {
	spec := hugepagesSpec(4)
	nodes := cluster()
	nodes[2].Annotations[k4allv1alpha1.NodeReleaseAnnotation] = "v2.6.6"
	r := Decide(input(spec, nodes...))
	expectKinds(t, r.Actions, "WriteConfig ", "Start w-1")
	if got := r.Status.PerNode[0]; got.Name != w0 || got.Phase != PhaseSkipped ||
		!strings.Contains(got.Message, "runs k4all v2.6.6") {
		t.Errorf("w-0 = %+v, want Skipped with a message", got)
	}

	// Everyone else done: blocked until w-0 is upgraded.
	for i := 0; i < 2; i++ {
		nodes[i].Annotations[TargetAnnotation] = FromSpec(spec).Hash()
		nodes[i].Annotations[AppliedAnnotation] = FromSpec(spec).Hash()
	}
	in := input(spec, nodes...)
	in.Record = rolled(spec)
	r = Decide(in)
	expectKinds(t, r.Actions)
	expectRollout(t, r, k4allv1alpha1.NodeRolloutBlocked, "NodesSkipped")

	nodes[2].Annotations[k4allv1alpha1.NodeReleaseAnnotation] = "v2.7.0"
	expectKinds(t, Decide(in).Actions, "Start w-0")
}

func TestDecide_RefusesSettingsThatDoNotFit(t *testing.T) {
	spec := &k4allv1alpha1.NodesConfig{Kubelet: &k4allv1alpha1.KubeletSettings{CPUManagerPolicy: "static", ReservedSystemCPUs: "0-9"}}
	r := Decide(input(spec, cluster()...))
	expectKinds(t, r.Actions)
	expectRollout(t, r, k4allv1alpha1.NodeRolloutBlocked, "Invalid")
	if !strings.Contains(r.Condition.Message, "node cp-0 has 8 CPUs") {
		t.Errorf("message = %q, want the node named", r.Condition.Message)
	}
}

func TestDecide_ClearingRemovesWhatK4allWrote(t *testing.T) {
	old := &k4allv1alpha1.NodesConfig{Kubelet: &k4allv1alpha1.KubeletSettings{CPUManagerPolicy: "static", ReservedSystemCPUs: "0-1"}}
	doc, _, _, _ := MergeKubelet(kubeadmKubelet, FromSpec(old).Kubelet, nil)
	nodes := cluster()
	for i := range nodes {
		nodes[i].Annotations[TargetAnnotation] = FromSpec(old).Hash()
		nodes[i].Annotations[AppliedAnnotation] = FromSpec(old).Hash()
	}
	in := input(nil, nodes...)
	in.KubeletConfig = doc
	in.Record = rolled(old)

	r := Decide(in)
	expectKinds(t, r.Actions, "WriteConfig ", "Start w-0")
	if cfg := r.Actions[0].KubeletConfig; strings.Contains(cfg, "cpuManagerPolicy") || !strings.Contains(cfg, "maxPods: 250") {
		t.Errorf("kubelet-config:\n%s", cfg)
	}
	if rec := r.Actions[0].Record; rec.Hash != (Settings{}).Hash() || rec.History[FromSpec(old).Hash()].Kubelet.CPUManagerPolicy != "static" {
		t.Errorf("record = %+v, want the empty settings and the old ones in the history", rec)
	}
	if s := r.Actions[1].Step; s.Class != DrainRestart {
		t.Errorf("step class = %s, want drain-restart (the CPU manager goes back to none)", s.Class)
	}
}

// What a node does depends on what it has: the history of the record.
func TestDecide_ClassFollowsWhatTheNodeHas(t *testing.T) {
	first := &k4allv1alpha1.NodesConfig{Kubelet: &k4allv1alpha1.KubeletSettings{CPUManagerPolicy: "static", ReservedSystemCPUs: "0"}}
	second := &k4allv1alpha1.NodesConfig{Kubelet: &k4allv1alpha1.KubeletSettings{CPUManagerPolicy: "static", ReservedSystemCPUs: "0",
		MaxPods: ptr(int32(200))}}
	nodes := cluster()
	nodes[2].Annotations[AppliedAnnotation] = FromSpec(first).Hash() // w-0 had the first rollout
	in := input(second, nodes...)
	in.Record = rolled(first)

	r := Decide(in)
	expectKinds(t, r.Actions, "WriteConfig ", "Start w-0", "SetTarget w-0")
	if s := r.Actions[1].Step; s.Class != Restart {
		t.Errorf("w-0 class = %s, want restart (only maxPods differs from what it has)", s.Class)
	}

	nodes[2].Annotations[AppliedAnnotation] = "unknown-hash"
	r = Decide(in)
	if s := r.Actions[1].Step; s.Class != DrainReboot {
		t.Errorf("class from an unknown hash = %s, want drain-reboot", s.Class)
	}
}

// Changing the settings while a node drains: it gets the new ones, unless
// it no longer needs anything.
func TestDecide_SettingsChangedDuringTheDrain(t *testing.T) {
	spec := hugepagesSpec(4)
	nodes := cluster()
	nodes[2] = inStep(nodes[2], spec, CordonByK4all, now.Add(-time.Minute))
	in := input(hugepagesSpec(8), nodes...)
	in.Record = rolled(spec)
	in.Pods = map[string][]drain.Pod{w0: {{Namespace: "app", Name: "web", Created: now.Add(-time.Hour)}}}

	r := Decide(in)
	expectKinds(t, r.Actions, "WriteConfig ", "UpdateStep w-0", "Evict w-0")
	if s := r.Actions[1].Step; s.Hash != FromSpec(hugepagesSpec(8)).Hash() || !s.Started.Equal(now.Add(-time.Minute)) {
		t.Errorf("step = %+v, want the new hash and the same start", s)
	}

	in.Spec = nil // w-0 has nothing of k4all's: let it go
	r = Decide(in)
	expectKinds(t, r.Actions, "LetGo w-0")
}

func TestDecide_DoesNotCordonANodeItCouldNeverDrain(t *testing.T) {
	in := input(hugepagesSpec(4), cluster()...)
	in.Blockers = map[string][]drain.Blocker{w0: {{PDB: "db/pg-primary", Pod: "db/pg-1"}}}
	r := Decide(in)
	expectKinds(t, r.Actions)
	expectRollout(t, r, k4allv1alpha1.NodeRolloutBlocked, "DrainBlocked")
	if !strings.Contains(r.Status.Rollout.Message, "w-0 cannot be drained: PDB db/pg-primary") {
		t.Errorf("message = %q", r.Status.Rollout.Message)
	}
}

func TestDecide_DrainTimeoutBlocksAndKeepsEvicting(t *testing.T) {
	spec := hugepagesSpec(4)
	nodes := cluster()
	nodes[2] = inStep(nodes[2], spec, CordonByK4all, now.Add(-31*time.Minute))
	in := input(spec, nodes...)
	in.Record = rolled(spec)
	in.Pods = map[string][]drain.Pod{w0: {{Namespace: "app", Name: "web", Created: now.Add(-time.Hour)}}}

	r := Decide(in)
	expectKinds(t, r.Actions, "Evict w-0")
	expectRollout(t, r, k4allv1alpha1.NodeRolloutBlocked, "DrainTimeout")
}

func TestDecide_WaitsForEveryNodeToBeReady(t *testing.T) {
	nodes := cluster()
	nodes[0].Ready = false
	r := Decide(input(hugepagesSpec(4), nodes...))
	expectKinds(t, r.Actions)
	if r.Status.Rollout.Message != "waiting for cp-0 to be Ready" {
		t.Errorf("message = %q", r.Status.Rollout.Message)
	}
}

func TestDecide_WithoutKubeletConfigNothingStarts(t *testing.T) {
	in := input(hugepagesSpec(4), cluster()...)
	in.KubeletConfigMissing = true
	r := Decide(in)
	expectKinds(t, r.Actions)
	expectRollout(t, r, k4allv1alpha1.NodeRolloutBlocked, "KubeletConfigMissing")
}

// What k4all owns in kubelet-config comes back if edited by hand.
func TestDecide_RewritesOwnedFieldsEditedByHand(t *testing.T) {
	spec := maxPodsSpec(200)
	nodes := cluster()
	for i := range nodes {
		nodes[i].Annotations[AppliedAnnotation] = FromSpec(spec).Hash()
	}
	in := input(spec, nodes...)
	in.Record = rolled(spec) // the ConfigMap still says 250
	r := Decide(in)
	expectKinds(t, r.Actions, "WriteConfig ")
	expectRollout(t, r, k4allv1alpha1.NodeRolloutIdle, "RolledOut")
}

func TestStep_RoundTrip(t *testing.T) {
	s := Step{Hash: "h", Class: DrainRestart, Cordon: CordonAlready, Started: now}
	var raw map[string]interface{}
	if err := json.Unmarshal([]byte(s.Encode()), &raw); err != nil || raw["class"] != "drain-restart" || raw["cordon"] != "already" {
		t.Errorf("step JSON = %s (%v)", s.Encode(), err)
	}
	if back := parseStep(s.Encode()); back != s {
		t.Errorf("parseStep = %+v, want %+v", back, s)
	}
	if back := parseStep("{"); back.Cordon != CordonAlready {
		t.Errorf("an unreadable step = %+v, want one that never uncordons", back)
	}
}

// A node the admin had cordoned stays cordoned after its step.
func TestDecide_KeepsTheAdminsCordon(t *testing.T) {
	spec := hugepagesSpec(4)
	nodes := cluster()
	nodes[2].Unschedulable = true
	r := Decide(input(spec, nodes...))
	if s := r.Actions[1].Step; r.Actions[1].Node != w0 || s.Cordon != CordonAlready {
		t.Fatalf("start = %+v, want w-0 marked cordoned already", r.Actions[1])
	}

	nodes = cluster()
	nodes[2] = inStep(nodes[2], spec, CordonAlready, now.Add(-time.Minute))
	nodes[2].Annotations[TargetAnnotation] = FromSpec(spec).Hash()
	nodes[2].Annotations[AppliedAnnotation] = FromSpec(spec).Hash()
	in := input(spec, nodes...)
	in.Record = rolled(spec)
	r = Decide(in)
	expectKinds(t, r.Actions, "Finish w-0", "WriteConfig ")
	if r.Actions[0].Step.Cordon != CordonAlready {
		t.Errorf("finish = %+v, want the admin's cordon kept", r.Actions[0].Step)
	}
}

// The first write of a field records what kubelet-config had there.
func TestDecide_FirstWriteRecordsThePriorValue(t *testing.T) {
	r := Decide(input(maxPodsSpec(300), cluster()...))
	rec := r.Actions[0].Record
	if r.Actions[0].Kind != WriteConfig || rec.Prior["maxPods"] == nil || *rec.Prior["maxPods"] != "250" {
		t.Errorf("record = %+v, want the prior maxPods 250", rec)
	}
}

// Clearing maxPods puts kubeadm's 250 back, not the kubelet's 110, and the
// nodes take it with a restart.
func TestDecide_ClearingPutsBackThePriorValueWithARestart(t *testing.T) {
	spec := maxPodsSpec(300)
	doc, _, _, _ := MergeKubelet(kubeadmKubelet, FromSpec(spec).Kubelet, nil)
	nodes := cluster()
	for i := range nodes {
		nodes[i].Annotations[TargetAnnotation] = FromSpec(spec).Hash()
		nodes[i].Annotations[AppliedAnnotation] = FromSpec(spec).Hash()
	}
	in := input(nil, nodes...)
	in.KubeletConfig = doc
	in.Record = rolled(spec)

	r := Decide(in)
	expectKinds(t, r.Actions, "WriteConfig ", "Start w-0", "SetTarget w-0")
	if cfg := r.Actions[0].KubeletConfig; !strings.Contains(cfg, "maxPods: 250") || strings.Contains(cfg, "300") {
		t.Errorf("kubelet-config:\n%s", cfg)
	}
	if rec := r.Actions[0].Record; len(rec.Prior) != 0 || rec.Hash != (Settings{}).Hash() {
		t.Errorf("record = %+v, want nothing owned", rec)
	}
	if s := r.Actions[1].Step; s.Class != Restart || s.Cordon != CordonNone {
		t.Errorf("step = %+v, want a restart without a cordon", s)
	}
}
