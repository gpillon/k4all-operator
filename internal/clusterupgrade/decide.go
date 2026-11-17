// Package clusterupgrade decides the next step of a ClusterUpgrade from the
// cluster's nodes and the upgrade graph. It does no I/O: the controller reads
// the Nodes, runs the returned actions and writes the returned status.
package clusterupgrade

import (
	"fmt"
	"sort"
	"time"

	k4allv1alpha1 "github.com/gpillon/k4all-operator/api/v1alpha1"
	"github.com/gpillon/k4all-operator/internal/upgradegraph"
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
	// Cordon the node and mark it upgrade-cordoned.
	Cordon ActionKind = "Cordon"
	// Evict the pods the drain moves off the node.
	Evict ActionKind = "Evict"
	// Write upgrade-target: Release, the hop k4all-upgrade runs.
	SetTarget ActionKind = "SetTarget"
	// Uncordon the node if the operator cordoned it, and clear the
	// upgrade-target, upgrade-done and upgrade-cordoned annotations.
	Finish ActionKind = "Finish"
)

// Action is one change to one Node.
type Action struct {
	Kind    ActionKind
	Node    string
	Release string // SetTarget only
}

// Input is the state Decide works from.
type Input struct {
	Spec     k4allv1alpha1.ClusterUpgradeSpec
	Previous k4allv1alpha1.ClusterUpgradeStatus
	Nodes    []Node
	// Pods still to evict, for the nodes the operator cordoned.
	PodsLeft map[string]int
	// Graph is nil when it could not be read (GraphErr).
	Graph    *upgradegraph.Graph
	GraphErr error
	GraphURL string
	Now      time.Time
	// Retry asks again for the nodes that failed: the retry annotation.
	Retry bool
}

var roleOrder = map[string]int{"bootstrap": 0, "control": 1, "worker": 2}

// Decide returns the ClusterUpgrade status and the actions to run now. It
// moves at most one node: the one the operator cordoned, or else the next
// one behind the current hop.
func Decide(in Input) (k4allv1alpha1.ClusterUpgradeStatus, []Action) {
	nodes := sortedNodes(in.Nodes)
	status := observe(in, nodes)

	inFlight := -1
	for i, n := range nodes {
		if n.Annotations[k4allv1alpha1.NodeUpgradeCordonedAnnotation] != "" {
			inFlight = i
		}
	}

	hop, phase, message := plan(in, &status)
	if hop != "" {
		for i := range status.Nodes {
			if older(status.Nodes[i].Release, hop) {
				status.Nodes[i].Phase = k4allv1alpha1.NodeUpgradePending
			}
		}
	}

	status.Phase, status.Message = phase, message
	if inFlight >= 0 {
		return status, moveInFlight(in, nodes[inFlight], &status, inFlight, hop)
	}
	if hop == "" {
		return status, nil
	}
	if in.Spec.Paused {
		status.Phase = k4allv1alpha1.ClusterUpgradePaused
		return status, nil
	}
	if wait := waitFor(nodes); wait != "" {
		status.Message = hop + ": " + wait
		return status, nil
	}
	return status, cordonNext(in, &status, hop)
}

// observe fills in what the nodes and the graph say, before any decision.
func observe(in Input, nodes []Node) k4allv1alpha1.ClusterUpgradeStatus {
	status := k4allv1alpha1.ClusterUpgradeStatus{
		Phase:    k4allv1alpha1.ClusterUpgradeIdle,
		GraphURL: in.GraphURL,
	}
	for _, n := range nodes {
		release := n.Annotations[k4allv1alpha1.NodeReleaseAnnotation]
		status.Nodes = append(status.Nodes, k4allv1alpha1.NodeUpgradeStatus{
			Name:    n.Name,
			Role:    n.Annotations[k4allv1alpha1.NodeTypeAnnotation],
			Release: release,
			Phase:   k4allv1alpha1.NodeUpgradeDone,
		})
		if release != "" && (status.CurrentRelease == "" || older(release, status.CurrentRelease)) {
			status.CurrentRelease = release
		}
	}
	if in.Graph != nil && status.CurrentRelease != "" {
		for _, r := range in.Graph.Newer(status.CurrentRelease) {
			status.AvailableReleases = append(status.AvailableReleases, k4allv1alpha1.ReleaseInfo{
				Version: r.Version, Kind: r.Kind, Kubernetes: r.Kubernetes, Fedora: int32(r.Fedora), Notes: r.Notes,
			})
		}
	}
	return status
}

// plan sets the path towards spec.targetRelease and returns its first hop,
// or "" with the phase and message of an upgrade that has no hop to run.
func plan(in Input, status *k4allv1alpha1.ClusterUpgradeStatus) (string, k4allv1alpha1.ClusterUpgradePhase, string) {
	target := in.Spec.TargetRelease
	switch {
	case target == "":
		return "", k4allv1alpha1.ClusterUpgradeIdle, ""
	case missingAnnotation(in.Nodes) != "":
		return "", k4allv1alpha1.ClusterUpgradeFailed, missingAnnotation(in.Nodes)
	case in.Graph == nil:
		return "", k4allv1alpha1.ClusterUpgradeProgressing, fmt.Sprintf("cannot read the upgrade graph: %v", in.GraphErr)
	case older(target, status.CurrentRelease):
		return "", k4allv1alpha1.ClusterUpgradeFailed,
			fmt.Sprintf("targetRelease %s is older than the cluster's release %s", target, status.CurrentRelease)
	}
	hops, err := in.Graph.Plan(status.CurrentRelease, target)
	if err != nil {
		return "", k4allv1alpha1.ClusterUpgradeFailed, err.Error()
	}
	status.Path = doneHops(in.Previous.Path, status.CurrentRelease, target)
	if len(hops) == 0 {
		return "", k4allv1alpha1.ClusterUpgradeSucceeded, ""
	}
	for i, r := range hops {
		phase := k4allv1alpha1.HopPending
		if i == 0 {
			phase = k4allv1alpha1.HopInProgress
		}
		status.Path = append(status.Path, k4allv1alpha1.Hop{Release: r.Version, Kind: r.Kind, Phase: phase})
	}
	status.CurrentHop = hops[0].Version
	return hops[0].Version, k4allv1alpha1.ClusterUpgradeProgressing, ""
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

// waitFor says what keeps the next node from starting: a node not Ready, or
// a hop k4all-upgrade runs on its own.
func waitFor(nodes []Node) string {
	for _, n := range nodes {
		if !n.Ready {
			return "waiting for " + n.Name + " to be Ready"
		}
		if hop := n.Annotations[upgradeInProgressAnnotation]; hop != "" {
			return fmt.Sprintf("waiting for the hop %s runs on its own (%s)", n.Name, hop)
		}
	}
	return ""
}

// upgradeInProgressAnnotation is the pause gate k4all-upgrade sets during any hop.
const upgradeInProgressAnnotation = "k4all.magesgate.com/upgrade-in-progress"

// cordonNext starts the first node behind hop.
func cordonNext(in Input, status *k4allv1alpha1.ClusterUpgradeStatus, hop string) []Action {
	for i := range status.Nodes {
		n := &status.Nodes[i]
		if n.Phase != k4allv1alpha1.NodeUpgradePending {
			continue
		}
		n.Phase = k4allv1alpha1.NodeUpgradeDraining
		started := metav1.NewTime(in.Now)
		n.DrainStartedAt = &started
		actions := []Action{{Kind: Cordon, Node: n.Name}}
		if len(in.Nodes) == 1 {
			n.Phase = k4allv1alpha1.NodeUpgradeUpgrading
			actions = append(actions, Action{Kind: SetTarget, Node: n.Name, Release: hop})
		}
		return actions
	}
	return nil
}

// step moves on the node the operator cordoned: drain it, ask for the hop,
// wait for k4all-upgrade, then finish it.
func step(in Input, node Node, n *k4allv1alpha1.NodeUpgradeStatus, hop string) []Action {
	target := node.Annotations[k4allv1alpha1.NodeUpgradeTargetAnnotation]
	if target == "" {
		return drain(in, node, n, hop)
	}
	n.DrainStartedAt = previous(in, n.Name).DrainStartedAt
	if node.Annotations[k4allv1alpha1.NodeUpgradeDoneAnnotation] == target && !older(n.Release, target) {
		n.Phase = k4allv1alpha1.NodeUpgradeDone
		return []Action{{Kind: Finish, Node: n.Name}}
	}
	n.Phase = k4allv1alpha1.NodeUpgradeUpgrading
	n.HostPhase = node.Annotations[k4allv1alpha1.NodeUpgradePhaseAnnotation]
	if n.HostPhase == hostHold {
		n.Phase = k4allv1alpha1.NodeUpgradeFailed
		n.Message = hostMessage(node, "k4all-upgrade stopped the hop")
	}
	return nil
}

// Phases of k4all-upgrade that stop a hop.
const (
	hostHold      = "HOLD"
	hostAbandoned = "ABANDONED"
)

func hostMessage(node Node, fallback string) string {
	if msg := node.Annotations[k4allv1alpha1.NodeUpgradeMessageAnnotation]; msg != "" {
		return msg
	}
	return fallback
}

// doneHops keeps the hops of the previous path the cluster has finished,
// as long as the path still leads to target.
func doneHops(path []k4allv1alpha1.Hop, current, target string) []k4allv1alpha1.Hop {
	if len(path) == 0 || path[len(path)-1].Release != target {
		return nil
	}
	var done []k4allv1alpha1.Hop
	for _, h := range path {
		if !older(current, h.Release) {
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
			return nil
		case prev.Phase == k4allv1alpha1.NodeUpgradeUpgrading:
			n.Phase, n.Message = k4allv1alpha1.NodeUpgradeFailed, "upgrade-target was removed before the hop was done"
			return nil
		case prev.Phase == k4allv1alpha1.NodeUpgradeFailed:
			n.Phase, n.Message = k4allv1alpha1.NodeUpgradeFailed, prev.Message
			return nil
		}
	} else {
		n.DrainStartedAt = nil
	}
	if n.DrainStartedAt == nil {
		started := metav1.NewTime(in.Now)
		n.DrainStartedAt = &started
	}
	if left := in.PodsLeft[n.Name]; left > 0 && len(in.Nodes) > 1 {
		timeout := time.Duration(drainTimeoutMinutes(in.Spec)) * time.Minute
		switch {
		case in.Now.Sub(n.DrainStartedAt.Time) < timeout:
			n.Phase = k4allv1alpha1.NodeUpgradeDraining
			n.Message = fmt.Sprintf("%d pods left", left)
			return []Action{{Kind: Evict, Node: n.Name}}
		case !in.Spec.Drain.ContinueOnTimeout:
			n.Phase = k4allv1alpha1.NodeUpgradeFailed
			n.Message = fmt.Sprintf("drain timed out after %d minutes, %d pods left", drainTimeoutMinutes(in.Spec), left)
			return nil
		}
	}
	n.Phase = k4allv1alpha1.NodeUpgradeUpgrading
	return []Action{{Kind: SetTarget, Node: n.Name, Release: hop}}
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
