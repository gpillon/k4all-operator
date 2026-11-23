package hooks

import (
	"context"
	"testing"
	"time"

	"github.com/go-logr/logr"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	k4allv1alpha1 "github.com/gpillon/k4all-operator/api/v1alpha1"
)

var (
	vmGVK    = schema.GroupVersionKind{Group: "kubevirt.io", Version: "v1", Kind: "VirtualMachine"}
	imageGVK = schema.GroupVersionKind{Group: "kubevirt-manager.io", Version: "v1", Kind: "Image"}
)

// kubevirtManagerObjects is what kubevirt-manager v1.5.4 (bundled.yaml and
// crd.yaml, the version k4all v2.2-v2.6 pins) creates.
func kubevirtManagerObjects() []client.Object {
	ns := "kubevirt-manager"
	objs := []client.Object{
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "kubevirt-manager"}},
		&corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "kubevirt-manager"}},
		&corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "kubevirt-manager"}},
		&apiextensionsv1.CustomResourceDefinition{ObjectMeta: metav1.ObjectMeta{Name: "images.kubevirt-manager.io"}},
	}
	for _, name := range []string{"kubevirt-manager", "kubevirt-manager-kccm", "kubevirt-manager-cas-workload", "kubevirt-manager-cas-management"} {
		objs = append(objs,
			&rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: name}},
			&rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: name}})
	}
	return objs
}

// priorityClass is one of the two kubevirt-manager ships.
func priorityClass(name string) *unstructured.Unstructured {
	pc := &unstructured.Unstructured{}
	pc.SetAPIVersion("scheduling.k8s.io/v1")
	pc.SetKind("PriorityClass")
	pc.SetName(name)
	return pc
}

// virtualMachine is a VM in ns whose pods run with priorityClass ("" none).
func virtualMachine(ns, name, priorityClass string) *unstructured.Unstructured {
	vm := &unstructured.Unstructured{}
	vm.SetGroupVersionKind(vmGVK)
	vm.SetNamespace(ns)
	vm.SetName(name)
	spec := map[string]interface{}{"domain": map[string]interface{}{}}
	if priorityClass != "" {
		spec["priorityClassName"] = priorityClass
	}
	_ = unstructured.SetNestedMap(vm.Object, map[string]interface{}{"spec": spec}, "spec", "template")
	return vm
}

// kubevirtCluster is a fake cluster that knows KubeVirt's and
// kubevirt-manager's kinds, with objs in it.
func kubevirtCluster(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := apiextensionsv1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	for _, gvk := range []schema.GroupVersionKind{vmGVK, imageGVK} {
		s.AddKnownTypeWithName(gvk, &unstructured.Unstructured{})
		s.AddKnownTypeWithName(gvk.GroupVersion().WithKind(gvk.Kind+"List"), &unstructured.UnstructuredList{})
	}
	return fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).Build()
}

// removeKubevirtManager runs the removal of kubevirt-manager as the
// reconciler does for management: Removed.
func removeKubevirtManager(t *testing.T, c client.Client) {
	t.Helper()
	hook, ok := NewRegistry(c, logr.Discard()).Get("kubevirt-manager").(Remover)
	if !ok {
		t.Fatal("kubevirt-manager has no hook that removes it")
	}
	if err := hook.Remove(context.Background(), k4allv1alpha1.ComponentSpec{Namespace: "kubevirt-manager"}, k4allv1alpha1.ClusterConfigSpec{}); err != nil {
		t.Fatal(err)
	}
}

func exists(t *testing.T, c client.Client, obj client.Object) bool {
	t.Helper()
	err := c.Get(context.Background(), client.ObjectKeyFromObject(obj), obj)
	if apierrors.IsNotFound(err) {
		return false
	}
	if err != nil {
		t.Fatal(err)
	}
	return true
}

// Hangar replaces kubevirt-manager (k4all v2.7.0): its console, service and
// cluster-wide roles go; the VMs it made stay as they are.
func TestKubevirtManagerRemoval_DeletesTheConsoleAndKeepsTheVMs(t *testing.T) {
	vm := virtualMachine("production", "web", "")
	c := kubevirtCluster(t, append(kubevirtManagerObjects(), vm)...)

	removeKubevirtManager(t, c)

	for _, obj := range kubevirtManagerObjects() {
		if _, crd := obj.(*apiextensionsv1.CustomResourceDefinition); crd {
			continue // its own test
		}
		if exists(t, c, obj) {
			t.Errorf("%T %s still there", obj, client.ObjectKeyFromObject(obj))
		}
	}
	if !exists(t, c, vm) {
		t.Error("the VM production/web was deleted")
	}
}

var imagesCRD = &apiextensionsv1.CustomResourceDefinition{ObjectMeta: metav1.ObjectMeta{Name: "images.kubevirt-manager.io"}}

// kubevirt-manager's image catalog (CRD images.kubevirt-manager.io) goes with
// it when nobody put an image in it.
func TestKubevirtManagerRemoval_DeletesAnEmptyImageCatalog(t *testing.T) {
	c := kubevirtCluster(t, kubevirtManagerObjects()...)

	removeKubevirtManager(t, c)

	if exists(t, c, imagesCRD.DeepCopy()) {
		t.Error("CRD images.kubevirt-manager.io kept with no image in it")
	}
}

// Images in the catalog are the user's: deleting the CRD would delete them.
func TestKubevirtManagerRemoval_KeepsAnImageCatalogWithImages(t *testing.T) {
	image := &unstructured.Unstructured{}
	image.SetGroupVersionKind(imageGVK)
	image.SetNamespace("default")
	image.SetName("fedora-cloud")
	c := kubevirtCluster(t, append(kubevirtManagerObjects(), image)...)

	removeKubevirtManager(t, c)

	if !exists(t, c, imagesCRD.DeepCopy()) {
		t.Error("CRD images.kubevirt-manager.io deleted with the image default/fedora-cloud in it")
	}
	if !exists(t, c, image) {
		t.Error("image default/fedora-cloud deleted")
	}
}

// kubevirt-manager's VM form sets priorityClassName to one of its two
// PriorityClasses. Without it a VM that names it can no longer start, nor
// move to another host (the new pod is refused): a class still named by a
// VM or a pod stays; an unused one goes.
func TestKubevirtManagerRemoval_KeepsThePriorityClassesVMsUse(t *testing.T) {
	stopped := virtualMachine("production", "db", "vm-standard")
	launcher := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "lab", Name: "virt-launcher-batch-x"},
		Spec:       corev1.PodSpec{PriorityClassName: "vm-preemptible", Containers: []corev1.Container{{Name: "compute", Image: "x"}}},
	}
	c := kubevirtCluster(t, append(kubevirtManagerObjects(),
		priorityClass("vm-standard"), priorityClass("vm-preemptible"), stopped, launcher)...)

	removeKubevirtManager(t, c)

	for _, name := range []string{"vm-standard", "vm-preemptible"} {
		if !exists(t, c, priorityClass(name)) {
			t.Errorf("PriorityClass %s deleted while in use", name)
		}
	}
}

func TestKubevirtManagerRemoval_DeletesUnusedPriorityClasses(t *testing.T) {
	c := kubevirtCluster(t, append(kubevirtManagerObjects(),
		priorityClass("vm-standard"), priorityClass("vm-preemptible"), virtualMachine("production", "web", ""))...)

	removeKubevirtManager(t, c)

	for _, name := range []string{"vm-standard", "vm-preemptible"} {
		if exists(t, c, priorityClass(name)) {
			t.Errorf("PriorityClass %s kept with nothing using it", name)
		}
	}
}

// kubevirtManagerNamespaceLeft is the namespace of kubevirt-manager as the
// removal finds it: its console, what Kubernetes puts in every namespace,
// and the console's pod that the garbage collector removes after it.
func kubevirtManagerNamespaceLeft(t *testing.T, c client.Client, ns string) {
	t.Helper()
	ctx := context.Background()
	owner := []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "ReplicaSet", Name: "gone", UID: "gone-uid"}}
	objs := []client.Object{
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}},
		&corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "kubevirt-manager"}},
		&corev1.Service{
			ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "kubevirt-manager"},
			Spec:       corev1.ServiceSpec{Ports: []corev1.ServicePort{{Port: 8080}}},
		},
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "kube-root-ca.crt"}},
		&corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "default"}},
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "kubevirt-manager-x", OwnerReferences: owner},
			Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "kubevirtmgr", Image: "x"}}},
		},
	}
	for _, o := range objs {
		if err := c.Create(ctx, o); err != nil {
			t.Fatalf("create %T %s: %v", o, o.GetName(), err)
		}
	}
}

func TestKubevirtManagerRemoval_DeletesTheNamespaceItLeaves(t *testing.T) {
	c, cfg := envtestClient(t)
	ns := "kubevirt-manager-left"
	kubevirtManagerNamespaceLeft(t, c, ns)
	h := &KubevirtManagerHook{client: c, log: logr.Discard(), restConfig: cfg, namespace: ns,
		settle: 2 * time.Second, poll: 200 * time.Millisecond}

	if err := h.Remove(context.Background(), k4allv1alpha1.ComponentSpec{}, k4allv1alpha1.ClusterConfigSpec{}); err != nil {
		t.Fatal(err)
	}
	if !deleting(t, c, ns) {
		t.Errorf("namespace %s kept, holding only what kubevirt-manager left", ns)
	}
}

// kubevirt-manager reads optional ConfigMaps and a Secret the user creates in
// its namespace (auth-config, oauth-config, auth-secret, prometheus-config):
// they and their namespace stay.
func TestKubevirtManagerRemoval_KeepsANamespaceWithTheUsersObjects(t *testing.T) {
	c, cfg := envtestClient(t)
	ns := "kubevirt-manager-configured"
	kubevirtManagerNamespaceLeft(t, c, ns)
	mine := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "auth-config"}}
	if err := c.Create(context.Background(), mine); err != nil {
		t.Fatal(err)
	}
	h := &KubevirtManagerHook{client: c, log: logr.Discard(), restConfig: cfg, namespace: ns,
		settle: time.Second, poll: 200 * time.Millisecond}

	if err := h.Remove(context.Background(), k4allv1alpha1.ComponentSpec{}, k4allv1alpha1.ClusterConfigSpec{}); err != nil {
		t.Fatal(err)
	}
	if deleting(t, c, ns) {
		t.Errorf("namespace %s deleted with ConfigMap auth-config in it", ns)
	}
	if !exists(t, c, mine) {
		t.Error("ConfigMap auth-config deleted")
	}
}
