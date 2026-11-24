// Package clusterupgrade decides the next step of a ClusterUpgrade from the
// cluster's nodes and the upgrade graph. It does no I/O: the controller reads
// the Nodes, runs the returned actions and writes the returned status.
package clusterupgrade

import (
	"fmt"
	"sort"
	"strings"
	"time"

	k4allv1alpha1 "github.com/gpillon/k4all-operator/api/v1alpha1"
	"github.com/gpillon/k4all-operator/internal/upgradegraph"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Node is what Decide needs of a cluster Node.
type Node struct {
	Name          string
	Annotations   map[string]string
	Ready         bool
	Unschedulable bool
}

// ActionKind is a change the controller makes to a Node.
type ActionKind string

const (
	// Cordon the node, mark it upgrade-cordoned and label it draining.
	Cordon ActionKind = "Cordon"
	// Evict the pods the drain moves off the node.
	Evict ActionKind = "Evict"
	// Write upgrade-target: Release, the hop k4all-upgrade runs.
	SetTarget ActionKind = "SetTarget"
	// Uncordon the node past its reboot, mark it upgrade-cordoned: released
	// and remove the draining label.
	Uncordon ActionKind = "Uncordon"
	// Uncordon the node if the operator cordoned it, clear the
	// upgrade-target, upgrade-done and upgrade-cordoned annotations and
	// remove the draining label.
	Finish ActionKind = "Finish"
)

// Action is one change to one Node.
type Action struct {
	Kind    ActionKind
	Node    string
	Release string   // SetTarget only
	Pods    []string // Evict only: namespace/name
}

// Pod is a pod the drain moves off its node: not a DaemonSet, static or
// finished pod, nor the operator itself.
type Pod struct {
	Namespace, Name string
	Created         time.Time
	Terminating     bool
	// NoController: no ownerReference is its controller, so nothing
	// recreates it once evicted. The drain leaves it to the admin, as
	// `kubectl drain` without --force does (k4all#99).
	NoController bool
}

func (p Pod) String() string { return p.Namespace + "/" + p.Name }

// PDB is a PodDisruptionBudget with fewer healthy pods than it wants.
type PDB struct {
	Namespace, Name  string
	Healthy, Desired int32
}

// Blocker is a pod a drain cannot move: its PodDisruptionBudget allows no
// disruption although every pod it expects is healthy, so waiting changes
// nothing (the primary of a single-instance CloudNativePG cluster).
type Blocker struct {
	PDB, Pod string // namespace/name
}

// Input is the state Decide works from.
type Input struct {
	Spec     k4allv1alpha1.ClusterUpgradeSpec
	Previous k4allv1alpha1.ClusterUpgradeStatus
	Nodes    []Node
	// The pods on the nodes the operator cordoned.
	Pods map[string][]Pod
	// PodDisruptionBudgets short of healthy pods, sorted.
	UnhealthyPDBs []PDB
	// Blockers holds, for each node, the pods on it that its drain could
	// never evict, sorted.
	Blockers map[string][]Blocker
	// BarePods holds, for each node not cordoned by the operator, the pods
	// on it the drain would move that declare no controller
	// (namespace/name, sorted): a drain would delete them for good.
	BarePods map[string][]string
	// Graph is nil when it could not be read (GraphErr).
	Graph    *upgradegraph.Graph
	GraphErr error
	GraphURL string
	Now      time.Time
	// Retry asks again for the nodes that failed: the retry annotation.
	Retry bool
	// Generation of the ClusterUpgrade, for the conditions.
	Generation int64
}

var roleOrder = map[string]int{"bootstrap": 0, "control": 1, "worker": 2}

// Decide returns the ClusterUpgrade status and the actions to run now. It
// moves at most one node: the one the operator cordoned, or else the next
// one behind the current hop.
func Decide(in Input) (k4allv1alpha1.ClusterUpgradeStatus, []Action) {
	status, actions, reason := decide(in)
	setConditions(in, &status, reason)
	return status, actions
}

// decide returns the status, the actions and, for a Failed upgrade with no
// failed node, the reason of the Failed condition.
func decide(in Input) (k4allv1alpha1.ClusterUpgradeStatus, []Action, string) {
	nodes := sortedNodes(in.Nodes)
	status := observe(in, nodes)

	inFlight := -1
	for i, n := range nodes {
		if n.Annotations[k4allv1alpha1.NodeUpgradeCordonedAnnotation] != "" {
			inFlight = i
		}
	}

	hop, phase, message, reason := plan(in, &status)
	if hop != "" {
		for i := range status.Nodes {
			if older(status.Nodes[i].Release, hop) {
				status.Nodes[i].Phase = k4allv1alpha1.NodeUpgradePending
			}
		}
	}

	status.Phase, status.Message = phase, message
	if inFlight >= 0 {
		return status, moveInFlight(in, nodes[inFlight], &status, inFlight, hop), reason
	}
	if hop == "" {
		return status, nil, reason
	}
	if in.Spec.Paused {
		status.Phase = k4allv1alpha1.ClusterUpgradePaused
		return status, nil, ""
	}
	if wait, i := waitFor(nodes); wait != "" {
		status.Message = hop + ": " + wait
		if !nodes[i].Ready {
			status.Nodes[i].Reason = k4allv1alpha1.NodeReasonNotReady
		}
		return status, nil, ""
	}
	if wait := waitForPDBs(in, status); wait != "" {
		status.Message = hop + ": " + wait
		return status, nil, ""
	}
	if wait := blockedNext(in, &status); wait != "" {
		status.Message = hop + ": " + wait
		return status, nil, ""
	}
	return status, cordonNext(in, &status, hop), ""
}

// blockedNext says why the next node is not cordoned: pods on it that its
// drain could never evict (k4all#87), or would delete for good because they
// declare no controller (k4all#99). The upgrade waits, without a cordon,
// until the admin lets them go; changing a budget or deleting a pod is the
// admin's call. A single node is not drained, so nothing blocks it.
func blockedNext(in Input, status *k4allv1alpha1.ClusterUpgradeStatus) string {
	i := nextPending(*status)
	if i < 0 || len(in.Nodes) <= 1 {
		return ""
	}
	n := &status.Nodes[i]
	var why []string
	if blockers := in.Blockers[n.Name]; len(blockers) > 0 {
		named := make([]string, 0, 4)
		for j, b := range blockers {
			if j == 3 {
				named = append(named, fmt.Sprintf("and %d more", len(blockers)-3))
				break
			}
			named = append(named, fmt.Sprintf("PDB %s allows no disruption of pod %s", b.PDB, b.Pod))
		}
		why = append(why, "cannot be drained: "+strings.Join(named, "; ")+
			" (a single-instance CloudNativePG cluster? see docs/upgrades.md, Drains that never end)")
	}
	if bare := in.BarePods[n.Name]; len(bare) > 0 {
		why = append(why, "runs pods that declare no controller: "+nameSome(bare)+
			" (a drain would delete them, and nothing recreates them: delete them, or recreate them under a"+
			" controller; see docs/upgrades.md, Drains that never end)")
	}
	if len(why) == 0 {
		return ""
	}
	n.Message = strings.Join(why, "; ") + "; the node is not cordoned"
	return n.Name + " " + n.Message
}

// nameSome names up to three of names and counts the rest.
func nameSome(names []string) string {
	if len(names) <= 3 {
		return strings.Join(names, ", ")
	}
	return fmt.Sprintf("%s, and %d more", strings.Join(names[:3], ", "), len(names)-3)
}

// waitForPDBs names the PodDisruptionBudget the next node waits for. After a
// node finishes its hop its pods start again elsewhere; until every budget
// has its healthy pods back, the next drain would be refused. The wait ends
// settleMinutes after the last node finished.
func waitForPDBs(in Input, status k4allv1alpha1.ClusterUpgradeStatus) string {
	if len(in.UnhealthyPDBs) == 0 {
		return ""
	}
	var last time.Time
	for _, n := range status.Nodes {
		if n.CompletedAt != nil && n.CompletedAt.After(last) {
			last = n.CompletedAt.Time
		}
	}
	if last.IsZero() || in.Now.Sub(last) >= settleTime(in.Spec) {
		return ""
	}
	p := in.UnhealthyPDBs[0]
	return fmt.Sprintf("waiting for PDB %s/%s: %d/%d healthy", p.Namespace, p.Name, p.Healthy, p.Desired)
}

// settleTime is spec.drain.settleMinutes, 10 when unset.
func settleTime(spec k4allv1alpha1.ClusterUpgradeSpec) time.Duration {
	if spec.Drain.SettleMinutes != nil {
		return time.Duration(*spec.Drain.SettleMinutes) * time.Minute
	}
	return 10 * time.Minute
}

// setConditions derives the conditions from the phase, keeping the
// transition times of the previous ones.
func setConditions(in Input, status *k4allv1alpha1.ClusterUpgradeStatus, reason string) {
	status.Conditions = append([]metav1.Condition(nil), in.Previous.Conditions...)
	set := func(typ string, on bool, reason, message string) {
		c := metav1.Condition{Type: typ, Status: metav1.ConditionFalse, Reason: reason, Message: message,
			ObservedGeneration: in.Generation, LastTransitionTime: metav1.NewTime(in.Now)}
		if on {
			c.Status = metav1.ConditionTrue
		}
		meta.SetStatusCondition(&status.Conditions, c)
	}
	if in.Graph != nil {
		set(k4allv1alpha1.ConditionGraphAvailable, true, "Read", in.GraphURL)
	} else {
		set(k4allv1alpha1.ConditionGraphAvailable, false, "ReadFailed", fmt.Sprint(in.GraphErr))
	}
	phase := string(status.Phase)
	failed := phase
	if status.Phase == k4allv1alpha1.ClusterUpgradeFailed {
		failed = reason
		for _, n := range status.Nodes {
			if n.Phase == k4allv1alpha1.NodeUpgradeFailed && n.Reason != "" {
				failed = string(n.Reason)
			}
		}
	}
	if failed == "" { // a Condition reason may not be empty
		failed = string(k4allv1alpha1.ClusterUpgradeFailed)
	}
	set(k4allv1alpha1.ConditionProgressing, status.Phase == k4allv1alpha1.ClusterUpgradeProgressing, phase, status.Message)
	set(k4allv1alpha1.ConditionSucceeded, status.Phase == k4allv1alpha1.ClusterUpgradeSucceeded, phase, status.Message)
	set(k4allv1alpha1.ConditionFailed, status.Phase == k4allv1alpha1.ClusterUpgradeFailed, failed, status.Message)
}

// observe fills in what the nodes and the graph say, before any decision.
func observe(in Input, nodes []Node) k4allv1alpha1.ClusterUpgradeStatus {
	status := k4allv1alpha1.ClusterUpgradeStatus{
		Phase:    k4allv1alpha1.ClusterUpgradeIdle,
		GraphURL: in.GraphURL,
	}
	for _, n := range nodes {
		release := n.Annotations[k4allv1alpha1.NodeReleaseAnnotation]
		prev := previous(in, n.Name)
		status.Nodes = append(status.Nodes, k4allv1alpha1.NodeUpgradeStatus{
			Name:        n.Name,
			Role:        n.Annotations[k4allv1alpha1.NodeTypeAnnotation],
			Release:     release,
			Phase:       k4allv1alpha1.NodeUpgradeDone,
			HostPhase:   n.Annotations[k4allv1alpha1.NodeUpgradePhaseAnnotation],
			StartedAt:   prev.StartedAt,
			CompletedAt: prev.CompletedAt,
		})
		if release != "" && (status.CurrentRelease == "" || older(release, status.CurrentRelease)) {
			status.CurrentRelease = release
		}
	}
	if in.Graph != nil && status.CurrentRelease != "" {
		if r, ok := in.Graph.Find(status.CurrentRelease); ok {
			info := releaseInfo(r)
			status.CurrentReleaseInfo = &info
		}
		for _, r := range in.Graph.Newer(status.CurrentRelease) {
			info := releaseInfo(r)
			if hops, err := in.Graph.Plan(status.CurrentRelease, r.Version); err == nil {
				for _, h := range hops {
					info.Path = append(info.Path, h.Version)
				}
			}
			status.AvailableReleases = append(status.AvailableReleases, info)
		}
	}
	return status
}

func releaseInfo(r upgradegraph.Release) k4allv1alpha1.ReleaseInfo {
	return k4allv1alpha1.ReleaseInfo{
		Version: r.Version, Kind: r.Kind, MinFrom: r.MinFrom, Kubernetes: r.Kubernetes, Crio: r.Crio,
		Fedora: int32(r.Fedora), Notes: r.Notes,
	}
}

// plan sets the path towards spec.targetRelease and returns its first hop,
// or "" with the phase and message of an upgrade that has no hop to run, and
// the reason when that phase is Failed.
func plan(in Input, status *k4allv1alpha1.ClusterUpgradeStatus) (string, k4allv1alpha1.ClusterUpgradePhase, string, string) {
	target := in.Spec.TargetRelease
	noGraph := fmt.Sprintf("cannot read the upgrade graph: %v", in.GraphErr)
	switch {
	case target == "" && in.Graph == nil:
		return "", k4allv1alpha1.ClusterUpgradeIdle, noGraph, ""
	case target == "":
		return "", k4allv1alpha1.ClusterUpgradeIdle, "", ""
	case missingAnnotation(in.Nodes) != "":
		return "", k4allv1alpha1.ClusterUpgradeFailed, missingAnnotation(in.Nodes), "NodeNotReported"
	case in.Graph == nil:
		return "", k4allv1alpha1.ClusterUpgradeProgressing, noGraph, ""
	case older(target, status.CurrentRelease):
		return "", k4allv1alpha1.ClusterUpgradeFailed,
			fmt.Sprintf("targetRelease %s is older than the cluster's release %s", target, status.CurrentRelease), "Downgrade"
	}
	hops, err := in.Graph.Plan(status.CurrentRelease, target)
	if err != nil {
		return "", k4allv1alpha1.ClusterUpgradeFailed, err.Error(), "NoPath"
	}
	status.Path = doneHops(in, status.CurrentRelease, target)
	if len(hops) == 0 {
		return "", k4allv1alpha1.ClusterUpgradeSucceeded, "", ""
	}
	for i, r := range hops {
		hop := k4allv1alpha1.Hop{Release: r.Version, Kind: r.Kind, Phase: k4allv1alpha1.HopPending}
		if i == 0 {
			hop.Phase = k4allv1alpha1.HopInProgress
			hop.StartedAt = previousHop(in, r.Version).StartedAt
			if hop.StartedAt == nil {
				started := metav1.NewTime(in.Now)
				hop.StartedAt = &started
			}
		}
		status.Path = append(status.Path, hop)
	}
	status.CurrentHop = hops[0].Version
	return hops[0].Version, k4allv1alpha1.ClusterUpgradeProgressing, "", ""
}

// previousHop is the hop of the previous path with that release.
func previousHop(in Input, release string) k4allv1alpha1.Hop {
	for _, h := range in.Previous.Path {
		if h.Release == release {
			return h
		}
	}
	return k4allv1alpha1.Hop{Release: release}
}

// missingAnnotation names the first node k4all-upgrade has not described.
func missingAnnotation(nodes []Node) string {
	for _, n := range sortedNodes(nodes) {
		for _, key := range []string{k4allv1alpha1.NodeReleaseAnnotation, k4allv1alpha1.NodeTypeAnnotation} {
			if n.Annotations[key] == "" {
				return fmt.Sprintf("%s has no %s annotation: is k4all-upgrade v2.4.0 or later running there?", n.Name, key)
			}
		}
	}
	return ""
}

// moveInFlight moves on the node the operator cordoned. Without a hop to run
// (target emptied, reached or unreachable) a node still draining is let go,
// unless the graph is only unreadable for now, and a node in the middle of
// its hop finishes it.
func moveInFlight(in Input, node Node, status *k4allv1alpha1.ClusterUpgradeStatus, i int, hop string) []Action {
	n := &status.Nodes[i]
	if hop == "" && node.Annotations[k4allv1alpha1.NodeUpgradeTargetAnnotation] == "" {
		if in.Graph == nil && in.Spec.TargetRelease != "" {
			n.Phase = k4allv1alpha1.NodeUpgradeDraining
			return nil
		}
		n.Phase = k4allv1alpha1.NodeUpgradeDone
		return []Action{{Kind: Finish, Node: n.Name}}
	}

	actions := step(in, node, n, hop)
	status.Phase = k4allv1alpha1.ClusterUpgradeProgressing
	switch {
	case n.Phase == k4allv1alpha1.NodeUpgradeFailed && n.HostPhase != "":
		status.Phase, status.Message = k4allv1alpha1.ClusterUpgradeFailed, n.Name+": "+n.HostPhase+": "+n.Message
	case n.Phase == k4allv1alpha1.NodeUpgradeFailed:
		status.Phase, status.Message = k4allv1alpha1.ClusterUpgradeFailed, n.Name+": "+n.Message
	case hop == "" && n.Phase != k4allv1alpha1.NodeUpgradeDone:
		status.Message = "stopping after " + n.Name + " finishes its hop"
	case n.Phase == k4allv1alpha1.NodeUpgradeDraining:
		status.Message = fmt.Sprintf("%s: draining %s, %s", hop, n.Name, n.Message)
	case n.Phase == k4allv1alpha1.NodeUpgradeUpgrading:
		status.Message = fmt.Sprintf("%s: %s is upgrading %s", askedHop(node, hop), n.Name, n.HostPhase)
	}
	return actions
}

// askedHop is the hop the node was asked for, or the current one.
func askedHop(node Node, hop string) string {
	if asked := node.Annotations[k4allv1alpha1.NodeUpgradeTargetAnnotation]; asked != "" {
		return asked
	}
	return hop
}

// waitFor says what keeps the next node from starting, and which node: one
// not Ready, or one running a hop of its own.
func waitFor(nodes []Node) (string, int) {
	for i, n := range nodes {
		if !n.Ready {
			return "waiting for " + n.Name + " to be Ready", i
		}
		if hop := n.Annotations[upgradeInProgressAnnotation]; hop != "" {
			return fmt.Sprintf("waiting for the hop %s runs on its own (%s)", n.Name, hop), i
		}
	}
	return "", -1
}

// upgradeInProgressAnnotation is the pause gate k4all-upgrade sets during any hop.
const upgradeInProgressAnnotation = "k4all.magesgate.com/upgrade-in-progress"

// DrainingLabel is on a Node from its Cordon to its Uncordon or Finish. The
// operator's pod tolerates the cordon, which a single node needs, and prefers
// the nodes without this label (config/manager/manager.yaml): a drain that
// evicts it does not put it back on the node about to reboot (k4all#86).
// k4all-upgrade sets it too when it drains a node itself.
const DrainingLabel = "k4all.magesgate.com/draining"

// nextPending is the index of the first node behind the current hop, -1
// when there is none.
func nextPending(status k4allv1alpha1.ClusterUpgradeStatus) int {
	for i := range status.Nodes {
		if status.Nodes[i].Phase == k4allv1alpha1.NodeUpgradePending {
			return i
		}
	}
	return -1
}

// cordonNext starts the first node behind hop.
func cordonNext(in Input, status *k4allv1alpha1.ClusterUpgradeStatus, hop string) []Action {
	i := nextPending(*status)
	if i < 0 {
		return nil
	}
	n := &status.Nodes[i]
	n.Phase = k4allv1alpha1.NodeUpgradeDraining
	started := metav1.NewTime(in.Now)
	n.DrainStartedAt, n.StartedAt, n.CompletedAt = &started, &started, nil
	actions := []Action{{Kind: Cordon, Node: n.Name}}
	if len(in.Nodes) == 1 {
		n.Phase = k4allv1alpha1.NodeUpgradeUpgrading
		actions = append(actions, Action{Kind: SetTarget, Node: n.Name, Release: hop})
	}
	return actions
}

// step moves on the node the operator cordoned: drain it, ask for the hop,
// uncordon it once k4all-upgrade is past the reboot, then finish it.
func step(in Input, node Node, n *k4allv1alpha1.NodeUpgradeStatus, hop string) []Action {
	target := node.Annotations[k4allv1alpha1.NodeUpgradeTargetAnnotation]
	if target == "" {
		return drain(in, node, n, hop)
	}
	n.DrainStartedAt = previous(in, n.Name).DrainStartedAt
	if node.Annotations[k4allv1alpha1.NodeUpgradeDoneAnnotation] == target && !older(n.Release, target) {
		n.Phase = k4allv1alpha1.NodeUpgradeDone
		completed := metav1.NewTime(in.Now)
		n.CompletedAt = &completed
		return []Action{{Kind: Finish, Node: n.Name}}
	}
	n.Phase = k4allv1alpha1.NodeUpgradeUpgrading
	if prev := previous(in, n.Name).Message; strings.HasPrefix(prev, backMessagePrefix) {
		n.Message = prev
	}
	switch n.HostPhase {
	case hostHold:
		n.Phase = k4allv1alpha1.NodeUpgradeFailed
		n.Message = hostMessage(node, "k4all-upgrade stopped the hop")
		n.Reason = k4allv1alpha1.NodeReasonHostHold
		switch r := k4allv1alpha1.NodeUpgradeReason(node.Annotations[k4allv1alpha1.NodeUpgradeReasonAnnotation]); r {
		case k4allv1alpha1.NodeReasonTargetRejected, k4allv1alpha1.NodeReasonRollbackRebooted:
			n.Reason = r
		}
	case hostSync, hostCommit:
		// Past the reboot: the node is Ready on the new release, and the
		// SYNC may start pods that only fit on it.
		if node.Annotations[k4allv1alpha1.NodeUpgradeCordonedAnnotation] == k4allv1alpha1.NodeCordonedByOperator {
			return []Action{{Kind: Uncordon, Node: n.Name}}
		}
	}
	return nil
}

// Phases of k4all-upgrade: the ones that stop a hop, and the ones after the
// node rebooted healthy on the new release.
const (
	hostHold      = "HOLD"
	hostAbandoned = "ABANDONED"
	hostSync      = "SYNC"
	hostCommit    = "COMMIT"
)

func hostMessage(node Node, fallback string) string {
	if msg := node.Annotations[k4allv1alpha1.NodeUpgradeMessageAnnotation]; msg != "" {
		return msg
	}
	return fallback
}

// doneHops keeps the hops of the previous path the cluster has finished,
// as long as the path still leads to target.
func doneHops(in Input, current, target string) []k4allv1alpha1.Hop {
	path := in.Previous.Path
	if len(path) == 0 || path[len(path)-1].Release != target {
		return nil
	}
	var done []k4allv1alpha1.Hop
	for _, h := range path {
		if !older(current, h.Release) {
			if h.Phase != k4allv1alpha1.HopDone || h.CompletedAt == nil {
				completed := metav1.NewTime(in.Now)
				h.CompletedAt = &completed
			}
			h.Phase = k4allv1alpha1.HopDone
			done = append(done, h)
		}
	}
	return done
}

// drain moves the cordoned node n on: evict its pods, then ask for the hop.
func drain(in Input, node Node, n *k4allv1alpha1.NodeUpgradeStatus, hop string) []Action {
	prev := previous(in, n.Name)
	n.DrainStartedAt = prev.DrainStartedAt
	if !in.Retry {
		// The node ran a hop and k4all-upgrade gave it up, or the drain failed.
		switch {
		case node.Annotations[k4allv1alpha1.NodeUpgradePhaseAnnotation] == hostAbandoned:
			n.Phase, n.Message = k4allv1alpha1.NodeUpgradeFailed, hostMessage(node, "the hop was abandoned on the node")
			n.Reason = k4allv1alpha1.NodeReasonAbandoned
			return nil
		case prev.Phase == k4allv1alpha1.NodeUpgradeUpgrading:
			n.Phase, n.Message = k4allv1alpha1.NodeUpgradeFailed, "upgrade-target was removed before the hop was done"
			n.Reason = k4allv1alpha1.NodeReasonAbandoned
			return nil
		case prev.Phase == k4allv1alpha1.NodeUpgradeFailed:
			n.Phase, n.Message, n.Reason = k4allv1alpha1.NodeUpgradeFailed, prev.Message, prev.Reason
			return nil
		}
	} else {
		n.DrainStartedAt = nil
	}
	if n.DrainStartedAt == nil {
		started := metav1.NewTime(in.Now)
		n.DrainStartedAt = &started
	}
	left, back := splitPods(in.Pods[n.Name], n.DrainStartedAt.Time)
	if len(left) > 0 && len(in.Nodes) > 1 {
		timeout := time.Duration(drainTimeoutMinutes(in.Spec)) * time.Minute
		switch {
		case in.Now.Sub(n.DrainStartedAt.Time) < timeout:
			n.Phase = k4allv1alpha1.NodeUpgradeDraining
			n.Message = fmt.Sprintf("%d pods left: %s", len(left), podList(left))
			var evict, bare []string
			for _, p := range left {
				switch {
				case p.NoController:
					bare = append(bare, p.String())
				case !p.Terminating:
					evict = append(evict, p.String())
				}
			}
			if len(bare) > 0 {
				n.Message += "; not evicted, they declare no controller: " + nameSome(bare) +
					" (delete them, or recreate them under a controller)"
			}
			return []Action{{Kind: Evict, Node: n.Name, Pods: evict}}
		case !in.Spec.Drain.ContinueOnTimeout:
			n.Phase = k4allv1alpha1.NodeUpgradeFailed
			n.Message = fmt.Sprintf("drain timed out after %d minutes, %d pods left: %s",
				drainTimeoutMinutes(in.Spec), len(left), podList(left))
			n.Reason = k4allv1alpha1.NodeReasonDrainTimeout
			return nil
		}
	}
	n.Phase = k4allv1alpha1.NodeUpgradeUpgrading
	if len(back) > 0 {
		n.Message = backMessagePrefix + podList(back)
	}
	return []Action{{Kind: SetTarget, Node: n.Name, Release: hop}}
}

// NoteRefusedEvictions adds to the status of a node being drained, and to the
// upgrade's, why its evictions were refused, e.g. by a PodDisruptionBudget.
func NoteRefusedEvictions(status *k4allv1alpha1.ClusterUpgradeStatus, node string, refused []string) {
	if len(refused) == 0 {
		return
	}
	why := strings.Join(refused, "; ")
	for i := range status.Nodes {
		if status.Nodes[i].Name == node {
			status.Nodes[i].Message += "; " + why
		}
	}
	status.Message += "; " + why
}

// backMessagePrefix starts the node message that names the pods put back on
// it after the drain started; it stays for the rest of the hop.
const backMessagePrefix = "back on the node after the drain started: "

// splitPods sorts the pods of a node being drained: the ones it had when the
// drain started, which the drain moves, and the ones put back on it since,
// which tolerate the cordon or were bound to the node. Like kubectl drain,
// the drain neither evicts nor waits for the second ones.
func splitPods(pods []Pod, drainStarted time.Time) (left, back []Pod) {
	for _, p := range pods {
		if p.Created.After(drainStarted) {
			back = append(back, p)
		} else {
			left = append(left, p)
		}
	}
	return left, back
}

// podList names up to three pods.
func podList(pods []Pod) string {
	names := make([]string, 0, 3)
	for i, p := range pods {
		if i == 3 {
			names = append(names, fmt.Sprintf("and %d more", len(pods)-3))
			break
		}
		names = append(names, p.String())
	}
	return strings.Join(names, ", ")
}

// drainTimeoutMinutes is spec.drain.timeoutMinutes, 30 when unset.
func drainTimeoutMinutes(spec k4allv1alpha1.ClusterUpgradeSpec) int32 {
	if spec.Drain.TimeoutMinutes > 0 {
		return spec.Drain.TimeoutMinutes
	}
	return 30
}

// previous is the node's entry in the last status written.
func previous(in Input, name string) k4allv1alpha1.NodeUpgradeStatus {
	for _, n := range in.Previous.Nodes {
		if n.Name == name {
			return n
		}
	}
	return k4allv1alpha1.NodeUpgradeStatus{Name: name}
}

// sortedNodes orders nodes as they are upgraded: bootstrap, control, worker,
// then by name.
func sortedNodes(nodes []Node) []Node {
	out := append([]Node(nil), nodes...)
	rank := func(n Node) int {
		if r, ok := roleOrder[n.Annotations[k4allv1alpha1.NodeTypeAnnotation]]; ok {
			return r
		}
		return len(roleOrder)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if rank(out[i]) != rank(out[j]) {
			return rank(out[i]) < rank(out[j])
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// older tells whether release a comes before b. Unparsable versions sort first,
// so a node with a broken annotation holds the cluster back.
func older(a, b string) bool {
	c, err := upgradegraph.Compare(a, b)
	return err != nil || c < 0
}
