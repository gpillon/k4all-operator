package controller

import (
	"context"
	"errors"
	"testing"

	"github.com/go-logr/logr"
	k4allv1alpha1 "github.com/gpillon/k4all-operator/api/v1alpha1"
	"github.com/gpillon/k4all-operator/internal/controller/hooks"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

// defaultPodNetwork is spec.cluster.podNetwork in k4all-config.yaml.default:
// a host address, not a network.
const defaultPodNetwork = "10.100.0.1/18"

// ciliumSpec is the cilium component as k4all-release.yaml pins it.
var ciliumSpec = k4allv1alpha1.ComponentSpec{Type: "helm-oci", Version: "1.20.4", Namespace: "kube-system"}

// ciliumValues returns the Helm values the operator computes for Cilium on a
// cluster holding objects.
func ciliumValues(t *testing.T, config k4allv1alpha1.ClusterConfigSpec, objects ...client.Object) map[string]interface{} {
	t.Helper()
	r := &ReleaseReconciler{
		hookReg: hooks.NewRegistry(fake.NewClientBuilder().WithObjects(objects...).Build(), logr.Discard()),
	}
	got, err := r.componentValues(context.Background(), "cilium", ciliumSpec, config, nil)
	if err != nil {
		t.Fatal(err)
	}
	return normalize(t, got)
}

// A new cluster: Cilium hands out the pod network of the ClusterConfig, a
// /24 per node, not its chart default 10.0.0.0/8 (k4all#72). The default
// podNetwork is a host address (10.100.0.1/18): the pool is its network.
func TestComponentValues_CiliumNewClusterTakesThePodNetwork(t *testing.T) {
	config := ciliumConfig()
	config.Cluster.PodNetwork = defaultPodNetwork

	got := ciliumValues(t, config)

	expectValue(t, got, "cluster-pool", "ipam", "mode")
	expectValue(t, got, []interface{}{"10.100.0.0/18"}, "ipam", "operator", "clusterPoolIPv4PodCIDRList")
	expectValue(t, got, float64(24), "ipam", "operator", "clusterPoolIPv4MaskSize")
}

// A ClusterConfig without podNetwork has k4all's default, as with Calico.
func TestComponentValues_CiliumNewClusterWithoutPodNetworkTakesTheDefault(t *testing.T) {
	got := ciliumValues(t, ciliumConfig())

	expectValue(t, got, []interface{}{"10.100.0.0/18"}, "ipam", "operator", "clusterPoolIPv4PodCIDRList")
}

// ciliumConfigMap is kube-system/cilium-config as the Cilium chart (1.19,
// 1.20) renders it for a cluster-pool: the CIDRs joined by spaces, the mask
// quoted.
func ciliumConfigMap(cidrs, maskSize string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: "kube-system", Name: "cilium-config"},
		Data: map[string]string{
			"ipam":                        "cluster-pool",
			"cluster-pool-ipv4-cidr":      cidrs,
			"cluster-pool-ipv4-mask-size": maskSize,
		},
	}
}

// A cluster that runs Cilium keeps the pool it runs: the chart default an
// older operator left (10.0.0.0/8), not the pod network of the ClusterConfig.
// Moving it would re-address the pods.
func TestComponentValues_CiliumKeepsThePoolTheClusterRuns(t *testing.T) {
	config := ciliumConfig()
	config.Cluster.PodNetwork = defaultPodNetwork

	got := ciliumValues(t, config, ciliumConfigMap("10.0.0.0/8", "24"))

	expectValue(t, got, "cluster-pool", "ipam", "mode")
	expectValue(t, got, []interface{}{"10.0.0.0/8"}, "ipam", "operator", "clusterPoolIPv4PodCIDRList")
	expectValue(t, got, float64(24), "ipam", "operator", "clusterPoolIPv4MaskSize")
}

// Every CIDR of the pool is kept, with the mask the cluster runs.
func TestComponentValues_CiliumKeepsEveryCIDROfThePool(t *testing.T) {
	got := ciliumValues(t, ciliumConfig(), ciliumConfigMap("10.0.0.0/16 172.20.0.0/16", "26"))

	expectValue(t, got, []interface{}{"10.0.0.0/16", "172.20.0.0/16"}, "ipam", "operator", "clusterPoolIPv4PodCIDRList")
	expectValue(t, got, float64(26), "ipam", "operator", "clusterPoolIPv4MaskSize")
}

// A cluster whose Cilium runs another IPAM mode (k4all 1.x installed it with
// cilium-cli) keeps it: the operator does not switch it to a cluster-pool.
func TestComponentValues_CiliumKeepsTheIPAMModeTheClusterRuns(t *testing.T) {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: "kube-system", Name: "cilium-config"},
		Data:       map[string]string{"ipam": "kubernetes"},
	}

	got := ciliumValues(t, ciliumConfig(), cm)

	expectValue(t, got, "kubernetes", "ipam", "mode")
}

// When cilium-config cannot be read, the operator does not know whether
// Cilium runs a pool: it renders nothing rather than the pool of a new
// cluster, which would re-address the pods of a running one.
func TestComponentValues_CiliumUnreadableConfigIsAnError(t *testing.T) {
	c := fake.NewClientBuilder().WithInterceptorFuncs(interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if key.Name == "cilium-config" {
				return errors.New("connection refused")
			}
			return c.Get(ctx, key, obj, opts...)
		},
	}).Build()
	r := &ReleaseReconciler{hookReg: hooks.NewRegistry(c, logr.Discard())}

	got, err := r.componentValues(context.Background(), "cilium", ciliumSpec, ciliumConfig(), nil)

	if err == nil {
		t.Fatalf("componentValues = %v, want an error", got)
	}
}

// A pod network Cilium cannot hand /24s from stops the install with its
// reason, instead of a Cilium operator that allocates nothing.
func TestComponentValues_CiliumRefusesAPodNetworkItCannotUse(t *testing.T) {
	for _, podNetwork := range []string{"10.100.0.0/25", "fd00:10::/48", "10.100.0.0"} {
		t.Run(podNetwork, func(t *testing.T) {
			r := &ReleaseReconciler{hookReg: hooks.NewRegistry(fake.NewClientBuilder().Build(), logr.Discard())}
			config := ciliumConfig()
			config.Cluster.PodNetwork = podNetwork

			got, err := r.componentValues(context.Background(), "cilium", ciliumSpec, config, nil)

			if err == nil {
				t.Fatalf("componentValues = %v, want an error", got)
			}
		})
	}
}

// componentOverrides.cilium still sets the pool: the way to move it, on
// purpose, before nodes take CIDRs from it.
func TestComponentValues_CiliumPoolOverrideWins(t *testing.T) {
	config := ciliumConfig()
	config.ComponentOverrides = map[string]k4allv1alpha1.ComponentOverride{
		"cilium": {Values: overrideValues(t, `{"ipam":{"operator":{"clusterPoolIPv4PodCIDRList":["10.42.0.0/16"]}}}`)},
	}

	got := ciliumValues(t, config, ciliumConfigMap("10.0.0.0/8", "24"))

	expectValue(t, got, []interface{}{"10.42.0.0/16"}, "ipam", "operator", "clusterPoolIPv4PodCIDRList")
	expectValue(t, got, float64(24), "ipam", "operator", "clusterPoolIPv4MaskSize")
}
