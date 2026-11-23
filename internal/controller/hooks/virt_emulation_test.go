package hooks

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/go-logr/logr"
	k4allv1alpha1 "github.com/gpillon/k4all-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// virtNode is a node as the kubelet and virt-handler report it:
//   - kvmAvailable: virt-handler is up, devices.kubevirt.io/kvm capacity and
//     allocatable 1k (/dev/kvm exists);
//   - kvmMissing: virt-handler is up, capacity 1k and allocatable 0 (no
//     /dev/kvm, or virt-handler crashed and the kubelet marked its devices
//     unhealthy);
//   - kvmUnknown: virt-handler has not run there.
func virtNode(name string, kvm kvmOnNodes) *corev1.Node {
	n := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{}}}
	setVirtHandler(n, kvm)
	return n
}

func setVirtHandler(n *corev1.Node, kvm kvmOnNodes) {
	n.Status = corev1.NodeStatus{}
	delete(n.Labels, schedulableLabel)
	allocatable := map[kvmOnNodes]string{kvmAvailable: "1k", kvmMissing: "0"}[kvm]
	if allocatable == "" {
		return
	}
	if n.Labels == nil {
		n.Labels = map[string]string{}
	}
	n.Labels[schedulableLabel] = strconv.FormatBool(true)
	n.Status.Capacity = corev1.ResourceList{kvmResource: resource.MustParse("1k")}
	n.Status.Allocatable = corev1.ResourceList{kvmResource: resource.MustParse(allocatable)}
}

// kubeVirtWithEmulation is the KubeVirt CR with useEmulation set, or unset
// when useEmulation is nil (as kubevirt-cr.yaml creates it).
func kubeVirtWithEmulation(useEmulation *bool) *unstructured.Unstructured {
	cr := &unstructured.Unstructured{}
	cr.SetAPIVersion(kubeVirtAPIVersion)
	cr.SetKind(kubeVirtKind)
	cr.SetNamespace(kubeVirtKey.Namespace)
	cr.SetName(kubeVirtKey.Name)
	dev := map[string]interface{}{"featureGates": []interface{}{}}
	if useEmulation != nil {
		dev["useEmulation"] = *useEmulation
	}
	cr.Object["spec"] = map[string]interface{}{"configuration": map[string]interface{}{"developerConfiguration": dev}}
	return cr
}

func emulationIs(v bool) *bool { return &v }

// virtCluster is a fake API server with KubeVirt and nodes, and the VirtHook
// on a clock the test moves.
type virtCluster struct {
	t     *testing.T
	c     client.Client
	h     *VirtHook
	clock time.Time
}

func newVirtCluster(t *testing.T, c client.Client) *virtCluster {
	vc := &virtCluster{t: t, c: c, clock: time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)}
	vc.h = NewVirtHook(c, logr.Discard())
	vc.h.now = func() time.Time { return vc.clock }
	return vc
}

func virtClusterWith(t *testing.T, cr client.Object, nodes ...*corev1.Node) *virtCluster {
	objs := []client.Object{cr}
	for _, n := range nodes {
		objs = append(objs, n)
	}
	return newVirtCluster(t, fake.NewClientBuilder().WithObjects(objs...).Build())
}

// sync moves the clock by elapsed and runs Sync with features.virt.emulation
// set to mode. It returns how soon Sync wants to run again.
func (vc *virtCluster) sync(mode string, elapsed time.Duration) time.Duration {
	vc.t.Helper()
	vc.clock = vc.clock.Add(elapsed)
	var cfg k4allv1alpha1.ClusterConfigSpec
	cfg.Features.Virt = k4allv1alpha1.VirtConfig{Enabled: true, Emulation: mode}
	after, err := vc.h.Sync(context.Background(), k4allv1alpha1.ComponentSpec{}, cfg)
	if err != nil {
		vc.t.Fatal(err)
	}
	return after
}

// syncAutoEvery runs Sync with emulation auto every 30 seconds for d, as the
// reconciler does while Sync asks for it.
func (vc *virtCluster) syncAutoEvery(d time.Duration) {
	vc.t.Helper()
	for elapsed := time.Duration(0); elapsed < d; elapsed += emulationRecheckInterval {
		vc.sync("auto", emulationRecheckInterval)
	}
}

// setNode1 changes what virt-handler reports on node1.
func (vc *virtCluster) setNode1(kvm kvmOnNodes) {
	vc.t.Helper()
	n := &corev1.Node{}
	if err := vc.c.Get(context.Background(), client.ObjectKey{Name: "node1"}, n); err != nil {
		vc.t.Fatal(err)
	}
	setVirtHandler(n, kvm)
	status := n.Status
	if err := vc.c.Update(context.Background(), n); err != nil {
		vc.t.Fatal(err)
	}
	n.Status = status
	if err := vc.c.Status().Update(context.Background(), n); err != nil {
		vc.t.Fatal(err)
	}
}

func (vc *virtCluster) kubeVirt() *unstructured.Unstructured {
	vc.t.Helper()
	cr, found, err := getOperatorCR(context.Background(), vc.c, kubeVirtAPIVersion, kubeVirtKind, kubeVirtKey)
	if err != nil || !found {
		vc.t.Fatalf("KubeVirt CR: found %t, %v", found, err)
	}
	return cr
}

// emulation is the CR's useEmulation: "true", "false", or "unset".
func (vc *virtCluster) emulation() string {
	vc.t.Helper()
	v, found, err := unstructured.NestedBool(vc.kubeVirt().Object, useEmulationPath...)
	if err != nil {
		vc.t.Fatal(err)
	}
	if !found {
		return "unset"
	}
	return strconv.FormatBool(v)
}

func (vc *virtCluster) wantEmulation(want, when string) {
	vc.t.Helper()
	if got := vc.emulation(); got != want {
		vc.t.Fatalf("%s: useEmulation %s, want %s", when, got, want)
	}
}

// gpillon/k4all#57: auto looked for /dev/kvm in the operator's own pod, which
// mounts no host device, so every KubeVirt got useEmulation true. The nodes
// decide now, and a cluster upgraded from v0.5.3 is corrected without
// applying KubeVirt again.
func TestVirtHook_AutoTurnsEmulationOffOnceANodeKeepsOfferingKVM(t *testing.T) {
	vc := virtClusterWith(t, kubeVirtWithEmulation(emulationIs(true)), virtNode("node1", kvmAvailable))

	if after := vc.sync("auto", 0); after != kvmSettle {
		t.Errorf("first Sync asks to come back in %v, want %v to confirm KVM", after, kvmSettle)
	}
	vc.wantEmulation("true", "KVM seen once")

	if after := vc.sync("auto", kvmSettle); after != 0 {
		t.Errorf("Sync asks to come back in %v, want 0: nothing left to do", after)
	}
	vc.wantEmulation("false", "KVM seen for 30s")
}

func TestVirtHook_EmptyEmulationIsAuto(t *testing.T) {
	vc := virtClusterWith(t, kubeVirtWithEmulation(emulationIs(true)), virtNode("node1", kvmAvailable))

	vc.sync("", 0)
	vc.sync("", kvmSettle)

	vc.wantEmulation("false", "KVM seen for 30s")
}

// No KVM on any node (kind on a runner without /dev/kvm, a VM without nested
// virtualization): emulation goes on once that has held for 10 minutes.
func TestVirtHook_AutoTurnsEmulationOnAfterTenMinutesWithoutKVM(t *testing.T) {
	vc := virtClusterWith(t, kubeVirtWithEmulation(nil), virtNode("node1", kvmMissing))

	if after := vc.sync("auto", 0); after != emulationRecheckInterval {
		t.Errorf("Sync asks to come back in %v, want %v while the nodes settle", after, emulationRecheckInterval)
	}
	vc.syncAutoEvery(noKVMSettle - emulationRecheckInterval)
	vc.wantEmulation("unset", "no KVM for 9m30s")

	vc.sync("auto", emulationRecheckInterval)
	vc.wantEmulation("true", "no KVM for 10m")
}

// virt-handler's kvm plugin reports its devices healthy for an instant when
// it registers on a node without /dev/kvm: one report of KVM changes nothing.
func TestVirtHook_ABriefKVMReportDoesNotTurnEmulationOff(t *testing.T) {
	vc := virtClusterWith(t, kubeVirtWithEmulation(emulationIs(true)), virtNode("node1", kvmAvailable))
	vc.sync("auto", 0)

	vc.setNode1(kvmMissing)
	vc.syncAutoEvery(time.Hour)

	vc.wantEmulation("true", "KVM reported once, then never again")
}

// A node with /dev/kvm whose virt-handler crashed has nothing allocatable
// until virt-handler is back: that never turns emulation on, however often
// it happens.
func TestVirtHook_ACrashedVirtHandlerDoesNotTurnEmulationOn(t *testing.T) {
	vc := virtClusterWith(t, kubeVirtWithEmulation(emulationIs(false)), virtNode("node1", kvmAvailable))
	vc.sync("auto", 0)

	for range 3 {
		vc.setNode1(kvmMissing)
		vc.syncAutoEvery(9 * time.Minute)
		vc.setNode1(kvmAvailable)
		vc.sync("auto", emulationRecheckInterval)
	}

	vc.wantEmulation("false", "three virt-handler outages of 9 minutes")
}

// useEmulation is cluster-wide: with KVM on some nodes, VMs run on those
// rather than everywhere emulated.
func TestVirtHook_AutoUsesKVMWhenSomeNodesHaveIt(t *testing.T) {
	vc := virtClusterWith(t, kubeVirtWithEmulation(emulationIs(true)),
		virtNode("node1", kvmMissing), virtNode("node2", kvmAvailable), virtNode("node3", kvmUnknown))

	vc.sync("auto", 0)
	vc.sync("auto", kvmSettle)

	vc.wantEmulation("false", "KVM on one node of three")
}

// The KVM nodes of a mixed cluster leave: the others run VMs in emulation.
func TestVirtHook_AutoTurnsEmulationOnWhenTheKVMNodesLeave(t *testing.T) {
	vc := virtClusterWith(t, kubeVirtWithEmulation(emulationIs(false)),
		virtNode("node1", kvmMissing), virtNode("node2", kvmAvailable))
	vc.sync("auto", 0)

	if err := vc.c.Delete(context.Background(), virtNode("node2", kvmAvailable)); err != nil {
		t.Fatal(err)
	}
	vc.sync("auto", emulationRecheckInterval)
	vc.syncAutoEvery(noKVMSettle)

	vc.wantEmulation("true", "the KVM node left 10 minutes ago")
}

// On a fresh install nothing reports until virt-handler runs: the CR is left
// as kubevirt-cr.yaml made it. Sync comes back every 30 seconds for 10
// minutes, then only with the periodic reconcile.
func TestVirtHook_AutoWaitsForVirtHandler(t *testing.T) {
	vc := virtClusterWith(t, kubeVirtWithEmulation(nil), virtNode("node1", kvmUnknown))

	if after := vc.sync("auto", 0); after != emulationRecheckInterval {
		t.Errorf("Sync asks to come back in %v, want %v while nothing reports", after, emulationRecheckInterval)
	}
	vc.syncAutoEvery(unreportedRecheckWindow - emulationRecheckInterval)
	if after := vc.sync("auto", emulationRecheckInterval); after != 0 {
		t.Errorf("after 10 minutes without reports Sync asks to come back in %v, want 0 (the periodic reconcile)", after)
	}
	vc.wantEmulation("unset", "nothing reports")

	vc.setNode1(kvmMissing)
	if after := vc.sync("auto", 5*time.Minute); after != emulationRecheckInterval {
		t.Errorf("virt-handler is up: Sync asks to come back in %v, want %v", after, emulationRecheckInterval)
	}
	vc.syncAutoEvery(noKVMSettle)
	vc.wantEmulation("true", "virt-handler up for 10 minutes on a node without KVM")
}

// Sync runs on every reconcile, also before virt-operator has created the
// KubeVirt CRD or the CR: it waits, it does not fail.
func TestVirtHook_SyncWaitsForTheKubeVirtCR(t *testing.T) {
	for name, c := range map[string]client.Client{
		"no CR":  fake.NewClientBuilder().WithObjects(virtNode("node1", kvmAvailable)).Build(),
		"no CRD": noKubeVirtCRD(),
	} {
		t.Run(name, func(t *testing.T) {
			vc := newVirtCluster(t, c)
			if after := vc.sync("auto", 0); after != emulationRecheckInterval {
				t.Errorf("Sync asks to come back in %v, want %v", after, emulationRecheckInterval)
			}
			vc.syncAutoEvery(unreportedRecheckWindow - emulationRecheckInterval)
			if after := vc.sync("auto", emulationRecheckInterval); after != 0 {
				t.Errorf("after 10 minutes Sync asks to come back in %v, want 0 (the periodic reconcile)", after)
			}
		})
	}
}

// A node whose virt-handler is down (kubevirt.io/schedulable "false") does not
// count, even with a stale kvm resource.
func TestVirtHook_NodesWithoutVirtHandlerDoNotCount(t *testing.T) {
	gone := virtNode("gone", kvmAvailable)
	gone.Labels[schedulableLabel] = strconv.FormatBool(false)
	vc := virtClusterWith(t, kubeVirtWithEmulation(nil), gone, virtNode("node1", kvmMissing))

	vc.sync("auto", 0)
	vc.syncAutoEvery(noKVMSettle)

	vc.wantEmulation("true", "KVM only on a node whose virt-handler is down")
}

// The ClusterConfig owns useEmulation: "true" and "false" are set at once,
// whatever the nodes report, and a value changed by hand is put back.
func TestVirtHook_ExplicitEmulationIsSetAndKept(t *testing.T) {
	for _, tc := range []struct {
		on   bool
		node kvmOnNodes
	}{
		{true, kvmAvailable},
		{false, kvmMissing},
		{true, kvmUnknown},
		{false, kvmUnknown},
	} {
		mode := strconv.FormatBool(tc.on)
		t.Run(mode+"/"+tc.node.String(), func(t *testing.T) {
			vc := virtClusterWith(t, kubeVirtWithEmulation(emulationIs(!tc.on)), virtNode("node1", tc.node))
			if after := vc.sync(mode, 0); after != 0 {
				t.Errorf("Sync asks to come back in %v, want 0", after)
			}
			vc.wantEmulation(mode, "first Sync")

			cr := vc.kubeVirt()
			if err := unstructured.SetNestedField(cr.Object, !tc.on, useEmulationPath...); err != nil {
				t.Fatal(err)
			}
			if err := vc.c.Update(context.Background(), cr); err != nil {
				t.Fatal(err)
			}
			vc.sync(mode, time.Minute)
			vc.wantEmulation(mode, "Sync after an edit by hand")
		})
	}
}

// Switching away from auto and back starts the wait again.
func TestVirtHook_BackToAutoWaitsAgain(t *testing.T) {
	vc := virtClusterWith(t, kubeVirtWithEmulation(nil), virtNode("node1", kvmMissing))
	vc.syncAutoEvery(noKVMSettle - time.Minute)

	vc.sync("false", emulationRecheckInterval)
	vc.syncAutoEvery(time.Minute)
	vc.wantEmulation("unset", "one minute back on auto")

	vc.syncAutoEvery(noKVMSettle)
	vc.wantEmulation("true", "eleven minutes back on auto")
}

func TestRegistry_KubeVirtHookIsASyncer(t *testing.T) {
	if _, ok := NewRegistry(fake.NewClientBuilder().Build(), logr.Discard()).Get("kubevirt").(Syncer); !ok {
		t.Fatal("the kubevirt hook is not a Syncer: emulation auto would never follow the nodes")
	}
}
