package engine

import (
	"fmt"
	"sort"

	k4allv1alpha1 "github.com/gpillon/k4all-operator/api/v1alpha1"
)

// DependencyGraph manages the installation order of components
// based on their dependsOn relationships.
type DependencyGraph struct {
	adjacency map[string][]string // component -> components that depend on it
	inDegree  map[string]int
	allNodes  map[string]bool
}

// NewDependencyGraph builds a DAG from a set of component specs.
func NewDependencyGraph(components map[string]k4allv1alpha1.ComponentSpec) *DependencyGraph {
	g := &DependencyGraph{
		adjacency: make(map[string][]string),
		inDegree:  make(map[string]int),
		allNodes:  make(map[string]bool),
	}

	for name := range components {
		g.allNodes[name] = true
		if _, ok := g.inDegree[name]; !ok {
			g.inDegree[name] = 0
		}
	}

	for name, spec := range components {
		for _, dep := range spec.DependsOn {
			if !g.allNodes[dep] {
				continue // skip references to unknown components
			}
			g.adjacency[dep] = append(g.adjacency[dep], name)
			g.inDegree[name]++
		}
	}

	return g
}

// TopologicalSort returns component names in dependency order.
// Returns an error if a cycle is detected.
func (g *DependencyGraph) TopologicalSort() ([]string, error) {
	inDegree := make(map[string]int, len(g.inDegree))
	for k, v := range g.inDegree {
		inDegree[k] = v
	}

	// Seed the queue with zero-dependency components (sorted for determinism)
	var queue []string
	for name := range g.allNodes {
		if inDegree[name] == 0 {
			queue = append(queue, name)
		}
	}
	sort.Strings(queue)

	var result []string
	for len(queue) > 0 {
		node := queue[0]
		queue = queue[1:]
		result = append(result, node)

		neighbors := g.adjacency[node]
		sort.Strings(neighbors)
		for _, neighbor := range neighbors {
			inDegree[neighbor]--
			if inDegree[neighbor] == 0 {
				queue = append(queue, neighbor)
			}
		}
	}

	if len(result) != len(g.allNodes) {
		return nil, fmt.Errorf("dependency cycle detected: resolved %d of %d components", len(result), len(g.allNodes))
	}

	return result, nil
}

// FilterActiveComponents returns only components that should be installed
// based on the ClusterConfig feature gates, CNI selection, and management policy.
// Unmanaged and Removed components are excluded from the active set.
func FilterActiveComponents(
	components map[string]k4allv1alpha1.ComponentSpec,
	config k4allv1alpha1.ClusterConfigSpec,
) map[string]k4allv1alpha1.ComponentSpec {
	active := make(map[string]k4allv1alpha1.ComponentSpec)

	for name, spec := range components {
		if spec.Management == k4allv1alpha1.ComponentManagementUnmanaged ||
			spec.Management == k4allv1alpha1.ComponentManagementRemoved {
			continue
		}
		if shouldInstall(name, spec, config) {
			active[name] = spec
		}
	}

	return active
}

// FilterRemovedComponents returns components explicitly marked for removal.
func FilterRemovedComponents(
	components map[string]k4allv1alpha1.ComponentSpec,
) map[string]k4allv1alpha1.ComponentSpec {
	removed := make(map[string]k4allv1alpha1.ComponentSpec)
	for name, spec := range components {
		if spec.Management == k4allv1alpha1.ComponentManagementRemoved {
			removed[name] = spec
		}
	}
	return removed
}

// Feature gates a component can name in its featureGate, also read by the
// hooks that add or leave out parts of a component (headlamp's plugins).
const (
	FeatureVirt = "virt"
	FeatureRook = "rook"
	// FeatureSnapshots is on with virt or rook: the CSI snapshot controller
	// serves both (KubeVirt and Hangar snapshot VM disks, ceph-rbd has a
	// VolumeSnapshotClass). It has no ClusterConfig switch of its own.
	FeatureSnapshots = "snapshots"
	// FeatureVirtMetadata is the metadata service of VMs: virt, plus
	// features.virt.metadata.enabled. An operator before v0.6.0 does not
	// know it and leaves the k4all-metadata component out.
	FeatureVirtMetadata = "virtMetadata"
)

// FeatureEnabled says whether the ClusterConfig turns on the feature gate; an
// unknown gate is off.
func FeatureEnabled(gate string, config k4allv1alpha1.ClusterConfigSpec) bool {
	switch gate {
	case FeatureVirt:
		return config.Features.Virt.Enabled
	case "argocd":
		return config.Features.ArgoCD.Enabled
	case "ovsCni":
		return config.Features.OVSCNI.Enabled
	case FeatureRook:
		return rookEnabled(config)
	case FeatureSnapshots:
		return config.Features.Virt.Enabled || rookEnabled(config)
	case FeatureVirtMetadata:
		md := config.Features.Virt.Metadata
		return config.Features.Virt.Enabled && md != nil && md.Enabled
	}
	return false
}

// rookEnabled says whether k4all runs Rook: features.rook.enabled. A cluster
// without the field (every cluster before k4all v2.7.0) runs none.
func rookEnabled(config k4allv1alpha1.ClusterConfigSpec) bool {
	return config.Features.Rook != nil && config.Features.Rook.Enabled
}

// hangarNodeAgent is the component of Hangar's privileged node agent.
const hangarNodeAgent = "hangar-node-agent"

// hangarNodeAgentWanted says whether the ClusterConfig keeps Hangar's node
// agent: yes unless features.virt.hangar.nodeAgent is false. Absent is the
// release's default, which installs it with virt (D6).
func hangarNodeAgentWanted(config k4allv1alpha1.ClusterConfigSpec) bool {
	hangar := config.Features.Virt.Hangar
	return hangar == nil || hangar.NodeAgent == nil || *hangar.NodeAgent
}

// CNI types accepted by ClusterConfig.spec.networking.cni.type.
const (
	cniCalico = "calico"
	cniCilium = "cilium"
)

func shouldInstall(name string, spec k4allv1alpha1.ComponentSpec, config k4allv1alpha1.ClusterConfigSpec) bool {
	// CNI selection: only install the chosen CNI and its CLI
	switch name {
	case cniCalico:
		return config.Networking.CNI.Type == cniCalico
	case cniCilium, "cilium-cli":
		return config.Networking.CNI.Type == cniCilium
	}

	// HA components: kube-vip only when HA type matches
	if name == "kube-vip" {
		return config.Cluster.HA.Type == "kubevip"
	}

	// MetalLB is skipped when Cilium handles L2 announcements and no dedicated IPs
	if name == "metallb" {
		if config.Networking.CNI.Type == cniCilium {
			if len(config.Ingress.DedicatedIPs()) == 0 {
				return false
			}
		}
	}

	// Hangar's node agent: with its feature gate (virt), unless left out.
	if name == hangarNodeAgent && !hangarNodeAgentWanted(config) {
		return false
	}

	// Feature-gated components
	if spec.FeatureGate != "" {
		return FeatureEnabled(spec.FeatureGate, config)
	}

	// Virtualization components without explicit featureGate
	switch name {
	case "kubevirt", "virtctl", "cdi", "kubevirt-manager":
		return config.Features.Virt.Enabled
	case "argocd":
		return config.Features.ArgoCD.Enabled
	case "multus-cni", "ovs-cni":
		return config.Features.OVSCNI.Enabled
	}

	// All other components are always installed
	return true
}
