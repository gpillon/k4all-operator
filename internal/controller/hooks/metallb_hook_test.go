package hooks_test

import (
	"context"
	"reflect"
	"testing"

	"github.com/go-logr/logr"
	k4allv1alpha1 "github.com/gpillon/k4all-operator/api/v1alpha1"
	"github.com/gpillon/k4all-operator/internal/controller/hooks"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// ingressPoolAddresses runs the metallb post-install hook on a cluster where
// MetalLB is installed and returns the addresses of the pool it announces.
func ingressPoolAddresses(t *testing.T, ingress k4allv1alpha1.IngressConfig) []interface{} {
	t.Helper()
	mapper := meta.NewDefaultRESTMapper(nil)
	for _, kind := range []string{"IPAddressPool", "L2Advertisement"} {
		mapper.Add(schema.GroupVersionKind{Group: "metallb.io", Version: "v1beta1", Kind: kind}, meta.RESTScopeNamespace)
	}
	mapper.Add(schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"}, meta.RESTScopeNamespace)
	c := fake.NewClientBuilder().WithRESTMapper(mapper).Build()

	hook := hooks.NewRegistry(c, logr.Discard()).Get("metallb")
	config := k4allv1alpha1.ClusterConfigSpec{Ingress: ingress}
	if err := hook.PostInstall(context.Background(), k4allv1alpha1.ComponentSpec{}, config); err != nil {
		t.Fatal(err)
	}

	pool := &unstructured.Unstructured{}
	pool.SetAPIVersion("metallb.io/v1beta1")
	pool.SetKind("IPAddressPool")
	key := types.NamespacedName{Namespace: "metallb-system", Name: "k4all-ingress-pool"}
	if err := c.Get(context.Background(), key, pool); client.IgnoreNotFound(err) != nil {
		t.Fatal(err)
	}
	addresses, _, _ := unstructured.NestedSlice(pool.Object, "spec", "addresses")
	return addresses
}

// The installer writes ingress.traefik from k4all v2.6.0 on, with no nginx key.
func TestMetalLBHook_AnnouncesTraefikDedicatedIP(t *testing.T) {
	got := ingressPoolAddresses(t, k4allv1alpha1.IngressConfig{
		Traefik: &k4allv1alpha1.TraefikIngressConfig{DedicatedIP: "192.168.6.193", IsDefault: true},
	})

	if want := []interface{}{"192.168.6.193/32"}; !reflect.DeepEqual(got, want) {
		t.Errorf("k4all-ingress-pool addresses = %v, want %v", got, want)
	}
}

// A ClusterConfig can carry the same IP under nginx and traefik, as when the
// nginx settings are copied over. MetalLB refuses a pool whose CIDRs overlap.
func TestMetalLBHook_AnnouncesASharedIPOnce(t *testing.T) {
	got := ingressPoolAddresses(t, k4allv1alpha1.IngressConfig{
		Nginx:   &k4allv1alpha1.NginxIngressConfig{DedicatedIP: "192.168.6.193", IsDefault: true},
		Traefik: &k4allv1alpha1.TraefikIngressConfig{DedicatedIP: "192.168.6.193", IsDefault: true},
		Cilium:  k4allv1alpha1.CiliumIngressConfig{DedicatedIP: "192.168.6.194"},
	})

	if want := []interface{}{"192.168.6.193/32", "192.168.6.194/32"}; !reflect.DeepEqual(got, want) {
		t.Errorf("k4all-ingress-pool addresses = %v, want %v", got, want)
	}
}
