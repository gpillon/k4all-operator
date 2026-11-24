package controller

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/go-logr/logr"
	k4allv1alpha1 "github.com/gpillon/k4all-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

// newCiliumCluster is the fake cluster of applied_spec_test.go with Cilium
// as the CNI, config's features and network, and objects (Nodes,
// CiliumNodes, cilium-config). Its one component fails at once, without
// network access.
func newCiliumCluster(t *testing.T, config k4allv1alpha1.ClusterConfigSpec, objects ...client.Object) *cluster {
	t.Helper()
	return newCiliumClusterWith(t, interceptor.Funcs{}, config, objects...)
}

// newCiliumClusterWith is newCiliumCluster whose API server answers through
// funcs.
func newCiliumClusterWith(
	t *testing.T, funcs interceptor.Funcs, config k4allv1alpha1.ClusterConfigSpec, objects ...client.Object,
) *cluster {
	t.Helper()
	s := gateScheme(t)
	config.Networking.CNI.Type = ciliumCNI
	rm := &k4allv1alpha1.ReleaseManifest{
		ObjectMeta: metav1.ObjectMeta{Name: "k4all"},
		Spec: k4allv1alpha1.ReleaseManifestSpec{Components: map[string]k4allv1alpha1.ComponentSpec{
			"probe": {Type: "unsupported", Version: "1"},
		}},
	}
	cc := &k4allv1alpha1.ClusterConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "k4all-cluster-config"},
		Spec:       config,
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(append([]client.Object{rm, cc}, objects...)...).
		WithStatusSubresource(&k4allv1alpha1.ReleaseManifest{}, &k4allv1alpha1.ClusterConfig{}).
		WithInterceptorFuncs(funcs).
		Build()
	return &cluster{t: t, c: c, r: &ReleaseReconciler{Client: c, Scheme: s, Log: logr.Discard()}}
}

// condition returns the ClusterConfig condition of type conditionType, nil
// when it has none.
func (cl *cluster) condition(conditionType string) *metav1.Condition {
	cl.t.Helper()
	cc := &k4allv1alpha1.ClusterConfig{}
	if err := cl.c.Get(context.Background(), types.NamespacedName{Name: "k4all-cluster-config"}, cc); err != nil {
		cl.t.Fatal(err)
	}
	return meta.FindStatusCondition(cc.Status.Conditions, conditionType)
}

// overlapCondition reconciles once and returns PodNetworkOverlap, which
// must be there.
func (cl *cluster) overlapCondition() metav1.Condition {
	cl.t.Helper()
	cl.reconcile()
	cond := cl.condition(k4allv1alpha1.ConditionPodNetworkOverlap)
	if cond == nil {
		cl.t.Fatalf("no %s condition on the ClusterConfig", k4allv1alpha1.ConditionPodNetworkOverlap)
	}
	return *cond
}

func expectOverlap(t *testing.T, cond metav1.Condition, fragments ...string) {
	t.Helper()
	if cond.Status != metav1.ConditionTrue || cond.Reason != "OverlapFound" {
		t.Errorf("%s = %s (%s), want True (OverlapFound): %s", cond.Type, cond.Status, cond.Reason, cond.Message)
	}
	for _, f := range fragments {
		if !strings.Contains(cond.Message, f) {
			t.Errorf("message %q\nlacks %q", cond.Message, f)
		}
	}
}

func expectNoOverlap(t *testing.T, cond metav1.Condition, fragments ...string) {
	t.Helper()
	if cond.Status != metav1.ConditionFalse || cond.Reason != "NoOverlap" {
		t.Errorf("%s = %s (%s), want False (NoOverlap): %s", cond.Type, cond.Status, cond.Reason, cond.Message)
	}
	for _, f := range fragments {
		if !strings.Contains(cond.Message, f) {
			t.Errorf("message %q\nlacks %q", cond.Message, f)
		}
	}
}

func virtConfig() k4allv1alpha1.ClusterConfigSpec {
	var config k4allv1alpha1.ClusterConfigSpec
	config.Features.Virt.Enabled = true
	return config
}

// The chart default 10.0.0.0/8 holds KubeVirt's masquerade network: the
// node that gets 10.0.2.0/24 has VMs that cannot reach its pods (k4all#72).
// The fix for a VM is its own vmNetworkCIDR.
func TestPodNetworkOverlap_PoolHoldsKubeVirtsMasqueradeNetwork(t *testing.T) {
	cl := newCiliumCluster(t, virtConfig(), ciliumConfigMap("10.0.0.0/8", "24"))

	expectOverlap(t, cl.overlapCondition(),
		"Cilium pool 10.0.0.0/8 overlaps KubeVirt's masquerade network 10.0.2.0/24",
		"vmNetworkCIDR")
}

// ciliumNode is the CiliumNode of a node with the pod CIDRs Cilium's
// operator gave it.
func ciliumNode(name string, podCIDRs ...string) *unstructured.Unstructured {
	cn := &unstructured.Unstructured{}
	cn.SetAPIVersion("cilium.io/v2")
	cn.SetKind("CiliumNode")
	cn.SetName(name)
	cidrs := make([]interface{}, 0, len(podCIDRs))
	for _, c := range podCIDRs {
		cidrs = append(cidrs, c)
	}
	_ = unstructured.SetNestedSlice(cn.Object, cidrs, "spec", "ipam", "podCIDRs")
	return cn
}

// The CIDRs nodes already hold count, also from another pool than the one
// cilium-config has now: the VMs of node k3 are the ones affected.
func TestPodNetworkOverlap_NodePodCIDRIsKubeVirtsMasqueradeNetwork(t *testing.T) {
	cl := newCiliumCluster(t, virtConfig(),
		ciliumConfigMap("10.100.0.0/18", "24"),
		ciliumNode("k1", "10.0.0.0/24"), ciliumNode("k3", "10.0.2.0/24"))

	expectOverlap(t, cl.overlapCondition(),
		"pod CIDR 10.0.2.0/24 of node k3 overlaps KubeVirt's masquerade network 10.0.2.0/24",
		"vmNetworkCIDR")
}

// lanNode is a Node with an InternalIP.
func lanNode(name, internalIP string) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status: corev1.NodeStatus{Addresses: []corev1.NodeAddress{
			{Type: corev1.NodeHostName, Address: name},
			{Type: corev1.NodeInternalIP, Address: internalIP},
		}},
	}
}

// The nodes hold CIDRs of an older pool, cilium-config now has 10.0.0.0/8
// and the LAN is 10.0.1.0/24: nothing overlaps yet, but the second node to
// join gets 10.0.1.0/24. The pool itself is checked, against the /24 of
// each node address (the real prefix is only known on the host).
func TestPodNetworkOverlap_PoolCanHandOutANodeSubnet(t *testing.T) {
	cl := newCiliumCluster(t, k4allv1alpha1.ClusterConfigSpec{},
		ciliumConfigMap("10.0.0.0/8", "24"),
		lanNode("k1", "10.0.1.11"), lanNode("k2", "10.0.1.12"),
		ciliumNode("k1", "10.100.0.0/24"), ciliumNode("k2", "10.100.1.0/24"))

	expectOverlap(t, cl.overlapCondition(),
		"Cilium pool 10.0.0.0/8 overlaps the subnet 10.0.1.0/24 of nodes k1, k2",
		"the /24 of each node's InternalIP")
}

// The pool must also stay clear of the LAN addresses the ClusterConfig
// names: the ingress IPs and the control-plane VIP.
func TestPodNetworkOverlap_PoolHoldsIngressAndTheVIP(t *testing.T) {
	var config k4allv1alpha1.ClusterConfigSpec
	config.Cluster.HA = k4allv1alpha1.HAConfig{Type: "kubevip", ApiControlEndpoint: "10.0.5.250"}
	config.Ingress.Traefik = &k4allv1alpha1.TraefikIngressConfig{DedicatedIP: "10.0.5.240"}
	config.Ingress.Cilium.DedicatedIP = "192.168.1.240"
	cl := newCiliumCluster(t, config, ciliumConfigMap("10.0.0.0/8", "24"))

	cond := cl.overlapCondition()

	expectOverlap(t, cond,
		"Cilium pool 10.0.0.0/8 overlaps the ingress IP 10.0.5.240",
		"Cilium pool 10.0.0.0/8 overlaps the control-plane VIP 10.0.5.250")
	if strings.Contains(cond.Message, "192.168.1.240") {
		t.Errorf("message %q names an ingress IP outside the pool", cond.Message)
	}
}

// Every cluster on the chart default 10.0.0.0/8 has the service network
// 10.96.0.0/16 in its pool, and only its ~24,000th node would get a CIDR
// there: alone it is a note, not an overlap.
func TestPodNetworkOverlap_PoolHoldingTheServiceNetworkIsANote(t *testing.T) {
	var config k4allv1alpha1.ClusterConfigSpec
	config.Cluster.ServiceNetwork = defaultServiceNetwork
	cl := newCiliumCluster(t, config,
		ciliumConfigMap("10.0.0.0/8", "24"),
		lanNode("k1", "192.168.1.11"), lanNode("k2", "192.168.1.12"),
		ciliumNode("k1", "10.0.0.0/24"), ciliumNode("k2", "10.0.1.0/24"))

	expectNoOverlap(t, cl.overlapCondition(), "Note: Cilium pool 10.0.0.0/8 holds the service network 10.96.0.0/16")
}

// A node that holds a pod CIDR in the service network is an overlap.
func TestPodNetworkOverlap_NodePodCIDRInTheServiceNetwork(t *testing.T) {
	var config k4allv1alpha1.ClusterConfigSpec
	config.Cluster.ServiceNetwork = defaultServiceNetwork
	cl := newCiliumCluster(t, config, ciliumConfigMap("10.0.0.0/8", "24"), ciliumNode("k9", "10.96.0.0/24"))

	expectOverlap(t, cl.overlapCondition(), "pod CIDR 10.96.0.0/24 of node k9 overlaps the service network 10.96.0.0/16")
}

// A new cluster with k4all's defaults overlaps nothing, KubeVirt included:
// the condition says so, and which pool it checked.
func TestPodNetworkOverlap_DefaultsOverlapNothing(t *testing.T) {
	config := virtConfig()
	config.Cluster.PodNetwork = defaultPodNetwork
	config.Cluster.ServiceNetwork = defaultServiceNetwork
	cl := newCiliumCluster(t, config, lanNode("k1", "192.168.1.11"), ciliumNode("k1", "10.100.0.0/24"))

	expectNoOverlap(t, cl.overlapCondition(), "Cilium pool 10.100.0.0/18")
}

// Before Cilium is installed the pool is the ClusterConfig's pod network:
// that is what to change.
func TestPodNetworkOverlap_BeforeTheInstallTheFixIsThePodNetwork(t *testing.T) {
	var config k4allv1alpha1.ClusterConfigSpec
	config.Cluster.PodNetwork = "10.0.0.0/16"
	cl := newCiliumCluster(t, config, lanNode("k1", "10.0.1.11"))

	expectOverlap(t, cl.overlapCondition(),
		"Cilium pool 10.0.0.0/16 overlaps the subnet 10.0.1.0/24 of node k1",
		"spec.cluster.podNetwork clear of these before Cilium is installed")
}

// Cilium runs the pool: the nodes hold CIDRs of an older, narrower pool
// inside it, and the next node to join may get the LAN's /24. Cilium does
// not support changing the CIDRs of a running pool (only adding new ones),
// so the message does not tell to: neither in componentOverrides nor in
// spec.cluster.podNetwork, which does not move a running pool anyway.
func TestPodNetworkOverlap_RunningPoolStays(t *testing.T) {
	cl := newCiliumCluster(t, k4allv1alpha1.ClusterConfigSpec{},
		ciliumConfigMap("10.0.0.0/8", "24"),
		lanNode("k1", "10.0.1.11"), ciliumNode("k1", "10.100.0.0/24"))

	cond := cl.overlapCondition()

	expectOverlap(t, cond,
		"Cilium does not support changing the CIDRs of a running pool",
		"k4all never changes it")
	for _, advice := range []string{"clusterPoolIPv4PodCIDRList", "podNetwork"} {
		if strings.Contains(cond.Message, advice) {
			t.Errorf("message %q tells to change the running pool (%s)", cond.Message, advice)
		}
	}
}

// The check runs at every reconcile: a node that joins later, and gets
// KubeVirt's masquerade network from the chart default pool, shows up.
func TestPodNetworkOverlap_NodesThatJoinLaterAreChecked(t *testing.T) {
	cl := newCiliumCluster(t, virtConfig(), ciliumConfigMap("10.100.0.0/18", "24"), ciliumNode("k1", "10.100.0.0/24"))
	if cond := cl.overlapCondition(); cond.Status != metav1.ConditionFalse {
		t.Fatalf("before the node joins: %s (%s): %s", cond.Status, cond.Reason, cond.Message)
	}

	if err := cl.c.Create(context.Background(), ciliumNode("k3", "10.0.2.0/24")); err != nil {
		t.Fatal(err)
	}

	expectOverlap(t, cl.overlapCondition(), "pod CIDR 10.0.2.0/24 of node k3 overlaps KubeVirt's masquerade network 10.0.2.0/24")
}

// listFails makes the List of kind fail with err.
func listFails(kind string, err error) interceptor.Funcs {
	return interceptor.Funcs{
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if list.GetObjectKind().GroupVersionKind().Kind == kind {
				return err
			}
			return c.List(ctx, list, opts...)
		},
	}
}

// Before Cilium is installed its CRDs are not there either: no node holds a
// CIDR yet, and the pool a new install gets is checked.
func TestPodNetworkOverlap_BeforeCiliumsCRDsThePoolIsChecked(t *testing.T) {
	noCRD := &meta.NoKindMatchError{GroupKind: schema.GroupKind{Group: "cilium.io", Kind: "CiliumNode"}}
	cl := newCiliumClusterWith(t, listFails("CiliumNodeList", noCRD), virtConfig())

	cond := cl.overlapCondition()

	if cond.Status != metav1.ConditionFalse || !strings.Contains(cond.Message, "Cilium pool 10.100.0.0/18") {
		t.Errorf("%s (%s): %s, want False for the pool 10.100.0.0/18", cond.Status, cond.Reason, cond.Message)
	}
}

// What cannot be read is not reported as clear.
func TestPodNetworkOverlap_UnreadableClusterIsUnknown(t *testing.T) {
	cl := newCiliumClusterWith(t, listFails("CiliumNodeList", errors.New("connection refused")), virtConfig())

	cond := cl.overlapCondition()

	if cond.Status != metav1.ConditionUnknown || cond.Reason != "CheckFailed" ||
		!strings.Contains(cond.Message, "connection refused") {
		t.Errorf("%s (%s): %s, want Unknown (CheckFailed) with the error", cond.Status, cond.Reason, cond.Message)
	}
}

// With Calico the condition is not there, also after a switch from Cilium.
func TestPodNetworkOverlap_NotReportedWithCalico(t *testing.T) {
	cl := newCluster(t,
		map[string]k4allv1alpha1.ComponentSpec{"probe": {Type: "unsupported", Version: "1"}}, nil)
	cc := &k4allv1alpha1.ClusterConfig{}
	if err := cl.c.Get(context.Background(), types.NamespacedName{Name: "k4all-cluster-config"}, cc); err != nil {
		t.Fatal(err)
	}
	meta.SetStatusCondition(&cc.Status.Conditions, metav1.Condition{
		Type: k4allv1alpha1.ConditionPodNetworkOverlap, Status: metav1.ConditionTrue, Reason: "OverlapFound", Message: "left by Cilium",
	})
	if err := cl.c.Status().Update(context.Background(), cc); err != nil {
		t.Fatal(err)
	}

	cl.reconcile()

	if cond := cl.condition(k4allv1alpha1.ConditionPodNetworkOverlap); cond != nil {
		t.Errorf("%s with Calico: %s (%s): %s", k4allv1alpha1.ConditionPodNetworkOverlap, cond.Status, cond.Reason, cond.Message)
	}
}
