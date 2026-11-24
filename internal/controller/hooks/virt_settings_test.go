package hooks

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/go-logr/logr"
	k4allv1alpha1 "github.com/gpillon/k4all-operator/api/v1alpha1"
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

var (
	gatesPath         = []string{"spec", "configuration", "developerConfiguration", "featureGates"}
	disabledGatesPath = []string{"spec", "configuration", "developerConfiguration", "disabledFeatureGates"}
	vmStatePath       = []string{"spec", "configuration", "vmStateStorageClass"}
)

// upstreamKubeVirtCR is kubevirt-cr.yaml as the engine hands it to the hook:
// its empty fields stripped (engine.stripEmptyFields).
func upstreamKubeVirtCR() *unstructured.Unstructured {
	cr := &unstructured.Unstructured{}
	cr.SetAPIVersion(kubeVirtAPIVersion)
	cr.SetKind(kubeVirtKind)
	cr.SetNamespace(kubeVirtKey.Namespace)
	cr.SetName(kubeVirtKey.Name)
	cr.Object["spec"] = map[string]interface{}{
		"configuration":   map[string]interface{}{"imagePullPolicy": "IfNotPresent"},
		"imagePullPolicy": "IfNotPresent",
	}
	return cr
}

// liveKubeVirt is the KubeVirt CR in the cluster: gates and disabled gates
// (nil: unset), k4all's annotations (nil: none), the version virt-operator
// reports ("": none).
func liveKubeVirt(t *testing.T, gates, disabled []string, annotations map[string]string, observed string) *unstructured.Unstructured {
	t.Helper()
	cr := upstreamKubeVirtCR()
	if gates != nil {
		if err := unstructured.SetNestedStringSlice(cr.Object, gates, gatesPath...); err != nil {
			t.Fatal(err)
		}
	}
	if disabled != nil {
		if err := unstructured.SetNestedStringSlice(cr.Object, disabled, disabledGatesPath...); err != nil {
			t.Fatal(err)
		}
	}
	cr.SetAnnotations(annotations)
	if observed != "" {
		if err := unstructured.SetNestedField(cr.Object, observed, "status", "observedKubeVirtVersion"); err != nil {
			t.Fatal(err)
		}
	}
	return cr
}

func virtConfig(settings *k4allv1alpha1.KubeVirtSettings) k4allv1alpha1.ClusterConfigSpec {
	return k4allv1alpha1.ClusterConfigSpec{Features: k4allv1alpha1.FeaturesConfig{
		Virt: k4allv1alpha1.VirtConfig{Enabled: true, KubeVirt: settings},
	}}
}

// mutated is the KubeVirt CR the hook makes of upstream's for the KubeVirt
// version, the live CR and the settings.
func mutated(t *testing.T, version string, live *unstructured.Unstructured, settings *k4allv1alpha1.KubeVirtSettings) *unstructured.Unstructured {
	t.Helper()
	desired := upstreamKubeVirtCR()
	h := NewVirtHook(nil, logr.Discard())
	if !h.MutatesManifest(desired) {
		t.Fatal("the VirtHook does not claim the KubeVirt CR")
	}
	spec := k4allv1alpha1.ComponentSpec{Type: k4allv1alpha1.ComponentTypeManifest, Version: version}
	if err := h.MutateManifest(context.Background(), spec, virtConfig(settings), desired, live); err != nil {
		t.Fatal(err)
	}
	return desired
}

// list is the string list at path; nil when unset.
func list(t *testing.T, obj *unstructured.Unstructured, path []string) []string {
	t.Helper()
	l, found, err := unstructured.NestedStringSlice(obj.Object, path...)
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		return nil
	}
	return l
}

func TestVirtHook_MutatesOnlyTheKubeVirtCR(t *testing.T) {
	h := NewVirtHook(nil, logr.Discard())
	other := upstreamKubeVirtCR()
	other.SetAPIVersion("example.com/v1")
	cdi := &unstructured.Unstructured{}
	cdi.SetAPIVersion("cdi.kubevirt.io/v1beta1")
	cdi.SetKind("CDI")
	for _, obj := range []*unstructured.Unstructured{other, cdi} {
		if h.MutatesManifest(obj) {
			t.Errorf("the VirtHook claims %s %s", obj.GetAPIVersion(), obj.GetKind())
		}
	}
}

// The gate merge of spec §7.2 as the hook folds it into the KubeVirt CR, and
// what the annotations record.
func TestVirtHook_GateMerge(t *testing.T) {
	ours := func(on, off string) map[string]string {
		a := map[string]string{}
		if on != "" {
			a[FeatureGatesAnnotation] = on
		}
		if off != "" {
			a[DisabledFeatureGatesAnnotation] = off
		}
		return a
	}
	for _, tc := range []struct {
		name                    string
		version                 string
		live                    *unstructured.Unstructured
		settings                *k4allv1alpha1.KubeVirtSettings
		gates, disabled         []string // nil: left out
		gatesNote, disabledNote string   // the annotations; "": absent
	}{
		{name: "fresh install, no settings: upstream's empty list stays out",
			version: "v1.9.1"},
		{name: "fresh install: the gates turned on",
			version: "v1.9.1", settings: &k4allv1alpha1.KubeVirtSettings{FeatureGates: []string{"VSOCK", "Sidecar"}},
			gates: []string{"VSOCK", "Sidecar"}, gatesNote: "Sidecar,VSOCK"},
		{name: "added after the gates set by others",
			version: "v1.9.1", live: liveKubeVirt(t, []string{"HostDevices"}, nil, nil, "v1.9.1"),
			settings: &k4allv1alpha1.KubeVirtSettings{FeatureGates: []string{"Sidecar"}},
			gates:    []string{"HostDevices", "Sidecar"}, gatesNote: "Sidecar"},
		{name: "ours removed once the ClusterConfig drops it, others' kept",
			version:  "v1.9.1",
			live:     liveKubeVirt(t, []string{"HostDevices", "Sidecar", "VSOCK"}, nil, ours("Sidecar,VSOCK", ""), "v1.9.1"),
			settings: &k4allv1alpha1.KubeVirtSettings{FeatureGates: []string{"VSOCK"}},
			gates:    []string{"HostDevices", "VSOCK"}, gatesNote: "VSOCK"},
		{name: "the settings removed: ours go, the annotation too",
			version: "v1.9.1",
			live:    liveKubeVirt(t, []string{"HostDevices", "Sidecar"}, nil, ours("Sidecar", ""), "v1.9.1"),
			gates:   []string{"HostDevices"}},
		{name: "the last gate was ours: the list written empty",
			version: "v1.9.1", live: liveKubeVirt(t, []string{"Sidecar"}, nil, ours("Sidecar", ""), "v1.9.1"),
			settings: &k4allv1alpha1.KubeVirtSettings{},
			gates:    []string{}},
		{name: "upgrade from an operator with no annotation: nothing removed",
			version: "v1.9.1", live: liveKubeVirt(t, []string{"HostDevices", "Sidecar"}, nil, nil, "v1.9.1"),
			gates: []string{"HostDevices", "Sidecar"}},
		{name: "KubeVirt 1.8: turned off is removed from featureGates and written to disabledFeatureGates",
			version: "v1.8.4", live: liveKubeVirt(t, []string{"HostDevices", "Snapshot"}, nil, nil, "v1.8.4"),
			settings: &k4allv1alpha1.KubeVirtSettings{DisabledFeatureGates: []string{"Snapshot"}},
			gates:    []string{"HostDevices"}, disabled: []string{"Snapshot"}, disabledNote: "Snapshot"},
		{name: "KubeVirt 1.7: turned off is only removed from featureGates",
			version: "v1.7.4", live: liveKubeVirt(t, []string{"HostDevices", "Snapshot"}, nil, nil, "v1.7.4"),
			settings: &k4allv1alpha1.KubeVirtSettings{DisabledFeatureGates: []string{"Snapshot"}},
			gates:    []string{"HostDevices"}},
		// The CRD comes with the component's own kubevirt-operator.yaml, applied
		// first: on the step to 1.8 it serves disabledFeatureGates while 1.7
		// still runs.
		{name: "the step to KubeVirt 1.8 writes disabledFeatureGates while 1.7 runs",
			version: "v1.8.4", live: liveKubeVirt(t, []string{"Snapshot"}, nil, nil, "v1.7.4"),
			settings: &k4allv1alpha1.KubeVirtSettings{DisabledFeatureGates: []string{"Snapshot"}},
			gates:    []string{}, disabled: []string{"Snapshot"}, disabledNote: "Snapshot"},
		{name: "a version that is no semver: the one KubeVirt reports decides",
			version: "latest", live: liveKubeVirt(t, nil, nil, nil, "v1.9.1"),
			settings: &k4allv1alpha1.KubeVirtSettings{DisabledFeatureGates: []string{"Snapshot"}},
			disabled: []string{"Snapshot"}, disabledNote: "Snapshot"},
		{name: "no version known: disabledFeatureGates not written",
			version: "latest", live: nil,
			settings: &k4allv1alpha1.KubeVirtSettings{DisabledFeatureGates: []string{"Snapshot"}}},
		{name: "disabledFeatureGates: ours dropped, others' kept, a gate turned on out",
			version: "v1.9.1",
			live: liveKubeVirt(t, nil, []string{"DownwardMetrics", "VSOCK", "Sidecar"},
				ours("", "Sidecar,VSOCK"), "v1.9.1"),
			settings: &k4allv1alpha1.KubeVirtSettings{
				FeatureGates: []string{"DownwardMetrics"}, DisabledFeatureGates: []string{"Sidecar"}},
			gates: []string{"DownwardMetrics"}, disabled: []string{"Sidecar"},
			gatesNote: "DownwardMetrics", disabledNote: "Sidecar"},
		{name: "exclusive pair: HotplugVolumes in the CR keeps DeclarativeHotplugVolumes out",
			version: "v1.9.1", live: liveKubeVirt(t, []string{"HotplugVolumes"}, nil, nil, "v1.9.1"),
			settings: &k4allv1alpha1.KubeVirtSettings{FeatureGates: []string{"DeclarativeHotplugVolumes"}},
			gates:    []string{"HotplugVolumes"}, gatesNote: "DeclarativeHotplugVolumes"},
		{name: "exclusive pair: k4all's HotplugVolumes replaced by DeclarativeHotplugVolumes",
			version:  "v1.9.1",
			live:     liveKubeVirt(t, []string{"HotplugVolumes"}, nil, ours("HotplugVolumes", ""), "v1.9.1"),
			settings: &k4allv1alpha1.KubeVirtSettings{FeatureGates: []string{"DeclarativeHotplugVolumes"}},
			gates:    []string{"DeclarativeHotplugVolumes"}, gatesNote: "DeclarativeHotplugVolumes"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cr := mutated(t, tc.version, tc.live, tc.settings)
			if got := list(t, cr, gatesPath); !slices.Equal(got, tc.gates) || (got == nil) != (tc.gates == nil) {
				t.Errorf("featureGates = %#v, want %#v", got, tc.gates)
			}
			if got := list(t, cr, disabledGatesPath); !slices.Equal(got, tc.disabled) || (got == nil) != (tc.disabled == nil) {
				t.Errorf("disabledFeatureGates = %#v, want %#v", got, tc.disabled)
			}
			for key, want := range map[string]string{FeatureGatesAnnotation: tc.gatesNote, DisabledFeatureGatesAnnotation: tc.disabledNote} {
				got, set := cr.GetAnnotations()[key]
				if got != want || set != (want != "") {
					t.Errorf("annotation %s = %q (set: %v), want %q", key, got, set, want)
				}
			}
		})
	}
}

// vmStateStorageClass is written when the ClusterConfig sets it, and left out
// otherwise: the apply then removes the value only if k4all wrote it.
func TestVirtHook_VMStateStorageClass(t *testing.T) {
	set := mutated(t, "v1.9.1", nil, &k4allv1alpha1.KubeVirtSettings{VMStateStorageClass: "ceph-rbd"})
	if got, _, _ := unstructured.NestedString(set.Object, vmStatePath...); got != "ceph-rbd" {
		t.Errorf("vmStateStorageClass = %q, want ceph-rbd", got)
	}

	live := liveKubeVirt(t, nil, nil, nil, "v1.9.1")
	if err := unstructured.SetNestedField(live.Object, "set-by-hangar", vmStatePath...); err != nil {
		t.Fatal(err)
	}
	for _, settings := range []*k4allv1alpha1.KubeVirtSettings{nil, {}} {
		cleared := mutated(t, "v1.9.1", live, settings)
		if got, found, _ := unstructured.NestedString(cleared.Object, vmStatePath...); found {
			t.Errorf("settings %+v: vmStateStorageClass = %q, want it left out of the apply", settings, got)
		}
	}
}

// The reconciler hands the engine an ObjectMutator only for a hook that is a
// ManifestMutator, with the ClusterConfig at hand.
func TestObjectMutator(t *testing.T) {
	config := virtConfig(&k4allv1alpha1.KubeVirtSettings{FeatureGates: []string{"Sidecar"}})
	if m := ObjectMutator(NewCDIHook(nil), config); m != nil {
		t.Errorf("CDIHook: ObjectMutator = %v, want nil", m)
	}
	if m := ObjectMutator(nil, config); m != nil {
		t.Errorf("no hook: ObjectMutator = %v, want nil", m)
	}
	m := ObjectMutator(NewVirtHook(nil, logr.Discard()), config)
	if m == nil {
		t.Fatal("VirtHook: ObjectMutator = nil")
	}
	desired := upstreamKubeVirtCR()
	if !m.Mutates(desired) {
		t.Fatal("the VirtHook's mutator does not claim the KubeVirt CR")
	}
	spec := k4allv1alpha1.ComponentSpec{Version: "v1.9.1"}
	if err := m.Mutate(context.Background(), spec, desired, nil); err != nil {
		t.Fatal(err)
	}
	if got := list(t, desired, gatesPath); !slices.Equal(got, []string{"Sidecar"}) {
		t.Errorf("featureGates = %v, want the ClusterConfig's [Sidecar]", got)
	}
}

func settingsCluster(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).Build()
}

func TestKubeVirtSettingsCondition(t *testing.T) {
	cephRBD := &storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: "ceph-rbd"}, Provisioner: "rbd.csi.ceph.com"}
	hotplugCR := liveKubeVirt(t, []string{"HotplugVolumes"}, nil, nil, "v1.9.1")
	for _, tc := range []struct {
		name     string
		objs     []client.Object
		settings k4allv1alpha1.KubeVirtSettings
		status   metav1.ConditionStatus
		reason   string
		messages []string
	}{
		{name: "nothing to report",
			settings: k4allv1alpha1.KubeVirtSettings{FeatureGates: []string{"Sidecar"}},
			status:   metav1.ConditionTrue, reason: "Valid"},
		{name: "the StorageClass exists",
			objs:     []client.Object{cephRBD},
			settings: k4allv1alpha1.KubeVirtSettings{VMStateStorageClass: "ceph-rbd"},
			status:   metav1.ConditionTrue, reason: "Valid"},
		{name: "no such StorageClass: reported, still written",
			settings: k4allv1alpha1.KubeVirtSettings{VMStateStorageClass: "ceph-rbd"},
			status:   metav1.ConditionFalse, reason: "StorageClassNotFound",
			messages: []string{`vmStateStorageClass "ceph-rbd": no such StorageClass`}},
		{name: "a gate kept out by its exclusive partner in the CR",
			objs:     []client.Object{hotplugCR},
			settings: k4allv1alpha1.KubeVirtSettings{FeatureGates: []string{"DeclarativeHotplugVolumes"}},
			status:   metav1.ConditionFalse, reason: "ExclusiveGate",
			messages: []string{"DeclarativeHotplugVolumes is not turned on: the KubeVirt CR has HotplugVolumes"}},
		{name: "both, the StorageClass first",
			objs: []client.Object{hotplugCR},
			settings: k4allv1alpha1.KubeVirtSettings{FeatureGates: []string{"DeclarativeHotplugVolumes"},
				VMStateStorageClass: "ceph-rbd"},
			status: metav1.ConditionFalse, reason: "StorageClassNotFound",
			messages: []string{`vmStateStorageClass "ceph-rbd"`, "DeclarativeHotplugVolumes is not turned on"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cond := KubeVirtSettingsCondition(context.Background(), settingsCluster(t, tc.objs...), tc.settings)
			if cond.Type != k4allv1alpha1.ConditionKubeVirtSettings || cond.Status != tc.status || cond.Reason != tc.reason {
				t.Fatalf("condition %s %s (%s: %s), want %s %s", cond.Type, cond.Status, cond.Reason, cond.Message,
					tc.status, tc.reason)
			}
			for _, m := range tc.messages {
				if !strings.Contains(cond.Message, m) {
					t.Errorf("message %q lacks %q", cond.Message, m)
				}
			}
		})
	}
}
