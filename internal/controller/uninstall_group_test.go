package controller

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	k4allv1alpha1 "github.com/gpillon/k4all-operator/api/v1alpha1"
	"github.com/gpillon/k4all-operator/internal/controller/hooks"
)

// gatedComponent is a component of the feature gate gate that depends on
// deps, as an inline manifest of one ConfigMap named after it
// (componentObject), so it applies and goes without network access.
func gatedComponent(name, gate string, deps ...string) k4allv1alpha1.ComponentSpec {
	return k4allv1alpha1.ComponentSpec{
		Type: k4allv1alpha1.ComponentTypeManifest, Version: "v1", FeatureGate: gate, DependsOn: deps,
		InlineManifests: []string{fmt.Sprintf(`apiVersion: v1
kind: ConfigMap
metadata:
  name: %s
  namespace: default
`, componentObjectName(name))},
	}
}

func componentObjectName(name string) string { return "component-" + name }

// componentObject is the ConfigMap that stands for the objects of the
// component name (gatedComponent).
func componentObject(name string) *corev1.ConfigMap {
	return &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: componentObjectName(name), Namespace: "default"}}
}

// virtGroup is the virt feature as the k4all v2.7.0 release has it: its
// components, their featureGate and dependsOn.
func virtGroup() map[string]k4allv1alpha1.ComponentSpec {
	return map[string]k4allv1alpha1.ComponentSpec{
		"kubevirt":          gatedComponent("kubevirt", "virt"),
		"cdi":               gatedComponent("cdi", "virt", "kubevirt"),
		"virtctl":           gatedComponent("virtctl", "virt", "kubevirt"),
		"hangar":            gatedComponent("hangar", "virt", "kubevirt", "cdi"),
		"hangar-node-agent": gatedComponent("hangar-node-agent", "virt", "kubevirt"),
	}
}

// rookGroup is the rook feature as the k4all v2.7.0 release has it.
func rookGroup() map[string]k4allv1alpha1.ComponentSpec {
	return map[string]k4allv1alpha1.ComponentSpec{
		"rook-ceph":        gatedComponent("rook-ceph", "rook"),
		"ceph-csi-drivers": gatedComponent("ceph-csi-drivers", "rook", "rook-ceph"),
		"cairn":            gatedComponent("cairn", "rook"),
	}
}

// groupsOnEnvtest is a cluster with every feature off where the components
// are Installed at v1, with their objects (componentObject), and the
// reconciler gives each its built-in hook (standInHooks).
func groupsOnEnvtest(t *testing.T, components map[string]k4allv1alpha1.ComponentSpec) (client.Client, *ReleaseReconciler) {
	t.Helper()
	cfg := envtestConfig(t)
	s := gateScheme(t)
	c, err := client.New(cfg, client.Options{Scheme: s})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	cc := &k4allv1alpha1.ClusterConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "k4all-cluster-config"},
		Spec: k4allv1alpha1.ClusterConfigSpec{
			Networking: k4allv1alpha1.NetworkingConfig{CNI: k4allv1alpha1.CNIConfig{Type: "calico"}},
		},
	}
	rm := &k4allv1alpha1.ReleaseManifest{
		ObjectMeta: metav1.ObjectMeta{Name: "k4all"},
		Spec:       k4allv1alpha1.ReleaseManifestSpec{Components: components},
	}
	for _, obj := range []client.Object{cc, rm} {
		if err := c.Create(ctx, obj); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = c.Delete(context.Background(), obj) })
	}
	rm.Status.Components = map[string]k4allv1alpha1.ComponentStatus{}
	for name := range components {
		rm.Status.Components[name] = k4allv1alpha1.ComponentStatus{
			State: k4allv1alpha1.ComponentStateInstalled, InstalledVersion: "v1", LastTransitionTime: metav1.Now(),
		}
		createAll(t, c, componentObject(name))
	}
	if err := c.Status().Update(ctx, rm); err != nil {
		t.Fatal(err)
	}
	r := &ReleaseReconciler{Client: c, Scheme: s, RestConfig: cfg, Log: logr.Discard()}
	standInHooks(t, r)
	return c, r
}

// standInRemover stands in for the Remover of a built-in hook, its
// UninstallGuard and Cleanup kept: snapshot-controller's Remover deletes
// the named objects of upstream's deploy files, which the stand-in
// component does not have. It deletes the
// component's stand-in ConfigMap (componentObject) through the reconciler's
// client, so the delete recorder (recordDeletes) sees the Remover at its
// place in the removal order, and counts its calls (removals): the engine,
// which would delete the same ConfigMap, does not run for a Remover.
type standInRemover struct {
	hooks.Hook
	name    string
	client  client.Client
	removed int
}

func (s *standInRemover) UninstallBlocked(ctx context.Context, name string) (string, error) {
	if guard, ok := s.Hook.(hooks.UninstallGuard); ok {
		return guard.UninstallBlocked(ctx, name)
	}
	return "", nil
}

func (s *standInRemover) Remove(ctx context.Context, _ k4allv1alpha1.ComponentSpec, _ k4allv1alpha1.ClusterConfigSpec) error {
	s.removed++
	return client.IgnoreNotFound(s.client.Delete(ctx, componentObject(s.name)))
}

// standInHooks gives r the built-in hooks on its client, with
// snapshot-controller's Remover as standInRemover. hangar-node-agent keeps
// its own: it deletes the objects of the component's manifests, inline ones
// included, so the stand-in ConfigMap (builtInRemover).
func standInHooks(t *testing.T, r *ReleaseReconciler) {
	t.Helper()
	r.hookReg = hooks.NewRegistry(r.Client, r.Log, hooks.WithRestConfig(r.RestConfig))
	hook := r.hookReg.Get("snapshot-controller")
	if _, ok := hook.(hooks.Remover); !ok {
		t.Fatalf("snapshot-controller's built-in hook is %T, no Remover: standInHooks stands in for one", hook)
	}
	r.hookReg.Register("snapshot-controller", &standInRemover{Hook: hook, name: "snapshot-controller", client: r.Client})
}

// builtInRemover fails the test unless the component name has its built-in
// hook and it is a Remover: the reconciler removes the component through it,
// never through the engine.
func builtInRemover(t *testing.T, r *ReleaseReconciler, name string) {
	t.Helper()
	hook := r.hookReg.Get(name)
	if _, standIn := hook.(*standInRemover); standIn {
		t.Fatalf("%s's hook is the stand-in, want its built-in Remover", name)
	}
	if _, ok := hook.(hooks.Remover); !ok {
		t.Fatalf("%s's hook is %T, want a Remover", name, hook)
	}
}

// recordDeletes makes the reconciler's client record, in order, the
// components whose objects it deletes (by componentObject) and the KubeVirt
// and CDI CRs it deletes (by kind).
func recordDeletes(t *testing.T, r *ReleaseReconciler) *[]string {
	t.Helper()
	watching, err := client.NewWithWatch(envtestConfig(t), client.Options{Scheme: gateScheme(t)})
	if err != nil {
		t.Fatal(err)
	}
	var deleted []string
	r.Client = interceptor.NewClient(watching, interceptor.Funcs{
		Delete: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			kind := obj.GetObjectKind().GroupVersionKind().Kind
			switch {
			case kind == kubeVirtCR.gvk.Kind || kind == cdiCR.gvk.Kind:
				deleted = append(deleted, kind+" CR")
			case strings.HasPrefix(obj.GetName(), "component-"):
				deleted = append(deleted, strings.TrimPrefix(obj.GetName(), "component-"))
			}
			return cl.Delete(ctx, obj, opts...)
		},
	})
	standInHooks(t, r)
	return &deleted
}

// removals is how many times the stand-in Remover of the component name
// (standInHooks) removed it.
func removals(t *testing.T, r *ReleaseReconciler, name string) int {
	t.Helper()
	stand, ok := r.hookReg.Get(name).(*standInRemover)
	if !ok {
		t.Fatalf("%s's hook is %T, want the stand-in Remover", name, r.hookReg.Get(name))
	}
	return stand.removed
}

// assertKept fails the test unless every component of names is Installed
// at v1 with the message want, its objects still there.
func assertKept(t *testing.T, c client.Client, want string, names ...string) {
	t.Helper()
	for _, name := range names {
		if err := c.Get(context.Background(), client.ObjectKeyFromObject(componentObject(name)), &corev1.ConfigMap{}); err != nil {
			t.Errorf("%s's objects: %v, want them kept", name, err)
		}
		st := componentStatus(t, c, name)
		if st.State != k4allv1alpha1.ComponentStateInstalled || st.InstalledVersion != "v1" || st.Message != want {
			t.Errorf("%s: %s at %q (%q), want Installed at v1 (%q)", name, st.State, st.InstalledVersion, st.Message, want)
		}
	}
}

// assertGone fails the test unless every component of names went: its
// objects deleted, its status Skipped.
func assertGone(t *testing.T, c client.Client, names ...string) {
	t.Helper()
	for _, name := range names {
		err := c.Get(context.Background(), client.ObjectKeyFromObject(componentObject(name)), &corev1.ConfigMap{})
		if !apierrors.IsNotFound(err) {
			t.Errorf("%s's objects: get = %v, want them uninstalled", name, err)
		}
		if st := componentStatus(t, c, name); st.State != k4allv1alpha1.ComponentStateSkipped {
			t.Errorf("%s: %s (%s), want Skipped", name, st.State, st.Message)
		}
	}
}

// gpillon/k4all#96: virt off with a VM on a container disk. KubeVirt
// refuses (a VM exists), CDI has no DataVolume to refuse for, virtctl and
// Hangar have no guard: v0.5.7 removed all of them but KubeVirt, and the
// VM ran on with nothing to import a disk. The feature is one unit: while
// any of its components refuses, all of them stay, each saying who refuses
// and why; nothing is deleted, not even the CDI CR.
func TestReconcile_AFeatureStaysWholeWhileOneOfItsComponentsRefuses(t *testing.T) {
	virtWorkloadAPIs(t)
	c, r := groupsOnEnvtest(t, virtGroup())
	runningOperatorCR(t, c, kubeVirtCR, kubeVirtFinalizer)
	runningOperatorCR(t, c, cdiCR, cdiFinalizer)
	vm := kubevirtObject("VirtualMachine", "vm1")
	createAll(t, c, vm)
	deleted := recordDeletes(t, r)

	res := reconcileProbe(t, r)

	if len(*deleted) > 0 {
		t.Errorf("deleted %v while kubevirt refuses, want nothing", *deleted)
	}
	refused := "uninstall refused: kubevirt: 1 VirtualMachine (default/vm1) exists: " +
		"delete it or set kubevirt Unmanaged first"
	assertKept(t, c, refused, "kubevirt", "cdi", "virtctl", "hangar", "hangar-node-agent")
	if cdiCR.deleting(t, c) || kubeVirtCR.deleting(t, c) {
		t.Error("the CDI or KubeVirt CR is being deleted")
	}
	ready := readyCondition(t, c)
	if ready.Status != metav1.ConditionFalse || ready.Reason != "UninstallRefused" ||
		!strings.HasSuffix(ready.Message, ": cdi, hangar, hangar-node-agent, kubevirt, virtctl") {
		t.Errorf("Ready %s (%s: %s), want False, UninstallRefused, naming every component kept", ready.Status, ready.Reason, ready.Message)
	}
	if res.RequeueAfter <= 0 || res.RequeueAfter > time.Minute {
		t.Errorf("RequeueAfter %s with the uninstall refused, want at most a minute", res.RequeueAfter)
	}
	// hangar-node-agent's Remover would have deleted its objects (assertKept).
	builtInRemover(t, r, "hangar-node-agent")

	if err := deleteNow(c, vm); err != nil {
		t.Fatal(err)
	}
	reconcileProbe(t, r)

	assertGone(t, c, "virtctl", "hangar", "hangar-node-agent")
	if st := componentStatus(t, c, "cdi"); st.State != k4allv1alpha1.ComponentStateUninstalling {
		t.Errorf("cdi: %s (%s), want Uninstalling once nothing refuses", st.State, st.Message)
	}
}

// Every component that refuses is named, KubeVirt (what the others run on)
// first: the user can clear them all at once.
func TestReconcile_AFeatureHeldByTwoComponentsNamesBoth(t *testing.T) {
	virtWorkloadAPIs(t)
	c, r := groupsOnEnvtest(t, virtGroup())
	dv := &unstructured.Unstructured{}
	dv.SetAPIVersion("cdi.kubevirt.io/v1beta1")
	dv.SetKind("DataVolume")
	dv.SetNamespace("default")
	dv.SetName("disk")
	createAll(t, c, kubevirtObject("VirtualMachine", "vm1"), dv)

	reconcileProbe(t, r)

	assertKept(t, c, "uninstall refused: "+
		"kubevirt: 1 VirtualMachine (default/vm1) exists: delete it or set kubevirt Unmanaged first; "+
		"cdi: 1 DataVolume (default/disk) exists: delete it or set cdi Unmanaged first",
		"kubevirt", "cdi", "virtctl", "hangar", "hangar-node-agent")
}

// Once nothing refuses, a feature's components go in reverse dependency
// order, one after the other: the consoles first, then CDI, then KubeVirt.
// One waits, Installed and untouched, while the one before it is being
// uninstalled: KubeVirt's CR is deleted only once CDI is gone.
func TestReconcile_AFeatureGoesInReverseDependencyOrder(t *testing.T) {
	c, r := groupsOnEnvtest(t, virtGroup())
	runningOperatorCR(t, c, kubeVirtCR, kubeVirtFinalizer)
	runningOperatorCR(t, c, cdiCR, cdiFinalizer)
	deleted := recordDeletes(t, r)

	reconcileProbe(t, r)

	// hangar-node-agent goes through its own Remover, at its place in the
	// order.
	builtInRemover(t, r, "hangar-node-agent")
	if want := []string{"hangar", "hangar-node-agent", "virtctl", "CDI CR"}; !slices.Equal(*deleted, want) {
		t.Fatalf("deleted %v, want %v", *deleted, want)
	}
	assertGone(t, c, "hangar", "hangar-node-agent", "virtctl")
	if st := componentStatus(t, c, "cdi"); st.State != k4allv1alpha1.ComponentStateUninstalling {
		t.Fatalf("cdi: %s (%s), want Uninstalling", st.State, st.Message)
	}
	assertKept(t, c, "uninstall queued: waiting for cdi to be uninstalled first", "kubevirt")
	if kubeVirtCR.deleting(t, c) {
		t.Fatal("the KubeVirt CR is deleted while CDI is being uninstalled")
	}
	if ready := readyCondition(t, c); ready.Status != metav1.ConditionFalse || ready.Reason != "ComponentsUninstalling" ||
		!strings.HasSuffix(ready.Message, ": cdi, kubevirt") {
		t.Errorf("Ready %s (%s: %s), want False, ComponentsUninstalling: cdi, kubevirt", ready.Status, ready.Reason, ready.Message)
	}

	*deleted = nil
	cdiCR.operatorDone(t, c)
	reconcileProbe(t, r)

	if want := []string{"cdi", "KubeVirt CR"}; !slices.Equal(*deleted, want) {
		t.Fatalf("deleted %v, want %v", *deleted, want)
	}
	assertGone(t, c, "cdi")
	if st := componentStatus(t, c, "kubevirt"); st.State != k4allv1alpha1.ComponentStateUninstalling {
		t.Fatalf("kubevirt: %s (%s), want Uninstalling", st.State, st.Message)
	}

	*deleted = nil
	kubeVirtCR.operatorDone(t, c)
	reconcileProbe(t, r)

	if want := []string{"kubevirt"}; !slices.Equal(*deleted, want) {
		t.Fatalf("deleted %v, want %v", *deleted, want)
	}
	assertGone(t, c, "kubevirt")
}

// Rook: Cairn, then the CSI drivers, then the Rook operator; held whole
// while a CephCluster exists (rook-ceph and ceph-csi-drivers both refuse).
func TestReconcile_RookGoesCairnFirstOnceNoCephClusterIsLeft(t *testing.T) {
	c, r := groupsOnEnvtest(t, rookGroup())
	withCephClusterCRD(t, c)
	cluster := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "ceph.rook.io/v1",
		"kind":       "CephCluster",
		"metadata":   map[string]interface{}{"name": "rook-ceph", "namespace": metav1.NamespaceDefault},
		"spec": map[string]interface{}{
			"cephVersion":     map[string]interface{}{"image": "quay.io/ceph/ceph:v20.2.4"},
			"dataDirHostPath": "/var/lib/rook",
		},
	}}
	createAll(t, c, cluster)
	deleted := recordDeletes(t, r)

	reconcileProbe(t, r)

	if len(*deleted) > 0 {
		t.Errorf("deleted %v while a CephCluster exists, want nothing", *deleted)
	}
	for _, name := range []string{"rook-ceph", "ceph-csi-drivers", "cairn"} {
		st := componentStatus(t, c, name)
		if st.State != k4allv1alpha1.ComponentStateInstalled ||
			!strings.HasPrefix(st.Message, "uninstall refused: rook-ceph: CephCluster default/rook-ceph exists") ||
			!strings.Contains(st.Message, "; ceph-csi-drivers: CephCluster default/rook-ceph exists") {
			t.Errorf("%s: %s (%s), want Installed, refused by rook-ceph and ceph-csi-drivers", name, st.State, st.Message)
		}
	}

	if err := deleteNow(c, cluster); err != nil {
		t.Fatal(err)
	}
	reconcileProbe(t, r)

	if want := []string{"cairn", "ceph-csi-drivers", "rook-ceph"}; !slices.Equal(*deleted, want) {
		t.Fatalf("deleted %v, want %v", *deleted, want)
	}
	assertGone(t, c, "cairn", "ceph-csi-drivers", "rook-ceph")
}

// Groups are the components that name the same featureGate, not every
// component a feature needs: snapshot-controller, wanted by virt (Hangar)
// and by Rook (ceph-csi-drivers), has a gate of its own. Both features off:
// the free rook group goes while the virt group is held; snapshot-controller,
// its own gate off too, waits for what depends on it, in any group (Hangar,
// held), then goes. Its own gate on, it stays.
func TestReconcile_AComponentWithItsOwnGateGoesOnlyWithIt(t *testing.T) {
	both := func(snapshotGate string) map[string]k4allv1alpha1.ComponentSpec {
		components := virtGroup()
		for name, spec := range rookGroup() {
			components[name] = spec
		}
		hangar := components["hangar"]
		hangar.DependsOn = append(hangar.DependsOn, "snapshot-controller")
		components["hangar"] = hangar
		drivers := components["ceph-csi-drivers"]
		drivers.DependsOn = append(drivers.DependsOn, "snapshot-controller")
		components["ceph-csi-drivers"] = drivers
		components["snapshot-controller"] = gatedComponent("snapshot-controller", snapshotGate)
		return components
	}
	virt := []string{"kubevirt", "cdi", "virtctl", "hangar", "hangar-node-agent"}
	rook := []string{"cairn", "ceph-csi-drivers", "rook-ceph"}
	vmRefusal := "uninstall refused: kubevirt: 1 VirtualMachine (default/vm1) exists: " +
		"delete it or set kubevirt Unmanaged first"

	t.Run("its own gate off", func(t *testing.T) {
		virtWorkloadAPIs(t)
		// An unknown gate is off: it stands for "neither virt nor rook".
		c, r := groupsOnEnvtest(t, both("snapshots"))
		vm := kubevirtObject("VirtualMachine", "vm1")
		createAll(t, c, vm)

		reconcileProbe(t, r)

		assertGone(t, c, rook...)
		assertKept(t, c, vmRefusal, virt...)
		assertKept(t, c, "uninstall queued: waiting for hangar to be uninstalled first", "snapshot-controller")
		if n := removals(t, r, "snapshot-controller"); n != 0 {
			t.Errorf("snapshot-controller's Remover ran %d times while queued, want never", n)
		}

		if err := deleteNow(c, vm); err != nil {
			t.Fatal(err)
		}
		reconcileProbe(t, r)

		assertGone(t, c, append(virt, "snapshot-controller")...)
		if n := removals(t, r, "snapshot-controller"); n != 1 {
			t.Errorf("snapshot-controller's Remover ran %d times, want once", n)
		}
	})

	t.Run("its own gate on", func(t *testing.T) {
		// No gate: always on, as with virt or rook on.
		c, r := groupsOnEnvtest(t, both(""))

		reconcileProbe(t, r)

		assertGone(t, c, append(virt, rook...)...)
		if st := componentStatus(t, c, "snapshot-controller"); st.State != k4allv1alpha1.ComponentStateInstalled {
			t.Errorf("snapshot-controller: %s (%s), want Installed", st.State, st.Message)
		}
	})
}

// A component the release sets management Removed is not held by its
// feature: kubevirt-manager, replaced by Hangar, goes through its hook while
// KubeVirt refuses. Still Managed (a v2.6 release), it is one of the group.
func TestReconcile_ARemovedComponentIsNotHeldByItsFeature(t *testing.T) {
	for _, tc := range []struct {
		management k4allv1alpha1.ComponentManagement
		want       k4allv1alpha1.ComponentState
	}{
		{k4allv1alpha1.ComponentManagementRemoved, k4allv1alpha1.ComponentStateRemoved},
		{k4allv1alpha1.ComponentManagementManaged, k4allv1alpha1.ComponentStateInstalled},
	} {
		t.Run(string(tc.management), func(t *testing.T) {
			virtWorkloadAPIs(t)
			components := virtGroup()
			manager := gatedComponent("kubevirt-manager", "virt")
			manager.Management = tc.management
			components["kubevirt-manager"] = manager
			c, r := groupsOnEnvtest(t, components)
			createAll(t, c, kubevirtObject("VirtualMachine", "vm1"))
			probe := &removeProbe{}
			r.hookReg = hooks.NewRegistry(c, logr.Discard())
			r.hookReg.Register("kubevirt-manager", probe)

			reconcileProbe(t, r)

			st := componentStatus(t, c, "kubevirt-manager")
			if st.State != tc.want {
				t.Fatalf("kubevirt-manager: %s (%s), want %s", st.State, st.Message, tc.want)
			}
			if tc.want == k4allv1alpha1.ComponentStateInstalled &&
				!strings.HasPrefix(st.Message, "uninstall refused: kubevirt: 1 VirtualMachine (default/vm1) exists") {
				t.Errorf("kubevirt-manager: message %q, want kubevirt's refusal", st.Message)
			}
			if st := componentStatus(t, c, "kubevirt"); !strings.HasPrefix(st.Message, "uninstall refused: ") {
				t.Errorf("kubevirt: %s (%s), want its uninstall refused", st.State, st.Message)
			}
		})
	}
}

// A component queued behind another and back in the release (its feature
// on again) drops the queue message: nothing is being uninstalled.
func TestReconcile_AQueuedComponentBackInTheReleaseDropsTheQueue(t *testing.T) {
	c, r := groupsOnEnvtest(t, map[string]k4allv1alpha1.ComponentSpec{
		"base": gatedComponent("base", "virt"),
		"top":  gatedComponent("top", "virt", "base"),
	})
	r.hookReg = hooks.NewRegistry(c, logr.Discard())
	r.hookReg.Register("top", &preparerProbe{pending: "waiting for the operator"})

	reconcileProbe(t, r)
	assertKept(t, c, "uninstall queued: waiting for top to be uninstalled first", "base")

	cc := &k4allv1alpha1.ClusterConfig{}
	if err := c.Get(context.Background(), client.ObjectKey{Name: "k4all-cluster-config"}, cc); err != nil {
		t.Fatal(err)
	}
	cc.Spec.Features.Virt.Enabled = true
	if err := c.Update(context.Background(), cc); err != nil {
		t.Fatal(err)
	}
	reconcileProbe(t, r)

	if st := componentStatus(t, c, "base"); st.State != k4allv1alpha1.ComponentStateInstalled ||
		strings.HasPrefix(st.Message, "uninstall queued: ") {
		t.Fatalf("base: %s (%s), want Installed without the queue message", st.State, st.Message)
	}
}

// metadataComponent is k4all-metadata as the k4all v2.7.0 release has it:
// gate virtMetadata (virt and features.virt.metadata.enabled), outside the
// virt group, on KubeVirt.
func metadataComponent() k4allv1alpha1.ComponentSpec {
	return gatedComponent("k4all-metadata", "virtMetadata", "kubevirt")
}

// The v2.7.0 VM round (F2): virt off with VMs holds the virt group, and
// k4all-metadata, outside the group, went at once: its policies and every
// VM's Secret with it. Running VMs got 503 from 169.254.169.254, and a VM
// that restarted got a random MAC and no asset tag. It serves the VMs that
// hold the group: it stays, queued behind the group while the group is held
// and while it is uninstalled, and goes once the group is gone.
func TestReconcile_TheMetadataServiceGoesAfterTheVirtGroup(t *testing.T) {
	virtWorkloadAPIs(t)
	components := virtGroup()
	components["k4all-metadata"] = metadataComponent()
	c, r := groupsOnEnvtest(t, components)
	runningOperatorCR(t, c, kubeVirtCR, kubeVirtFinalizer)
	runningOperatorCR(t, c, cdiCR, cdiFinalizer)
	vm := kubevirtObject("VirtualMachine", "vm1")
	createAll(t, c, vm)
	deleted := recordDeletes(t, r)

	reconcileProbe(t, r)

	if len(*deleted) > 0 {
		t.Errorf("deleted %v while kubevirt refuses, want nothing", *deleted)
	}
	assertKept(t, c, "uninstall refused: kubevirt: 1 VirtualMachine (default/vm1) exists: "+
		"delete it or set kubevirt Unmanaged first", "kubevirt", "cdi", "virtctl", "hangar", "hangar-node-agent")
	assertKept(t, c, "uninstall queued: waiting for hangar to be uninstalled first", "k4all-metadata")

	if err := deleteNow(c, vm); err != nil {
		t.Fatal(err)
	}
	reconcileProbe(t, r)
	assertGone(t, c, "hangar", "hangar-node-agent", "virtctl")
	assertKept(t, c, "uninstall queued: waiting for cdi to be uninstalled first", "k4all-metadata")

	cdiCR.operatorDone(t, c)
	reconcileProbe(t, r)
	assertGone(t, c, "cdi")
	assertKept(t, c, "uninstall queued: waiting for kubevirt to be uninstalled first", "k4all-metadata")

	*deleted = nil
	kubeVirtCR.operatorDone(t, c)
	reconcileProbe(t, r)

	if want := []string{"kubevirt", "k4all-metadata"}; !slices.Equal(*deleted, want) {
		t.Fatalf("deleted %v, want %v", *deleted, want)
	}
	assertGone(t, c, "kubevirt", "k4all-metadata")
}

// Turned off on its own, virt on, the metadata service has nothing to wait
// for: it goes at once, as before.
func TestReconcile_TheMetadataServiceTurnedOffAloneGoesAtOnce(t *testing.T) {
	virtWorkloadAPIs(t)
	components := virtGroup()
	components["k4all-metadata"] = metadataComponent()
	c, r := groupsOnEnvtest(t, components)
	createAll(t, c, kubevirtObject("VirtualMachine", "vm1"))
	cc := &k4allv1alpha1.ClusterConfig{}
	if err := c.Get(context.Background(), client.ObjectKey{Name: "k4all-cluster-config"}, cc); err != nil {
		t.Fatal(err)
	}
	cc.Spec.Features.Virt.Enabled = true
	cc.Spec.Features.Virt.Metadata = &k4allv1alpha1.MetadataSettings{Enabled: false}
	if err := c.Update(context.Background(), cc); err != nil {
		t.Fatal(err)
	}

	reconcileProbe(t, r)

	assertGone(t, c, "k4all-metadata")
	for _, name := range []string{"kubevirt", "cdi", "virtctl", "hangar", "hangar-node-agent"} {
		if st := componentStatus(t, c, name); st.State != k4allv1alpha1.ComponentStateInstalled ||
			strings.HasPrefix(st.Message, "uninstall ") {
			t.Errorf("%s: %s (%s), want Installed, its feature on", name, st.State, st.Message)
		}
	}
}
