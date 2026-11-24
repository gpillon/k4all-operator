package nodesettings

import (
	"fmt"
	"net/netip"
	"sort"
	"strconv"
	"strings"
)

// Node is what the rollout needs of a cluster Node.
type Node struct {
	Name          string
	Annotations   map[string]string
	Ready         bool
	Unschedulable bool
	// CPUs and Memory (bytes) are the node's capacity.
	CPUs   int64
	Memory int64
	// PodCIDR is spec.podCIDR, the node's pod CIDR when the IPAM uses it.
	PodCIDR string
}

// PodNetwork is how the nodes get their pod addresses, for the maxPods check.
type PodNetwork struct {
	// MaskSize of every node's pod CIDR; 0: each node's spec.podCIDR.
	MaskSize int
	// Source names where the pod CIDRs come from, for the message.
	Source string
	// Unlimited: addresses come in blocks on demand (Calico), no limit.
	Unlimited bool
}

// reservedAddresses is what a node's pod CIDR keeps for itself: network and
// broadcast, Cilium's router and health addresses and some room: a /24
// holds 250 pods (spec §6.3).
const reservedAddresses = 6

// maxHugepagesPercent is the share of the smallest node's memory hugepages
// may take. The addon warns from 50 %.
const maxHugepagesPercent = 80

// Validate checks s against the nodes (spec §6.3) and says what does not
// fit, naming the node; "" when everything fits.
func Validate(s Settings, nodes []Node, pods PodNetwork) string {
	var problems []string
	if cpus := s.Kubelet.ReservedSystemCPUs; cpus != "" {
		problems = append(problems, checkReserved(cpus, nodes)...)
	}
	if len(s.Hugepages) > 0 {
		problems = append(problems, checkHugepages(s.Hugepages, nodes)...)
	}
	if s.Kubelet.MaxPods != nil && !pods.Unlimited {
		problems = append(problems, checkMaxPods(int(*s.Kubelet.MaxPods), nodes, pods)...)
	}
	return strings.Join(problems, "; ")
}

func checkReserved(cpus string, nodes []Node) []string {
	set, err := ParseCPUSet(cpus)
	if err != nil {
		return []string{fmt.Sprintf("reservedSystemCPUs %q is not a CPU list (e.g. 0-1 or 0,2): %v", cpus, err)}
	}
	for _, n := range nodes {
		switch {
		case n.CPUs <= 0:
			continue // capacity not reported yet: checked once it is
		case int64(set[len(set)-1]) >= n.CPUs:
			return []string{fmt.Sprintf("reservedSystemCPUs %s: node %s has %d CPUs (0-%d)", cpus, n.Name, n.CPUs, n.CPUs-1)}
		case int64(len(set)) >= n.CPUs:
			return []string{fmt.Sprintf("reservedSystemCPUs %s leaves no CPU for the pods of node %s (%d CPUs)", cpus, n.Name, n.CPUs)}
		}
	}
	return nil
}

// pageBytes is the size of each hugepage size the API accepts.
var pageBytes = map[string]int64{"1Gi": 1 << 30, "2Mi": 2 << 20}

func checkHugepages(pages map[string]int32, nodes []Node) []string {
	var total int64
	for size, n := range pages {
		total += pageBytes[size] * int64(n)
	}
	var smallest *Node
	for i := range nodes {
		if nodes[i].Memory > 0 && (smallest == nil || nodes[i].Memory < smallest.Memory) {
			smallest = &nodes[i]
		}
	}
	if smallest == nil || total*100 <= smallest.Memory*maxHugepagesPercent {
		return nil
	}
	return []string{fmt.Sprintf("hugepages take %dMi, more than %d %% of the %dMi of node %s",
		total>>20, maxHugepagesPercent, smallest.Memory>>20, smallest.Name)}
}

func checkMaxPods(maxPods int, nodes []Node, pods PodNetwork) []string {
	for _, n := range nodes {
		mask := pods.MaskSize
		if mask == 0 {
			prefix, err := netip.ParsePrefix(n.PodCIDR)
			if err != nil || !prefix.Addr().Is4() {
				continue // no IPv4 pod CIDR on the Node: nothing to check
			}
			mask = prefix.Bits()
		}
		if mask < 1 || mask > 32 {
			continue
		}
		limit := (1 << (32 - mask)) - reservedAddresses
		if maxPods > limit {
			source := ""
			if pods.Source != "" {
				source = " (" + pods.Source + ")"
			}
			return []string{fmt.Sprintf("maxPods %d does not fit the /%d pod CIDR of node %s%s: at most %d pods",
				maxPods, mask, n.Name, source, max(limit, 0))}
		}
	}
	return nil
}

// ParseCPUSet reads a Linux CPU list ("0-1", "0,2-3") into sorted CPU ids.
func ParseCPUSet(s string) ([]int, error) {
	seen := map[int]bool{}
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		lo, hi, isRange := strings.Cut(part, "-")
		first, err := strconv.Atoi(lo)
		if err != nil || first < 0 {
			return nil, fmt.Errorf("%q is not a CPU or a range", part)
		}
		last := first
		if isRange {
			if last, err = strconv.Atoi(hi); err != nil || last < first {
				return nil, fmt.Errorf("%q is not a CPU range", part)
			}
		}
		for cpu := first; cpu <= last; cpu++ {
			seen[cpu] = true
		}
	}
	out := make([]int, 0, len(seen))
	for cpu := range seen {
		out = append(out, cpu)
	}
	sort.Ints(out)
	return out, nil
}
