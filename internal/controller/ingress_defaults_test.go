package controller

import (
	"context"
	"testing"

	k4allv1alpha1 "github.com/gpillon/k4all-operator/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// isDefault left out of a hand-written ClusterConfig means "default
// IngressClass", as update-routes.sh on the host reads it; an explicit false
// written by a Go client stays false.
func TestClusterConfig_IngressIsDefault(t *testing.T) {
	cfg := envtestConfig(t)
	c, err := client.New(cfg, client.Options{Scheme: gateScheme(t)})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	cni := map[string]interface{}{"type": "calico"}

	t.Run("left out, it is the default", func(t *testing.T) {
		u := &unstructured.Unstructured{Object: map[string]interface{}{
			"apiVersion": "k4all.magesgate.com/v1alpha1",
			"kind":       "ClusterConfig",
			"metadata":   map[string]interface{}{"name": "isdefault-left-out"},
			"spec": map[string]interface{}{
				"networking": map[string]interface{}{"cni": cni},
				"cluster":    map[string]interface{}{},
				"features":   map[string]interface{}{},
				"ingress": map[string]interface{}{
					"nginx":   map[string]interface{}{},
					"traefik": map[string]interface{}{},
				},
			},
		}}
		if err := c.Create(ctx, u); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = c.Delete(context.Background(), u) })

		got := &k4allv1alpha1.ClusterConfig{}
		if err := c.Get(ctx, types.NamespacedName{Name: "isdefault-left-out"}, got); err != nil {
			t.Fatal(err)
		}
		if !got.Spec.Ingress.Nginx.IsDefault {
			t.Error("ingress.nginx: {} reads isDefault false, want true")
		}
		if got.Spec.Ingress.Traefik == nil || !got.Spec.Ingress.Traefik.IsDefault {
			t.Errorf("ingress.traefik: {} reads %+v, want isDefault true", got.Spec.Ingress.Traefik)
		}
	})

	t.Run("an explicit false survives a Go client update", func(t *testing.T) {
		cc := &k4allv1alpha1.ClusterConfig{
			ObjectMeta: metav1.ObjectMeta{Name: "isdefault-false"},
			Spec: k4allv1alpha1.ClusterConfigSpec{
				Networking: k4allv1alpha1.NetworkingConfig{CNI: k4allv1alpha1.CNIConfig{Type: "calico"}},
				Ingress: k4allv1alpha1.IngressConfig{
					Nginx:   &k4allv1alpha1.NginxIngressConfig{IsDefault: true},
					Traefik: &k4allv1alpha1.TraefikIngressConfig{IsDefault: true},
				},
			},
		}
		if err := c.Create(ctx, cc); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = c.Delete(context.Background(), cc) })

		cc.Spec.Ingress.Nginx.IsDefault = false
		cc.Spec.Ingress.Traefik.IsDefault = false
		if err := c.Update(ctx, cc); err != nil {
			t.Fatal(err)
		}

		got := &k4allv1alpha1.ClusterConfig{}
		if err := c.Get(ctx, types.NamespacedName{Name: "isdefault-false"}, got); err != nil {
			t.Fatal(err)
		}
		if got.Spec.Ingress.Nginx.IsDefault || got.Spec.Ingress.Traefik.IsDefault {
			t.Errorf("after setting both to false: nginx %t, traefik %t, want false and false",
				got.Spec.Ingress.Nginx.IsDefault, got.Spec.Ingress.Traefik.IsDefault)
		}
	})
}
