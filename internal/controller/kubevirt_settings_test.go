package controller

import (
	"context"
	"encoding/json"
	"slices"
	"testing"

	"github.com/go-logr/logr"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	k4allv1alpha1 "github.com/gpillon/k4all-operator/api/v1alpha1"
	"github.com/gpillon/k4all-operator/internal/controller/hooks"
)

// features.virt.kubevirt folded into the KubeVirt CR (spec §7), on envtest,
// through Reconcile, with kubevirt-cr.yaml as upstream publishes it.

var (
	kvGatesPath    = []string{"spec", "configuration", "developerConfiguration", "featureGates"}
	kvDisabledPath = []string{"spec", "configuration", "developerConfiguration", "disabledFeatureGates"}
	kvVMStatePath  = []string{"spec", "configuration", "vmStateStorageClass"}
)

// kubeVirtComponent is the kubevirt component of the k4all release, its CR
// only (kubevirt-cr.yaml of version).
func kubeVirtComponent(srv, version string) k4allv1alpha1.ComponentSpec {
	return k4allv1alpha1.ComponentSpec{
		Type: k4allv1alpha1.ComponentTypeManifest, Version: version, FeatureGate: "virt",
		Sources: []string{srv + "/kubevirt/{{ version }}/kubevirt-cr.yaml"}, Namespace: "kubevirt",
	}
}

// kubeVirtOnEnvtest is the kubevirt component at version, not installed yet,
// virt on with settings. The reconciler writes as the operator does; c is a
// client of the test's own.
func kubeVirtOnEnvtest(t *testing.T, version string, settings *k4allv1alpha1.KubeVirtSettings) (string, client.Client, *ReleaseReconciler) {
	t.Helper()
	srv := upstreamReleases(t)
	c, r := componentOnEnvtest(t, "kubevirt", kubeVirtComponent(srv, version), k4allv1alpha1.ComponentStatus{})
	ensureNamespace(t, c, "kubevirt")
	kubeVirtCR.deleteOnCleanup(t, c)
	setVirt(t, c, settings)
	r.Client = operatorClient(t)
	return srv, c, r
}

// setVirt turns virt on in the ClusterConfig, with settings.
func setVirt(t *testing.T, c client.Client, settings *k4allv1alpha1.KubeVirtSettings) {
	t.Helper()
	cc := &k4allv1alpha1.ClusterConfig{}
	if err := c.Get(context.Background(), client.ObjectKey{Name: "k4all-cluster-config"}, cc); err != nil {
		t.Fatal(err)
	}
	cc.Spec.Features.Virt.Enabled = true
	cc.Spec.Features.Virt.KubeVirt = settings
	if err := c.Update(context.Background(), cc); err != nil {
		t.Fatal(err)
	}
}

// setComponent replaces the component name in the ReleaseManifest.
func setComponent(t *testing.T, c client.Client, name string, spec k4allv1alpha1.ComponentSpec) {
	t.Helper()
	rm := &k4allv1alpha1.ReleaseManifest{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: "k4all"}, rm); err != nil {
		t.Fatal(err)
	}
	rm.Spec.Components[name] = spec
	if err := c.Update(context.Background(), rm); err != nil {
		t.Fatal(err)
	}
}

// reconcileKubeVirt runs one Reconcile and expects kubevirt Installed.
func reconcileKubeVirt(t *testing.T, c client.Client, r *ReleaseReconciler) {
	t.Helper()
	reconcileProbe(t, r)
	if st := componentStatus(t, c, "kubevirt"); st.State != k4allv1alpha1.ComponentStateInstalled {
		t.Fatalf("kubevirt is %s (%s), want Installed", st.State, st.Message)
	}
}

// kvList is the string list at path of the KubeVirt CR; nil when unset.
func kvList(t *testing.T, c client.Client, path []string) []string {
	t.Helper()
	l, found, err := unstructured.NestedStringSlice(kubeVirtCR.get(t, c).Object, path...)
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		return nil
	}
	return l
}

func assertList(t *testing.T, c client.Client, what string, path []string, want []string) {
	t.Helper()
	if got := kvList(t, c, path); !slices.Equal(got, want) || (got == nil) != (want == nil) {
		t.Errorf("%s: %s = %#v, want %#v", what, path[len(path)-1], got, want)
	}
}

func assertAnnotation(t *testing.T, c client.Client, what, key, want string) {
	t.Helper()
	got, set := kubeVirtCR.get(t, c).GetAnnotations()[key]
	if got != want || set != (want != "") {
		t.Errorf("%s: annotation %s = %q (set: %v), want %q", what, key, got, set, want)
	}
}

// The rows of spec §7.2 one after the other on KubeVirt v1.9.1: k4all adds
// its gates to the ones others set, removes only its own, writes what it
// turns off to disabledFeatureGates, and records both in the annotations.
func TestReconcile_KubeVirtGateMerge(t *testing.T) {
	_, c, r := kubeVirtOnEnvtest(t, "v1.9.1", &k4allv1alpha1.KubeVirtSettings{FeatureGates: []string{"Sidecar"}})

	reconcileKubeVirt(t, c, r)
	assertList(t, c, "fresh install", kvGatesPath, []string{"Sidecar"})
	assertList(t, c, "fresh install", kvDisabledPath, nil)
	assertAnnotation(t, c, "fresh install", hooks.FeatureGatesAnnotation, "Sidecar")
	if !owns(managedFieldsOf(t, kubeVirtCR.get(t, c), metav1.ManagedFieldsOperationApply),
		"metadata", "annotations", hooks.FeatureGatesAnnotation) {
		t.Errorf("the annotation is not k4all-operator's; managers: %v", managers(kubeVirtCR.get(t, c)))
	}

	// Hangar turns on HostDevices and turns off DownwardMetrics in the CR.
	kubeVirtCR.patchByHand(t, c, `{"spec":{"configuration":{"developerConfiguration":{
		"featureGates":["Sidecar","HostDevices","Snapshot"],"disabledFeatureGates":["DownwardMetrics"]}}}}`)

	setVirt(t, c, &k4allv1alpha1.KubeVirtSettings{
		FeatureGates: []string{"VSOCK"}, DisabledFeatureGates: []string{"Snapshot", "ExpandDisks"}})
	reconcileKubeVirt(t, c, r)
	// Sidecar was k4all's: gone. HostDevices is Hangar's: kept. Snapshot,
	// Hangar's, is turned off: out of featureGates, in disabledFeatureGates.
	assertList(t, c, "settings changed", kvGatesPath, []string{"HostDevices", "VSOCK"})
	assertList(t, c, "settings changed", kvDisabledPath, []string{"DownwardMetrics", "Snapshot", "ExpandDisks"})
	assertAnnotation(t, c, "settings changed", hooks.FeatureGatesAnnotation, "VSOCK")
	assertAnnotation(t, c, "settings changed", hooks.DisabledFeatureGatesAnnotation, "ExpandDisks,Snapshot")

	// The settings removed: k4all's entries go, the others' stay.
	setVirt(t, c, nil)
	reconcileKubeVirt(t, c, r)
	assertList(t, c, "settings removed", kvGatesPath, []string{"HostDevices"})
	assertList(t, c, "settings removed", kvDisabledPath, []string{"DownwardMetrics"})
	assertAnnotation(t, c, "settings removed", hooks.FeatureGatesAnnotation, "")
	assertAnnotation(t, c, "settings removed", hooks.DisabledFeatureGatesAnnotation, "")
}

// KubeVirt has disabledFeatureGates from 1.8: on 1.7 a gate turned off is
// only removed from featureGates; the apply of 1.8 writes it there too.
func TestReconcile_KubeVirtDisabledGatesFrom18(t *testing.T) {
	srv, c, r := kubeVirtOnEnvtest(t, "v1.7.4", &k4allv1alpha1.KubeVirtSettings{DisabledFeatureGates: []string{"Snapshot"}})
	reconcileKubeVirt(t, c, r)
	kubeVirtCR.patchByHand(t, c, `{"spec":{"configuration":{"developerConfiguration":{"featureGates":["HostDevices","Snapshot"]}}}}`)
	setVirt(t, c, &k4allv1alpha1.KubeVirtSettings{DisabledFeatureGates: []string{"Snapshot", "VSOCK"}})

	reconcileKubeVirt(t, c, r)
	assertList(t, c, "v1.7.4", kvGatesPath, []string{"HostDevices"})
	assertList(t, c, "v1.7.4", kvDisabledPath, nil)
	assertAnnotation(t, c, "v1.7.4", hooks.DisabledFeatureGatesAnnotation, "")

	setComponent(t, c, "kubevirt", kubeVirtComponent(srv, "v1.8.4"))
	reconcileKubeVirt(t, c, r)
	assertList(t, c, "v1.8.4", kvGatesPath, []string{"HostDevices"})
	assertList(t, c, "v1.8.4", kvDisabledPath, []string{"Snapshot", "VSOCK"})
	assertAnnotation(t, c, "v1.8.4", hooks.DisabledFeatureGatesAnnotation, "Snapshot,VSOCK")
}

func kubeVirtSettingsCondition(t *testing.T, c client.Client) *metav1.Condition {
	t.Helper()
	cc := &k4allv1alpha1.ClusterConfig{}
	if err := c.Get(context.Background(), client.ObjectKey{Name: "k4all-cluster-config"}, cc); err != nil {
		t.Fatal(err)
	}
	return meta.FindStatusCondition(cc.Status.Conditions, k4allv1alpha1.ConditionKubeVirtSettings)
}

func assertKubeVirtSettings(t *testing.T, c client.Client, what string, status metav1.ConditionStatus, reason string) {
	t.Helper()
	cond := kubeVirtSettingsCondition(t, c)
	if cond == nil {
		t.Fatalf("%s: no KubeVirtSettings condition, want %s %s", what, status, reason)
	}
	if cond.Status != status || cond.Reason != reason {
		t.Errorf("%s: KubeVirtSettings %s %s (%s), want %s %s", what, cond.Status, cond.Reason, cond.Message, status, reason)
	}
}

// Of the exclusive pair, the gate in the CR wins over the ClusterConfig's,
// and the ClusterConfig says why its gate is not on.
func TestReconcile_KubeVirtExclusivePair(t *testing.T) {
	_, c, r := kubeVirtOnEnvtest(t, "v1.9.1", nil)
	reconcileKubeVirt(t, c, r)
	if cond := kubeVirtSettingsCondition(t, c); cond != nil {
		t.Errorf("no settings: KubeVirtSettings %s %s, want none", cond.Status, cond.Reason)
	}
	kubeVirtCR.patchByHand(t, c, `{"spec":{"configuration":{"developerConfiguration":{"featureGates":["HotplugVolumes"]}}}}`)

	setVirt(t, c, &k4allv1alpha1.KubeVirtSettings{FeatureGates: []string{"DeclarativeHotplugVolumes"}})
	reconcileKubeVirt(t, c, r)
	assertList(t, c, "HotplugVolumes in the CR", kvGatesPath, []string{"HotplugVolumes"})
	assertKubeVirtSettings(t, c, "HotplugVolumes in the CR", metav1.ConditionFalse, "ExclusiveGate")

	// Turned off by the ClusterConfig, HotplugVolumes makes room.
	setVirt(t, c, &k4allv1alpha1.KubeVirtSettings{
		FeatureGates: []string{"DeclarativeHotplugVolumes"}, DisabledFeatureGates: []string{"HotplugVolumes"}})
	reconcileKubeVirt(t, c, r)
	assertList(t, c, "HotplugVolumes turned off", kvGatesPath, []string{"DeclarativeHotplugVolumes"})
	assertList(t, c, "HotplugVolumes turned off", kvDisabledPath, []string{"HotplugVolumes"})
	assertKubeVirtSettings(t, c, "HotplugVolumes turned off", metav1.ConditionTrue, "Valid")
}

// vmStateStorageClass: written when set, even before its StorageClass exists
// (the ClusterConfig says it is missing); removed when cleared; a value
// Hangar set while the ClusterConfig had none stays.
func TestReconcile_KubeVirtVMStateStorageClass(t *testing.T) {
	_, c, r := kubeVirtOnEnvtest(t, "v1.9.1", &k4allv1alpha1.KubeVirtSettings{VMStateStorageClass: "vm-state-test"})

	reconcileKubeVirt(t, c, r)
	if got, _, _ := unstructured.NestedString(kubeVirtCR.get(t, c).Object, kvVMStatePath...); got != "vm-state-test" {
		t.Fatalf("vmStateStorageClass = %q, want vm-state-test though no such StorageClass exists", got)
	}
	assertKubeVirtSettings(t, c, "no StorageClass", metav1.ConditionFalse, "StorageClassNotFound")
	// Hangar shows the field read-only when its manager is exactly k4all-operator.
	if !owns(managedFieldsOf(t, kubeVirtCR.get(t, c), metav1.ManagedFieldsOperationApply), kvVMStatePath...) {
		t.Errorf("vmStateStorageClass is not k4all-operator's; managers: %v", managers(kubeVirtCR.get(t, c)))
	}

	createAll(t, c, &storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: "vm-state-test"}, Provisioner: "example.com/csi"})
	reconcileKubeVirt(t, c, r)
	assertKubeVirtSettings(t, c, "StorageClass created", metav1.ConditionTrue, "Valid")

	setVirt(t, c, &k4allv1alpha1.KubeVirtSettings{})
	reconcileKubeVirt(t, c, r)
	if got, found, _ := unstructured.NestedString(kubeVirtCR.get(t, c).Object, kvVMStatePath...); found {
		t.Fatalf("cleared: vmStateStorageClass = %q, want it removed", got)
	}

	kubeVirtCR.patchByHand(t, c, `{"spec":{"configuration":{"vmStateStorageClass":"set-by-hangar"}}}`)
	setVirt(t, c, &k4allv1alpha1.KubeVirtSettings{FeatureGates: []string{"Sidecar"}})
	reconcileKubeVirt(t, c, r)
	if got, _, _ := unstructured.NestedString(kubeVirtCR.get(t, c).Object, kvVMStatePath...); got != "set-by-hangar" {
		t.Errorf("set by Hangar: vmStateStorageClass = %q, want it kept", got)
	}
}

// v057Hook is the kubevirt hook as k4all-operator v0.5.7 had it: install
// hooks and the values revision 1, no ManifestMutator.
type v057Hook struct{ hooks.Hook }

func (v057Hook) ValuesRevision() string { return "1" }

// upstreamKubeVirtV191 is kubevirt-cr.yaml v1.9.1 with the uninstallStrategy
// v0.5.7 adds: what v0.5.7 applied on a fresh install.
const upstreamKubeVirtV191 = `{"apiVersion":"kubevirt.io/v1","kind":"KubeVirt",
"metadata":{"name":"kubevirt","namespace":"kubevirt"},
"spec":{"certificateRotateStrategy":{},
 "configuration":{"developerConfiguration":{"featureGates":[]},"imagePullPolicy":"IfNotPresent"},
 "customizeComponents":{},"imagePullPolicy":"IfNotPresent","workloadUpdateStrategy":{},
 "uninstallStrategy":"BlockUninstallIfWorkloadsExist"}}`

// The operator upgrade from v0.5.7 re-applies KubeVirt once (apply revision
// 2): without upstream's empty fields, which then leave k4all-operator's
// managedFields, and with the gate merge. What the admins set by hand
// survives it: gates (no annotation: none is k4all's), host devices, the
// workload update methods set inside the workloadUpdateStrategy: {} that
// k4all-operator owned.
func TestReconcile_KubeVirtReappliedOnceAfterTheUpgradeFromV057(t *testing.T) {
	srv := upstreamReleases(t)
	c, previous := componentOnEnvtest(t, "kubevirt", kubeVirtComponent(srv, "v1.9.1"), k4allv1alpha1.ComponentStatus{})
	ctx := context.Background()
	ensureNamespace(t, c, "kubevirt")
	kubeVirtCR.deleteOnCleanup(t, c)
	setVirt(t, c, nil)

	previous.hookReg = hooks.NewRegistry(c, logr.Discard())
	previous.hookReg.Register("kubevirt", v057Hook{previous.hookReg.Get("kubevirt")})
	reconcileKubeVirt(t, c, previous)
	// The CR as v0.5.7 left it: its apply, empty fields and all.
	cr := &unstructured.Unstructured{}
	if err := json.Unmarshal([]byte(upstreamKubeVirtV191), &cr.Object); err != nil {
		t.Fatal(err)
	}
	if err := c.Patch(ctx, cr, client.Apply, client.FieldOwner("k4all-operator"), client.ForceOwnership); err != nil {
		t.Fatal(err)
	}
	kubeVirtCR.patchByHand(t, c, `{"spec":{"configuration":{
		"developerConfiguration":{"featureGates":["HostDevices","Snapshot"]},
		"permittedHostDevices":{"pciHostDevices":[{"pciVendorSelector":"8086:46a6","resourceName":"example.com/iris-xe-vf"}]}},
		"workloadUpdateStrategy":{"workloadUpdateMethods":["LiveMigrate","Evict"]}}}`)
	before := managedFieldsOf(t, kubeVirtCR.get(t, c), metav1.ManagedFieldsOperationApply)
	for _, f := range []string{"workloadUpdateStrategy", "certificateRotateStrategy", "customizeComponents"} {
		if !owns(before, "spec", f) {
			t.Fatalf("v0.5.7: k4all-operator does not own spec.%s, the test does not start from v0.5.7", f)
		}
	}

	watching, err := client.NewWithWatch(envtestConfig(t), client.Options{Scheme: gateScheme(t)})
	if err != nil {
		t.Fatal(err)
	}
	applies := 0
	upgraded := &ReleaseReconciler{Scheme: previous.Scheme, RestConfig: previous.RestConfig, Log: logr.Discard(),
		Client: interceptor.NewClient(watching, interceptor.Funcs{
			Patch: func(ctx context.Context, cl client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
				if patch.Type() == types.ApplyPatchType && obj.GetObjectKind().GroupVersionKind().Kind == "KubeVirt" {
					applies++
				}
				return cl.Patch(ctx, obj, patch, opts...)
			},
		})}

	reconcileKubeVirt(t, c, upgraded)

	if applies != 1 {
		t.Fatalf("KubeVirt applied %d times at the first reconcile of the new operator, want once", applies)
	}
	after := kubeVirtCR.get(t, c)
	assertList(t, c, "after the upgrade", kvGatesPath, []string{"HostDevices", "Snapshot"})
	if devices, _, _ := unstructured.NestedSlice(after.Object, "spec", "configuration", "permittedHostDevices", "pciHostDevices"); len(devices) != 1 {
		t.Errorf("pciHostDevices %v, want the one set by hand", devices)
	}
	if methods, _, _ := unstructured.NestedStringSlice(after.Object, "spec", "workloadUpdateStrategy", "workloadUpdateMethods"); !slices.Equal(methods, []string{"LiveMigrate", "Evict"}) {
		t.Errorf("workloadUpdateMethods %v, want the ones set by hand [LiveMigrate Evict]", methods)
	}
	if strategy, _, _ := unstructured.NestedString(after.Object, "spec", "uninstallStrategy"); strategy != blockUninstall {
		t.Errorf("uninstallStrategy %q, want %s", strategy, blockUninstall)
	}
	for _, f := range []string{"certificateRotateStrategy", "customizeComponents"} {
		if v, found, _ := unstructured.NestedFieldNoCopy(after.Object, "spec", f); found {
			t.Errorf("spec.%s = %v, want the empty field gone", f, v)
		}
	}
	owned := managedFieldsOf(t, after, metav1.ManagedFieldsOperationApply)
	for _, f := range []string{"workloadUpdateStrategy", "certificateRotateStrategy", "customizeComponents"} {
		if owns(owned, "spec", f) {
			t.Errorf("k4all-operator still owns spec.%s: %v", f, owned)
		}
	}
	if !owns(owned, "spec", "uninstallStrategy") {
		t.Errorf("k4all-operator does not own spec.uninstallStrategy: %v", owned)
	}

	reconcileKubeVirt(t, c, upgraded)
	if applies != 1 {
		t.Fatalf("KubeVirt applied again at the next reconcile (%d applies in all), want once", applies)
	}
}

// The release sets workloadUpdateMethods with a merge patch after the apply
// (spec D3, k4all-release.yaml), the infra placement the same way. The apply
// leaves workloadUpdateStrategy out, empty upstream: the next apply must not
// remove what the patch wrote, and the patch's fields are k4all-operator's
// too.
func TestReconcile_KubeVirtReleasePatchesSurviveTheApply(t *testing.T) {
	srv, c, r := kubeVirtOnEnvtest(t, "v1.9.1", nil)
	spec := kubeVirtComponent(srv, "v1.9.1")
	spec.Patches = []k4allv1alpha1.ComponentPatch{{
		Target: k4allv1alpha1.PatchTarget{APIVersion: "kubevirt.io/v1", Kind: "KubeVirt", Name: "kubevirt", Namespace: "kubevirt"},
		Type:   k4allv1alpha1.PatchTypeMerge,
		Patch:  `{"spec":{"workloadUpdateStrategy":{"workloadUpdateMethods":["LiveMigrate"]}}}`,
	}}
	setComponent(t, c, "kubevirt", spec)

	reconcileKubeVirt(t, c, r)
	setVirt(t, c, &k4allv1alpha1.KubeVirtSettings{FeatureGates: []string{"Sidecar"}}) // a new apply
	reconcileKubeVirt(t, c, r)

	cr := kubeVirtCR.get(t, c)
	methods, _, _ := unstructured.NestedStringSlice(cr.Object, "spec", "workloadUpdateStrategy", "workloadUpdateMethods")
	if !slices.Equal(methods, []string{"LiveMigrate"}) {
		t.Errorf("workloadUpdateMethods %v, want the release's [LiveMigrate]", methods)
	}
	if !owns(managedFieldsOf(t, cr, metav1.ManagedFieldsOperationUpdate),
		"spec", "workloadUpdateStrategy", "workloadUpdateMethods") {
		t.Errorf("workloadUpdateMethods is not k4all-operator's; managers: %v", managers(cr))
	}
	if owns(managedFieldsOf(t, cr, metav1.ManagedFieldsOperationApply), "spec", "workloadUpdateStrategy") {
		t.Error("the apply owns workloadUpdateStrategy, which upstream leaves empty")
	}
}
