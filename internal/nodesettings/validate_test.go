package nodesettings

import (
	"reflect"
	"strings"
	"testing"

	k4allv1alpha1 "github.com/gpillon/k4all-operator/api/v1alpha1"
)

const gib = int64(1) << 30

func TestParseCPUSet(t *testing.T) {
	for in, want := range map[string][]int{"0": {0}, "0-1": {0, 1}, "0,2-3": {0, 2, 3}, " 1 , 3 ": {1, 3}, "3,1-2,1": {1, 2, 3}} {
		got, err := ParseCPUSet(in)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Errorf("ParseCPUSet(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, in := range []string{"", "a", "1-", "3-1", "-1", "0,,1"} {
		if _, err := ParseCPUSet(in); err == nil {
			t.Errorf("ParseCPUSet(%q) = nil error", in)
		}
	}
}

func kubeletSpec(k k4allv1alpha1.KubeletSettings) Settings {
	return FromSpec(&k4allv1alpha1.NodesConfig{Kubelet: &k})
}

func facts(name string, cpus int64, memory int64) Node {
	return Node{Name: name, CPUs: cpus, Memory: memory}
}

func TestValidate_ReservedCPUsMustExistOnEveryNode(t *testing.T) {
	nodes := []Node{facts("big", 16, 64*gib), facts("small", 4, 16*gib)}
	if msg := Validate(kubeletSpec(k4allv1alpha1.KubeletSettings{CPUManagerPolicy: "static", ReservedSystemCPUs: "0-1"}), nodes, PodNetwork{}); msg != "" {
		t.Errorf("0-1 on 4 and 16 CPUs refused: %s", msg)
	}
	msg := Validate(kubeletSpec(k4allv1alpha1.KubeletSettings{CPUManagerPolicy: "static", ReservedSystemCPUs: "0,6"}), nodes, PodNetwork{})
	if !strings.Contains(msg, "reservedSystemCPUs 0,6: node small has 4 CPUs (0-3)") {
		t.Errorf("message = %q, want the node named", msg)
	}
	msg = Validate(kubeletSpec(k4allv1alpha1.KubeletSettings{CPUManagerPolicy: "static", ReservedSystemCPUs: "0-3"}), nodes, PodNetwork{})
	if !strings.Contains(msg, "reservedSystemCPUs 0-3 leaves no CPU for the pods of node small") {
		t.Errorf("message = %q", msg)
	}
	msg = Validate(kubeletSpec(k4allv1alpha1.KubeletSettings{ReservedSystemCPUs: "zero"}), nodes, PodNetwork{})
	if !strings.Contains(msg, `reservedSystemCPUs "zero" is not a CPU list`) {
		t.Errorf("message = %q", msg)
	}
}

// The operator refuses above 80 % of the smallest node's memory (the addon
// warns from 50 %).
func TestValidate_HugepagesLeaveMemoryOnTheSmallestNode(t *testing.T) {
	nodes := []Node{facts("big", 8, 64*gib), facts("small", 8, 10*gib)}
	ok := FromSpec(&k4allv1alpha1.NodesConfig{Hugepages: map[string]int32{"1Gi": 7, "2Mi": 512}}) // 8 GiB of 10
	if msg := Validate(ok, nodes, PodNetwork{}); msg != "" {
		t.Errorf("8 GiB of 10 refused: %s", msg)
	}
	tooMuch := FromSpec(&k4allv1alpha1.NodesConfig{Hugepages: map[string]int32{"1Gi": 8, "2Mi": 512}}) // 9 GiB
	msg := Validate(tooMuch, nodes, PodNetwork{})
	if !strings.Contains(msg, "hugepages take 9216Mi, more than 80 % of the 10240Mi of node small") {
		t.Errorf("message = %q", msg)
	}
}

func TestValidate_MaxPodsFitsThePodCIDROfEachNode(t *testing.T) {
	nodes := []Node{facts("a", 8, 64*gib), {Name: "b", CPUs: 8, Memory: 64 * gib, PodCIDR: "10.100.1.0/25"}}
	cilium := PodNetwork{MaskSize: 24, Source: "Cilium's cluster-pool"}
	if msg := Validate(kubeletSpec(k4allv1alpha1.KubeletSettings{MaxPods: ptr(int32(250))}), nodes, cilium); msg != "" {
		t.Errorf("250 on a /24 refused: %s", msg)
	}
	msg := Validate(kubeletSpec(k4allv1alpha1.KubeletSettings{MaxPods: ptr(int32(251))}), nodes, cilium)
	if !strings.Contains(msg, "maxPods 251 does not fit the /24 pod CIDR of node a (Cilium's cluster-pool): at most 250 pods") {
		t.Errorf("message = %q", msg)
	}
	perNode := PodNetwork{Source: "the node's spec.podCIDR"}
	msg = Validate(kubeletSpec(k4allv1alpha1.KubeletSettings{MaxPods: ptr(int32(200))}), nodes, perNode)
	if !strings.Contains(msg, "maxPods 200 does not fit the /25 pod CIDR of node b") {
		t.Errorf("message = %q, want node b's /25", msg)
	}
	calico := PodNetwork{Unlimited: true}
	if msg := Validate(kubeletSpec(k4allv1alpha1.KubeletSettings{MaxPods: ptr(int32(1000))}), nodes, calico); msg != "" {
		t.Errorf("Calico allocates blocks on demand, refused: %s", msg)
	}
}

func TestRecord_RoundTrip(t *testing.T) {
	s := FromSpec(&k4allv1alpha1.NodesConfig{Kubelet: &k4allv1alpha1.KubeletSettings{MaxPods: ptr(int32(250))},
		Hugepages: map[string]int32{"1Gi": 2}, IOMMU: ptr(true)})
	r := Record{Hash: s.Hash(), Settings: s, History: map[string]Settings{"old": {}}}
	text := r.Encode()
	for _, want := range []string{`"hash":"` + s.Hash() + `"`, `"kubelet":{"maxPods":250}`, `"hugepages":{"1Gi":2}`, `"iommu":true`} {
		if !strings.Contains(text, want) {
			t.Errorf("record %s lacks %s (the k4all-upgrade agent reads these names)", text, want)
		}
	}
	back, err := ParseRecord(text)
	if err != nil || back.Hash != r.Hash || !reflect.DeepEqual(back.Settings, s) || len(back.History) != 1 {
		t.Errorf("ParseRecord = %+v, %v", back, err)
	}
	if _, err := ParseRecord("{"); err == nil {
		t.Errorf("ParseRecord of broken JSON: nil error")
	}
}
