package controller

import (
	"context"
	"fmt"
	"net/netip"
	"slices"
	"sort"
	"strings"

	k4allv1alpha1 "github.com/gpillon/k4all-operator/api/v1alpha1"
	"github.com/gpillon/k4all-operator/internal/controller/hooks"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// +kubebuilder:rbac:groups=cilium.io,resources=ciliumnodes,verbs=get;list

// ciliumCNI is spec.networking.cni.type with Cilium.
const ciliumCNI = "cilium"

// PodNetworkOverlapCondition is the ClusterConfig condition that lists what
// Cilium's pod addresses overlap (k4all#72). It only reports: the operator
// changes nothing in the cluster for it.
const PodNetworkOverlapCondition = "PodNetworkOverlap"

// kubevirtMasqueradeNetwork is KubeVirt's default masquerade network inside
// every virt-launcher pod: a VM whose node's pod CIDR overlaps it routes
// that node's pods to its own interface.
var kubevirtMasqueradeNetwork = netip.MustParsePrefix("10.0.2.0/24")

// addressRange is a network, or an address as a single-address prefix,
// named for the condition message. note, when set, is what the message adds
// when something overlaps it. poolNoteOnly: the pool holding it is only a
// note, an overlap only once a node holds a pod CIDR in it.
type addressRange struct {
	name         string
	prefix       netip.Prefix
	note         string
	poolNoteOnly bool
}

// Notes of the condition message.
const (
	kubevirtNote = "For KubeVirt, set networks[].pod.vmNetworkCIDR on each VM to a network outside the pod network."
	subnetNote   = "Node subnets are taken as the /24 of each node's InternalIP: the real prefix is only known on the host."
)

// podNetworkOverlap checks Cilium's pod addresses, its pool and the pod CIDRs
// the nodes hold, against the networks they must stay clear of.
func (r *ReleaseReconciler) podNetworkOverlap(
	ctx context.Context, config *k4allv1alpha1.ClusterConfig, rm *k4allv1alpha1.ReleaseManifest,
) metav1.Condition {
	cond := metav1.Condition{Type: PodNetworkOverlapCondition, ObservedGeneration: config.Generation}
	checkFailed := func(err error) metav1.Condition {
		cond.Status, cond.Reason, cond.Message = metav1.ConditionUnknown, "CheckFailed", err.Error()
		return cond
	}
	ipam, err := hooks.ReadCiliumIPAM(ctx, r.Client, rm.Spec.Components["cilium"].Namespace, config.Spec)
	if err != nil {
		return checkFailed(err)
	}
	allocated, err := nodePodCIDRs(ctx, r.Client)
	if err != nil {
		return checkFailed(err)
	}
	subnets, err := nodeSubnets(ctx, r.Client)
	if err != nil {
		return checkFailed(err)
	}

	targets := append(subnets, configRanges(config.Spec)...)

	var findings, notes, poolOnlyNotes []string
	check := func(pod addressRange, isPool bool) bool {
		overlaps := false
		for _, target := range targets {
			if !pod.prefix.Overlaps(target.prefix) {
				continue
			}
			if isPool && target.poolNoteOnly {
				poolOnlyNotes = append(poolOnlyNotes, fmt.Sprintf(
					"Note: %s holds %s: a node is affected only if it gets a pod CIDR there.", pod.name, target.name))
				continue
			}
			overlaps = true
			findings = append(findings, fmt.Sprintf("%s overlaps %s", pod.name, target.name))
			if target.note != "" && !slices.Contains(notes, target.note) {
				notes = append(notes, target.note)
			}
		}
		return overlaps
	}
	poolOverlaps := false
	for _, pool := range ipam.CIDRs {
		poolOverlaps = check(addressRange{name: "Cilium pool " + pool.String(), prefix: pool}, true) || poolOverlaps
	}
	for _, pod := range allocated {
		check(pod, false)
	}
	if poolOverlaps {
		notes = append(notes, poolNote(ipam))
	}
	notes = append(notes, poolOnlyNotes...)

	if len(findings) == 0 {
		clearOf := []string{"node subnet", "service network", "ingress IP or VIP"}
		if len(poolOnlyNotes) > 0 {
			clearOf = []string{"node subnet", "ingress IP or VIP"}
		}
		message := fmt.Sprintf("%s and %s overlap no %s", poolName(ipam), countOf(len(allocated), "node pod CIDR"),
			strings.Join(clearOf, ", "))
		if config.Spec.Features.Virt.Enabled {
			message += ", nor KubeVirt's masquerade network"
		}
		cond.Status, cond.Reason = metav1.ConditionFalse, "NoOverlap"
		cond.Message = strings.Join(append([]string{message + "."}, notes...), " ")
		return cond
	}
	message := append([]string{strings.Join(findings, "; ") + "."}, notes...)
	cond.Status, cond.Reason, cond.Message = metav1.ConditionTrue, "OverlapFound", strings.Join(message, " ")
	return cond
}

// poolNote says what to do about a pool that overlaps. Before the install
// it is spec.cluster.podNetwork. Once Cilium runs it, nothing: Cilium does
// not support changing the CIDRs of its pool, only adding new ones (its
// cluster-pool IPAM documentation), so k4all leaves it as it is.
func poolNote(ipam hooks.CiliumIPAM) string {
	if !ipam.Running {
		return "Set spec.cluster.podNetwork clear of these before Cilium is installed."
	}
	return "Cilium does not support changing the CIDRs of a running pool, only adding new ones, so k4all never changes it: " +
		"a node that gets an overlapping pod CIDR keeps it, and only a cluster installed with a pod network clear of these avoids it."
}

// poolName names Cilium's pool, or its IPAM mode when it has none.
func poolName(ipam hooks.CiliumIPAM) string {
	if len(ipam.CIDRs) == 0 {
		return "Cilium (ipam " + ipam.Mode + ")"
	}
	cidrs := make([]string, 0, len(ipam.CIDRs))
	for _, c := range ipam.CIDRs {
		cidrs = append(cidrs, c.String())
	}
	return "Cilium pool " + strings.Join(cidrs, " ")
}

// countOf is "n thing", or "n things" for n other than one.
func countOf(n int, thing string) string {
	if n == 1 {
		return "1 " + thing
	}
	return fmt.Sprintf("%d %ss", n, thing)
}

// defaultServiceNetwork is the service network of a ClusterConfig without one.
const defaultServiceNetwork = "10.96.0.0/16"

// configRanges are the networks and addresses of the ClusterConfig the pod
// addresses must stay clear of: the service network, KubeVirt's masquerade
// network with the virtualization feature, the ingress IPs and the
// control-plane VIP. A value that does not parse is left out: kubeadm and
// the hooks report it.
func configRanges(config k4allv1alpha1.ClusterConfigSpec) []addressRange {
	var ranges []addressRange
	serviceNetwork := config.Cluster.ServiceNetwork
	if serviceNetwork == "" {
		serviceNetwork = defaultServiceNetwork
	}
	if prefix, err := netip.ParsePrefix(serviceNetwork); err == nil {
		// The pool holding it is only a note: every cluster on the chart
		// default 10.0.0.0/8 has 10.96.0.0/16 in its pool, and only its
		// ~24,000th node would get a CIDR there.
		ranges = append(ranges, addressRange{
			name: "the service network " + prefix.Masked().String(), prefix: prefix.Masked(), poolNoteOnly: true,
		})
	}
	if config.Features.Virt.Enabled {
		ranges = append(ranges, addressRange{
			name:   "KubeVirt's masquerade network " + kubevirtMasqueradeNetwork.String(),
			prefix: kubevirtMasqueradeNetwork, note: kubevirtNote,
		})
	}
	addAddress := func(name, address string) {
		if ip, err := netip.ParseAddr(address); err == nil {
			ranges = append(ranges, addressRange{name: name + " " + ip.String(), prefix: netip.PrefixFrom(ip, ip.BitLen())})
		}
	}
	for _, ip := range config.Ingress.DedicatedIPs() {
		addAddress("the ingress IP", ip)
	}
	if ha := config.Cluster.HA; ha.Type != "" && ha.Type != "none" {
		addAddress("the control-plane VIP", ha.ApiControlEndpoint)
	}
	return ranges
}

// nodeSubnetBits is the prefix a node's subnet is taken to have: the real
// one is only known on the host.
const nodeSubnetBits = 24

// nodeSubnets are the subnets of the nodes' InternalIPs, each named with its
// nodes.
func nodeSubnets(ctx context.Context, c client.Reader) ([]addressRange, error) {
	nodes := &corev1.NodeList{}
	if err := c.List(ctx, nodes); err != nil {
		return nil, fmt.Errorf("list nodes: %w", err)
	}
	sort.Slice(nodes.Items, func(i, j int) bool { return nodes.Items[i].Name < nodes.Items[j].Name })
	var order []netip.Prefix
	names := map[netip.Prefix][]string{}
	for _, node := range nodes.Items {
		for _, addr := range node.Status.Addresses {
			ip, err := netip.ParseAddr(addr.Address)
			if addr.Type != corev1.NodeInternalIP || err != nil || !ip.Is4() {
				continue
			}
			subnet := netip.PrefixFrom(ip, nodeSubnetBits).Masked()
			if _, seen := names[subnet]; !seen {
				order = append(order, subnet)
			}
			if !slices.Contains(names[subnet], node.Name) {
				names[subnet] = append(names[subnet], node.Name)
			}
		}
	}
	ranges := make([]addressRange, 0, len(order))
	for _, subnet := range order {
		of := "node " + names[subnet][0]
		if len(names[subnet]) > 1 {
			of = "nodes " + strings.Join(names[subnet], ", ")
		}
		ranges = append(ranges, addressRange{
			name: fmt.Sprintf("the subnet %s of %s", subnet, of), prefix: subnet, note: subnetNote,
		})
	}
	return ranges, nil
}

// nodePodCIDRs are the pod CIDRs Cilium's operator gave the nodes
// (CiliumNode spec.ipam.podCIDRs), by node name. None while Cilium's CRDs
// are not installed.
func nodePodCIDRs(ctx context.Context, c client.Reader) ([]addressRange, error) {
	list := &unstructured.UnstructuredList{}
	list.SetAPIVersion("cilium.io/v2")
	list.SetKind("CiliumNodeList")
	if err := c.List(ctx, list); err != nil {
		if meta.IsNoMatchError(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("list CiliumNodes: %w", err)
	}
	sort.Slice(list.Items, func(i, j int) bool { return list.Items[i].GetName() < list.Items[j].GetName() })
	var ranges []addressRange
	for _, cn := range list.Items {
		cidrs, _, _ := unstructured.NestedStringSlice(cn.Object, "spec", "ipam", "podCIDRs")
		for _, cidr := range cidrs {
			prefix, err := netip.ParsePrefix(cidr)
			if err != nil {
				continue // not an address Cilium hands out
			}
			ranges = append(ranges, addressRange{
				name: fmt.Sprintf("pod CIDR %s of node %s", prefix, cn.GetName()), prefix: prefix,
			})
		}
	}
	return ranges, nil
}
