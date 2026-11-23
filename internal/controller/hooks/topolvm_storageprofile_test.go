package hooks

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/go-logr/logr"
	storagev1 "k8s.io/api/storage/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	k4allv1alpha1 "github.com/gpillon/k4all-operator/api/v1alpha1"
)

// What the admin set in StorageProfile/topolvm-provisioner (cloneStrategy
// and claimPropertySets as on the lab of gpillon/k4all#80).
var adminProfileSpec = map[string]interface{}{
	"cloneStrategy": "csi-clone",
	"claimPropertySets": []interface{}{
		map[string]interface{}{"accessModes": []interface{}{"ReadWriteOnce"}, "volumeMode": "Block"},
	},
}

// The ConfigMap that keeps the spec while the class is recreated.
var savedProfileKey = types.NamespacedName{Namespace: "topolvm-system", Name: "topolvm-storageprofile-saved"}

var storageProfileGVK = schema.GroupVersionKind{Group: "cdi.kubevirt.io", Version: "v1beta1", Kind: "StorageProfile"}

// cdiCluster is envtest with CDI's StorageProfile API, the topolvm-provisioner
// class with mode, and its StorageProfile with spec (none when spec is nil).
// The test plays helm and CDI around the hook.
type cdiCluster struct {
	t     *testing.T
	c     client.Client
	clock time.Time
}

func newCDICluster(t *testing.T, mode storagev1.VolumeBindingMode, spec map[string]interface{}) *cdiCluster {
	t.Helper()
	c, cfg := envtestClient(t)
	installStorageProfileCRD(t, cfg)
	cc := &cdiCluster{t: t, c: c, clock: time.Now()}
	t.Cleanup(func() {
		ctx := context.Background()
		_ = c.Delete(ctx, &storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: topolvmStorageClass}})
		_ = c.Delete(ctx, newStorageProfile())
		cm := &unstructured.Unstructured{}
		cm.SetAPIVersion("v1")
		cm.SetKind("ConfigMap")
		cm.SetNamespace(savedProfileKey.Namespace)
		cm.SetName(savedProfileKey.Name)
		_ = c.Delete(ctx, cm)
	})
	cc.helmCreatesClass(mode)
	if spec != nil {
		cc.cdiCreatesProfile(spec)
	}
	return cc
}

// installStorageProfileCRD gives envtest CDI's StorageProfile API: cluster
// scoped, spec and status left open as the test needs them.
func installStorageProfileCRD(t *testing.T, cfg *rest.Config) {
	t.Helper()
	open := apiextensionsv1.JSONSchemaProps{Type: "object", XPreserveUnknownFields: ptrTo(true)}
	crd := &apiextensionsv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: "storageprofiles.cdi.kubevirt.io"},
		Spec: apiextensionsv1.CustomResourceDefinitionSpec{
			Group: storageProfileGVK.Group,
			Scope: apiextensionsv1.ClusterScoped,
			Names: apiextensionsv1.CustomResourceDefinitionNames{
				Kind: "StorageProfile", ListKind: "StorageProfileList", Plural: "storageprofiles", Singular: "storageprofile",
			},
			Versions: []apiextensionsv1.CustomResourceDefinitionVersion{{
				Name: storageProfileGVK.Version, Served: true, Storage: true,
				Schema: &apiextensionsv1.CustomResourceValidation{OpenAPIV3Schema: &apiextensionsv1.JSONSchemaProps{
					Type:       "object",
					Properties: map[string]apiextensionsv1.JSONSchemaProps{"spec": open, "status": open},
				}},
				Subresources: &apiextensionsv1.CustomResourceSubresources{Status: &apiextensionsv1.CustomResourceSubresourceStatus{}},
			}},
		},
	}
	if _, err := envtest.InstallCRDs(cfg, envtest.CRDInstallOptions{
		CRDs: []*apiextensionsv1.CustomResourceDefinition{crd},
	}); err != nil {
		t.Fatalf("install the StorageProfile CRD: %v", err)
	}
}

func ptrTo[T any](v T) *T { return &v }

func newStorageProfile() *unstructured.Unstructured {
	p := &unstructured.Unstructured{}
	p.SetGroupVersionKind(storageProfileGVK)
	p.SetName(topolvmStorageClass)
	return p
}

// helmCreatesClass creates the topolvm-provisioner class, as helm does.
func (cc *cdiCluster) helmCreatesClass(mode storagev1.VolumeBindingMode) {
	cc.t.Helper()
	sc := &storagev1.StorageClass{
		ObjectMeta: metav1.ObjectMeta{Name: topolvmStorageClass}, Provisioner: "topolvm.io", VolumeBindingMode: &mode,
	}
	if err := cc.c.Create(context.Background(), sc); err != nil {
		cc.t.Fatalf("helm: create the StorageClass: %v", err)
	}
}

// cdiCreatesProfile creates the class's StorageProfile with spec: CDI creates
// it empty, then the admin edits it.
func (cc *cdiCluster) cdiCreatesProfile(spec map[string]interface{}) {
	cc.t.Helper()
	p := newStorageProfile()
	p.Object["spec"] = spec
	if err := cc.c.Create(context.Background(), p); err != nil {
		cc.t.Fatalf("CDI: create the StorageProfile: %v", err)
	}
}

// cdiDeletesProfile is what CDI does once the profile's class is gone.
func (cc *cdiCluster) cdiDeletesProfile() {
	cc.t.Helper()
	if err := cc.c.Delete(context.Background(), newStorageProfile()); err != nil {
		cc.t.Fatalf("CDI: delete the StorageProfile: %v", err)
	}
}

// hook is a TopolvmHook on the cluster's clock: a new one stands for an
// operator restart.
func (cc *cdiCluster) hook() *TopolvmHook {
	h := NewTopolvmHook(cc.c, logr.Discard())
	h.now = func() time.Time { return cc.clock }
	return h
}

// preInstall runs the hook before a helm upgrade with values.
func (cc *cdiCluster) preInstall(h *TopolvmHook, values string) {
	cc.t.Helper()
	spec := k4allv1alpha1.ComponentSpec{Type: "helm", Version: "17.3.1", Namespace: "topolvm-system",
		Values: &apiextensionsv1.JSON{Raw: []byte(values)}}
	if _, err := h.PreInstall(context.Background(), "topolvm", spec, k4allv1alpha1.ClusterConfigSpec{}, map[string]interface{}{}); err != nil {
		cc.t.Fatalf("PreInstall: %v", err)
	}
}

// sync runs the hook's Sync as the reconciler does after the install and on
// every reconcile, and returns how soon it asks to run again.
func (cc *cdiCluster) sync(h Hook) time.Duration {
	cc.t.Helper()
	syncer, ok := h.(Syncer)
	if !ok {
		cc.t.Fatal("TopolvmHook is no Syncer: nothing puts the StorageProfile spec back after helm")
	}
	after, err := syncer.Sync(context.Background(), k4allv1alpha1.ComponentSpec{}, k4allv1alpha1.ClusterConfigSpec{})
	if err != nil {
		cc.t.Fatalf("Sync: %v", err)
	}
	return after
}

func (cc *cdiCluster) classGone() {
	cc.t.Helper()
	if _, ok := storageClassMode(cc.t, cc.c); ok {
		cc.t.Fatal("StorageClass kept, want it deleted for helm to recreate")
	}
}

// profile is the StorageProfile's spec and uid; ok is false when there is none.
func (cc *cdiCluster) profile() (map[string]interface{}, types.UID, bool) {
	cc.t.Helper()
	p := newStorageProfile()
	err := cc.c.Get(context.Background(), types.NamespacedName{Name: topolvmStorageClass}, p)
	if apierrors.IsNotFound(err) {
		return nil, "", false
	}
	if err != nil {
		cc.t.Fatal(err)
	}
	spec, _, _ := unstructured.NestedMap(p.Object, "spec")
	return spec, p.GetUID(), true
}

func (cc *cdiCluster) expectProfileSpec(want map[string]interface{}) {
	cc.t.Helper()
	spec, _, ok := cc.profile()
	if !ok {
		cc.t.Fatal("no StorageProfile")
	}
	if len(want) == 0 && len(spec) == 0 {
		return
	}
	if !reflect.DeepEqual(spec, want) {
		cc.t.Errorf("StorageProfile spec = %v, want %v", spec, want)
	}
}

// saved tells whether the ConfigMap that keeps the spec exists.
func (cc *cdiCluster) saved() bool {
	cc.t.Helper()
	cm := &unstructured.Unstructured{}
	cm.SetAPIVersion("v1")
	cm.SetKind("ConfigMap")
	err := cc.c.Get(context.Background(), savedProfileKey, cm)
	if apierrors.IsNotFound(err) {
		return false
	}
	if err != nil {
		cc.t.Fatal(err)
	}
	return true
}

// gpillon/k4all#80: when the hook deletes the class for a new
// volumeBindingMode, CDI deletes its StorageProfile and creates it empty once
// helm has created the class again. The admin's spec comes back.
func TestTopolvmHook_KeepsTheStorageProfileSpecAcrossTheRecreate(t *testing.T) {
	cc := newCDICluster(t, storagev1.VolumeBindingImmediate, adminProfileSpec)
	h := cc.hook()

	cc.preInstall(h, valuesWFFC)
	cc.classGone()
	cc.cdiDeletesProfile()
	cc.helmCreatesClass(storagev1.VolumeBindingWaitForFirstConsumer)
	cc.cdiCreatesProfile(map[string]interface{}{})

	if after := cc.sync(h); after != 0 {
		t.Errorf("Sync asks to run again in %s, want nothing left to do", after)
	}
	cc.expectProfileSpec(adminProfileSpec)
	if cc.saved() {
		t.Error("the saved spec is kept after the restore, want it cleaned up")
	}
}

// CDI creates the profile seconds after helm created the class: Sync asks to
// run again soon until it is there.
func TestTopolvmHook_WaitsForCDIToCreateTheStorageProfile(t *testing.T) {
	cc := newCDICluster(t, storagev1.VolumeBindingImmediate, adminProfileSpec)
	h := cc.hook()

	cc.preInstall(h, valuesWFFC)
	cc.cdiDeletesProfile()
	cc.helmCreatesClass(storagev1.VolumeBindingWaitForFirstConsumer)

	after := cc.sync(h)
	if after <= 0 || after > time.Minute {
		t.Errorf("Sync with no StorageProfile yet asks to run again in %s, want within a minute", after)
	}
	if !cc.saved() {
		t.Fatal("the saved spec is gone before CDI created the StorageProfile")
	}

	cc.cdiCreatesProfile(map[string]interface{}{})
	cc.clock = cc.clock.Add(after)
	if after := cc.sync(h); after != 0 {
		t.Errorf("Sync asks to run again in %s, want nothing left to do", after)
	}
	cc.expectProfileSpec(adminProfileSpec)
	if cc.saved() {
		t.Error("the saved spec is kept after the restore, want it cleaned up")
	}
}

// CDI never creates the profile (its controller is down): after a while Sync
// stops asking to run soon, and the periodic reconcile puts the spec back
// once the profile is there.
func TestTopolvmHook_KeepsTheSavedSpecUntilCDICreatesTheStorageProfile(t *testing.T) {
	cc := newCDICluster(t, storagev1.VolumeBindingImmediate, adminProfileSpec)
	h := cc.hook()

	cc.preInstall(h, valuesWFFC)
	cc.cdiDeletesProfile()
	cc.helmCreatesClass(storagev1.VolumeBindingWaitForFirstConsumer)

	cc.clock = cc.clock.Add(time.Hour)
	if after := cc.sync(h); after != 0 {
		t.Errorf("Sync an hour later asks to run again in %s, want the periodic reconcile", after)
	}
	if !cc.saved() {
		t.Fatal("the saved spec is gone before CDI created the StorageProfile")
	}

	cc.cdiCreatesProfile(map[string]interface{}{})
	cc.sync(h)
	cc.expectProfileSpec(adminProfileSpec)
	if cc.saved() {
		t.Error("the saved spec is kept after the restore, want it cleaned up")
	}
}

// The spec lives in the cluster, not in the operator: an operator restarted
// between the delete and helm runs PreInstall again (no class, no profile)
// and still puts it back.
func TestTopolvmHook_RestoresTheStorageProfileAfterAnOperatorRestart(t *testing.T) {
	cc := newCDICluster(t, storagev1.VolumeBindingImmediate, adminProfileSpec)

	cc.preInstall(cc.hook(), valuesWFFC)
	cc.cdiDeletesProfile()

	restarted := cc.hook()
	cc.preInstall(restarted, valuesWFFC)
	cc.helmCreatesClass(storagev1.VolumeBindingWaitForFirstConsumer)
	cc.cdiCreatesProfile(map[string]interface{}{})
	cc.sync(restarted)

	cc.expectProfileSpec(adminProfileSpec)
	if cc.saved() {
		t.Error("the saved spec is kept after the restore, want it cleaned up")
	}
}

// The mode changes again before the spec is back: the second recreate finds
// CDI's empty profile, and the first saved spec still wins.
func TestTopolvmHook_KeepsTheFirstSavedSpecOverASecondRecreate(t *testing.T) {
	cc := newCDICluster(t, storagev1.VolumeBindingImmediate, adminProfileSpec)
	h := cc.hook()

	cc.preInstall(h, valuesWFFC)
	cc.cdiDeletesProfile()
	cc.helmCreatesClass(storagev1.VolumeBindingWaitForFirstConsumer)
	cc.cdiCreatesProfile(map[string]interface{}{})

	cc.preInstall(h, valuesImmediate)
	cc.classGone()
	cc.cdiDeletesProfile()
	cc.helmCreatesClass(storagev1.VolumeBindingImmediate)
	cc.cdiCreatesProfile(map[string]interface{}{})
	cc.sync(h)

	cc.expectProfileSpec(adminProfileSpec)
}

// Helm can create the class again before CDI sees it gone: CDI then keeps the
// profile with its spec. Sync waits a little to be sure, then cleans up
// without touching the profile.
func TestTopolvmHook_LeavesAStorageProfileCDIKept(t *testing.T) {
	cc := newCDICluster(t, storagev1.VolumeBindingImmediate, adminProfileSpec)
	h := cc.hook()
	_, uid, _ := cc.profile()

	cc.preInstall(h, valuesWFFC)
	cc.helmCreatesClass(storagev1.VolumeBindingWaitForFirstConsumer)

	if after := cc.sync(h); after <= 0 {
		t.Error("Sync right after the recreate asks for no recheck, want to wait for CDI")
	}
	if !cc.saved() {
		t.Fatal("the saved spec is gone while CDI may still delete the profile")
	}

	cc.clock = time.Now().Add(5 * time.Minute)
	if after := cc.sync(h); after != 0 {
		t.Errorf("Sync asks to run again in %s, want nothing left to do", after)
	}
	cc.expectProfileSpec(adminProfileSpec)
	if _, now, _ := cc.profile(); now != uid {
		t.Errorf("StorageProfile uid = %s, want %s untouched", now, uid)
	}
	if cc.saved() {
		t.Error("the saved spec is kept, want it cleaned up")
	}
}

// Nothing to keep: no profile for the class, or one with CDI's empty spec.
func TestTopolvmHook_SavesNothingWithoutAStorageProfileSpec(t *testing.T) {
	for name, spec := range map[string]map[string]interface{}{
		"no StorageProfile": nil,
		"empty spec":        {},
	} {
		t.Run(name, func(t *testing.T) {
			cc := newCDICluster(t, storagev1.VolumeBindingImmediate, spec)
			h := cc.hook()

			cc.preInstall(h, valuesWFFC)
			cc.classGone()
			if cc.saved() {
				t.Error("a spec is saved, want nothing to keep")
			}
			cc.helmCreatesClass(storagev1.VolumeBindingWaitForFirstConsumer)
			if after := cc.sync(h); after != 0 {
				t.Errorf("Sync asks to run again in %s, want nothing to do", after)
			}
		})
	}
}

// The class keeps its mode: the hook leaves the profile alone.
func TestTopolvmHook_LeavesTheStorageProfileWhenTheClassStays(t *testing.T) {
	cc := newCDICluster(t, storagev1.VolumeBindingWaitForFirstConsumer, adminProfileSpec)
	h := cc.hook()

	cc.preInstall(h, valuesWFFC)

	if cc.saved() {
		t.Error("a spec is saved, want nothing to keep")
	}
	cc.sync(h)
	cc.expectProfileSpec(adminProfileSpec)
}

// Without CDI there is no StorageProfile API: the class is recreated as
// before, nothing is saved, and Sync has nothing to do.
func TestTopolvmHook_RecreatesTheClassWithoutCDI(t *testing.T) {
	immediate := storagev1.VolumeBindingImmediate
	c := fake.NewClientBuilder().WithScheme(scheme.Scheme).
		WithObjects(&storagev1.StorageClass{
			ObjectMeta: metav1.ObjectMeta{Name: topolvmStorageClass}, Provisioner: "topolvm.io", VolumeBindingMode: &immediate,
		}).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if obj.GetObjectKind().GroupVersionKind() == storageProfileGVK {
					return &meta.NoKindMatchError{GroupKind: storageProfileGVK.GroupKind(), SearchedVersions: []string{"v1beta1"}}
				}
				return c.Get(ctx, key, obj, opts...)
			},
		}).Build()
	cc := &cdiCluster{t: t, c: c, clock: time.Now()}
	h := cc.hook()

	cc.preInstall(h, valuesWFFC)

	cc.classGone()
	if cc.saved() {
		t.Error("a spec is saved without CDI")
	}
	if after := cc.sync(h); after != 0 {
		t.Errorf("Sync asks to run again in %s, want nothing to do", after)
	}
}
