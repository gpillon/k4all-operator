package nodesettings

import (
	"reflect"
	"strings"
	"testing"

	k4allv1alpha1 "github.com/gpillon/k4all-operator/api/v1alpha1"
)

func ptr[T any](v T) *T { return &v }

func TestFromSpec_KeepsOnlyWhatIsSet(t *testing.T) {
	if s := FromSpec(nil); !s.Empty() {
		t.Errorf("FromSpec(nil) = %+v, want empty", s)
	}
	s := FromSpec(&k4allv1alpha1.NodesConfig{
		Kubelet:   &k4allv1alpha1.KubeletSettings{MaxPods: ptr(int32(250))},
		Hugepages: map[string]int32{"1Gi": 4, "2Mi": 0},
		IOMMU:     ptr(false),
		Rollout:   &k4allv1alpha1.NodeRollout{Paused: true},
	})
	if s.Kubelet.MaxPods == nil || *s.Kubelet.MaxPods != 250 || s.Kubelet.CPUManagerPolicy != "" {
		t.Errorf("kubelet = %+v, want maxPods 250 only", s.Kubelet)
	}
	if len(s.Hugepages) != 1 || s.Hugepages["1Gi"] != 4 {
		t.Errorf("hugepages = %v, want 1Gi: 4 (0 reserves none, as absent)", s.Hugepages)
	}
	if s.IOMMU {
		t.Errorf("iommu = true from false")
	}
}

// The hash covers what the nodes must have, not how the rollout runs.
func TestHash_IsTheNodeStateOnly(t *testing.T) {
	a := FromSpec(&k4allv1alpha1.NodesConfig{Hugepages: map[string]int32{"1Gi": 4}})
	b := FromSpec(&k4allv1alpha1.NodesConfig{Hugepages: map[string]int32{"1Gi": 4, "2Mi": 0}, IOMMU: ptr(false),
		Rollout: &k4allv1alpha1.NodeRollout{Paused: true}})
	if a.Hash() != b.Hash() {
		t.Errorf("hash differs for the same node state: %s %s", a.Hash(), b.Hash())
	}
	if len(a.Hash()) != 16 || strings.Trim(a.Hash(), "0123456789abcdef") != "" {
		t.Errorf("hash = %q, want 16 hex characters", a.Hash())
	}
	c := FromSpec(&k4allv1alpha1.NodesConfig{Hugepages: map[string]int32{"1Gi": 8}})
	if a.Hash() == c.Hash() {
		t.Errorf("hash unchanged by a different hugepage count")
	}
	if (Settings{}).Hash() != FromSpec(nil).Hash() {
		t.Errorf("empty settings have two hashes")
	}
}

func TestClassOf(t *testing.T) {
	kubelet := func(k k4allv1alpha1.KubeletSettings) Settings {
		return FromSpec(&k4allv1alpha1.NodesConfig{Kubelet: &k})
	}
	none := Settings{}
	cases := []struct {
		name     string
		from, to Settings
		want     Class
	}{
		{"nothing changes", none, none, ""},
		{"maxPods only", none, kubelet(k4allv1alpha1.KubeletSettings{MaxPods: ptr(int32(250))}), Restart},
		{"cpu manager", none, kubelet(k4allv1alpha1.KubeletSettings{CPUManagerPolicy: "static", ReservedSystemCPUs: "0-1"}), DrainRestart},
		{"reserved cpus", kubelet(k4allv1alpha1.KubeletSettings{ReservedSystemCPUs: "0"}), kubelet(k4allv1alpha1.KubeletSettings{ReservedSystemCPUs: "0-1"}), DrainRestart},
		{"topology policy", none, kubelet(k4allv1alpha1.KubeletSettings{TopologyManagerPolicy: "single-numa-node"}), DrainRestart},
		{"topology scope", none, kubelet(k4allv1alpha1.KubeletSettings{TopologyManagerScope: "pod"}), DrainRestart},
		{"maxPods and cpu manager", none, kubelet(k4allv1alpha1.KubeletSettings{MaxPods: ptr(int32(200)), CPUManagerPolicy: "static", ReservedSystemCPUs: "0"}), DrainRestart},
		{"hugepages", none, FromSpec(&k4allv1alpha1.NodesConfig{Hugepages: map[string]int32{"2Mi": 512}}), DrainReboot},
		{"iommu off again", FromSpec(&k4allv1alpha1.NodesConfig{IOMMU: ptr(true)}), none, DrainReboot},
		{"maxPods cleared", kubelet(k4allv1alpha1.KubeletSettings{MaxPods: ptr(int32(250))}), none, Restart},
	}
	for _, tc := range cases {
		if got := ClassOf(tc.from, tc.to); got != tc.want {
			t.Errorf("%s: ClassOf = %q, want %q", tc.name, got, tc.want)
		}
	}
	if Restart.Drains() || !DrainRestart.Drains() || !DrainReboot.Drains() {
		t.Errorf("Drains: restart %v drain-restart %v drain-reboot %v", Restart.Drains(), DrainRestart.Drains(), DrainReboot.Drains())
	}
}

// kubeadm's KubeletConfiguration, as kube-system/kubelet-config holds it.
const kubeadmKubelet = `apiVersion: kubelet.config.k8s.io/v1beta1
authentication:
  anonymous:
    enabled: false
cgroupDriver: systemd
clusterDNS:
- 10.96.0.10
clusterDomain: cluster.local
kind: KubeletConfiguration
maxPods: 250
rotateCertificates: true
`

func TestMergeKubelet_WritesOnlyTheOwnedFields(t *testing.T) {
	want := Kubelet{CPUManagerPolicy: "static", ReservedSystemCPUs: "0", TopologyManagerPolicy: "single-numa-node"}
	got, prior, changed, err := MergeKubelet(kubeadmKubelet, want, nil)
	if err != nil || !changed {
		t.Fatalf("MergeKubelet = changed %v, %v", changed, err)
	}
	for _, line := range []string{`cpuManagerPolicy: "static"`, `reservedSystemCPUs: "0"`, `topologyManagerPolicy: "single-numa-node"`,
		"maxPods: 250", "cgroupDriver: systemd", "clusterDomain: cluster.local", "rotateCertificates: true", "- 10.96.0.10"} {
		if !strings.Contains(got, line) {
			t.Errorf("merged config lacks %q:\n%s", line, got)
		}
	}
	if strings.Contains(got, "topologyManagerScope") {
		t.Errorf("wrote topologyManagerScope, not set:\n%s", got)
	}
	// The order of the fields kubeadm wrote stays.
	if strings.Index(got, "apiVersion") > strings.Index(got, "cgroupDriver") || strings.Index(got, "kind:") > strings.Index(got, "maxPods") {
		t.Errorf("fields reordered:\n%s", got)
	}
	// None of them was there before k4all.
	if len(prior) != 3 || prior["cpuManagerPolicy"] != nil || prior["reservedSystemCPUs"] != nil || prior["topologyManagerPolicy"] != nil {
		t.Errorf("prior = %v, want the three fields, absent before", prior)
	}

	again, prior2, changed, err := MergeKubelet(got, want, prior)
	if err != nil || changed || again != got || !reflect.DeepEqual(prior2, prior) {
		t.Errorf("second merge changed %v (%v), prior %v:\n%s", changed, err, prior2, again)
	}
}

// Production's hand-set maxPods 250: setting it to the same value is no
// change to the config, and the 250 is what was there before k4all.
func TestMergeKubelet_SameValueIsNoChange(t *testing.T) {
	_, prior, changed, err := MergeKubelet(kubeadmKubelet, Kubelet{MaxPods: ptr(int32(250))}, nil)
	if err != nil || changed || prior["maxPods"] == nil || *prior["maxPods"] != "250" {
		t.Errorf("maxPods 250 over 250: changed %v, prior %v, %v", changed, prior, err)
	}
	got, _, changed, _ := MergeKubelet(kubeadmKubelet, Kubelet{MaxPods: ptr(int32(110))}, nil)
	if !changed || !strings.Contains(got, "maxPods: 110") || strings.Contains(got, "maxPods: 250") {
		t.Errorf("maxPods 110: changed %v\n%s", changed, got)
	}
}

// Clearing a field puts back what kubelet-config had before k4all first wrote
// it: kubeadm's maxPods 250, not the kubelet's 110.
func TestMergeKubelet_ClearingPutsBackWhatWasThere(t *testing.T) {
	doc, prior, _, err := MergeKubelet(kubeadmKubelet, Kubelet{MaxPods: ptr(int32(300))}, nil)
	if err != nil || !strings.Contains(doc, "maxPods: 300") {
		t.Fatalf("set 300: %v\n%s", err, doc)
	}
	got, prior, changed, err := MergeKubelet(doc, Kubelet{}, prior)
	if err != nil || !changed || !strings.Contains(got, "maxPods: 250") || strings.Contains(got, "300") {
		t.Errorf("cleared: changed %v, %v\n%s", changed, err, got)
	}
	if len(prior) != 0 {
		t.Errorf("prior = %v after the clear, want nothing owned", prior)
	}
}

// A field that was not there before k4all goes again when cleared.
func TestMergeKubelet_ClearingAFieldThatWasNotThereRemovesIt(t *testing.T) {
	doc, prior, _, _ := MergeKubelet(kubeadmKubelet, Kubelet{CPUManagerPolicy: "static", ReservedSystemCPUs: "0-1"}, nil)
	got, _, changed, err := MergeKubelet(doc, Kubelet{}, prior)
	if err != nil || !changed {
		t.Fatalf("clearing: changed %v, %v", changed, err)
	}
	if strings.Contains(got, "cpuManagerPolicy") || strings.Contains(got, "reservedSystemCPUs") || !strings.Contains(got, "maxPods: 250") {
		t.Errorf("after clearing:\n%s", got)
	}
	// Nothing owned, nothing written.
	if _, _, changed, _ := MergeKubelet(kubeadmKubelet, Kubelet{}, nil); changed {
		t.Errorf("nothing owned, nothing written: changed")
	}
}

// Changing an owned field keeps what was there before k4all: the clear puts
// back that, not the first value k4all wrote.
func TestMergeKubelet_ThePriorValueIsTheOneBeforeK4all(t *testing.T) {
	doc, prior, _, _ := MergeKubelet(kubeadmKubelet, Kubelet{MaxPods: ptr(int32(300))}, nil)
	doc, prior, _, _ = MergeKubelet(doc, Kubelet{MaxPods: ptr(int32(200))}, prior)
	if !strings.Contains(doc, "maxPods: 200") || *prior["maxPods"] != "250" {
		t.Fatalf("200: prior %v\n%s", prior, doc)
	}
	got, _, _, _ := MergeKubelet(doc, Kubelet{}, prior)
	if !strings.Contains(got, "maxPods: 250") {
		t.Errorf("cleared after 300 then 200:\n%s", got)
	}
}

func TestMergeKubelet_RefusesAConfigItCannotRead(t *testing.T) {
	for _, doc := range []string{"", "- a list\n", "kind: [\n"} {
		if _, _, _, err := MergeKubelet(doc, Kubelet{MaxPods: ptr(int32(200))}, nil); err == nil {
			t.Errorf("MergeKubelet(%q) = nil error", doc)
		}
	}
}
