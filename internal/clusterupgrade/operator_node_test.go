package clusterupgrade

import (
	"testing"
	"time"

	k4allv1alpha1 "github.com/gpillon/k4all-operator/api/v1alpha1"
)

// opNode runs the operator in most cases here.
const opNode = "cp-0"

// The node that runs the operator goes last among the nodes of its type:
// the operator moves once, onto a node already done (k4all#97). The type
// order stays: the bootstrap node first, the workers after the control
// planes (kubeadm, and k4all-upgrade, refuse any other).
func TestDecide_OperatorsNodeGoesLastAmongItsType(t *testing.T) {
	nodes := []Node{node("w-0", "worker", "v2.5.0"), node("w-1", "worker", "v2.5.0"), node(opNode, "bootstrap", "v2.5.0")}
	status, _ := Decide(Input{Spec: target("v2.7.0"), Nodes: nodes, Graph: graph(t), Now: now, SelfNode: "w-0"})

	order := make([]string, 0, len(status.Nodes))
	for _, n := range status.Nodes {
		order = append(order, n.Name)
	}
	if want := "cp-0 w-1 w-0"; join(order) != want {
		t.Errorf("order = %s, want %s", join(order), want)
	}

	status, _ = Decide(Input{Spec: target("v2.7.0"), Nodes: nodes, Graph: graph(t), Now: now, SelfNode: opNode})
	if status.Nodes[0].Name != opNode {
		t.Errorf("first node = %s, want the bootstrap node first even with the operator on it", status.Nodes[0].Name)
	}
}

// selfDraining is cp-0 drained of every pod but the operator, which runs there.
func selfDraining(t *testing.T) Input {
	in := draining(t, now.Add(-time.Minute), 0)
	in.SelfNode = opNode
	in.Self = &Self{Node: opNode, Pod: Pod{Namespace: "k4all-operator-system", Name: "k4all-operator-abc", Created: now.Add(-time.Hour)}}
	return in
}

// k4all#97: once the node is drained but for the operator, the operator
// evicts itself, so the leader moves before the reboot.
func TestDecide_EvictsTheOperatorFromItsDrainedNode(t *testing.T) {
	status, actions := Decide(selfDraining(t))

	expectActions(t, actions, Action{Kind: EvictSelf, Node: opNode, Pods: []string{"k4all-operator-system/k4all-operator-abc"}})
	n := status.Nodes[0]
	if n.Phase != k4allv1alpha1.NodeUpgradeDraining || n.Message != "moving the k4all-operator off the node before its hop" {
		t.Errorf("cp-0 = %s %q, want Draining while the operator moves", n.Phase, n.Message)
	}
}

func TestDecide_AsksForTheHopOnceTheOperatorTriedToLeave(t *testing.T) {
	in := selfDraining(t)
	in.SelfEvictTried = true

	_, actions := Decide(in)
	expectActions(t, actions, Action{Kind: SetTarget, Node: opNode, Release: firstHop})
}

// A pod put back on the node after its drain started is not evicted again:
// it goes down with the reboot, as before k4all#97.
func TestDecide_LeavesAnOperatorPodPutBackOnTheNode(t *testing.T) {
	in := selfDraining(t)
	in.Self.Created = now

	_, actions := Decide(in)
	expectActions(t, actions, Action{Kind: SetTarget, Node: opNode, Release: firstHop})
}

func TestDecide_OperatorStaysWithoutAnotherNodeToGoTo(t *testing.T) {
	in := selfDraining(t)
	for i := range in.Nodes {
		if in.Nodes[i].Name != opNode {
			in.Nodes[i].Unschedulable = true
		}
	}

	_, actions := Decide(in)
	expectActions(t, actions, Action{Kind: SetTarget, Node: opNode, Release: firstHop})
}

func TestDecide_SingleNodeKeepsTheOperator(t *testing.T) {
	in := Input{
		Spec:     target("v2.7.0"),
		Nodes:    []Node{node(opNode, "bootstrap", "v2.5.0")},
		Graph:    graph(t),
		Now:      now,
		SelfNode: opNode,
		Self:     &Self{Node: opNode, Pod: Pod{Namespace: "k4all-operator-system", Name: "k4all-operator-abc", Created: now.Add(-time.Hour)}},
	}
	_, actions := Decide(in)
	expectActions(t, actions, Action{Kind: Cordon, Node: opNode}, Action{Kind: SetTarget, Node: opNode, Release: firstHop})
}

func join(s []string) string {
	out := ""
	for i, v := range s {
		if i > 0 {
			out += " "
		}
		out += v
	}
	return out
}
