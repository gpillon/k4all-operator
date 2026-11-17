package controller

import (
	"context"
	"testing"

	"github.com/go-logr/logr"
	k4allv1alpha1 "github.com/gpillon/k4all-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const handedOverConfigMap = `apiVersion: v1
kind: ConfigMap
metadata:
  name: handed-over
  namespace: default
`

// A component that takes over an object from a Removed one must still have it
// after the reconcile: traefik recreates the "nginx" IngressClass that
// uninstalling ingress-nginx deletes. Removals therefore run first.
func TestReconcile_RemovalRunsBeforeTheReplacementInstalls(t *testing.T) {
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
	manifest := func(management k4allv1alpha1.ComponentManagement) k4allv1alpha1.ComponentSpec {
		return k4allv1alpha1.ComponentSpec{
			Type:            k4allv1alpha1.ComponentTypeManifest,
			Version:         "v1.0.0",
			Management:      management,
			InlineManifests: []string{handedOverConfigMap},
		}
	}
	rm := &k4allv1alpha1.ReleaseManifest{
		ObjectMeta: metav1.ObjectMeta{Name: "k4all"},
		Spec: k4allv1alpha1.ReleaseManifestSpec{Components: map[string]k4allv1alpha1.ComponentSpec{
			"old": manifest(k4allv1alpha1.ComponentManagementRemoved),
			"new": manifest(k4allv1alpha1.ComponentManagementManaged),
		}},
	}
	for _, obj := range []client.Object{cc, rm} {
		if err := c.Create(ctx, obj); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = c.Delete(context.Background(), obj) })
	}
	// "old" was installed by an earlier release and still owns the object.
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "handed-over", Namespace: "default"}}
	if err := c.Create(ctx, cm); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Delete(context.Background(), cm) })
	rm.Status.Components = map[string]k4allv1alpha1.ComponentStatus{
		"old": {State: k4allv1alpha1.ComponentStateInstalled, InstalledVersion: "v1.0.0", LastTransitionTime: metav1.Now()},
	}
	if err := c.Status().Update(ctx, rm); err != nil {
		t.Fatal(err)
	}

	r := &ReleaseReconciler{Client: c, Scheme: s, RestConfig: cfg, Log: logr.Discard()}
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: "k4all"}}); err != nil {
		t.Fatal(err)
	}

	err = c.Get(ctx, types.NamespacedName{Name: "handed-over", Namespace: "default"}, &corev1.ConfigMap{})
	if errors.IsNotFound(err) {
		t.Fatal("the object handed over to \"new\" was deleted by the removal of \"old\" after \"new\" applied it")
	}
	if err != nil {
		t.Fatal(err)
	}
}
