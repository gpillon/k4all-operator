package controller

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	k4allv1alpha1 "github.com/gpillon/k4all-operator/api/v1alpha1"
	"github.com/gpillon/k4all-operator/internal/controller/hooks"
)

// Every write of the operator is k4all-operator's in managedFields, not only
// the manifest applies (spec R3, §16.2): consoles tell k4all's fields by that
// manager, and before this the hooks wrote as controller-runtime's default,
// "manager".

// operatorClient is a client of envtest as the operator's manager makes it.
func operatorClient(t *testing.T) client.Client {
	t.Helper()
	c, err := NewClient(envtestConfig(t), client.Options{Scheme: gateScheme(t)})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// managedFieldsOf is the fields set of k4all-operator's entry for the
// operation op on obj, as FieldsV1 JSON decoded; nil when it has none.
func managedFieldsOf(t *testing.T, obj *unstructured.Unstructured, op metav1.ManagedFieldsOperationType) map[string]interface{} {
	t.Helper()
	for _, e := range obj.GetManagedFields() {
		if e.Manager != "k4all-operator" || e.Operation != op || e.FieldsV1 == nil {
			continue
		}
		fields := map[string]interface{}{}
		if err := json.Unmarshal(e.FieldsV1.Raw, &fields); err != nil {
			t.Fatal(err)
		}
		return fields
	}
	return nil
}

// owns tells whether fields (managedFieldsOf) name the field at path.
func owns(fields map[string]interface{}, path ...string) bool {
	for _, p := range path {
		next, ok := fields["f:"+p].(map[string]interface{})
		if !ok {
			return false
		}
		fields = next
	}
	return true
}

// managers lists the field managers of obj, with their operations.
func managers(obj client.Object) []string {
	m := make([]string, 0, len(obj.GetManagedFields()))
	for _, e := range obj.GetManagedFields() {
		m = append(m, e.Manager+"/"+string(e.Operation))
	}
	return m
}

// The useEmulation the VirtHook's Sync writes.
func TestFieldOwner_VirtHookSync(t *testing.T) {
	c := envtestClient(t)
	ensureNamespace(t, c, "kubevirt")
	kubeVirtCR.deleteOnCleanup(t, c)
	cr := &unstructured.Unstructured{}
	cr.SetGroupVersionKind(kubeVirtCR.gvk)
	cr.SetNamespace(kubeVirtCR.key.Namespace)
	cr.SetName(kubeVirtCR.key.Name)
	cr.Object["spec"] = map[string]interface{}{}
	if err := c.Create(context.Background(), cr); err != nil {
		t.Fatal(err)
	}

	h := hooks.NewRegistry(operatorClient(t), logr.Discard()).Get("kubevirt").(hooks.Syncer)
	config := k4allv1alpha1.ClusterConfigSpec{Features: k4allv1alpha1.FeaturesConfig{
		Virt: k4allv1alpha1.VirtConfig{Enabled: true, Emulation: "true"}}}
	if _, err := h.Sync(context.Background(), k4allv1alpha1.ComponentSpec{}, config); err != nil {
		t.Fatal(err)
	}

	got := kubeVirtCR.get(t, c)
	if v, _, _ := unstructured.NestedBool(got.Object, useEmulation...); !v {
		t.Fatal("useEmulation not set: the test checks nothing")
	}
	if !owns(managedFieldsOf(t, got, metav1.ManagedFieldsOperationUpdate), useEmulation...) {
		t.Errorf("useEmulation is not k4all-operator's; managers: %v", managers(got))
	}
}

var useEmulation = []string{"spec", "configuration", "developerConfiguration", "useEmulation"}

// The namespace labels the TopolvmHook sets before TopoLVM's install.
func TestFieldOwner_TopolvmHook(t *testing.T) {
	c := envtestClient(t)
	// The hook creates topolvm-system, and labels kube-system, which has no
	// such label yet. (envtest never removes a namespace: no test removes it.)
	kubeSystem := &corev1.Namespace{}
	if err := c.Get(context.Background(), client.ObjectKey{Name: "kube-system"}, kubeSystem); err != nil {
		t.Fatal(err)
	}
	delete(kubeSystem.Labels, "topolvm.io/webhook")
	if err := c.Update(context.Background(), kubeSystem); err != nil {
		t.Fatal(err)
	}

	h := hooks.NewRegistry(operatorClient(t), logr.Discard()).Get("topolvm")
	spec := k4allv1alpha1.ComponentSpec{Type: k4allv1alpha1.ComponentTypeHelm, Namespace: "topolvm-system"}
	if _, err := h.PreInstall(context.Background(), "topolvm", spec, k4allv1alpha1.ClusterConfigSpec{}, nil); err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{"topolvm-system", "kube-system"} {
		got := &corev1.Namespace{}
		if err := c.Get(context.Background(), client.ObjectKey{Name: name}, got); err != nil {
			t.Fatal(err)
		}
		if got.Labels["topolvm.io/webhook"] != "ignore" {
			t.Fatalf("namespace %s: label not set, the test checks nothing", name)
		}
		u := &unstructured.Unstructured{}
		u.SetManagedFields(got.ManagedFields)
		owned := false
		for _, op := range []metav1.ManagedFieldsOperationType{metav1.ManagedFieldsOperationUpdate, metav1.ManagedFieldsOperationApply} {
			owned = owned || owns(managedFieldsOf(t, u, op), "metadata", "labels", "topolvm.io/webhook")
		}
		if !owned {
			t.Errorf("namespace %s: the label is not k4all-operator's; managers: %v", name, managers(got))
		}
	}
}
