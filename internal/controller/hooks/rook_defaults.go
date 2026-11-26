package hooks

import (
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"

	k4allv1alpha1 "github.com/gpillon/k4all-operator/api/v1alpha1"
)

// RookSettings is the Rook configuration of a cluster: what k4all creates
// the CephCluster and vm-pool with, from features.rook (rookSettingsFrom).
type RookSettings struct {
	// DeviceFilter selects the OSD devices: a regular expression on device
	// names, e.g. "^sdb$". Empty: every device or partition of every node
	// with nothing on it (Rook's useAllDevices). Rook skips whatever carries
	// a filesystem or an LVM signature, so the OS disk and TopoLVM's volume
	// group are safe; a blank spare disk or partition becomes an OSD.
	DeviceFilter string
	// SmallCluster is what k4all does on fewer than 3 nodes, Ceph's
	// minimum for 3 mons and 3 replicas on separate hosts:
	// k4allv1alpha1.RookSmallClusterRefuse creates no CephCluster (the
	// status says so; it is created once a third node has joined),
	// RookSmallClusterAllow creates it with one replica per node (1 node: a
	// single copy) and a warning in the StorageClass description and the
	// RookReady condition.
	SmallCluster string
	// PoolSize is vm-pool's replica count; 0: from the node count.
	PoolSize int
}

// rookSettingsFrom reads the creation inputs of features.rook: the device
// filter, and the small-cluster policy (empty means Refuse, D5). Without
// features.rook, the defaults: every blank device, nothing below 3 nodes.
func rookSettingsFrom(config k4allv1alpha1.ClusterConfigSpec) RookSettings {
	settings := RookSettings{SmallCluster: k4allv1alpha1.RookSmallClusterRefuse}
	rook := config.Features.Rook
	if rook == nil {
		return settings
	}
	if rook.Storage != nil {
		settings.DeviceFilter = rook.Storage.DeviceFilter
	}
	if rook.SmallCluster == k4allv1alpha1.RookSmallClusterAllow {
		settings.SmallCluster = k4allv1alpha1.RookSmallClusterAllow
	}
	return settings
}

// cephMinNodes is the node count Ceph's defaults need: 3 mons and 3
// replicas, each on its own host.
const cephMinNodes = 3

// rookPlan is what k4all creates the CephCluster and vm-pool with, for the
// nodes the cluster has when it creates them.
type rookPlan struct {
	Mons     int
	Mgrs     int
	PoolSize int
	// Warning says what the plan loses against Ceph's defaults; empty when
	// nothing.
	Warning string
}

// rookRefusal says why k4all does not create the CephCluster yet.
type rookRefusal struct{ reason string }

func (r rookRefusal) Error() string { return r.reason }

// planRook computes the Ceph defaults for a cluster of nodes nodes:
//   - mons: 1 below 3 nodes, 3 up to 4, 5 from 5 (an odd count: an even one
//     adds no quorum);
//   - mgrs: 2 from 2 nodes, so a lost node hands over to the standby in
//     about 30 s instead of waiting for the pod to move;
//   - vm-pool replicas: 3, or one per node below 3 nodes, or settings'
//     PoolSize.
//
// Below 3 nodes Refuse refuses; so does a pool size larger than
// the node count, since every replica needs its own host.
func planRook(nodes int, settings RookSettings) (rookPlan, error) {
	if nodes < 1 {
		return rookPlan{}, rookRefusal{"no node can run Ceph"}
	}
	if nodes < cephMinNodes && settings.SmallCluster != k4allv1alpha1.RookSmallClusterAllow {
		return rookPlan{}, rookRefusal{fmt.Sprintf(
			"Ceph needs %d nodes for 3 monitors and 3 replicas on separate hosts, this cluster has %d: "+
				"k4all creates the CephCluster once it has %d "+
				"(features.rook.smallCluster: Allow creates it now, without redundancy)",
			cephMinNodes, nodes, cephMinNodes)}
	}
	plan := rookPlan{Mons: 1, Mgrs: 1, PoolSize: min(nodes, 3)}
	switch {
	case nodes >= 5:
		plan.Mons = 5
	case nodes >= 3:
		plan.Mons = 3
	}
	if nodes >= 2 {
		plan.Mgrs = 2
	}
	if settings.PoolSize > 0 {
		if settings.PoolSize > nodes {
			return rookPlan{}, rookRefusal{fmt.Sprintf(
				"vm-pool size %d needs %d nodes, one replica per host; this cluster has %d",
				settings.PoolSize, settings.PoolSize, nodes)}
		}
		plan.PoolSize = settings.PoolSize
	}
	plan.Warning = planWarning(plan)
	return plan, nil
}

// conditionTaintPrefix starts the taints the node lifecycle controller
// puts on nodes that are not ready, unreachable or cordoned.
const conditionTaintPrefix = "node.kubernetes.io/"

// cephNodes counts the nodes that can hold a mon or an OSD: Rook's pods
// tolerate no taint by default, so a NoSchedule or NoExecute taint keeps
// them away. The node lifecycle's own taints (not ready, unreachable,
// cordoned) are passing states and do not count against a node.
func cephNodes(nodes []corev1.Node) int {
	n := 0
	for _, node := range nodes {
		if !keepsPodsAway(node.Spec.Taints) {
			n++
		}
	}
	return n
}

func keepsPodsAway(taints []corev1.Taint) bool {
	for _, t := range taints {
		if strings.HasPrefix(t.Key, conditionTaintPrefix) {
			continue
		}
		if t.Effect == corev1.TaintEffectNoSchedule || t.Effect == corev1.TaintEffectNoExecute {
			return true
		}
	}
	return false
}

// planWarning says what a plan loses against Ceph's defaults.
func planWarning(p rookPlan) string {
	var lost []string
	if p.Mons < 3 {
		lost = append(lost, "1 monitor: Ceph stops when its node is down")
	}
	switch p.PoolSize {
	case 1:
		lost = append(lost, "1 copy of the data: a lost disk or node loses it")
	case 2:
		lost = append(lost, "2 copies of the data: what is written while one is down has a single copy")
	}
	return strings.Join(lost, "; ")
}
