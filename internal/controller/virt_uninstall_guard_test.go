package controller

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	schedulingv1 "k8s.io/api/scheduling/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	k4allv1alpha1 "github.com/gpillon/k4all-operator/api/v1alpha1"
)

// workloadCRDs serves VirtualMachine, VirtualMachineInstance and DataVolume:
// KubeVirt's and CDI's operators create these kinds when they run.
var workloadCRDs = filepath.Join("testdata", "crds", "kubevirt-cdi-workloads.yaml")

// virtWorkloadAPIs makes the shared envtest serve KubeVirt's and CDI's
// workload kinds for the test, and takes them away after it: every other
// test sees a cluster where neither ever ran.
func virtWorkloadAPIs(t *testing.T) {
	t.Helper()
	cfg := envtestConfig(t)
	crds, err := envtest.InstallCRDs(cfg, envtest.CRDInstallOptions{Paths: []string{workloadCRDs}, ErrorIfPathMissing: true})
	if err != nil {
		t.Fatalf("install the workload CRDs: %v", err)
	}
	t.Cleanup(func() {
		s := runtime.NewScheme()
		if err := apiextensionsv1.AddToScheme(s); err != nil {
			t.Error(err)
			return
		}
		c, err := client.New(cfg, client.Options{Scheme: s})
		if err != nil {
			t.Error(err)
			return
		}
		ctx := context.Background()
		for _, crd := range crds {
			if err := c.Delete(ctx, crd); client.IgnoreNotFound(err) != nil {
				t.Errorf("delete CRD %s: %v", crd.Name, err)
			}
		}
		err = wait.PollUntilContextTimeout(ctx, 100*time.Millisecond, 30*time.Second, true,
			func(ctx context.Context) (bool, error) {
				for _, crd := range crds {
					err := c.Get(ctx, client.ObjectKeyFromObject(crd), &apiextensionsv1.CustomResourceDefinition{})
					if !apierrors.IsNotFound(err) {
						return false, client.IgnoreNotFound(err)
					}
				}
				return true, nil
			})
		if err != nil {
			t.Errorf("the workload CRDs are still there: %v", err)
		}
	})
}

// virtOnEnvtest is a cluster with the virt feature off where the component
// name (kubevirt or cdi, with its built-in hook) is still
// installed: the ConfigMap it returns stands for the component's objects.
func virtOnEnvtest(t *testing.T, name string) (client.Client, *ReleaseReconciler, *corev1.ConfigMap) {
	t.Helper()
	spec := k4allv1alpha1.ComponentSpec{
		Type: k4allv1alpha1.ComponentTypeManifest, Version: "v1", InlineManifests: []string{guardedConfigMap},
		FeatureGate: "virt",
	}
	c, r := componentOnEnvtest(t, name, spec, k4allv1alpha1.ComponentStatus{
		State: k4allv1alpha1.ComponentStateInstalled, InstalledVersion: "v1", LastTransitionTime: metav1.Now(),
	})
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "guarded", Namespace: "default"}}
	createAll(t, c, cm)
	return c, r, cm
}

// createAll creates the objects, deleted again after the test.
func createAll(t *testing.T, c client.Client, objs ...client.Object) {
	t.Helper()
	for _, obj := range objs {
		if err := c.Create(context.Background(), obj); err != nil {
			t.Fatalf("create %s %s: %v", obj.GetObjectKind().GroupVersionKind().Kind, obj.GetName(), err)
		}
		t.Cleanup(func() { _ = deleteNow(c, obj) })
	}
}

// deleteNow deletes the object and lets it go at once: the API server puts
// kubernetes.io/pvc-protection on PVCs, and no controller removes it here.
func deleteNow(c client.Client, obj client.Object) error {
	ctx := context.Background()
	if err := c.Delete(ctx, obj); err != nil {
		return client.IgnoreNotFound(err)
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(obj), obj); err != nil {
		return client.IgnoreNotFound(err)
	}
	obj.SetFinalizers(nil)
	return client.IgnoreNotFound(c.Update(ctx, obj))
}

// kubevirtObject is a KubeVirt workload of kind in the default namespace.
func kubevirtObject(kind, name string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetAPIVersion("kubevirt.io/v1")
	u.SetKind(kind)
	u.SetNamespace("default")
	u.SetName(name)
	return u
}

func componentStatus(t *testing.T, c client.Client, name string) k4allv1alpha1.ComponentStatus {
	t.Helper()
	rm := &k4allv1alpha1.ReleaseManifest{}
	if err := c.Get(context.Background(), client.ObjectKey{Name: "k4all"}, rm); err != nil {
		t.Fatal(err)
	}
	return rm.Status.Components[name]
}

// assertUninstallRefused fails the test unless the component name is still
// installed: its objects are there, and its status is Installed at v1 with
// a message that starts with want.
func assertUninstallRefused(t *testing.T, c client.Client, name string, objects *corev1.ConfigMap, want string) {
	t.Helper()
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(objects), &corev1.ConfigMap{}); err != nil {
		t.Fatalf("%s's objects: %v, want them kept", name, err)
	}
	st := componentStatus(t, c, name)
	if st.State != k4allv1alpha1.ComponentStateInstalled || st.InstalledVersion != "v1" ||
		!strings.HasPrefix(st.Message, want) {
		t.Fatalf("%s: %s at %q (%s), want Installed at v1 with the message %q…",
			name, st.State, st.InstalledVersion, st.Message, want)
	}
}

// assertUninstalled fails the test unless the component name went: its
// objects deleted, its status Skipped.
func assertUninstalled(t *testing.T, c client.Client, name string, objects *corev1.ConfigMap) {
	t.Helper()
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(objects), &corev1.ConfigMap{}); !apierrors.IsNotFound(err) {
		t.Fatalf("%s's objects: get = %v, want them uninstalled", name, err)
	}
	if st := componentStatus(t, c, name); st.State != k4allv1alpha1.ComponentStateSkipped {
		t.Fatalf("%s: %s (%s), want Skipped", name, st.State, st.Message)
	}
}

// Turning virt off keeps KubeVirt while a VM or a VMI exists: its uninstall
// deletes virt-operator and KubeVirt's namespace from under them. It goes
// at the first reconcile after they are gone.
func TestReconcile_KubeVirtStaysWhileVMsExist(t *testing.T) {
	virtWorkloadAPIs(t)
	for _, tc := range []struct{ kind, want string }{
		{"VirtualMachine", "uninstall refused: kubevirt: 1 VirtualMachine (default/vm1) exists: "},
		{"VirtualMachineInstance", "uninstall refused: kubevirt: 1 VirtualMachineInstance (default/vm1) exists: "},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			c, r, objects := virtOnEnvtest(t, "kubevirt")
			vm := kubevirtObject(tc.kind, "vm1")
			createAll(t, c, vm)

			reconcileProbe(t, r)
			assertUninstallRefused(t, c, "kubevirt", objects, tc.want)

			if err := deleteNow(c, vm); err != nil {
				t.Fatal(err)
			}
			reconcileProbe(t, r)
			assertUninstalled(t, c, "kubevirt", objects)
		})
	}
}

// k4all v2.7.0 removes kubevirt-manager (management Removed) on every
// cluster, VMs or not: Hangar replaces it. Its hook removes it in place of
// the engine, and keeps what VMs need: with a VM that names vm-standard, the
// console's roles go, vm-standard and the VM stay, and the unused
// vm-preemptible goes. Nothing listed by the component's manifests is
// deleted (the ConfigMap stands for them). A guard refusing while VMs exist
// would keep kubevirt-manager forever on such clusters.
func TestReconcile_KubevirtManagerRemovedWithVMsGoesThroughItsHook(t *testing.T) {
	virtWorkloadAPIs(t)
	spec := k4allv1alpha1.ComponentSpec{
		Type: k4allv1alpha1.ComponentTypeManifest, Version: "v1.5.4", InlineManifests: []string{guardedConfigMap},
		Management: k4allv1alpha1.ComponentManagementRemoved, FeatureGate: "virt",
	}
	c, r := componentOnEnvtest(t, "kubevirt-manager", spec, k4allv1alpha1.ComponentStatus{
		State: k4allv1alpha1.ComponentStateInstalled, InstalledVersion: "v1.5.4", LastTransitionTime: metav1.Now(),
	})
	manifests := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "guarded", Namespace: "default"}}
	role := &rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: "kubevirt-manager"}}
	standard := &schedulingv1.PriorityClass{ObjectMeta: metav1.ObjectMeta{Name: "vm-standard"}, Value: 999999999}
	preemptible := &schedulingv1.PriorityClass{ObjectMeta: metav1.ObjectMeta{Name: "vm-preemptible"}, Value: 1000000}
	vm := kubevirtObject("VirtualMachine", "vm1")
	if err := unstructured.SetNestedField(vm.Object, "vm-standard", "spec", "template", "spec", "priorityClassName"); err != nil {
		t.Fatal(err)
	}
	createAll(t, c, manifests, role, standard, preemptible, vm)

	reconcileProbe(t, r)

	if st := componentStatus(t, c, "kubevirt-manager"); st.State != k4allv1alpha1.ComponentStateRemoved {
		t.Fatalf("kubevirt-manager: %s (%s), want Removed", st.State, st.Message)
	}
	gone := func(obj client.Object) bool {
		return apierrors.IsNotFound(c.Get(context.Background(), client.ObjectKeyFromObject(obj), obj))
	}
	if !gone(role) {
		t.Error("ClusterRole kubevirt-manager kept")
	}
	if !gone(preemptible) {
		t.Error("PriorityClass vm-preemptible kept with nothing using it")
	}
	for _, kept := range []client.Object{vm, standard, manifests} {
		if gone(kept) {
			t.Errorf("%s %s deleted", kept.GetObjectKind().GroupVersionKind().Kind, kept.GetName())
		}
	}
}

// pvc is a PersistentVolumeClaim in the default namespace.
func pvc(name string, labels, annotations map[string]string) *corev1.PersistentVolumeClaim {
	return &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: name, Labels: labels, Annotations: annotations},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")},
			},
		},
	}
}

// Turning virt off keeps CDI while a DataVolume or a PVC CDI made exists:
// its uninstall would leave them half-managed. A PVC CDI did not make does
// not keep it.
func TestReconcile_CDIStaysWhileItsVolumesExist(t *testing.T) {
	virtWorkloadAPIs(t)
	dv := &unstructured.Unstructured{}
	dv.SetAPIVersion("cdi.kubevirt.io/v1beta1")
	dv.SetKind("DataVolume")
	dv.SetNamespace("default")
	dv.SetName("disk")
	for _, tc := range []struct {
		name string
		obj  client.Object
		want string
	}{
		{"a DataVolume", dv, "uninstall refused: cdi: 1 DataVolume (default/disk) exists: "},
		// CDI v1.66 marks every PVC it makes for a DataVolume with this
		// annotation; the DataVolume's own labels may replace its app label.
		{"the PVC of a DataVolume", pvc("disk", map[string]string{"app": "web"},
			map[string]string{"cdi.kubevirt.io/createdForDataVolume": "0c7e0d6a"}),
			"uninstall refused: cdi: 1 PVC made by CDI (default/disk) exists: "},
		// The label of the PVCs CDI makes (scratch space of an import).
		{"a PVC with CDI's label", pvc("disk-scratch", map[string]string{"app": "containerized-data-importer"}, nil),
			"uninstall refused: cdi: 1 PVC made by CDI (default/disk-scratch) exists: "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, r, objects := virtOnEnvtest(t, "cdi")
			createAll(t, c, pvc("data", map[string]string{"app": "postgres"}, nil), tc.obj)

			reconcileProbe(t, r)
			assertUninstallRefused(t, c, "cdi", objects, tc.want)

			if err := deleteNow(c, tc.obj); err != nil {
				t.Fatal(err)
			}
			reconcileProbe(t, r)
			assertUninstalled(t, c, "cdi", objects)
		})
	}
}

// On a cluster that does not serve KubeVirt's and CDI's workload kinds
// (their operators never ran, or the CRDs are gone), turning virt off
// uninstalls them: nothing can need them.
func TestReconcile_VirtUninstallsWhereTheWorkloadKindsAreNotServed(t *testing.T) {
	for _, component := range []string{"kubevirt", "cdi"} {
		t.Run(component, func(t *testing.T) {
			c, r, objects := virtOnEnvtest(t, component)
			crd := &unstructured.Unstructured{}
			crd.SetAPIVersion("apiextensions.k8s.io/v1")
			crd.SetKind("CustomResourceDefinition")
			for _, name := range []string{"virtualmachines.kubevirt.io", "datavolumes.cdi.kubevirt.io"} {
				if err := c.Get(context.Background(), client.ObjectKey{Name: name}, crd); !apierrors.IsNotFound(err) {
					t.Fatalf("CRD %s: get = %v, want it not there (left by another test?)", name, err)
				}
			}

			reconcileProbe(t, r)
			assertUninstalled(t, c, component, objects)
		})
	}
}

// The refusal counts the VMs of each kind and names the first few, for the
// ClusterConfig's user to find them.
func TestReconcile_KubeVirtRefusalCountsTheVMsAndNamesAFew(t *testing.T) {
	virtWorkloadAPIs(t)
	c, r, objects := virtOnEnvtest(t, "kubevirt")
	for _, name := range []string{"db", "web-1", "web-2", "web-3"} {
		createAll(t, c, kubevirtObject("VirtualMachine", name))
	}
	createAll(t, c, kubevirtObject("VirtualMachineInstance", "db"), kubevirtObject("VirtualMachineInstance", "web-1"))

	reconcileProbe(t, r)

	assertUninstallRefused(t, c, "kubevirt", objects, "uninstall refused: kubevirt: "+
		"4 VirtualMachines (default/db, default/web-1, default/web-2, …) and "+
		"2 VirtualMachineInstances (default/db, default/web-1) exist: delete them or set kubevirt Unmanaged first")
}

// While an uninstall is refused, the reconcile comes back within a minute,
// not at the 5-minute resync: the uninstall starts soon after the user
// deleted the last VM (on the lab it waited 4m20s). Nothing refused, the
// resync is back to 5 minutes.
func TestReconcile_ARefusedUninstallIsAskedAgainWithinAMinute(t *testing.T) {
	virtWorkloadAPIs(t)
	c, r, objects := virtOnEnvtest(t, "kubevirt")
	vm := kubevirtObject("VirtualMachine", "vm1")
	createAll(t, c, vm)

	res := reconcileProbe(t, r)

	assertUninstallRefused(t, c, "kubevirt", objects, "uninstall refused: ")
	if res.RequeueAfter <= 0 || res.RequeueAfter > time.Minute {
		t.Fatalf("RequeueAfter %s with the uninstall refused, want at most a minute", res.RequeueAfter)
	}

	if err := deleteNow(c, vm); err != nil {
		t.Fatal(err)
	}
	res = reconcileProbe(t, r)

	assertUninstalled(t, c, "kubevirt", objects)
	if res.RequeueAfter != 5*time.Minute {
		t.Fatalf("RequeueAfter %s with nothing refused, want the 5-minute resync", res.RequeueAfter)
	}
}
