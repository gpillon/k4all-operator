package controller

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/go-logr/logr"
	k4allv1alpha1 "github.com/gpillon/k4all-operator/api/v1alpha1"
	"github.com/gpillon/k4all-operator/internal/controller/hooks"
	appsv1 "k8s.io/api/apps/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// newValuesReconciler computes values on a cluster k4all 2.x installed:
// kube-proxy runs.
func newValuesReconciler() *ReleaseReconciler {
	return &ReleaseReconciler{
		hookReg: hooks.NewRegistry(fake.NewClientBuilder().WithObjects(kubeProxy()).Build(), logr.Discard()),
	}
}

// kubeProxy is kube-proxy's DaemonSet, as kubeadm installs it on every
// cluster k4all 2.x installs.
func kubeProxy() *appsv1.DaemonSet {
	return &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Namespace: "kube-system", Name: "kube-proxy"}}
}

func overrideValues(t *testing.T, raw string) *apiextensionsv1.JSON {
	t.Helper()
	if !json.Valid([]byte(raw)) {
		t.Fatalf("invalid JSON in test: %s", raw)
	}
	return &apiextensionsv1.JSON{Raw: []byte(raw)}
}

// decode turns a JSON literal into the map shape Helm values have, so expected
// values can be written the way a user writes them in a ClusterConfig.
func decode(t *testing.T, raw string) map[string]interface{} {
	t.Helper()
	out := map[string]interface{}{}
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatalf("invalid JSON in test: %v", err)
	}
	return out
}

// normalize round-trips through JSON so typed slices/maps compare equal to
// their decoded form.
func normalize(t *testing.T, v map[string]interface{}) map[string]interface{} {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal values: %v", err)
	}
	return decode(t, string(b))
}

func TestComponentValues_OverrideValuesReachComponentWithoutHook(t *testing.T) {
	r := newValuesReconciler()
	config := k4allv1alpha1.ClusterConfigSpec{
		ComponentOverrides: map[string]k4allv1alpha1.ComponentOverride{
			"cert-manager": {Values: overrideValues(t, `{"podLabels":{"team":"infra"}}`)},
		},
	}

	got, err := r.componentValues(context.Background(), "cert-manager", k4allv1alpha1.ComponentSpec{}, config, nil)
	if err != nil {
		t.Fatal(err)
	}

	want := decode(t, `{"podLabels":{"team":"infra"}}`)
	if !reflect.DeepEqual(normalize(t, got), want) {
		t.Errorf("componentValues = %v, want %v", got, want)
	}
}

// valueAt reads a dotted path ("hubble.relay.enabled") from Helm values.
func valueAt(values map[string]interface{}, path ...string) (interface{}, bool) {
	var cur interface{} = values
	for _, key := range path {
		m, ok := cur.(map[string]interface{})
		if !ok {
			return nil, false
		}
		if cur, ok = m[key]; !ok {
			return nil, false
		}
	}
	return cur, true
}

func expectValue(t *testing.T, values map[string]interface{}, want interface{}, path ...string) {
	t.Helper()
	got, ok := valueAt(normalize(t, values), path...)
	if !ok {
		t.Errorf("%v: missing, want %v (values: %v)", path, want, values)
		return
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("%v = %v, want %v", path, got, want)
	}
}

func ciliumConfig() k4allv1alpha1.ClusterConfigSpec {
	return k4allv1alpha1.ClusterConfigSpec{
		Networking: k4allv1alpha1.NetworkingConfig{CNI: k4allv1alpha1.CNIConfig{Type: "cilium"}},
	}
}

func TestComponentValues_OverrideWinsOverHookDefaults(t *testing.T) {
	r := newValuesReconciler()
	config := ciliumConfig()
	config.ComponentOverrides = map[string]k4allv1alpha1.ComponentOverride{
		"cilium": {Values: overrideValues(t, `{"externalIPs":{"enabled":false}}`)},
	}

	got, err := r.componentValues(context.Background(), "cilium", k4allv1alpha1.ComponentSpec{}, config, nil)
	if err != nil {
		t.Fatal(err)
	}

	expectValue(t, got, false, "externalIPs", "enabled")
	expectValue(t, got, true, "kubeProxyReplacement")
}

func TestComponentValues_OverrideHelmArgsAreAppliedWithHelmTypes(t *testing.T) {
	r := newValuesReconciler()
	config := ciliumConfig()
	config.ComponentOverrides = map[string]k4allv1alpha1.ComponentOverride{
		"cilium": {HelmArgs: []string{"--set hubble.enabled=false", "--set operator.replicas=2"}},
	}

	got, err := r.componentValues(context.Background(), "cilium", k4allv1alpha1.ComponentSpec{}, config, nil)
	if err != nil {
		t.Fatal(err)
	}

	expectValue(t, got, false, "hubble", "enabled")
	expectValue(t, got, float64(2), "operator", "replicas")
}

// Expected Cilium values below follow the pre-operator setup-cni-cilium.sh
// (fcos-k8s, removed in c98474c), which each installer option mapped onto.

func TestComponentValues_CiliumL2AnnouncementsFollowClusterConfig(t *testing.T) {
	r := newValuesReconciler()

	t.Run("off by default", func(t *testing.T) {
		got, err := r.componentValues(context.Background(), "cilium", k4allv1alpha1.ComponentSpec{}, ciliumConfig(), nil)
		if err != nil {
			t.Fatal(err)
		}
		if v, ok := valueAt(got, "l2announcements", "enabled"); ok && v == true {
			t.Errorf("l2announcements.enabled = true without spec.networking.cni.cilium.l2announcements")
		}
	})

	t.Run("on when enabled", func(t *testing.T) {
		config := ciliumConfig()
		config.Networking.CNI.Cilium.L2Announcements = true
		got, err := r.componentValues(context.Background(), "cilium", k4allv1alpha1.ComponentSpec{}, config, nil)
		if err != nil {
			t.Fatal(err)
		}
		expectValue(t, got, true, "l2announcements", "enabled")
	})
}

func TestComponentValues_CiliumHubbleDeploysRelayAndUI(t *testing.T) {
	r := newValuesReconciler()
	config := ciliumConfig()
	config.Networking.CNI.Cilium.Hubble = true

	got, err := r.componentValues(context.Background(), "cilium", k4allv1alpha1.ComponentSpec{}, config, nil)
	if err != nil {
		t.Fatal(err)
	}

	expectValue(t, got, true, "hubble", "relay", "enabled")
	expectValue(t, got, true, "hubble", "ui", "enabled")
}

func TestComponentValues_CiliumGatewayAPI(t *testing.T) {
	r := newValuesReconciler()
	config := ciliumConfig()
	config.Networking.CNI.Cilium.GatewayAPI = true

	got, err := r.componentValues(context.Background(), "cilium", k4allv1alpha1.ComponentSpec{}, config, nil)
	if err != nil {
		t.Fatal(err)
	}

	expectValue(t, got, true, "gatewayAPI", "enabled")
}

func TestComponentValues_CiliumDevices(t *testing.T) {
	r := newValuesReconciler()
	devicesFor := func(t *testing.T, ovsBridge bool, additional ...string) (interface{}, bool) {
		t.Helper()
		config := ciliumConfig()
		config.Features.OVSBridge.Enabled = ovsBridge
		config.Networking.CNI.Cilium.AdditionalDevices = additional
		got, err := r.componentValues(context.Background(), "cilium", k4allv1alpha1.ComponentSpec{}, config, nil)
		if err != nil {
			t.Fatal(err)
		}
		return valueAt(normalize(t, got), "devices")
	}

	// Cilium 1.20 auto-detection skips openvswitch devices: ovs-bridge is
	// named, and forceDeviceDetection keeps the NIC until the bridge exists.
	t.Run("ovs-bridge named, auto-detection kept for the rest", func(t *testing.T) {
		devices, _ := devicesFor(t, true)
		if want := []interface{}{"ovs-bridge"}; !reflect.DeepEqual(devices, want) {
			t.Errorf("devices = %v, want %v", devices, want)
		}
		config := ciliumConfig()
		config.Features.OVSBridge.Enabled = true
		got, err := r.componentValues(context.Background(), "cilium", k4allv1alpha1.ComponentSpec{}, config, nil)
		if err != nil {
			t.Fatal(err)
		}
		expectValue(t, got, true, "forceDeviceDetection")
	})

	t.Run("auto-detected without the OVS bridge and additional devices", func(t *testing.T) {
		if devices, ok := devicesFor(t, false); ok {
			t.Errorf("devices = %v, want unset so Cilium auto-detects", devices)
		}
	})

	t.Run("ovs-bridge first, then the additional devices", func(t *testing.T) {
		devices, _ := devicesFor(t, true, "eth1", "eth2")
		want := []interface{}{"ovs-bridge", "eth1", "eth2"}
		if !reflect.DeepEqual(devices, want) {
			t.Errorf("devices = %v, want %v", devices, want)
		}
	})

	t.Run("only the additional devices without the OVS bridge", func(t *testing.T) {
		devices, _ := devicesFor(t, false, "eth1")
		want := []interface{}{"eth1"}
		if !reflect.DeepEqual(devices, want) {
			t.Errorf("devices = %v, want %v", devices, want)
		}
	})
}
