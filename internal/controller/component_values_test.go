package controller

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/go-logr/logr"
	k4allv1alpha1 "github.com/gpillon/k4all-operator/api/v1alpha1"
	"github.com/gpillon/k4all-operator/internal/controller/hooks"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func newValuesReconciler() *ReleaseReconciler {
	return &ReleaseReconciler{
		hookReg: hooks.NewRegistry(fake.NewClientBuilder().Build(), logr.Discard()),
	}
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

	got, err := r.componentValues(context.Background(), "cert-manager", k4allv1alpha1.ComponentSpec{}, config)
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

	got, err := r.componentValues(context.Background(), "cilium", k4allv1alpha1.ComponentSpec{}, config)
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

	got, err := r.componentValues(context.Background(), "cilium", k4allv1alpha1.ComponentSpec{}, config)
	if err != nil {
		t.Fatal(err)
	}

	expectValue(t, got, false, "hubble", "enabled")
	expectValue(t, got, float64(2), "operator", "replicas")
}
