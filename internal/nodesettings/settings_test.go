package nodesettings

import (
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
	got, changed, err := MergeKubelet(kubeadmKubelet, want, Kubelet{})
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

	again, changed, err := MergeKubelet(got, want, want)
	if err != nil || changed || again != got {
		t.Errorf("second merge changed %v (%v):\n%s", changed, err, again)
	}
}

// A field k4all set by hand before is not k4all's until the ClusterConfig
// sets it: production's maxPods 250 stays. Setting it to the same value is
// no change.
func TestMergeKubelet_SameValueIsNoChange(t *testing.T) {
	_, changed, err := MergeKubelet(kubeadmKubelet, Kubelet{MaxPods: ptr(int32(250))}, Kubelet{})
	if err != nil || changed {
		t.Errorf("maxPods 250 over 250: changed %v, %v", changed, err)
	}
	got, changed, _ := MergeKubelet(kubeadmKubelet, Kubelet{MaxPods: ptr(int32(110))}, Kubelet{})
	if !changed || !strings.Contains(got, "maxPods: 110") || strings.Contains(got, "maxPods: 250") {
		t.Errorf("maxPods 110: changed %v\n%s", changed, got)
	}
}

func TestMergeKubelet_RemovesOnlyWhatK4allWrote(t *testing.T) {
	doc, _, _ := MergeKubelet(kubeadmKubelet, Kubelet{CPUManagerPolicy: "static", ReservedSystemCPUs: "0-1"}, Kubelet{})
	got, changed, err := MergeKubelet(doc, Kubelet{}, Kubelet{CPUManagerPolicy: "static", ReservedSystemCPUs: "0-1"})
	if err != nil || !changed {
		t.Fatalf("clearing: changed %v, %v", changed, err)
	}
	if strings.Contains(got, "cpuManagerPolicy") || strings.Contains(got, "reservedSystemCPUs") {
		t.Errorf("cleared fields left:\n%s", got)
	}
	if !strings.Contains(got, "maxPods: 250") {
		t.Errorf("maxPods, not k4all's, removed:\n%s", got)
	}
	// maxPods was never k4all's: clearing nothing leaves it.
	if _, changed, _ := MergeKubelet(kubeadmKubelet, Kubelet{}, Kubelet{}); changed {
		t.Errorf("nothing owned, nothing written: changed")
	}
}

func TestMergeKubelet_RefusesAConfigItCannotRead(t *testing.T) {
	for _, doc := range []string{"", "- a list\n", "kind: [\n"} {
		if _, _, err := MergeKubelet(doc, Kubelet{MaxPods: ptr(int32(200))}, Kubelet{}); err == nil {
			t.Errorf("MergeKubelet(%q) = nil error", doc)
		}
	}
}
