package hooks

import (
	"context"
	"fmt"
	"net/netip"
	"strconv"
	"strings"

	k4allv1alpha1 "github.com/gpillon/k4all-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// CiliumIPAM is how Cilium hands out pod addresses: in its cluster-pool mode,
// its operator gives each node a CIDR of MaskSize bits from CIDRs.
type CiliumIPAM struct {
	// Mode is Cilium's ipam mode: "cluster-pool", or the other mode a
	// running cluster has (no CIDRs then).
	Mode     string
	CIDRs    []netip.Prefix
	MaskSize int
	// Running: read from the cluster's cilium-config, Cilium runs it.
	// Otherwise it is what a new install gets.
	Running bool
}

// clusterPoolMode is Cilium's ipam mode with a pool of the Cilium operator.
const clusterPoolMode = "cluster-pool"

// nodeMaskSize is the size of a node's pod CIDR, Cilium's and kubeadm's
// default: a /24 holds 254 addresses, the agent's router and health
// endpoints and 252 pods, above kubelet's maxPods (110 by default, k4all
// sets none; 250 still fits).
const nodeMaskSize = 24

// defaultPodNetwork is the pod network of a ClusterConfig without one, as
// kubeadm and the Calico hook take it.
const defaultPodNetwork = "10.100.0.1/18"

// Cilium's configuration, rendered by its chart (1.19 and 1.20 alike) from
// ipam.operator.clusterPoolIPv4PodCIDRList (joined by spaces) and
// ipam.operator.clusterPoolIPv4MaskSize.
const (
	ciliumConfigName      = "cilium-config"
	ciliumIPAMModeKey     = "ipam"
	ciliumNamespace       = "kube-system"
	ciliumPoolCIDRKey     = "cluster-pool-ipv4-cidr"
	ciliumPoolMaskSizeKey = "cluster-pool-ipv4-mask-size"
	ciliumDefaultMaskSize = nodeMaskSize // the agent's default without the key
)

// ReadCiliumIPAM returns how Cilium hands out pod addresses. Once Cilium is
// installed it is what the cluster runs, read from <namespace>/cilium-config:
// neither spec.cluster.podNetwork nor a chart default moves its pool (that
// re-addresses the pods), and another mode it runs stays. The ConfigMap is
// what the Cilium agents and operator read; it outlives an operator restart
// and a lost ReleaseManifest status, a failed first install leaves in it the
// pool this operator rendered, and Helm deletes it with Cilium (a CNI
// switch), so the next install is a new one. Without it, the IPAM is the one
// a new install gets, from spec.cluster.podNetwork. Any other read error is
// returned: the pool is never guessed.
func ReadCiliumIPAM(ctx context.Context, c client.Reader, namespace string, config k4allv1alpha1.ClusterConfigSpec) (CiliumIPAM, error) {
	if namespace == "" {
		namespace = ciliumNamespace
	}
	cm := &corev1.ConfigMap{}
	err := c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: ciliumConfigName}, cm)
	switch {
	case apierrors.IsNotFound(err):
		return newClusterPool(config)
	case err != nil:
		return CiliumIPAM{}, fmt.Errorf("read the IPAM Cilium runs from %s/%s: %w", namespace, ciliumConfigName, err)
	}
	if cidrs, ok := cm.Data[ciliumPoolCIDRKey]; ok {
		return runningPool(cidrs, cm.Data[ciliumPoolMaskSizeKey])
	}
	if mode := cm.Data[ciliumIPAMModeKey]; mode != "" && mode != clusterPoolMode {
		return CiliumIPAM{Mode: mode, Running: true}, nil
	}
	return newClusterPool(config)
}

// runningPool parses the pool of cilium-config.
func runningPool(cidrs, maskSize string) (CiliumIPAM, error) {
	pool := CiliumIPAM{Mode: clusterPoolMode, MaskSize: ciliumDefaultMaskSize, Running: true}
	for _, field := range strings.FieldsFunc(cidrs, func(r rune) bool { return r == ' ' || r == ',' }) {
		prefix, err := netip.ParsePrefix(field)
		if err != nil {
			return CiliumIPAM{}, fmt.Errorf("cilium-config %s %q: %w", ciliumPoolCIDRKey, cidrs, err)
		}
		pool.CIDRs = append(pool.CIDRs, prefix)
	}
	if len(pool.CIDRs) == 0 {
		return CiliumIPAM{}, fmt.Errorf("cilium-config %s is empty", ciliumPoolCIDRKey)
	}
	if maskSize != "" {
		size, err := strconv.Atoi(maskSize)
		if err != nil {
			return CiliumIPAM{}, fmt.Errorf("cilium-config %s %q: %w", ciliumPoolMaskSizeKey, maskSize, err)
		}
		pool.MaskSize = size
	}
	return pool, nil
}

// newClusterPool is the pool of a new cluster: spec.cluster.podNetwork,
// normalized to its network (k4all's default 10.100.0.1/18 is a host
// address), a /24 per node.
func newClusterPool(config k4allv1alpha1.ClusterConfigSpec) (CiliumIPAM, error) {
	podNetwork := config.Cluster.PodNetwork
	if podNetwork == "" {
		podNetwork = defaultPodNetwork
	}
	prefix, err := netip.ParsePrefix(podNetwork)
	switch {
	case err != nil:
		return CiliumIPAM{}, fmt.Errorf("spec.cluster.podNetwork %q: %w", podNetwork, err)
	case !prefix.Addr().Is4():
		return CiliumIPAM{}, fmt.Errorf("spec.cluster.podNetwork %q: Cilium's pool needs an IPv4 network", podNetwork)
	case prefix.Bits() > nodeMaskSize:
		return CiliumIPAM{}, fmt.Errorf("spec.cluster.podNetwork %q: narrower than one node's /%d", podNetwork, nodeMaskSize)
	}
	return CiliumIPAM{Mode: clusterPoolMode, CIDRs: []netip.Prefix{prefix.Masked()}, MaskSize: nodeMaskSize}, nil
}

// helmValues are the chart's ipam values. ipam.mode is set with the pool too:
// a chart with another default mode would ignore it.
func (p CiliumIPAM) helmValues() map[string]interface{} {
	if p.Mode != clusterPoolMode {
		return map[string]interface{}{"mode": p.Mode}
	}
	cidrs := make([]string, 0, len(p.CIDRs))
	for _, c := range p.CIDRs {
		cidrs = append(cidrs, c.String())
	}
	return map[string]interface{}{
		"mode": clusterPoolMode,
		"operator": map[string]interface{}{
			"clusterPoolIPv4PodCIDRList": cidrs,
			"clusterPoolIPv4MaskSize":    p.MaskSize,
		},
	}
}
