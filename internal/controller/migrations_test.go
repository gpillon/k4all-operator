package controller

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/go-logr/logr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/yaml"

	k4allv1alpha1 "github.com/gpillon/k4all-operator/api/v1alpha1"
	"github.com/gpillon/k4all-operator/internal/migrations"
)

// The v2.6.0 ReleaseManifest moves ingress.nginx to ingress.traefik before
// any component is applied, and not while a node is upgrading.
func TestReconcile_MigratesTheClusterConfigForTheRelease(t *testing.T) {
	reconcile := func(t *testing.T, nodes ...client.Object) *k4allv1alpha1.ClusterConfig {
		t.Helper()
		s := gateScheme(t)
		rm := &k4allv1alpha1.ReleaseManifest{
			ObjectMeta: metav1.ObjectMeta{Name: "k4all"},
			Spec: k4allv1alpha1.ReleaseManifestSpec{Components: map[string]k4allv1alpha1.ComponentSpec{
				// Unsupported type: reconciling fails at once, without network.
				"traefik": {Type: "unsupported", Version: "1"},
				"ingress-nginx": {Type: "unsupported", Version: "1",
					Management: k4allv1alpha1.ComponentManagementRemoved},
			}},
		}
		cc := &k4allv1alpha1.ClusterConfig{
			ObjectMeta: metav1.ObjectMeta{Name: "k4all-cluster-config"},
			Spec: k4allv1alpha1.ClusterConfigSpec{
				Networking: k4allv1alpha1.NetworkingConfig{CNI: k4allv1alpha1.CNIConfig{Type: "calico"}},
				Ingress: k4allv1alpha1.IngressConfig{
					Nginx: &k4allv1alpha1.NginxIngressConfig{DedicatedIP: "192.168.6.193", IsDefault: false},
				},
			},
		}
		c := fake.NewClientBuilder().WithScheme(s).
			WithObjects(append([]client.Object{rm, cc}, nodes...)...).
			WithStatusSubresource(&k4allv1alpha1.ReleaseManifest{}, &k4allv1alpha1.ClusterConfig{}).
			Build()
		r := &ReleaseReconciler{Client: c, Scheme: s, Log: logr.Discard()}
		if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "k4all"}}); err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
		got := &k4allv1alpha1.ClusterConfig{}
		if err := c.Get(context.Background(), types.NamespacedName{Name: "k4all-cluster-config"}, got); err != nil {
			t.Fatal(err)
		}
		return got
	}

	t.Run("migrated", func(t *testing.T) {
		got := reconcile(t, node("cp1", nil))
		want := k4allv1alpha1.TraefikIngressConfig{DedicatedIP: "192.168.6.193", IsDefault: false}
		if got.Spec.Ingress.Traefik == nil || *got.Spec.Ingress.Traefik != want || got.Spec.Ingress.Nginx != nil {
			t.Errorf("ingress = nginx %+v, traefik %+v; want only traefik %+v",
				got.Spec.Ingress.Nginx, got.Spec.Ingress.Traefik, want)
		}
	})

	t.Run("not during a node upgrade", func(t *testing.T) {
		got := reconcile(t, node("cp1", map[string]string{upgradeInProgress: "v2.5.0->v2.6.0"}))
		if got.Spec.Ingress.Nginx == nil || got.Spec.Ingress.Traefik != nil {
			t.Errorf("ingress = nginx %+v, traefik %+v; want it untouched",
				got.Spec.Ingress.Nginx, got.Spec.Ingress.Traefik)
		}
	})
}

// On a real API server the migrated ClusterConfig has no ingress.nginx key
// left, not even an empty one that would read as isDefault true.
func TestMigrations_RemoveTheNginxKey(t *testing.T) {
	c := envtestClient(t)
	ctx := context.Background()
	cc := &k4allv1alpha1.ClusterConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "migrate-nginx"},
		Spec: k4allv1alpha1.ClusterConfigSpec{
			Networking: k4allv1alpha1.NetworkingConfig{CNI: k4allv1alpha1.CNIConfig{Type: "calico"}},
			Ingress: k4allv1alpha1.IngressConfig{
				Nginx: &k4allv1alpha1.NginxIngressConfig{DedicatedIP: "192.168.6.193", IsDefault: false},
			},
		},
	}
	if err := c.Create(ctx, cc); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Delete(context.Background(), cc) })
	rm := &k4allv1alpha1.ReleaseManifest{Spec: k4allv1alpha1.ReleaseManifestSpec{
		Components: map[string]k4allv1alpha1.ComponentSpec{"traefik": {Type: "helm", Version: "1"}},
	}}

	if _, err := migrations.Run(ctx, c, rm, cc); err != nil {
		t.Fatal(err)
	}

	got := &unstructured.Unstructured{}
	got.SetGroupVersionKind(k4allv1alpha1.GroupVersion.WithKind("ClusterConfig"))
	if err := c.Get(ctx, types.NamespacedName{Name: "migrate-nginx"}, got); err != nil {
		t.Fatal(err)
	}
	ingress, _, _ := unstructured.NestedMap(got.Object, "spec", "ingress")
	if _, ok := ingress["nginx"]; ok {
		t.Errorf("spec.ingress = %v, want no nginx key", ingress)
	}
	if traefik, _ := ingress["traefik"].(map[string]interface{}); traefik["dedicatedIP"] != "192.168.6.193" || traefik["isDefault"] != false {
		t.Errorf("spec.ingress.traefik = %v, want dedicatedIP 192.168.6.193 and isDefault false", ingress["traefik"])
	}
}

// Operator v0.5.0 runs from the k4all v2.4.0 sync: on the v2.4.0 and v2.5.0
// ReleaseManifests (ingress-nginx, no traefik) the nginx-to-traefik
// migration must neither convert nor count itself; on v2.6.0 it does both.
func TestMigrations_WaitForTheV26ReleaseManifest(t *testing.T) {
	c := envtestClient(t)
	ctx := context.Background()
	cc := &k4allv1alpha1.ClusterConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "migrate-from-v24"},
		Spec: k4allv1alpha1.ClusterConfigSpec{
			Networking: k4allv1alpha1.NetworkingConfig{CNI: k4allv1alpha1.CNIConfig{Type: "calico"}},
			Ingress: k4allv1alpha1.IngressConfig{
				Nginx: &k4allv1alpha1.NginxIngressConfig{DedicatedIP: "192.168.6.193", IsDefault: false},
			},
		},
	}
	if err := c.Create(ctx, cc); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Delete(context.Background(), cc) })

	stored := func() *k4allv1alpha1.ClusterConfig {
		got := &k4allv1alpha1.ClusterConfig{}
		if err := c.Get(ctx, types.NamespacedName{Name: cc.Name}, got); err != nil {
			t.Fatal(err)
		}
		return got
	}

	v24 := releaseManifestFile(t, "releasemanifest-v2.4.yaml")
	if changed, err := migrations.Run(ctx, c, v24, stored()); err != nil || changed {
		t.Fatalf("v2.4.0: changed %t, err %v; want no change", changed, err)
	}
	got := stored()
	if got.Spec.Ingress.Nginx == nil || got.Spec.Ingress.Traefik != nil {
		t.Errorf("v2.4.0: ingress = nginx %+v, traefik %+v; want it untouched", got.Spec.Ingress.Nginx, got.Spec.Ingress.Traefik)
	}
	if v, ok := got.Annotations[migrations.AppliedAnnotation]; ok {
		t.Errorf("v2.4.0: %s = %q, want none", migrations.AppliedAnnotation, v)
	}

	v26 := releaseManifestFile(t, "releasemanifest-v2.6.yaml")
	if changed, err := migrations.Run(ctx, c, v26, stored()); err != nil || !changed {
		t.Fatalf("v2.6.0: changed %t, err %v; want a change", changed, err)
	}
	got = stored()
	want := k4allv1alpha1.TraefikIngressConfig{DedicatedIP: "192.168.6.193", IsDefault: false}
	if got.Spec.Ingress.Nginx != nil || got.Spec.Ingress.Traefik == nil || *got.Spec.Ingress.Traefik != want {
		t.Errorf("v2.6.0: ingress = nginx %+v, traefik %+v; want only traefik %+v",
			got.Spec.Ingress.Nginx, got.Spec.Ingress.Traefik, want)
	}
	if v := got.Annotations[migrations.AppliedAnnotation]; v != "1" {
		t.Errorf("v2.6.0: %s = %q, want \"1\"", migrations.AppliedAnnotation, v)
	}
}

// releaseManifestFile reads a ReleaseManifest of the e2e suite, the shapes of
// the real k4all releases.
func releaseManifestFile(t *testing.T, name string) *k4allv1alpha1.ReleaseManifest {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "test", "e2e", "testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	rm := &k4allv1alpha1.ReleaseManifest{}
	if err := yaml.Unmarshal(raw, rm); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return rm
}
