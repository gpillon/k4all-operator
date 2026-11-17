package clusterupgrade

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	k4allv1alpha1 "github.com/gpillon/k4all-operator/api/v1alpha1"
	"github.com/gpillon/k4all-operator/internal/upgradegraph"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

var now = time.Date(2026, 11, 21, 2, 30, 0, 0, time.UTC)

// graph is a piece of the k4all ladder with a patch in the v2.6 stream.
func graph(t *testing.T) *upgradegraph.Graph {
	t.Helper()
	rel := func(version, kind, minFrom, kubernetes string, fedora int) map[string]interface{} {
		return map[string]interface{}{"version": version, "kind": kind, "minFrom": minFrom,
			"kubernetes": kubernetes, "crio": kubernetes[:4], "fedora": fedora}
	}
	doc, err := json.Marshal(map[string]interface{}{"releases": []interface{}{
		rel("v2.4.0", "minor", "v2.3.0", "1.35.10", 43),
		rel("v2.5.0", "minor", "v2.4.0", "1.35.10", 45),
		rel("v2.6.0", "minor", "v2.5.0", "1.35.10", 45),
		rel("v2.6.1", "patch", "v2.6.0", "1.35.11", 45),
		rel("v2.7.0", "minor", "v2.6.0", "1.36.6", 45),
	}})
	if err != nil {
		t.Fatal(err)
	}
	g, err := upgradegraph.Parse(doc)
	if err != nil {
		t.Fatal(err)
	}
	return g
}

// node is a Ready node whose k4all-upgrade reported its type and release.
func node(name, role, release string) Node {
	return Node{Name: name, Ready: true, Annotations: map[string]string{
		k4allv1alpha1.NodeTypeAnnotation:    role,
		k4allv1alpha1.NodeReleaseAnnotation: release,
	}}
}

// firstHop is the hop from v2.5.0, where most cases start.
const firstHop = "v2.6.0"

func expectActions(t *testing.T, got []Action, want ...Action) {
	t.Helper()
	if len(got) == 0 && len(want) == 0 {
		return
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("actions = %+v, want %+v", got, want)
	}
}

func nodePhases(status k4allv1alpha1.ClusterUpgradeStatus) map[string]k4allv1alpha1.NodeUpgradePhase {
	phases := map[string]k4allv1alpha1.NodeUpgradePhase{}
	for _, n := range status.Nodes {
		phases[n.Name] = n.Phase
	}
	return phases
}

func TestDecide_WithoutTargetReportsTheClusterRelease(t *testing.T) {
	status, actions := Decide(Input{
		Nodes: []Node{node("w-0", "worker", "v2.4.0"), node("cp-0", "bootstrap", "v2.5.0")},
		Graph: graph(t),
		Now:   now,
	})

	expectActions(t, actions)
	if status.Phase != k4allv1alpha1.ClusterUpgradeIdle {
		t.Errorf("phase = %s, want Idle", status.Phase)
	}
	if status.CurrentRelease != "v2.4.0" {
		t.Errorf("currentRelease = %s, want v2.4.0, the oldest node's", status.CurrentRelease)
	}
	available := make([]string, 0, len(status.AvailableReleases))
	for _, r := range status.AvailableReleases {
		available = append(available, r.Version)
	}
	if want := []string{"v2.5.0", "v2.6.0", "v2.6.1", "v2.7.0"}; !reflect.DeepEqual(available, want) {
		t.Errorf("availableReleases = %v, want %v", available, want)
	}
	order := make([]string, 0, len(status.Nodes))
	for _, n := range status.Nodes {
		order = append(order, n.Name+"/"+n.Role+"/"+n.Release)
	}
	if want := []string{"cp-0/bootstrap/v2.5.0", "w-0/worker/v2.4.0"}; !reflect.DeepEqual(order, want) {
		t.Errorf("nodes = %v, want %v (upgrade order)", order, want)
	}
}

func threeNodes(release string) []Node {
	return []Node{node("w-0", "worker", release), node("cp-1", "control", release), node("cp-0", "bootstrap", release)}
}

func TestDecide_StartsWithTheBootstrapNode(t *testing.T) {
	status, actions := Decide(Input{
		Spec:  k4allv1alpha1.ClusterUpgradeSpec{TargetRelease: "v2.7.0"},
		Nodes: threeNodes("v2.5.0"),
		Graph: graph(t),
		Now:   now,
	})

	expectActions(t, actions, Action{Kind: Cordon, Node: "cp-0"})
	if status.Phase != k4allv1alpha1.ClusterUpgradeProgressing || status.CurrentHop != firstHop {
		t.Errorf("phase %s, currentHop %s; want Progressing at v2.6.0", status.Phase, status.CurrentHop)
	}
	wantPath := []k4allv1alpha1.Hop{
		{Release: "v2.6.0", Kind: "minor", Phase: k4allv1alpha1.HopInProgress},
		{Release: "v2.6.1", Kind: "patch", Phase: k4allv1alpha1.HopPending},
		{Release: "v2.7.0", Kind: "minor", Phase: k4allv1alpha1.HopPending},
	}
	if !reflect.DeepEqual(hops(status.Path), hops(wantPath)) {
		t.Errorf("path = %+v, want %+v", status.Path, wantPath)
	}
	if want := map[string]k4allv1alpha1.NodeUpgradePhase{
		"cp-0": k4allv1alpha1.NodeUpgradeDraining, "cp-1": k4allv1alpha1.NodeUpgradePending, "w-0": k4allv1alpha1.NodeUpgradePending,
	}; !reflect.DeepEqual(nodePhases(status), want) {
		t.Errorf("node phases = %v, want %v", nodePhases(status), want)
	}
	if started := status.Nodes[0].DrainStartedAt; started == nil || !started.Time.Equal(now) {
		t.Errorf("cp-0 drainStartedAt = %v, want %v", started, now)
	}
}

// appPods are n pods of an app on cp-0 since before the drain.
func appPods(n int, drainStart time.Time) []Pod {
	pods := make([]Pod, 0, n)
	for i := 0; i < n; i++ {
		pods = append(pods, Pod{Namespace: "default", Name: fmt.Sprintf("app-%d", i), Created: drainStart.Add(-time.Hour)})
	}
	return pods
}

// draining is the state after the operator cordoned cp-0 at drainStart,
// with podsLeft of the pods it had then still on it.
func draining(t *testing.T, drainStart time.Time, podsLeft int) Input {
	nodes := threeNodes("v2.5.0")
	nodes[2].Annotations[k4allv1alpha1.NodeUpgradeCordonedAnnotation] = "true"
	nodes[2].Unschedulable = true
	started := metav1.NewTime(drainStart)
	return Input{
		Spec:  k4allv1alpha1.ClusterUpgradeSpec{TargetRelease: "v2.7.0", Drain: k4allv1alpha1.DrainSpec{TimeoutMinutes: 30}},
		Nodes: nodes,
		Previous: k4allv1alpha1.ClusterUpgradeStatus{Nodes: []k4allv1alpha1.NodeUpgradeStatus{
			{Name: "cp-0", Phase: k4allv1alpha1.NodeUpgradeDraining, DrainStartedAt: &started},
		}},
		Pods:  map[string][]Pod{"cp-0": appPods(podsLeft, drainStart)},
		Graph: graph(t),
		Now:   now,
	}
}

func TestDecide_EvictsUntilTheNodeIsDrained(t *testing.T) {
	status, actions := Decide(draining(t, now.Add(-time.Minute), 2))

	expectActions(t, actions, Action{Kind: Evict, Node: "cp-0", Pods: []string{"default/app-0", "default/app-1"}})
	if got := status.Nodes[0]; got.Phase != k4allv1alpha1.NodeUpgradeDraining || !got.DrainStartedAt.Time.Equal(now.Add(-time.Minute)) {
		t.Errorf("cp-0 = %s since %v, want Draining since the cordon", got.Phase, got.DrainStartedAt)
	}
	if got := status.Nodes[0].Message; got != "2 pods left: default/app-0, default/app-1" {
		t.Errorf("cp-0 message = %q, want the pods named", got)
	}
}

// Like kubectl drain: the drain moves the pods the node had when it started.
// A pod that tolerates the cordon can be put back on the node by its
// controller; it is neither evicted again nor waited for, only named.
func TestDecide_DrainsThePodsTheNodeHadWhenItStarted(t *testing.T) {
	drainStart := now.Add(-time.Minute)
	back := Pod{Namespace: "tigera-operator", Name: "tigera-operator-7d9f", Created: drainStart.Add(30 * time.Second)}

	t.Run("evicts only the pods from before", func(t *testing.T) {
		in := draining(t, drainStart, 1)
		in.Pods["cp-0"] = append(in.Pods["cp-0"], back)

		_, actions := Decide(in)
		expectActions(t, actions, Action{Kind: Evict, Node: "cp-0", Pods: []string{"default/app-0"}})
	})

	t.Run("asks for the hop with only the pods that came back", func(t *testing.T) {
		in := draining(t, drainStart, 0)
		in.Pods["cp-0"] = []Pod{back}

		status, actions := Decide(in)
		expectActions(t, actions, Action{Kind: SetTarget, Node: "cp-0", Release: firstHop})
		if got := status.Nodes[0].Message; !strings.Contains(got, "tigera-operator/tigera-operator-7d9f") {
			t.Errorf("cp-0 message = %q, want the pod that came back named", got)
		}
	})

	t.Run("a terminating pod is waited for, not evicted again", func(t *testing.T) {
		in := draining(t, drainStart, 2)
		in.Pods["cp-0"][0].Terminating = true

		status, actions := Decide(in)
		expectActions(t, actions, Action{Kind: Evict, Node: "cp-0", Pods: []string{"default/app-1"}})
		if got := status.Nodes[0].Phase; got != k4allv1alpha1.NodeUpgradeDraining {
			t.Errorf("cp-0 phase = %s, want Draining", got)
		}
	})
}

// The note about pods that came back stays while the node runs its hop.
func TestDecide_KeepsTheNoteAboutPodsThatCameBack(t *testing.T) {
	in := upgrading(t, "v2.5.0", k4allv1alpha1.NodeUpgradePhaseAnnotation, "STAGE")
	in.Previous.Nodes[0].Message = "back on the node after the drain started: tigera-operator/tigera-operator-7d9f"

	status, _ := Decide(in)
	if got := status.Nodes[0].Message; got != in.Previous.Nodes[0].Message {
		t.Errorf("cp-0 message = %q, want %q", got, in.Previous.Nodes[0].Message)
	}
}

func TestDecide_AsksForTheHopOnceDrained(t *testing.T) {
	status, actions := Decide(draining(t, now.Add(-time.Minute), 0))

	expectActions(t, actions, Action{Kind: SetTarget, Node: "cp-0", Release: firstHop})
	if got := status.Nodes[0].Phase; got != k4allv1alpha1.NodeUpgradeUpgrading {
		t.Errorf("cp-0 phase = %s, want Upgrading", got)
	}
}

// upgrading is cp-0 running the v2.6.0 hop the operator asked for.
func upgrading(t *testing.T, release string, annotations ...string) Input {
	in := draining(t, now.Add(-10*time.Minute), 0)
	cp0 := &in.Nodes[2]
	cp0.Annotations[k4allv1alpha1.NodeReleaseAnnotation] = release
	cp0.Annotations[k4allv1alpha1.NodeUpgradeTargetAnnotation] = firstHop
	for i := 0; i+1 < len(annotations); i += 2 {
		cp0.Annotations[annotations[i]] = annotations[i+1]
	}
	in.Previous.Nodes[0].Phase = k4allv1alpha1.NodeUpgradeUpgrading
	return in
}

func TestDecide_ReportsTheHostPhaseOfTheHop(t *testing.T) {
	status, actions := Decide(upgrading(t, "v2.5.0", k4allv1alpha1.NodeUpgradePhaseAnnotation, "REBOOT"))

	expectActions(t, actions)
	if got := status.Nodes[0]; got.Phase != k4allv1alpha1.NodeUpgradeUpgrading || got.HostPhase != "REBOOT" {
		t.Errorf("cp-0 = %s/%s, want Upgrading/REBOOT", got.Phase, got.HostPhase)
	}
	if status.Phase != k4allv1alpha1.ClusterUpgradeProgressing {
		t.Errorf("phase = %s, want Progressing", status.Phase)
	}
}

// Past the reboot the node is Ready on the new release, and the SYNC of
// k4all-upgrade may start pods that only fit on it (a single node, or a pod
// bound to the bootstrap node): the cordon ends there, not at upgrade-done.
func TestDecide_UncordonsTheNodeOnceTheHostIsPastTheReboot(t *testing.T) {
	for _, hostPhase := range []string{"SYNC", "COMMIT"} {
		t.Run(hostPhase, func(t *testing.T) {
			status, actions := Decide(upgrading(t, "v2.5.0", k4allv1alpha1.NodeUpgradePhaseAnnotation, hostPhase))

			expectActions(t, actions, Action{Kind: Uncordon, Node: "cp-0"})
			if got := nodePhases(status)["cp-0"]; got != k4allv1alpha1.NodeUpgradeUpgrading {
				t.Errorf("cp-0 phase = %s, want Upgrading", got)
			}
		})
	}
	for _, hostPhase := range []string{"STAGE", "REBOOT"} {
		t.Run("not on "+hostPhase, func(t *testing.T) {
			_, actions := Decide(upgrading(t, "v2.5.0", k4allv1alpha1.NodeUpgradePhaseAnnotation, hostPhase))
			expectActions(t, actions)
		})
	}
	for _, mark := range []string{k4allv1alpha1.NodeCordonReleased, k4allv1alpha1.NodeCordonedAlready} {
		t.Run("not when upgrade-cordoned is "+mark, func(t *testing.T) {
			_, actions := Decide(upgrading(t, "v2.5.0", k4allv1alpha1.NodeUpgradePhaseAnnotation, "SYNC",
				k4allv1alpha1.NodeUpgradeCordonedAnnotation, mark))
			expectActions(t, actions)
		})
	}
}

func TestDecide_FinishesTheNodeWhenTheHostIsDone(t *testing.T) {
	status, actions := Decide(upgrading(t, "v2.6.0", k4allv1alpha1.NodeUpgradeDoneAnnotation, "v2.6.0"))

	expectActions(t, actions, Action{Kind: Finish, Node: "cp-0"})
	if got := status.Nodes[0].Phase; got != k4allv1alpha1.NodeUpgradeDone {
		t.Errorf("cp-0 phase = %s, want Done", got)
	}
}

func TestDecide_GoesOnWithTheNextNodeOfTheHop(t *testing.T) {
	nodes := threeNodes("v2.5.0")
	nodes[2].Annotations[k4allv1alpha1.NodeReleaseAnnotation] = "v2.6.0"
	_, actions := Decide(Input{Spec: k4allv1alpha1.ClusterUpgradeSpec{TargetRelease: "v2.7.0"}, Nodes: nodes, Graph: graph(t), Now: now})

	expectActions(t, actions, Action{Kind: Cordon, Node: "cp-1"})
}

// Every node finishes a hop before any node starts the next one.
// settled is the cluster after cp-0 finished the first hop finishedAgo,
// with cp-1 next.
func settled(t *testing.T, finishedAgo time.Duration, pdbs ...PDB) Input {
	nodes := threeNodes("v2.5.0")
	nodes[2].Annotations[k4allv1alpha1.NodeReleaseAnnotation] = firstHop
	completed := metav1.NewTime(now.Add(-finishedAgo))
	return Input{
		Spec:  target("v2.7.0"),
		Nodes: nodes,
		Previous: k4allv1alpha1.ClusterUpgradeStatus{Nodes: []k4allv1alpha1.NodeUpgradeStatus{
			{Name: "cp-0", Phase: k4allv1alpha1.NodeUpgradeDone, CompletedAt: &completed},
		}},
		UnhealthyPDBs: pdbs,
		Graph:         graph(t),
		Now:           now,
	}
}

var calicoAPIServer = PDB{Namespace: "calico-system", Name: "calico-apiserver", Healthy: 1, Desired: 2}

// After a node's hop its pods come back elsewhere; until every
// PodDisruptionBudget has its healthy pods again, the next drain would be
// refused (calico-apiserver, topolvm-controller on VMs 954-956).
func TestDecide_WaitsForThePDBsBeforeTheNextNode(t *testing.T) {
	t.Run("waits", func(t *testing.T) {
		status, actions := Decide(settled(t, time.Minute, calicoAPIServer))
		expectActions(t, actions)
		expectPhase(t, status, k4allv1alpha1.ClusterUpgradeProgressing,
			"v2.6.0: waiting for PDB calico-system/calico-apiserver: 1/2 healthy")
	})

	t.Run("goes on after settleMinutes", func(t *testing.T) {
		_, actions := Decide(settled(t, 11*time.Minute, calicoAPIServer))
		expectActions(t, actions, Action{Kind: Cordon, Node: "cp-1"})
	})

	t.Run("settleMinutes 0 does not wait", func(t *testing.T) {
		in := settled(t, time.Minute, calicoAPIServer)
		zero := int32(0)
		in.Spec.Drain.SettleMinutes = &zero
		_, actions := Decide(in)
		expectActions(t, actions, Action{Kind: Cordon, Node: "cp-1"})
	})

	t.Run("healthy PDBs", func(t *testing.T) {
		_, actions := Decide(settled(t, time.Minute))
		expectActions(t, actions, Action{Kind: Cordon, Node: "cp-1"})
	})

	t.Run("not before the first node", func(t *testing.T) {
		_, actions := Decide(Input{Spec: target("v2.7.0"), Nodes: threeNodes("v2.5.0"),
			UnhealthyPDBs: []PDB{calicoAPIServer}, Graph: graph(t), Now: now})
		expectActions(t, actions, Action{Kind: Cordon, Node: "cp-0"})
	})
}

func TestDecide_StartsTheNextHopFromTheBootstrapNode(t *testing.T) {
	status, actions := Decide(Input{
		Spec:  k4allv1alpha1.ClusterUpgradeSpec{TargetRelease: "v2.7.0"},
		Nodes: threeNodes("v2.6.0"),
		Previous: k4allv1alpha1.ClusterUpgradeStatus{Path: []k4allv1alpha1.Hop{
			{Release: "v2.6.0", Kind: "minor", Phase: k4allv1alpha1.HopInProgress},
			{Release: "v2.6.1", Kind: "patch", Phase: k4allv1alpha1.HopPending},
			{Release: "v2.7.0", Kind: "minor", Phase: k4allv1alpha1.HopPending},
		}},
		Graph: graph(t),
		Now:   now,
	})

	expectActions(t, actions, Action{Kind: Cordon, Node: "cp-0"})
	wantPath := []k4allv1alpha1.Hop{
		{Release: "v2.6.0", Kind: "minor", Phase: k4allv1alpha1.HopDone},
		{Release: "v2.6.1", Kind: "patch", Phase: k4allv1alpha1.HopInProgress},
		{Release: "v2.7.0", Kind: "minor", Phase: k4allv1alpha1.HopPending},
	}
	if !reflect.DeepEqual(hops(status.Path), hops(wantPath)) {
		t.Errorf("path = %+v, want %+v", status.Path, wantPath)
	}
}

// A single node has nowhere to move its pods to: it is only cordoned.
func TestDecide_SingleNodeIsNotDrained(t *testing.T) {
	_, actions := Decide(Input{
		Spec:  k4allv1alpha1.ClusterUpgradeSpec{TargetRelease: firstHop},
		Nodes: []Node{node("k4all", "bootstrap", "v2.5.0")},
		Graph: graph(t),
		Now:   now,
	})

	expectActions(t, actions, Action{Kind: Cordon, Node: "k4all"}, Action{Kind: SetTarget, Node: "k4all", Release: firstHop})
}

func TestDecide_HoldOnTheNodeFailsTheUpgradeUntilItResumes(t *testing.T) {
	status, actions := Decide(upgrading(t, "v2.5.0",
		k4allv1alpha1.NodeUpgradePhaseAnnotation, "HOLD",
		k4allv1alpha1.NodeUpgradeMessageAnnotation, "POST_BOOT: kubelet not Ready"))

	expectActions(t, actions)
	if got := status.Nodes[0]; got.Phase != k4allv1alpha1.NodeUpgradeFailed || got.Message != "POST_BOOT: kubelet not Ready" {
		t.Errorf("cp-0 = %s %q, want Failed with the host's message", got.Phase, got.Message)
	}
	if status.Phase != k4allv1alpha1.ClusterUpgradeFailed || status.Message != "cp-0: HOLD: POST_BOOT: kubelet not Ready" {
		t.Errorf("phase %s %q, want Failed naming the node", status.Phase, status.Message)
	}

	resumed := upgrading(t, "v2.5.0", k4allv1alpha1.NodeUpgradePhaseAnnotation, "POST_BOOT")
	resumed.Previous = status
	if status, _ := Decide(resumed); status.Nodes[0].Phase != k4allv1alpha1.NodeUpgradeUpgrading {
		t.Errorf("after resume cp-0 = %s, want Upgrading", status.Nodes[0].Phase)
	}
}

// clear-hold --abandon on the node removes upgrade-target: only the retry
// annotation (which clears the failed phase) asks for the hop again.
func TestDecide_AbandonedHopStaysFailed(t *testing.T) {
	in := upgrading(t, "v2.5.0",
		k4allv1alpha1.NodeUpgradePhaseAnnotation, "ABANDONED",
		k4allv1alpha1.NodeUpgradeMessageAnnotation, "abandoned by the admin")
	delete(in.Nodes[2].Annotations, k4allv1alpha1.NodeUpgradeTargetAnnotation)

	status, actions := Decide(in)
	expectActions(t, actions)
	if got := status.Nodes[0]; got.Phase != k4allv1alpha1.NodeUpgradeFailed || got.Message != "abandoned by the admin" {
		t.Errorf("cp-0 = %s %q, want Failed", got.Phase, got.Message)
	}

	in.Previous = status
	delete(in.Nodes[2].Annotations, k4allv1alpha1.NodeUpgradePhaseAnnotation)
	if status, actions := Decide(in); status.Nodes[0].Phase != k4allv1alpha1.NodeUpgradeFailed || len(actions) > 0 {
		t.Errorf("later cp-0 = %s with %v, want still Failed and no action", status.Nodes[0].Phase, actions)
	}
}

func TestDecide_DrainTimeoutFailsTheNode(t *testing.T) {
	status, actions := Decide(draining(t, now.Add(-31*time.Minute), 2))

	expectActions(t, actions)
	if got := status.Nodes[0]; got.Phase != k4allv1alpha1.NodeUpgradeFailed || got.Message != "drain timed out after 30 minutes, 2 pods left: default/app-0, default/app-1" {
		t.Errorf("cp-0 = %s %q, want Failed on the drain timeout", got.Phase, got.Message)
	}
	if status.Phase != k4allv1alpha1.ClusterUpgradeFailed {
		t.Errorf("phase = %s, want Failed", status.Phase)
	}
}

func TestDecide_DrainTimeoutCanRunTheHopAnyway(t *testing.T) {
	in := draining(t, now.Add(-31*time.Minute), 2)
	in.Spec.Drain.ContinueOnTimeout = true

	_, actions := Decide(in)
	expectActions(t, actions, Action{Kind: SetTarget, Node: "cp-0", Release: firstHop})
}

func TestDecide_RetryDrainsTheFailedNodeAgain(t *testing.T) {
	in := draining(t, now.Add(-31*time.Minute), 2)
	failed, _ := Decide(in)
	in.Previous = failed
	in.Retry = true

	status, actions := Decide(in)
	expectActions(t, actions, Action{Kind: Evict, Node: "cp-0", Pods: []string{"default/app-0", "default/app-1"}})
	if got := status.Nodes[0]; got.Phase != k4allv1alpha1.NodeUpgradeDraining || !got.DrainStartedAt.Time.Equal(now) {
		t.Errorf("cp-0 = %s since %v, want Draining again from now", got.Phase, got.DrainStartedAt)
	}
}

func target(release string) k4allv1alpha1.ClusterUpgradeSpec {
	return k4allv1alpha1.ClusterUpgradeSpec{TargetRelease: release}
}

func expectPhase(t *testing.T, status k4allv1alpha1.ClusterUpgradeStatus, phase k4allv1alpha1.ClusterUpgradePhase, message string) {
	t.Helper()
	if status.Phase != phase || status.Message != message {
		t.Errorf("phase %s %q, want %s %q", status.Phase, status.Message, phase, message)
	}
}

func TestDecide_PausedStartsNoNode(t *testing.T) {
	spec := target("v2.7.0")
	spec.Paused = true
	status, actions := Decide(Input{Spec: spec, Nodes: threeNodes("v2.5.0"), Graph: graph(t), Now: now})

	expectActions(t, actions)
	expectPhase(t, status, k4allv1alpha1.ClusterUpgradePaused, "")
}

func TestDecide_PausedLetsTheNodeInProgressFinish(t *testing.T) {
	in := upgrading(t, "v2.6.0", k4allv1alpha1.NodeUpgradeDoneAnnotation, "v2.6.0")
	in.Spec.Paused = true

	_, actions := Decide(in)
	expectActions(t, actions, Action{Kind: Finish, Node: "cp-0"})
}

func TestDecide_SucceedsWhenEveryNodeRunsTheTarget(t *testing.T) {
	status, actions := Decide(Input{
		Spec:  target("v2.6.0"),
		Nodes: threeNodes("v2.6.0"),
		Previous: k4allv1alpha1.ClusterUpgradeStatus{Path: []k4allv1alpha1.Hop{
			{Release: "v2.6.0", Kind: "minor", Phase: k4allv1alpha1.HopInProgress},
		}},
		Graph: graph(t),
		Now:   now,
	})

	expectActions(t, actions)
	expectPhase(t, status, k4allv1alpha1.ClusterUpgradeSucceeded, "")
	if want := []k4allv1alpha1.Hop{{Release: "v2.6.0", Kind: "minor", Phase: k4allv1alpha1.HopDone}}; !reflect.DeepEqual(hops(status.Path), hops(want)) {
		t.Errorf("path = %+v, want %+v", status.Path, want)
	}
}

// k4all-upgrade v2.4.0 writes the release and node-type annotations.
func TestDecide_NodeWithoutAnnotationsStopsTheUpgrade(t *testing.T) {
	nodes := threeNodes("v2.5.0")
	delete(nodes[0].Annotations, k4allv1alpha1.NodeTypeAnnotation)
	status, actions := Decide(Input{Spec: target("v2.7.0"), Nodes: nodes, Graph: graph(t), Now: now})

	expectActions(t, actions)
	expectPhase(t, status, k4allv1alpha1.ClusterUpgradeFailed,
		"w-0 has no k4all.magesgate.com/node-type annotation: is k4all-upgrade v2.4.0 or later running there?")
}

func TestDecide_WaitsForEveryNodeToBeReady(t *testing.T) {
	nodes := threeNodes("v2.5.0")
	nodes[0].Ready = false
	status, actions := Decide(Input{Spec: target("v2.7.0"), Nodes: nodes, Graph: graph(t), Now: now})

	expectActions(t, actions)
	expectPhase(t, status, k4allv1alpha1.ClusterUpgradeProgressing, "v2.6.0: waiting for w-0 to be Ready")
}

// A hop k4all-upgrade started on its own (a patch in its window) runs first.
func TestDecide_WaitsForAHopTheNodeStartedItself(t *testing.T) {
	nodes := threeNodes("v2.5.0")
	nodes[1].Annotations["k4all.magesgate.com/upgrade-in-progress"] = "v2.5.0->v2.5.1"
	status, actions := Decide(Input{Spec: target("v2.7.0"), Nodes: nodes, Graph: graph(t), Now: now})

	expectActions(t, actions)
	expectPhase(t, status, k4allv1alpha1.ClusterUpgradeProgressing, "v2.6.0: waiting for the hop cp-1 runs on its own (v2.5.0->v2.5.1)")
}

func TestDecide_EmptiedTargetUncordonsTheDrainingNode(t *testing.T) {
	in := draining(t, now.Add(-time.Minute), 2)
	in.Spec.TargetRelease = ""

	status, actions := Decide(in)
	expectActions(t, actions, Action{Kind: Finish, Node: "cp-0"})
	expectPhase(t, status, k4allv1alpha1.ClusterUpgradeIdle, "")
}

func TestDecide_EmptiedTargetLetsTheHopInProgressFinish(t *testing.T) {
	in := upgrading(t, "v2.5.0", k4allv1alpha1.NodeUpgradePhaseAnnotation, "REBOOT")
	in.Spec.TargetRelease = ""

	status, actions := Decide(in)
	expectActions(t, actions)
	expectPhase(t, status, k4allv1alpha1.ClusterUpgradeProgressing, "stopping after cp-0 finishes its hop")
}

func TestDecide_RefusesADowngrade(t *testing.T) {
	status, actions := Decide(Input{Spec: target("v2.4.0"), Nodes: threeNodes("v2.5.0"), Graph: graph(t), Now: now})

	expectActions(t, actions)
	expectPhase(t, status, k4allv1alpha1.ClusterUpgradeFailed, "targetRelease v2.4.0 is older than the cluster's release v2.5.0")
}

func TestDecide_ReportsATargetTheGraphCannotReach(t *testing.T) {
	status, _ := Decide(Input{Spec: target("v2.9.0"), Nodes: threeNodes("v2.5.0"), Graph: graph(t), Now: now})

	expectPhase(t, status, k4allv1alpha1.ClusterUpgradeFailed,
		"no upgrade path: release v2.9.0 is not in the upgrade graph or is blocked")
}

func TestDecide_WaitsForTheGraph(t *testing.T) {
	status, actions := Decide(Input{
		Spec: target("v2.7.0"), Nodes: threeNodes("v2.5.0"), Now: now,
		GraphErr: errors.New("GET upgrade-graph.json: 503"),
	})

	expectActions(t, actions)
	expectPhase(t, status, k4allv1alpha1.ClusterUpgradeProgressing, "cannot read the upgrade graph: GET upgrade-graph.json: 503")
}

// Without the graph the drained node keeps its cordon until the hop is known.
func TestDecide_DrainingNodeWaitsForTheGraph(t *testing.T) {
	in := draining(t, now.Add(-time.Minute), 0)
	in.Graph, in.GraphErr = nil, errors.New("GET upgrade-graph.json: 503")

	status, actions := Decide(in)
	expectActions(t, actions)
	expectPhase(t, status, k4allv1alpha1.ClusterUpgradeProgressing, "cannot read the upgrade graph: GET upgrade-graph.json: 503")
}

// ── Status for the UI: reasons, graph, times, conditions ─────────────────

func TestDecide_FailedNodesCarryAMachineReadableReason(t *testing.T) {
	cases := map[string]struct {
		in   Input
		want k4allv1alpha1.NodeUpgradeReason
	}{
		"drain timeout": {draining(t, now.Add(-31*time.Minute), 2), k4allv1alpha1.NodeReasonDrainTimeout},
		"hold":          {upgrading(t, "v2.5.0", k4allv1alpha1.NodeUpgradePhaseAnnotation, "HOLD"), k4allv1alpha1.NodeReasonHostHold},
		"rejected target": {upgrading(t, "v2.5.0", k4allv1alpha1.NodeUpgradePhaseAnnotation, "HOLD",
			k4allv1alpha1.NodeUpgradeReasonAnnotation, "TargetRejected"), k4allv1alpha1.NodeReasonTargetRejected},
		"rollback": {upgrading(t, "v2.5.0", k4allv1alpha1.NodeUpgradePhaseAnnotation, "HOLD",
			k4allv1alpha1.NodeUpgradeReasonAnnotation, "RollbackRebooted"), k4allv1alpha1.NodeReasonRollbackRebooted},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			status, _ := Decide(c.in)
			if got := status.Nodes[0]; got.Phase != k4allv1alpha1.NodeUpgradeFailed || got.Reason != c.want {
				t.Errorf("cp-0 = %s/%s, want Failed/%s", got.Phase, got.Reason, c.want)
			}
			if failed := condition(status, k4allv1alpha1.ConditionFailed); failed == nil || failed.Reason != string(c.want) {
				t.Errorf("Failed condition = %+v, want reason %s", failed, c.want)
			}
		})
	}
}

func TestDecide_AbandonedHopShowsTheHostPhase(t *testing.T) {
	in := upgrading(t, "v2.5.0", k4allv1alpha1.NodeUpgradePhaseAnnotation, "ABANDONED")
	delete(in.Nodes[2].Annotations, k4allv1alpha1.NodeUpgradeTargetAnnotation)

	status, _ := Decide(in)
	if got := status.Nodes[0]; got.Reason != k4allv1alpha1.NodeReasonAbandoned || got.HostPhase != "ABANDONED" {
		t.Errorf("cp-0 = %s/%q, want Abandoned with hostPhase ABANDONED", got.Reason, got.HostPhase)
	}
}

func TestDecide_NamesTheNodeItWaitsFor(t *testing.T) {
	nodes := threeNodes("v2.5.0")
	nodes[0].Ready = false
	status, _ := Decide(Input{Spec: target("v2.7.0"), Nodes: nodes, Graph: graph(t), Now: now})

	for _, n := range status.Nodes {
		want := k4allv1alpha1.NodeUpgradeReason("")
		if n.Name == "w-0" {
			want = k4allv1alpha1.NodeReasonNotReady
		}
		if n.Reason != want {
			t.Errorf("%s reason = %q, want %q", n.Name, n.Reason, want)
		}
	}
}

// An unreadable graph must not look like an up-to-date cluster.
func TestDecide_UnreadableGraphShowsWithoutATarget(t *testing.T) {
	status, _ := Decide(Input{Nodes: threeNodes("v2.5.0"), Now: now, GraphErr: errors.New("GET upgrade-graph.json: 503")})

	expectPhase(t, status, k4allv1alpha1.ClusterUpgradeIdle, "cannot read the upgrade graph: GET upgrade-graph.json: 503")
	graphOK := condition(status, k4allv1alpha1.ConditionGraphAvailable)
	if graphOK == nil || graphOK.Status != metav1.ConditionFalse || graphOK.Message != "GET upgrade-graph.json: 503" {
		t.Errorf("GraphAvailable = %+v, want False with the error", graphOK)
	}
}

func TestDecide_AvailableReleasesCarryTheirRulesAndPath(t *testing.T) {
	status, _ := Decide(Input{Nodes: threeNodes("v2.5.0"), Graph: graph(t), Now: now})

	last := status.AvailableReleases[len(status.AvailableReleases)-1]
	if last.Version != "v2.7.0" || last.MinFrom != firstHop || last.Crio != "1.36" {
		t.Errorf("v2.7.0 = %+v, want minFrom v2.6.0 and crio 1.36", last)
	}
	if want := []string{firstHop, "v2.6.1", "v2.7.0"}; !reflect.DeepEqual(last.Path, want) {
		t.Errorf("path to v2.7.0 = %v, want %v", last.Path, want)
	}
	if cur := status.CurrentReleaseInfo; cur == nil || cur.Version != "v2.5.0" || cur.Kubernetes != "1.35.10" || cur.Fedora != 45 {
		t.Errorf("currentReleaseInfo = %+v, want v2.5.0 with Kubernetes 1.35.10 on Fedora 45", cur)
	}
}

func TestDecide_TimesHopsAndNodes(t *testing.T) {
	status, _ := Decide(Input{Spec: target("v2.7.0"), Nodes: threeNodes("v2.5.0"), Graph: graph(t), Now: now})
	if h := status.Path[0]; h.StartedAt == nil || !h.StartedAt.Time.Equal(now) {
		t.Errorf("hop %s startedAt = %v, want %v", h.Release, h.StartedAt, now)
	}
	if n := status.Nodes[0]; n.StartedAt == nil || !n.StartedAt.Time.Equal(now) || n.CompletedAt != nil {
		t.Errorf("cp-0 started %v completed %v, want started now", n.StartedAt, n.CompletedAt)
	}

	// cp-0 finishes ten minutes later; the hop keeps its start.
	in := upgrading(t, "v2.6.0", k4allv1alpha1.NodeUpgradeDoneAnnotation, firstHop)
	in.Previous.Path, in.Previous.Nodes[0].StartedAt = status.Path, status.Nodes[0].StartedAt
	in.Now = now.Add(10 * time.Minute)
	later, _ := Decide(in)
	if h := later.Path[0]; !h.StartedAt.Time.Equal(now) {
		t.Errorf("hop startedAt moved to %v", h.StartedAt)
	}
	if n := later.Nodes[0]; !n.StartedAt.Time.Equal(now) || n.CompletedAt == nil || !n.CompletedAt.Time.Equal(in.Now) {
		t.Errorf("cp-0 started %v completed %v, want %v and %v", n.StartedAt, n.CompletedAt, now, in.Now)
	}

	// Every node at v2.6.0: the hop is done from then on.
	done, _ := Decide(Input{Spec: target("v2.7.0"), Nodes: threeNodes(firstHop), Previous: later, Graph: graph(t), Now: now.Add(30 * time.Minute)})
	if h := done.Path[0]; h.Phase != k4allv1alpha1.HopDone || !h.StartedAt.Time.Equal(now) || !h.CompletedAt.Time.Equal(now.Add(30*time.Minute)) {
		t.Errorf("hop %s = %s from %v to %v", h.Release, h.Phase, h.StartedAt, h.CompletedAt)
	}
}

func TestDecide_ConditionsFollowThePhase(t *testing.T) {
	status, _ := Decide(Input{Spec: target("v2.7.0"), Nodes: threeNodes("v2.5.0"), Graph: graph(t), Now: now})
	for typ, want := range map[string]metav1.ConditionStatus{
		k4allv1alpha1.ConditionGraphAvailable: metav1.ConditionTrue,
		k4allv1alpha1.ConditionProgressing:    metav1.ConditionTrue,
		k4allv1alpha1.ConditionSucceeded:      metav1.ConditionFalse,
		k4allv1alpha1.ConditionFailed:         metav1.ConditionFalse,
	} {
		if c := condition(status, typ); c == nil || c.Status != want {
			t.Errorf("%s = %+v, want %s", typ, c, want)
		}
	}

	refused, _ := Decide(Input{Spec: target("v2.4.0"), Nodes: threeNodes("v2.5.0"), Graph: graph(t), Now: now})
	if c := condition(refused, k4allv1alpha1.ConditionFailed); c == nil || c.Status != metav1.ConditionTrue || c.Reason != "Downgrade" {
		t.Errorf("Failed = %+v, want True/Downgrade", c)
	}
}

// hops drops the times of a path, for the cases about its shape.
func hops(path []k4allv1alpha1.Hop) []string {
	out := make([]string, 0, len(path))
	for _, h := range path {
		out = append(out, h.Release+" "+h.Kind+" "+string(h.Phase))
	}
	return out
}

func condition(status k4allv1alpha1.ClusterUpgradeStatus, typ string) *metav1.Condition {
	for i := range status.Conditions {
		if status.Conditions[i].Type == typ {
			return &status.Conditions[i]
		}
	}
	return nil
}
