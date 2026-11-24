package hooks

import (
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"

	k4allv1alpha1 "github.com/gpillon/k4all-operator/api/v1alpha1"
)

// RookSettings is the Rook configuration of a cluster: what k4all creates
// the CephCluster and vm-pool with. A ClusterConfig field will carry it once
// its API is approved; until then every cluster gets defaultRookSettings
// (see rookSettingsFrom).
type RookSettings struct {
	// DeviceFilter selects the OSD devices: a regular expression on device
	// names, e.g. "^sdb$". Empty: every device or partition of every node
	// with nothing on it (Rook's useAllDevices). Rook skips whatever carries
	// a filesystem or an LVM signature, so the OS disk and TopoLVM's volume
	// group are safe; a blank spare disk or partition becomes an OSD.
	DeviceFilter string
	// SmallCluster is what k4all does on fewer than 3 nodes.
	SmallCluster SmallClusterPolicy
	// PoolSize is vm-pool's replica count; 0: from the node count.
	PoolSize int
}

// defaultRookSettings are k4all's Rook defaults: every empty device becomes
// an OSD, and no CephCluster below 3 nodes.
func defaultRookSettings() RookSettings {
	return RookSettings{SmallCluster: SmallClusterRefuse}
}

// rookSettingsFrom reads the cluster's Rook settings from the ClusterConfig.
// STUB until the ClusterConfig's Rook field is approved: every cluster gets
// defaultRookSettings.
func rookSettingsFrom(_ k4allv1alpha1.ClusterConfigSpec) RookSettings {
	return defaultRookSettings()
}

// SmallClusterPolicy is what k4all does with Rook on fewer than 3 nodes,
// where Ceph cannot keep its default 3 replicas on 3 hosts.
type SmallClusterPolicy string

const (
	// SmallClusterRefuse creates no CephCluster below 3 nodes: the
	// component's status says so, and the CephCluster is created once a
	// third node has joined. The default.
	SmallClusterRefuse SmallClusterPolicy = "Refuse"
	// SmallClusterAllow creates the CephCluster on 1 or 2 nodes, with one
	// replica per node (1 node: a single copy, no redundancy) and a warning
	// in the StorageClass description and the operator's log.
	SmallClusterAllow SmallClusterPolicy = "Allow"
)

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
// Below 3 nodes SmallClusterRefuse refuses; so does a pool size larger than
// the node count, since every replica needs its own host.
func planRook(nodes int, settings RookSettings) (rookPlan, error) {
	if nodes < 1 {
		return rookPlan{}, rookRefusal{"no node can run Ceph"}
	}
	if nodes < cephMinNodes && settings.SmallCluster != SmallClusterAllow {
		return rookPlan{}, rookRefusal{fmt.Sprintf(
			"Ceph needs %d nodes for 3 monitors and 3 replicas on separate hosts, this cluster has %d: "+
				"k4all creates the CephCluster once it has %d", cephMinNodes, nodes, cephMinNodes)}
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
