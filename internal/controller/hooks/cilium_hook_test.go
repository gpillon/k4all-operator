package hooks

import (
	"reflect"
	"testing"

	"github.com/go-logr/logr"

	k4allv1alpha1 "github.com/gpillon/k4all-operator/api/v1alpha1"
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
	for _, k := range []string{"socketLB", "cni", "rollOutCiliumPods"} {
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
	if v["rollOutCiliumPods"] != true {
		t.Errorf("rollOutCiliumPods = %v, want true", v["rollOutCiliumPods"])
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
	if v["rollOutCiliumPods"] != true {
		t.Errorf("rollOutCiliumPods = %v, want true", v["rollOutCiliumPods"])
	}
}

func TestBuildCiliumValues_KubeVirtAndMultus(t *testing.T) {
	v := BuildCiliumValues(ciliumConfig(true, true))
	if v["socketLB"] == nil || v["cni"] == nil || v["rollOutCiliumPods"] != true {
		t.Errorf("values = %v, want socketLB, cni and rollOutCiliumPods", v)
	}
}

func TestCiliumHookHasAValuesRevision(t *testing.T) {
	var h Hook = NewCiliumHook(nil, logr.Discard())
	rev, ok := h.(ValuesRevisioner)
	if !ok || rev.ValuesRevision() == "" {
		t.Fatalf("the cilium hook must report a values revision, so clusters re-apply Cilium after the operator upgrade")
	}
}
