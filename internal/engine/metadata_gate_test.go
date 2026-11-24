package engine

import (
	"context"
	"testing"

	"github.com/go-logr/logr"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	k4allv1alpha1 "github.com/gpillon/k4all-operator/api/v1alpha1"
)

// The metadata service needs virt and its own switch; absent, it is off
// (existing clusters: off until enabled, D10).
func TestFeatureEnabled_VirtMetadataNeedsVirtAndMetadata(t *testing.T) {
	config := func(virt bool, md *k4allv1alpha1.MetadataSettings) k4allv1alpha1.ClusterConfigSpec {
		return k4allv1alpha1.ClusterConfigSpec{Features: k4allv1alpha1.FeaturesConfig{
			Virt: k4allv1alpha1.VirtConfig{Enabled: virt, Metadata: md},
		}}
	}
	on := &k4allv1alpha1.MetadataSettings{Enabled: true}
	for name, c := range map[string]struct {
		spec k4allv1alpha1.ClusterConfigSpec
		want bool
	}{
		"virt and metadata":       {config(true, on), true},
		"virt, metadata absent":   {config(true, nil), false},
		"virt, metadata disabled": {config(true, &k4allv1alpha1.MetadataSettings{}), false},
		"metadata without virt":   {config(false, on), false},
		"nothing":                 {k4allv1alpha1.ClusterConfigSpec{}, false},
	} {
		if got := FeatureEnabled(FeatureVirtMetadata, c.spec); got != c.want {
			t.Errorf("%s: %v, want %v", name, got, c.want)
		}
	}
	active := FilterActiveComponents(map[string]k4allv1alpha1.ComponentSpec{
		"k4all-metadata": {Type: k4allv1alpha1.ComponentTypeManifest, Version: "v0.6.0", FeatureGate: FeatureVirtMetadata},
	}, config(true, nil))
	if _, ok := active["k4all-metadata"]; ok {
		t.Error("k4all-metadata is active with metadata absent")
	}
}

// A manifest component without sources is rendered by its hook
// (k4all-metadata): the engine has nothing to apply, and that is no error.
func TestManifestApply_NothingToApply(t *testing.T) {
	m := NewManifestApplier(fake.NewClientBuilder().Build(), logr.Discard())
	spec := k4allv1alpha1.ComponentSpec{Type: k4allv1alpha1.ComponentTypeManifest, Version: "v0.6.0"}
	if err := m.Apply(context.Background(), "k4all-metadata", spec, nil); err != nil {
		t.Fatal(err)
	}
	if err := m.Delete(context.Background(), "k4all-metadata", spec); err != nil {
		t.Fatal(err)
	}
}
