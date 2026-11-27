package hooks

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/go-logr/logr"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	k4allv1alpha1 "github.com/gpillon/k4all-operator/api/v1alpha1"
	"github.com/gpillon/k4all-operator/internal/engine"
)

func ciliumConfig(virt, ovsCni bool) k4allv1alpha1.ClusterConfigSpec {
	var c k4allv1alpha1.ClusterConfigSpec
	c.Networking.CNI.Type = "cilium"
	c.Features.Virt.Enabled = virt
	c.Features.OVSCNI.Enabled = ovsCni
	return c
}

func TestBuildCiliumValues_PlainClusterKeepsChartDefaults(t *testing.T) {
	v := BuildCiliumValues(ciliumConfig(false, false))
	for _, k := range []string{"socketLB", "cni"} {
		if _, ok := v[k]; ok {
			t.Errorf("%s set without KubeVirt or Multus: %v", k, v[k])
		}
	}
	if v["kubeProxyReplacement"] != true {
		t.Errorf("kubeProxyReplacement = %v, want true", v["kubeProxyReplacement"])
	}
}

func TestBuildCiliumValues_KubeVirtKeepsSocketLBInTheHost(t *testing.T) {
	v := BuildCiliumValues(ciliumConfig(true, false))
	if want := map[string]interface{}{"hostNamespaceOnly": true}; !reflect.DeepEqual(v["socketLB"], want) {
		t.Errorf("socketLB = %v, want %v", v["socketLB"], want)
	}
	if _, ok := v["cni"]; ok {
		t.Errorf("cni set without Multus: %v", v["cni"])
	}
}

func TestBuildCiliumValues_MultusKeepsItsCNIConfig(t *testing.T) {
	v := BuildCiliumValues(ciliumConfig(false, true))
	if want := map[string]interface{}{"exclusive": false}; !reflect.DeepEqual(v["cni"], want) {
		t.Errorf("cni = %v, want %v", v["cni"], want)
	}
	if _, ok := v["socketLB"]; ok {
		t.Errorf("socketLB set without KubeVirt: %v", v["socketLB"])
	}
}

func TestBuildCiliumValues_KubeVirtAndMultus(t *testing.T) {
	v := BuildCiliumValues(ciliumConfig(true, true))
	if v["socketLB"] == nil || v["cni"] == nil {
		t.Errorf("values = %v, want socketLB and cni", v)
	}
}

// The operator never rolls the agents: on a node with the OVS bridge an
// agent restarted without devices=ovs-bridge crash-loops (Cilium 1.20).
func TestBuildCiliumValues_NeverRollsTheAgents(t *testing.T) {
	for _, c := range []k4allv1alpha1.ClusterConfigSpec{ciliumConfig(true, true), ciliumConfig(false, false)} {
		if _, ok := BuildCiliumValues(c)["rollOutCiliumPods"]; ok {
			t.Errorf("rollOutCiliumPods set for %+v", c.Features)
		}
	}
}

func TestCiliumHookHasAValuesRevision(t *testing.T) {
	var h Hook = NewCiliumHook(nil, logr.Discard())
	rev, ok := h.(ValuesRevisioner)
	if !ok || rev.ValuesRevision() == "" {
		t.Fatalf("the cilium hook must report a values revision, so clusters re-apply Cilium after the operator upgrade")
	}
}

func TestBuildCiliumValues_OVSBridgeIsNamedAndAutoDetectionStays(t *testing.T) {
	c := ciliumConfig(false, false)
	c.Features.OVSBridge.Enabled = true
	v := BuildCiliumValues(c)
	if want := []string{"ovs-bridge"}; !reflect.DeepEqual(v["devices"], want) {
		t.Errorf("devices = %v, want %v", v["devices"], want)
	}
	if v["forceDeviceDetection"] != true {
		t.Errorf("forceDeviceDetection = %v, want true (the NIC before the bridge exists)", v["forceDeviceDetection"])
	}

	c.Networking.CNI.Cilium.AdditionalDevices = []string{"eth1"}
	v = BuildCiliumValues(c)
	if want := []string{"ovs-bridge", "eth1"}; !reflect.DeepEqual(v["devices"], want) {
		t.Errorf("devices = %v, want %v", v["devices"], want)
	}
}

func TestBuildCiliumValues_AdditionalDevicesWithoutTheBridge(t *testing.T) {
	c := ciliumConfig(false, false)
	c.Networking.CNI.Cilium.AdditionalDevices = []string{"eth1", "eth2"}
	v := BuildCiliumValues(c)
	if want := []string{"eth1", "eth2"}; !reflect.DeepEqual(v["devices"], want) {
		t.Errorf("devices = %v, want %v", v["devices"], want)
	}
	if _, ok := v["forceDeviceDetection"]; ok {
		t.Errorf("forceDeviceDetection set without the OVS bridge")
	}
}

func TestBuildCiliumValues_NoDevicesWithoutBridgeOrAdditionalDevices(t *testing.T) {
	v := BuildCiliumValues(ciliumConfig(true, true))
	for _, k := range []string{"devices", "forceDeviceDetection"} {
		if _, ok := v[k]; ok {
			t.Errorf("%s set: %v", k, v[k])
		}
	}
}

// gpillon/k4all#114: on a cluster without kube-proxy the Cilium agents reach
// the API server only through k8sServiceHost/k8sServicePort.

func kubeProxyDaemonSet() *appsv1.DaemonSet {
	return &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Namespace: "kube-system", Name: "kube-proxy"}}
}

// kubeadmConfig is kube-system/kubeadm-config with controlPlaneEndpoint
// endpoint ("" for none, as kubeadm writes it on a single node).
func kubeadmConfig(endpoint string) *corev1.ConfigMap {
	cluster := "apiVersion: kubeadm.k8s.io/v1beta4\nkind: ClusterConfiguration\nkubernetesVersion: v1.35.1\n"
	if endpoint != "" {
		cluster += "controlPlaneEndpoint: \"" + endpoint + "\"\n"
	}
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: "kube-system", Name: "kubeadm-config"},
		Data:       map[string]string{"ClusterConfiguration": cluster},
	}
}

// apiServerPod is the mirror pod of a kube-apiserver static pod kubeadm wrote,
// advertising endpoint.
func apiServerPod(node, endpoint string) *corev1.Pod {
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Namespace: "kube-system", Name: "kube-apiserver-" + node,
		Labels:      map[string]string{"component": "kube-apiserver", "tier": "control-plane"},
		Annotations: map[string]string{"kubeadm.kubernetes.io/kube-apiserver.advertise-address.endpoint": endpoint},
	}}
}

var ciliumComponent = k4allv1alpha1.ComponentSpec{Type: "helm-oci", Version: "1.20.4", Namespace: "kube-system"}

// ciliumPreInstall runs the Cilium hook's PreInstall on a cluster holding objects.
func ciliumPreInstall(t *testing.T, config k4allv1alpha1.ClusterConfigSpec, objects ...client.Object) (map[string]interface{}, error) {
	t.Helper()
	c := fake.NewClientBuilder().WithObjects(objects...).Build()
	return NewCiliumHook(c, logr.Discard()).PreInstall(context.Background(), "cilium", ciliumComponent, config, map[string]interface{}{})
}

// Every cluster k4all 2.x installs runs kube-proxy next to Cilium's
// kubeProxyReplacement: its Cilium values are what they were before the
// fix, so the operator upgrade changes neither cilium-config nor the agents.
func TestCiliumHook_WithKubeProxyTheValuesStayAsTheyWere(t *testing.T) {
	config := ciliumConfig(true, false)
	config.Features.OVSBridge.Enabled = true

	got, err := ciliumPreInstall(t, config, kubeProxyDaemonSet(), kubeadmConfig("192.0.2.10:6443"),
		apiServerPod("node1", "192.0.2.11:6443"))
	if err != nil {
		t.Fatal(err)
	}

	want := BuildCiliumValues(config)
	ipam, err := newClusterPool(config)
	if err != nil {
		t.Fatal(err)
	}
	want["ipam"] = ipam.helmValues()
	if !reflect.DeepEqual(got, want) {
		t.Errorf("values = %v, want exactly %v (no k8sServiceHost/k8sServicePort with kube-proxy)", got, want)
	}
}

// Without kube-proxy the agents get kubeadm's controlPlaneEndpoint: the API
// VIP with kube-vip or keepalived, or a custom endpoint, as a 1.x install
// gave its release.
func TestCiliumHook_WithoutKubeProxyNamesTheControlPlaneEndpoint(t *testing.T) {
	for _, tc := range []struct {
		endpoint, host string
		port           int
	}{
		{"192.0.2.10:6443", "192.0.2.10", 6443},
		{"api.k4all.lab:7443", "api.k4all.lab", 7443},
		// kubeadm takes its default port for an endpoint without one.
		{"192.0.2.10", "192.0.2.10", 6443},
		{"[2001:db8::10]:6443", "2001:db8::10", 6443},
	} {
		t.Run(tc.endpoint, func(t *testing.T) {
			got, err := ciliumPreInstall(t, ciliumConfig(false, false), kubeadmConfig(tc.endpoint),
				apiServerPod("node1", "192.0.2.11:6443"))
			if err != nil {
				t.Fatal(err)
			}
			if got["k8sServiceHost"] != tc.host || got["k8sServicePort"] != tc.port {
				t.Errorf("k8sServiceHost %v, k8sServicePort %v; want %s, %d",
					got["k8sServiceHost"], got["k8sServicePort"], tc.host, tc.port)
			}
		})
	}
}

// A single node without a controlPlaneEndpoint (no HA, no custom endpoint,
// a 1.x node restored as it was): the address its API server advertises,
// the node's IP that 1.x gave the release.
func TestCiliumHook_WithoutKubeProxyOrEndpointTakesTheAdvertisedAddress(t *testing.T) {
	got, err := ciliumPreInstall(t, ciliumConfig(false, false), kubeadmConfig(""), apiServerPod("node1", "192.0.2.53:6443"))
	if err != nil {
		t.Fatal(err)
	}
	if got["k8sServiceHost"] != "192.0.2.53" || got["k8sServicePort"] != 6443 {
		t.Errorf("k8sServiceHost %v, k8sServicePort %v; want 192.0.2.53, 6443", got["k8sServiceHost"], got["k8sServicePort"])
	}
}

// No endpoint to be found: the endpoint is never guessed, and Cilium is not
// applied without it (an agent restarted without one loses the API server).
// The error says how to go on: componentOverrides.cilium.
func TestCiliumHook_WithoutKubeProxyAndNoEndpointIsNotApplied(t *testing.T) {
	for name, objects := range map[string][]client.Object{
		"no kubeadm endpoint, no API server pod": {kubeadmConfig("")},
		"no kubeadm-config":                      nil,
		"several API servers, no endpoint": {kubeadmConfig(""),
			apiServerPod("node1", "192.0.2.11:6443"), apiServerPod("node2", "192.0.2.12:6443")},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := ciliumPreInstall(t, ciliumConfig(false, false), objects...)
			if err == nil {
				t.Fatalf("values = %v, want an error: no endpoint for k8sServiceHost", got)
			}
			for _, want := range []string{"kube-proxy", "k8sServiceHost", "componentOverrides.cilium"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not say %q", err, want)
				}
			}
		})
	}

	t.Run("the override sets it", func(t *testing.T) {
		config := ciliumConfig(false, false)
		config.ComponentOverrides = map[string]k4allv1alpha1.ComponentOverride{
			"cilium": {Values: &apiextensionsv1.JSON{Raw: []byte(`{"k8sServiceHost":"192.0.2.10","k8sServicePort":6443}`)}},
		}
		got, err := ciliumPreInstall(t, config, kubeadmConfig(""))
		if err != nil {
			t.Fatalf("error %v, want none: componentOverrides.cilium sets k8sServiceHost", err)
		}
		if _, ok := got["k8sServiceHost"]; ok {
			t.Errorf("k8sServiceHost = %v, want it left to the override", got["k8sServiceHost"])
		}
	})
}

// A cluster that cannot tell whether kube-proxy runs gets no values: an
// error, not a guess either way.
func TestCiliumHook_AnUnreadableKubeProxyIsAnError(t *testing.T) {
	c := fake.NewClientBuilder().WithInterceptorFuncs(interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if key.Name == "kube-proxy" {
				return errors.New("connection refused")
			}
			return c.Get(ctx, key, obj, opts...)
		},
	}).Build()

	got, err := NewCiliumHook(c, logr.Discard()).PreInstall(context.Background(), "cilium", ciliumComponent,
		ciliumConfig(false, false), map[string]interface{}{})

	if err == nil {
		t.Fatalf("values = %v, want an error", got)
	}
}

// restored1xRelease are the values of the Cilium release of a node restored
// from k4all 1.x, upgraded by hand while Unmanaged (`helm get values`, the
// node's IP replaced): no kube-proxy, KPR, egress gateway.
func restored1xRelease() map[string]interface{} {
	return map[string]interface{}{
		"bpf":                  map[string]interface{}{"masquerade": true},
		"cluster":              map[string]interface{}{"name": "kubernetes"},
		"egressGateway":        map[string]interface{}{"enabled": true},
		"k8sClientRateLimit":   map[string]interface{}{"burst": 20, "qps": 10},
		"k8sServiceHost":       "192.0.2.53",
		"k8sServicePort":       6443,
		"kubeProxyReplacement": true,
		"l2announcements":      map[string]interface{}{"enabled": true},
		"operator":             map[string]interface{}{"replicas": 1},
		"routingMode":          "tunnel",
		"tunnelProtocol":       "vxlan",
	}
}

// restored1xConfig is that node's ClusterConfig, as the 2.x installer wrote
// it: Cilium, virt, the OVS bridge; no l2announcements.
func restored1xConfig() k4allv1alpha1.ClusterConfigSpec {
	config := ciliumConfig(true, false)
	config.Features.OVSBridge.Enabled = true
	return config
}

// restored1xCluster is that node: no kube-proxy, no controlPlaneEndpoint, the
// pool Cilium's chart default gave it.
func restored1xCluster() []client.Object {
	return []client.Object{
		kubeadmConfig(""),
		apiServerPod("node1", "192.0.2.53:6443"),
		&corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Namespace: "kube-system", Name: "cilium-config"},
			Data:       map[string]string{"ipam": "cluster-pool", "cluster-pool-ipv4-cidr": "10.0.0.0/8"},
		},
	}
}

// restored1xValues are the values k4all applies to that node's Cilium
// (PreInstall, then componentOverrides.cilium).
func restored1xValues(t *testing.T, override string) map[string]interface{} {
	t.Helper()
	config := restored1xConfig()
	values, err := ciliumPreInstall(t, config, restored1xCluster()...)
	if err != nil {
		t.Fatal(err)
	}
	if override == "" {
		return values
	}
	values, err = engine.ApplyOverride(values, k4allv1alpha1.ComponentOverride{
		Values: &apiextensionsv1.JSON{Raw: []byte(override)}})
	if err != nil {
		t.Fatal(err)
	}
	return values
}

// The takeover of that release names every value k4all's values drop or
// change. k8sServiceHost/k8sServicePort are not among them (the hook sets
// the node's), nor kubeProxyReplacement (k4all sets it too).
func TestCiliumTakeover_A1xReleaseNamesEveryValueItWouldLose(t *testing.T) {
	got := engine.DroppedValues(restored1xRelease(), restored1xValues(t, ""))

	want := []string{
		"bpf.masquerade", "cluster.name", "egressGateway.enabled",
		"k8sClientRateLimit.burst", "k8sClientRateLimit.qps", "l2announcements.enabled",
		"operator.replicas", "routingMode", "tunnelProtocol",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("dropped = %v, want %v", got, want)
	}
}

// With the override the admin plans (egress gateway, BPF masquerading, one
// Cilium operator, the cluster's name), the values still left out are
// named: the admin sees them before the takeover.
func TestCiliumTakeover_ThePlannedOverrideLeavesTheRestNamed(t *testing.T) {
	override := `{"egressGateway":{"enabled":true},"bpf":{"masquerade":true},` +
		`"operator":{"replicas":1},"cluster":{"name":"kubernetes"}}`

	got := engine.DroppedValues(restored1xRelease(), restored1xValues(t, override))

	want := []string{
		"k8sClientRateLimit.burst", "k8sClientRateLimit.qps", "l2announcements.enabled",
		"routingMode", "tunnelProtocol",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("dropped = %v, want %v", got, want)
	}

	all := override[:len(override)-1] + `,"k8sClientRateLimit":{"burst":20,"qps":10},` +
		`"l2announcements":{"enabled":true},"routingMode":"tunnel","tunnelProtocol":"vxlan"}`
	if got := engine.DroppedValues(restored1xRelease(), restored1xValues(t, all)); len(got) != 0 {
		t.Errorf("dropped = %v with every value carried, want none", got)
	}
}
