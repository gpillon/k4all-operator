package hooks

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/go-logr/logr"
	k4allv1alpha1 "github.com/gpillon/k4all-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

// What virt-handler's device plugin makes the kubelet report for
// devices.kubevirt.io/kvm: 1k devices, allocatable while /dev/kvm exists.
const (
	withKVM    = "kvm"    // capacity 1k, allocatable 1k
	withoutKVM = "no-kvm" // capacity 1k, allocatable 0: the device is missing
	noHandler  = ""       // no resource: virt-handler has not run there
)

// virtNode is a node as the kubelet and virt-handler report it. schedulable is
// the kubevirt.io/schedulable label virt-handler sets once its device plugins
// are up.
func virtNode(name, kvm string, schedulable bool) *corev1.Node {
	n := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{}}}
	if schedulable || kvm != noHandler {
		n.Labels[schedulableLabel] = strconv.FormatBool(schedulable)
	}
	switch kvm {
	case withKVM:
		n.Status.Capacity = corev1.ResourceList{kvmResource: resource.MustParse("1k")}
		n.Status.Allocatable = corev1.ResourceList{kvmResource: resource.MustParse("1k")}
	case withoutKVM:
		n.Status.Capacity = corev1.ResourceList{kvmResource: resource.MustParse("1k")}
		n.Status.Allocatable = corev1.ResourceList{kvmResource: resource.MustParse("0")}
	}
	return n
}

// kubeVirtWithEmulation is the KubeVirt CR with useEmulation set, or unset
// when useEmulation is nil (as kubevirt-cr.yaml creates it).
func kubeVirtWithEmulation(useEmulation *bool) *unstructured.Unstructured {
	cr := &unstructured.Unstructured{}
	cr.SetAPIVersion("kubevirt.io/v1")
	cr.SetKind("KubeVirt")
	cr.SetNamespace("kubevirt")
	cr.SetName("kubevirt")
	dev := map[string]interface{}{"featureGates": []interface{}{}}
	if useEmulation != nil {
		dev["useEmulation"] = *useEmulation
	}
	cr.Object["spec"] = map[string]interface{}{"configuration": map[string]interface{}{"developerConfiguration": dev}}
	return cr
}

func emulationIs(v bool) *bool { return &v }

// readEmulation returns the CR's useEmulation, nil when unset.
func readEmulation(t *testing.T, c client.Client) *bool {
	t.Helper()
	cr := &unstructured.Unstructured{}
	cr.SetAPIVersion("kubevirt.io/v1")
	cr.SetKind("KubeVirt")
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "kubevirt", Name: "kubevirt"}, cr); err != nil {
		t.Fatal(err)
	}
	v, found, err := unstructured.NestedBool(cr.Object, "spec", "configuration", "developerConfiguration", "useEmulation")
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		return nil
	}
	return &v
}

func emulationString(v *bool) string {
	if v == nil {
		return "unset"
	}
	return strconv.FormatBool(*v)
}

func virtConfig(emulation string) k4allv1alpha1.ClusterConfigSpec {
	var cfg k4allv1alpha1.ClusterConfigSpec
	cfg.Features.Virt = k4allv1alpha1.VirtConfig{Enabled: true, Emulation: emulation}
	return cfg
}

func virtHookOn(cr client.Object, nodes ...*corev1.Node) (*VirtHook, client.Client) {
	objs := []client.Object{cr}
	for _, n := range nodes {
		objs = append(objs, n)
	}
	c := fake.NewClientBuilder().WithObjects(objs...).Build()
	return NewVirtHook(c, logr.Discard()), c
}

func postInstall(t *testing.T, h *VirtHook, emulation string) {
	t.Helper()
	if err := h.PostInstall(context.Background(), k4allv1alpha1.ComponentSpec{}, virtConfig(emulation)); err != nil {
		t.Fatal(err)
	}
}

func syncVirt(t *testing.T, h *VirtHook, emulation string) time.Duration {
	t.Helper()
	after, err := h.Sync(context.Background(), k4allv1alpha1.ComponentSpec{}, virtConfig(emulation))
	if err != nil {
		t.Fatal(err)
	}
	return after
}

func wantEmulation(t *testing.T, c client.Client, want *bool) {
	t.Helper()
	if got := readEmulation(t, c); emulationString(got) != emulationString(want) {
		t.Fatalf("useEmulation %s, want %s", emulationString(got), emulationString(want))
	}
}

// gpillon/k4all#57: auto looked for /dev/kvm in the operator's own pod, which
// mounts no host device, so every KubeVirt got useEmulation true. The nodes
// decide now.
func TestVirtHook_AutoUsesKVMWhenTheNodeHasIt(t *testing.T) {
	h, c := virtHookOn(kubeVirtWithEmulation(nil), virtNode("node1", withKVM, true))

	postInstall(t, h, "auto")

	wantEmulation(t, c, emulationIs(false))
}

func TestVirtHook_AutoEmulatesWhenNoNodeHasKVM(t *testing.T) {
	h, c := virtHookOn(kubeVirtWithEmulation(nil), virtNode("node1", withoutKVM, true))

	postInstall(t, h, "auto")

	wantEmulation(t, c, emulationIs(true))
}

// The default is auto.
func TestVirtHook_EmptyEmulationIsAuto(t *testing.T) {
	h, c := virtHookOn(kubeVirtWithEmulation(nil), virtNode("node1", withKVM, true))

	postInstall(t, h, "")

	wantEmulation(t, c, emulationIs(false))
}

// A cluster that got useEmulation true from an operator before v0.5.4 is
// corrected at the next reconcile: KubeVirt is not applied again.
func TestVirtHook_SyncTurnsTheOldEmulationOffWhereTheNodesHaveKVM(t *testing.T) {
	h, c := virtHookOn(kubeVirtWithEmulation(emulationIs(true)), virtNode("node1", withKVM, true))

	if after := syncVirt(t, h, "auto"); after != 0 {
		t.Errorf("Sync asks to come back in %v, want 0: the nodes have answered", after)
	}

	wantEmulation(t, c, emulationIs(false))
}

func TestVirtHook_SyncKeepsTheEmulationWhereNoNodeHasKVM(t *testing.T) {
	h, c := virtHookOn(kubeVirtWithEmulation(emulationIs(true)), virtNode("node1", withoutKVM, true))

	syncVirt(t, h, "auto")

	wantEmulation(t, c, emulationIs(true))
}

// On a fresh install PostInstall runs before virt-handler: nothing tells yet,
// so the CR is left as kubevirt-cr.yaml made it, and Sync comes back until
// virt-handler reports the nodes.
func TestVirtHook_AutoWaitsForVirtHandlerOnAFreshInstall(t *testing.T) {
	node := virtNode("node1", noHandler, false)
	h, c := virtHookOn(kubeVirtWithEmulation(nil), node)

	postInstall(t, h, "auto")
	wantEmulation(t, c, nil)
	after := syncVirt(t, h, "auto")
	wantEmulation(t, c, nil)
	if after <= 0 || after > time.Minute {
		t.Fatalf("Sync asks to come back in %v, want within a minute while virt-handler is not up", after)
	}

	// virt-handler starts: the node has no /dev/kvm.
	ready := virtNode("node1", withoutKVM, true)
	node.Labels = ready.Labels
	if err := c.Update(context.Background(), node); err != nil {
		t.Fatal(err)
	}
	node.Status = ready.Status
	if err := c.Status().Update(context.Background(), node); err != nil {
		t.Fatal(err)
	}
	if after := syncVirt(t, h, "auto"); after != 0 {
		t.Errorf("Sync asks to come back in %v, want 0: the node has answered", after)
	}
	wantEmulation(t, c, emulationIs(true))
}

func TestVirtHook_SyncSetsKVMOnAFreshInstallOnceVirtHandlerReports(t *testing.T) {
	h, c := virtHookOn(kubeVirtWithEmulation(nil), virtNode("node1", withKVM, true))

	syncVirt(t, h, "auto")

	wantEmulation(t, c, emulationIs(false))
}

// KubeVirt's useEmulation is cluster-wide: with KVM on some nodes, VMs run on
// those with KVM instead of everywhere emulated.
func TestVirtHook_AutoUsesKVMWhenSomeNodesHaveIt(t *testing.T) {
	h, c := virtHookOn(kubeVirtWithEmulation(emulationIs(true)),
		virtNode("node1", withoutKVM, true), virtNode("node2", withKVM, true), virtNode("node3", noHandler, false))

	syncVirt(t, h, "auto")

	wantEmulation(t, c, emulationIs(false))
}

// A virt-handler that crashes leaves its node with the kvm resource and no
// device allocatable until it is back. A reconcile in between must not turn
// emulation on for a cluster that has KVM.
func TestVirtHook_SyncDoesNotTurnEmulationOnWhenKVMGoesMissing(t *testing.T) {
	h, c := virtHookOn(kubeVirtWithEmulation(emulationIs(false)), virtNode("node1", withoutKVM, true))

	syncVirt(t, h, "auto")

	wantEmulation(t, c, emulationIs(false))
}

// Applying KubeVirt (a ClusterConfig change, a new version) decides again from
// what the nodes report: emulation "false" set back to auto on nodes with no
// KVM turns it on.
func TestVirtHook_PostInstallTurnsEmulationOnWhenNoNodeHasKVM(t *testing.T) {
	h, c := virtHookOn(kubeVirtWithEmulation(emulationIs(false)), virtNode("node1", withoutKVM, true))

	postInstall(t, h, "auto")

	wantEmulation(t, c, emulationIs(true))
}

// A node whose virt-handler is not up (kubevirt.io/schedulable not "true")
// does not count, even with a stale kvm resource.
func TestVirtHook_NodesWithoutVirtHandlerDoNotCount(t *testing.T) {
	h, c := virtHookOn(kubeVirtWithEmulation(nil),
		virtNode("gone", withKVM, false), virtNode("node1", withoutKVM, true))

	postInstall(t, h, "auto")

	wantEmulation(t, c, emulationIs(true))
}

func TestVirtHook_ExplicitEmulationKeepsItsMeaning(t *testing.T) {
	for _, tc := range []struct {
		emulation string
		node      string
		want      bool
	}{
		{"true", withKVM, true},
		{"false", withoutKVM, false},
		{"true", noHandler, true},
		{"false", noHandler, false},
	} {
		t.Run(tc.emulation+"/"+tc.node, func(t *testing.T) {
			h, c := virtHookOn(kubeVirtWithEmulation(nil), virtNode("node1", tc.node, tc.node != noHandler))
			postInstall(t, h, tc.emulation)
			wantEmulation(t, c, &tc.want)

			// Sync puts back a value changed by hand.
			cr := kubeVirtWithEmulation(emulationIs(!tc.want))
			cr.SetResourceVersion(currentResourceVersion(t, c))
			if err := c.Update(context.Background(), cr); err != nil {
				t.Fatal(err)
			}
			if after := syncVirt(t, h, tc.emulation); after != 0 {
				t.Errorf("Sync asks to come back in %v, want 0", after)
			}
			wantEmulation(t, c, &tc.want)
		})
	}
}

func currentResourceVersion(t *testing.T, c client.Client) string {
	t.Helper()
	cr := &unstructured.Unstructured{}
	cr.SetAPIVersion("kubevirt.io/v1")
	cr.SetKind("KubeVirt")
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "kubevirt", Name: "kubevirt"}, cr); err != nil {
		t.Fatal(err)
	}
	return cr.GetResourceVersion()
}

// Sync runs on every reconcile, also before virt-operator has created the
// KubeVirt CRD or the CR: it waits, it does not fail.
func TestVirtHook_SyncWaitsForTheKubeVirtCR(t *testing.T) {
	noCRD := interceptor.Funcs{Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
		return &meta.NoKindMatchError{GroupKind: schema.GroupKind{Group: "kubevirt.io", Kind: "KubeVirt"}}
	}}
	for name, c := range map[string]client.Client{
		"no CR":  fake.NewClientBuilder().WithObjects(virtNode("node1", withKVM, true)).Build(),
		"no CRD": fake.NewClientBuilder().WithInterceptorFuncs(noCRD).Build(),
	} {
		t.Run(name, func(t *testing.T) {
			after, err := NewVirtHook(c, logr.Discard()).Sync(context.Background(), k4allv1alpha1.ComponentSpec{}, virtConfig("auto"))
			if err != nil || after <= 0 {
				t.Fatalf("got (%v, %v), want to come back later without an error", after, err)
			}
		})
	}
}

func TestVirtHook_IsASyncer(t *testing.T) {
	if _, ok := NewRegistry(fake.NewClientBuilder().Build(), logr.Discard()).Get("kubevirt").(Syncer); !ok {
		t.Fatal("the kubevirt hook does not implement Syncer: emulation auto would never be re-checked")
	}
}
