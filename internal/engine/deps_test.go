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
