package hooks

import (
	"context"
	"fmt"
	"testing"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	k4allv1alpha1 "github.com/gpillon/k4all-operator/api/v1alpha1"
)

// The k4all values replace the chart's storageClasses list; up to v2.2.x
// without volumeBindingMode, so the API server made the class Immediate.
const (
	valuesImmediate = `{"storageClasses":[{"name":"topolvm-provisioner","storageClass":{"fsType":"xfs","isDefaultClass":true}}]}`
	valuesWFFC      = `{"storageClasses":[{"name":"topolvm-provisioner","storageClass":{"fsType":"xfs","isDefaultClass":true,` +
		`"volumeBindingMode":"WaitForFirstConsumer"}}]}`
)

// volumes numbers the PVs and PVCs: envtest never finishes their deletion
// (pv-protection, pvc-protection finalizers), so names are not reused.
var volumes int

// boundVolume creates the topolvm-provisioner StorageClass with mode, and a
// PVC bound to a PV of that class.
func boundVolume(t *testing.T, c client.Client, mode storagev1.VolumeBindingMode) *corev1.PersistentVolumeClaim {
	t.Helper()
	volumes++
	name := fmt.Sprintf("data-%d", volumes)
	ctx := context.Background()
	class := "topolvm-provisioner"
	sc := &storagev1.StorageClass{
		ObjectMeta: metav1.ObjectMeta{Name: class}, Provisioner: "topolvm.io", VolumeBindingMode: &mode,
	}
	pv := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "pv-" + name},
		Spec: corev1.PersistentVolumeSpec{
			StorageClassName: class,
			Capacity:         corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")},
			AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			PersistentVolumeSource: corev1.PersistentVolumeSource{
				CSI: &corev1.CSIPersistentVolumeSource{Driver: "topolvm.io", VolumeHandle: "v1"},
			},
		},
	}
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: name},
		Spec: corev1.PersistentVolumeClaimSpec{
			StorageClassName: &class,
			VolumeName:       pv.Name,
			AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")},
			},
		},
	}
	for _, o := range []client.Object{sc, pv, pvc} {
		if err := c.Create(ctx, o); err != nil {
			t.Fatalf("create %s: %v", o.GetName(), err)
		}
		t.Cleanup(func() { _ = c.Delete(context.Background(), o) })
	}
	pvc.Status.Phase = corev1.ClaimBound
	if err := c.Status().Update(ctx, pvc); err != nil {
		t.Fatal(err)
	}
	return pvc
}

// preInstallTopolvm runs the topolvm hook before a helm upgrade with the
// ReleaseManifest values and the ClusterConfig componentOverrides.
func preInstallTopolvm(t *testing.T, c client.Client, values string, override *k4allv1alpha1.ComponentOverride) {
	t.Helper()
	spec := k4allv1alpha1.ComponentSpec{Type: "helm", Version: "17.3.1", Namespace: "topolvm-system"}
	if values != "" {
		spec.Values = &apiextensionsv1.JSON{Raw: []byte(values)}
	}
	var config k4allv1alpha1.ClusterConfigSpec
	if override != nil {
		config.ComponentOverrides = map[string]k4allv1alpha1.ComponentOverride{"topolvm": *override}
	}
	h := NewTopolvmHook(c, logr.Discard())
	if _, err := h.PreInstall(context.Background(), "topolvm", spec, config, map[string]interface{}{}); err != nil {
		t.Fatal(err)
	}
}

func storageClassMode(t *testing.T, c client.Client) (storagev1.VolumeBindingMode, bool) {
	t.Helper()
	sc := &storagev1.StorageClass{}
	err := c.Get(context.Background(), types.NamespacedName{Name: "topolvm-provisioner"}, sc)
	if apierrors.IsNotFound(err) {
		return "", false
	}
	if err != nil {
		t.Fatal(err)
	}
	return *sc.VolumeBindingMode, true
}

func expectBound(t *testing.T, c client.Client, pvc *corev1.PersistentVolumeClaim) {
	t.Helper()
	got := &corev1.PersistentVolumeClaim{}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(pvc), got); err != nil {
		t.Fatalf("PVC %s: %v", pvc.Name, err)
	}
	if got.Status.Phase != corev1.ClaimBound || got.Spec.VolumeName != "pv-"+pvc.Name {
		t.Errorf("PVC %s = %s on %q, want Bound on pv-%s", pvc.Name, got.Status.Phase, got.Spec.VolumeName, pvc.Name)
	}
}

// volumeBindingMode is immutable: when the values change it, the hook
// deletes the StorageClass so that helm creates it again. PVs and PVCs name
// their class and stay bound.
func TestTopolvmHook_RecreatesTheStorageClassForANewBindingMode(t *testing.T) {
	for name, tc := range map[string]struct {
		values   string
		override *k4allv1alpha1.ComponentOverride
	}{
		"values ask WaitForFirstConsumer": {values: valuesWFFC},
		"chart default list":              {values: `{"foo":1}`},
		"override asks WaitForFirstConsumer": {values: valuesImmediate, override: &k4allv1alpha1.ComponentOverride{
			Values: &apiextensionsv1.JSON{Raw: []byte(valuesWFFC)},
		}},
	} {
		t.Run(name, func(t *testing.T) {
			c, _ := envtestClient(t)
			pvc := boundVolume(t, c, storagev1.VolumeBindingImmediate)

			preInstallTopolvm(t, c, tc.values, tc.override)

			if mode, ok := storageClassMode(t, c); ok {
				t.Errorf("StorageClass kept with %s, want it deleted for helm to recreate", mode)
			}
			expectBound(t, c, pvc)
		})
	}
}

func TestTopolvmHook_KeepsAStorageClassWithTheSameBindingMode(t *testing.T) {
	for name, tc := range map[string]struct {
		mode     storagev1.VolumeBindingMode
		values   string
		override *k4allv1alpha1.ComponentOverride
	}{
		"Immediate, not in the values": {mode: storagev1.VolumeBindingImmediate, values: valuesImmediate},
		"WaitForFirstConsumer":         {mode: storagev1.VolumeBindingWaitForFirstConsumer, values: valuesWFFC},
		"the user's override wins": {mode: storagev1.VolumeBindingImmediate, values: valuesWFFC,
			override: &k4allv1alpha1.ComponentOverride{Values: &apiextensionsv1.JSON{Raw: []byte(valuesImmediate)}}},
	} {
		t.Run(name, func(t *testing.T) {
			c, _ := envtestClient(t)
			pvc := boundVolume(t, c, tc.mode)

			preInstallTopolvm(t, c, tc.values, tc.override)

			if mode, ok := storageClassMode(t, c); !ok || mode != tc.mode {
				t.Errorf("StorageClass = %s (exists %t), want %s kept", mode, ok, tc.mode)
			}
			expectBound(t, c, pvc)
		})
	}
}
