package engine

import (
	"testing"

	k4allv1alpha1 "github.com/gpillon/k4all-operator/api/v1alpha1"
)

func TestTopologicalSort_Linear(t *testing.T) {
	components := map[string]k4allv1alpha1.ComponentSpec{
		"a": {Type: k4allv1alpha1.ComponentTypeManifest, Version: "1"},
		"b": {Type: k4allv1alpha1.ComponentTypeManifest, Version: "1", DependsOn: []string{"a"}},
		"c": {Type: k4allv1alpha1.ComponentTypeManifest, Version: "1", DependsOn: []string{"b"}},
	}

	g := NewDependencyGraph(components)
	order, err := g.TopologicalSort()
	if err != nil {
		t.Fatal(err)
	}

	if len(order) != 3 {
		t.Fatalf("expected 3 components, got %d", len(order))
	}

	indexOf := func(name string) int {
		for i, n := range order {
			if n == name {
				return i
			}
		}
		return -1
	}

	if indexOf("a") > indexOf("b") {
		t.Error("a must come before b")
	}
	if indexOf("b") > indexOf("c") {
		t.Error("b must come before c")
	}
}

func TestTopologicalSort_Diamond(t *testing.T) {
	components := map[string]k4allv1alpha1.ComponentSpec{
		"base":  {Type: k4allv1alpha1.ComponentTypeManifest, Version: "1"},
		"left":  {Type: k4allv1alpha1.ComponentTypeManifest, Version: "1", DependsOn: []string{"base"}},
		"right": {Type: k4allv1alpha1.ComponentTypeManifest, Version: "1", DependsOn: []string{"base"}},
		"top":   {Type: k4allv1alpha1.ComponentTypeManifest, Version: "1", DependsOn: []string{"left", "right"}},
	}

	g := NewDependencyGraph(components)
	order, err := g.TopologicalSort()
	if err != nil {
		t.Fatal(err)
	}

	if len(order) != 4 {
		t.Fatalf("expected 4 components, got %d", len(order))
	}

	indexOf := func(name string) int {
		for i, n := range order {
			if n == name {
				return i
			}
		}
		return -1
	}

	if indexOf("base") > indexOf("left") || indexOf("base") > indexOf("right") {
		t.Error("base must come before left and right")
	}
	if indexOf("left") > indexOf("top") || indexOf("right") > indexOf("top") {
		t.Error("left and right must come before top")
	}
}

func TestTopologicalSort_Cycle(t *testing.T) {
	components := map[string]k4allv1alpha1.ComponentSpec{
		"a": {Type: k4allv1alpha1.ComponentTypeManifest, Version: "1", DependsOn: []string{"c"}},
		"b": {Type: k4allv1alpha1.ComponentTypeManifest, Version: "1", DependsOn: []string{"a"}},
		"c": {Type: k4allv1alpha1.ComponentTypeManifest, Version: "1", DependsOn: []string{"b"}},
	}

	g := NewDependencyGraph(components)
	_, err := g.TopologicalSort()
	if err == nil {
		t.Fatal("expected cycle detection error")
	}
}

func TestTopologicalSort_NoDeps(t *testing.T) {
	components := map[string]k4allv1alpha1.ComponentSpec{
		"x": {Type: k4allv1alpha1.ComponentTypeManifest, Version: "1"},
		"y": {Type: k4allv1alpha1.ComponentTypeManifest, Version: "1"},
		"z": {Type: k4allv1alpha1.ComponentTypeManifest, Version: "1"},
	}

	g := NewDependencyGraph(components)
	order, err := g.TopologicalSort()
	if err != nil {
		t.Fatal(err)
	}

	if len(order) != 3 {
		t.Fatalf("expected 3 components, got %d", len(order))
	}
}

func TestFilterActiveComponents(t *testing.T) {
	components := map[string]k4allv1alpha1.ComponentSpec{
		"calico":       {Type: k4allv1alpha1.ComponentTypeManifest, Version: "1"},
		"cilium":       {Type: k4allv1alpha1.ComponentTypeHelmOCI, Version: "1"},
		"cert-manager": {Type: k4allv1alpha1.ComponentTypeHelm, Version: "1"},
		"kubevirt":     {Type: k4allv1alpha1.ComponentTypeManifest, Version: "1", FeatureGate: "virt"},
		"argocd":       {Type: k4allv1alpha1.ComponentTypeHelm, Version: "1", FeatureGate: "argocd"},
	}

	config := k4allv1alpha1.ClusterConfigSpec{
		Networking: k4allv1alpha1.NetworkingConfig{
			CNI: k4allv1alpha1.CNIConfig{Type: "calico"},
		},
		Features: k4allv1alpha1.FeaturesConfig{
			Virt:   k4allv1alpha1.VirtConfig{Enabled: true},
			ArgoCD: k4allv1alpha1.ArgoCDConfig{Enabled: false},
		},
	}

	active := FilterActiveComponents(components, config)

	if _, ok := active["calico"]; !ok {
		t.Error("calico should be active (selected CNI)")
	}
	if _, ok := active["cilium"]; ok {
		t.Error("cilium should NOT be active (calico selected)")
	}
	if _, ok := active["cert-manager"]; !ok {
		t.Error("cert-manager should always be active")
	}
	if _, ok := active["kubevirt"]; !ok {
		t.Error("kubevirt should be active (virt enabled)")
	}
	if _, ok := active["argocd"]; ok {
		t.Error("argocd should NOT be active (argocd disabled)")
	}
}

// Rook is on only with features.rook.enabled: a cluster without the rook
// field (every v2.6.x cluster) or with it off runs no Rook, and nothing
// behind the rook gate (Cairn).
func TestFeatureEnabled_RookFollowsTheClusterConfig(t *testing.T) {
	everythingElse := k4allv1alpha1.FeaturesConfig{
		Virt:   k4allv1alpha1.VirtConfig{Enabled: true},
		ArgoCD: k4allv1alpha1.ArgoCDConfig{Enabled: true},
		OVSCNI: k4allv1alpha1.OVSCNIConfig{Enabled: true},
	}
	cairn := map[string]k4allv1alpha1.ComponentSpec{
		"cairn": {Type: k4allv1alpha1.ComponentTypeManifest, Version: "v0.1.0", FeatureGate: FeatureRook},
	}
	for _, tc := range []struct {
		name string
		rook *k4allv1alpha1.RookConfig
		want bool
	}{
		{"no rook field", nil, false},
		{"rook: {}", &k4allv1alpha1.RookConfig{}, false},
		{"enabled false", &k4allv1alpha1.RookConfig{Enabled: false, SmallCluster: k4allv1alpha1.RookSmallClusterAllow}, false},
		{"enabled true", &k4allv1alpha1.RookConfig{Enabled: true}, true},
	} {
		features := everythingElse
		features.Rook = tc.rook
		config := k4allv1alpha1.ClusterConfigSpec{Features: features}
		if got := FeatureEnabled(FeatureRook, config); got != tc.want {
			t.Errorf("%s: rook on %v, want %v", tc.name, got, tc.want)
		}
		if _, got := FilterActiveComponents(cairn, config)["cairn"]; got != tc.want {
			t.Errorf("%s: cairn active %v, want %v", tc.name, got, tc.want)
		}
	}
}

// The CSI snapshot controller serves both features (D13): KubeVirt and
// Hangar snapshot VM disks, and ceph-rbd has a VolumeSnapshotClass. It is on
// with either, off with neither.
func TestFeatureEnabled_SnapshotsWithVirtOrRook(t *testing.T) {
	rookOn := &k4allv1alpha1.RookConfig{Enabled: true}
	for _, tc := range []struct {
		name string
		virt bool
		rook *k4allv1alpha1.RookConfig
		want bool
	}{
		{"neither", false, nil, false},
		{"rook field off", false, &k4allv1alpha1.RookConfig{}, false},
		{"virt", true, nil, true},
		{"rook", false, rookOn, true},
		{"both", true, rookOn, true},
	} {
		config := k4allv1alpha1.ClusterConfigSpec{Features: k4allv1alpha1.FeaturesConfig{
			Virt: k4allv1alpha1.VirtConfig{Enabled: tc.virt},
			Rook: tc.rook,
		}}
		if got := FeatureEnabled(FeatureSnapshots, config); got != tc.want {
			t.Errorf("%s: snapshots on %v, want %v", tc.name, got, tc.want)
		}
	}
}

// Hangar's node agent comes with virt (D6) unless the ClusterConfig says
// nodeAgent: false; absent means the release's default, installed.
func TestFilterActiveComponents_HangarNodeAgent(t *testing.T) {
	components := map[string]k4allv1alpha1.ComponentSpec{
		"hangar":            {Type: k4allv1alpha1.ComponentTypeManifest, Version: "v0.1.0", FeatureGate: FeatureVirt},
		"hangar-node-agent": {Type: k4allv1alpha1.ComponentTypeManifest, Version: "v0.1.0", FeatureGate: FeatureVirt},
	}
	yes, no := true, false
	for _, tc := range []struct {
		name   string
		virt   bool
		hangar *k4allv1alpha1.HangarSettings
		want   bool
	}{
		{"virt off", false, nil, false},
		{"virt off, nodeAgent true", false, &k4allv1alpha1.HangarSettings{NodeAgent: &yes}, false},
		{"virt, no hangar field", true, nil, true},
		{"virt, hangar: {}", true, &k4allv1alpha1.HangarSettings{}, true},
		{"virt, nodeAgent true", true, &k4allv1alpha1.HangarSettings{NodeAgent: &yes}, true},
		{"virt, nodeAgent false", true, &k4allv1alpha1.HangarSettings{NodeAgent: &no}, false},
	} {
		config := k4allv1alpha1.ClusterConfigSpec{Features: k4allv1alpha1.FeaturesConfig{
			Virt: k4allv1alpha1.VirtConfig{Enabled: tc.virt, Hangar: tc.hangar},
		}}
		active := FilterActiveComponents(components, config)
		if _, got := active["hangar-node-agent"]; got != tc.want {
			t.Errorf("%s: hangar-node-agent active %v, want %v", tc.name, got, tc.want)
		}
		if _, got := active["hangar"]; got != tc.virt {
			t.Errorf("%s: hangar active %v, want %v (with virt)", tc.name, got, tc.virt)
		}
	}
}

// The gates the ReleaseManifest names follow their ClusterConfig switch.
func TestFeatureEnabled_FollowsTheClusterConfig(t *testing.T) {
	for _, gate := range []string{FeatureVirt, "argocd", "ovsCni"} {
		on := k4allv1alpha1.ClusterConfigSpec{Features: k4allv1alpha1.FeaturesConfig{
			Virt:   k4allv1alpha1.VirtConfig{Enabled: true},
			ArgoCD: k4allv1alpha1.ArgoCDConfig{Enabled: true},
			OVSCNI: k4allv1alpha1.OVSCNIConfig{Enabled: true},
		}}
		if !FeatureEnabled(gate, on) {
			t.Errorf("%s: off with its switch on", gate)
		}
		if FeatureEnabled(gate, k4allv1alpha1.ClusterConfigSpec{}) {
			t.Errorf("%s: on with its switch off", gate)
		}
	}
	if FeatureEnabled("unknown", k4allv1alpha1.ClusterConfigSpec{}) {
		t.Error("an unknown gate is on")
	}
}
