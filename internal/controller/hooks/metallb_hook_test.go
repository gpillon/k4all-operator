package hooks_test

import (
	"context"
	"reflect"
	"testing"

	"github.com/go-logr/logr"
	k4allv1alpha1 "github.com/gpillon/k4all-operator/api/v1alpha1"
	"github.com/gpillon/k4all-operator/internal/controller/hooks"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
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
	addresses, _ := poolAfterPostInstall(t, k4allv1alpha1.ClusterConfigSpec{Ingress: ingress})
	return addresses
}

// poolAfterPostInstall runs the metallb post-install hook on config, with
// existing objects, and returns the addresses of k4all-ingress-pool and
// whether it exists.
func poolAfterPostInstall(t *testing.T, config k4allv1alpha1.ClusterConfigSpec, existing ...client.Object) ([]interface{}, bool) {
	t.Helper()
	mapper := meta.NewDefaultRESTMapper(nil)
	for _, kind := range []string{"IPAddressPool", "L2Advertisement"} {
		mapper.Add(schema.GroupVersionKind{Group: "metallb.io", Version: "v1beta1", Kind: kind}, meta.RESTScopeNamespace)
	}
	mapper.Add(schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"}, meta.RESTScopeNamespace)
	c := fake.NewClientBuilder().WithRESTMapper(mapper).WithObjects(existing...).Build()

	hook := hooks.NewRegistry(c, logr.Discard()).Get("metallb")
	if err := hook.PostInstall(context.Background(), k4allv1alpha1.ComponentSpec{}, config); err != nil {
		t.Fatal(err)
	}

	pool := ingressPool(nil)
	key := types.NamespacedName{Namespace: "metallb-system", Name: "k4all-ingress-pool"}
	err := c.Get(context.Background(), key, pool)
	if apierrors.IsNotFound(err) {
		return nil, false
	}
	if err != nil {
		t.Fatal(err)
	}
	addresses, _, _ := unstructured.NestedSlice(pool.Object, "spec", "addresses")
	return addresses, true
}

// ingressPool is k4all-ingress-pool with addresses.
func ingressPool(addresses []interface{}) *unstructured.Unstructured {
	pool := &unstructured.Unstructured{}
	pool.SetAPIVersion("metallb.io/v1beta1")
	pool.SetKind("IPAddressPool")
	pool.SetNamespace("metallb-system")
	pool.SetName("k4all-ingress-pool")
	if addresses != nil {
		_ = unstructured.SetNestedSlice(pool.Object, addresses, "spec", "addresses")
	}
	return pool
}

func kubeVIPConfig(ingress k4allv1alpha1.IngressConfig) k4allv1alpha1.ClusterConfigSpec {
	return k4allv1alpha1.ClusterConfigSpec{
		Cluster: k4allv1alpha1.ControlPlaneConfig{HA: k4allv1alpha1.HAConfig{Type: "kubevip"}},
		Ingress: ingress,
	}
}

// With HA kube-vip the host's kube-vip announces the Traefik IP: MetalLB
// must not have it, or it would hand it to another Service.
func TestMetalLBHook_LeavesTheKubeVIPAddressOut(t *testing.T) {
	got, _ := poolAfterPostInstall(t, kubeVIPConfig(k4allv1alpha1.IngressConfig{
		Traefik: &k4allv1alpha1.TraefikIngressConfig{DedicatedIP: "192.168.6.201", IsDefault: true},
		Cilium:  k4allv1alpha1.CiliumIngressConfig{DedicatedIP: "192.168.6.202"},
	}))

	if want := []interface{}{"192.168.6.202/32"}; !reflect.DeepEqual(got, want) {
		t.Errorf("k4all-ingress-pool addresses = %v, want %v", got, want)
	}
}

// Nothing left for MetalLB: no pool, and the one written before goes.
func TestMetalLBHook_NoPoolWhenKubeVIPAnnouncesTheOnlyAddress(t *testing.T) {
	config := kubeVIPConfig(k4allv1alpha1.IngressConfig{
		Traefik: &k4allv1alpha1.TraefikIngressConfig{DedicatedIP: "192.168.6.201", IsDefault: true},
	})
	for name, existing := range map[string][]client.Object{
		"new cluster":      nil,
		"pool from before": {ingressPool([]interface{}{"192.168.6.201/32"})},
	} {
		t.Run(name, func(t *testing.T) {
			if got, ok := poolAfterPostInstall(t, config, existing...); ok {
				t.Errorf("k4all-ingress-pool exists with %v, want none", got)
			}
		})
	}
}

// Before the migration to ingress.traefik the nginx IP stays MetalLB's:
// ingress-nginx's Service has no class.
func TestMetalLBHook_KeepsTheNginxAddressOnKubeVIP(t *testing.T) {
	got, _ := poolAfterPostInstall(t, kubeVIPConfig(k4allv1alpha1.IngressConfig{
		Nginx: &k4allv1alpha1.NginxIngressConfig{DedicatedIP: "192.168.6.201", IsDefault: true},
	}))

	if want := []interface{}{"192.168.6.201/32"}; !reflect.DeepEqual(got, want) {
		t.Errorf("k4all-ingress-pool addresses = %v, want %v", got, want)
	}
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
