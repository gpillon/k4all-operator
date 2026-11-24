// Package nodesettings decides the rollout of the ClusterConfig's node
// settings (spec.nodes, k4all#60): the kubelet fields k4all owns in
// kube-system/kubelet-config, hugepages and IOMMU as kernel arguments, one
// node at a time. It does no I/O: the controller reads the cluster, runs the
// returned actions and writes the returned status.
//
// The operator and each node talk through Node annotations (ADR-0006): the
// operator drains the node and writes node-settings-target; the node's
// k4all-upgrade agent pulls the kubelet config, sets its kernel arguments,
// restarts kubelet or reboots, and answers with node-settings-applied.
package nodesettings

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	k4allv1alpha1 "github.com/gpillon/k4all-operator/api/v1alpha1"
	yaml "sigs.k8s.io/yaml/goyaml.v3"
)

// Kubelet is the part of the KubeletConfiguration k4all owns; a field left
// empty is not owned. The JSON names are the KubeletConfiguration's.
type Kubelet struct {
	MaxPods               *int32 `json:"maxPods,omitempty"`
	CPUManagerPolicy      string `json:"cpuManagerPolicy,omitempty"`
	ReservedSystemCPUs    string `json:"reservedSystemCPUs,omitempty"`
	TopologyManagerPolicy string `json:"topologyManagerPolicy,omitempty"`
	TopologyManagerScope  string `json:"topologyManagerScope,omitempty"`
}

// Settings is what every node must have: the owned kubelet fields, the
// hugepages (page size to pages, only counts above 0) and IOMMU.
type Settings struct {
	Kubelet   Kubelet          `json:"kubelet"`
	Hugepages map[string]int32 `json:"hugepages,omitempty"`
	IOMMU     bool             `json:"iommu,omitempty"`
}

// FromSpec is the node state spec.nodes asks for. Absent fields are not
// owned; 0 hugepages and iommu false mean what absent does: no kernel
// argument from k4all.
func FromSpec(spec *k4allv1alpha1.NodesConfig) Settings {
	s := Settings{}
	if spec == nil {
		return s
	}
	if k := spec.Kubelet; k != nil {
		s.Kubelet = Kubelet{MaxPods: k.MaxPods, CPUManagerPolicy: k.CPUManagerPolicy, ReservedSystemCPUs: k.ReservedSystemCPUs,
			TopologyManagerPolicy: k.TopologyManagerPolicy, TopologyManagerScope: k.TopologyManagerScope}
		if k.MaxPods != nil {
			maxPods := *k.MaxPods
			s.Kubelet.MaxPods = &maxPods
		}
	}
	for size, pages := range spec.Hugepages {
		if pages > 0 {
			if s.Hugepages == nil {
				s.Hugepages = map[string]int32{}
			}
			s.Hugepages[size] = pages
		}
	}
	s.IOMMU = spec.IOMMU != nil && *spec.IOMMU
	return s
}

// Empty tells whether k4all owns no node setting.
func (s Settings) Empty() bool {
	return s.Kubelet == (Kubelet{}) && len(s.Hugepages) == 0 && !s.IOMMU
}

// Hash names s: 16 hex characters of the SHA-256 of its JSON (sorted map
// keys), what node-settings-target and node-settings-applied carry.
func (s Settings) Hash() string {
	data, err := json.Marshal(s)
	if err != nil { // a struct of strings, ints and maps: never
		panic(err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])[:16]
}

// Class is what a node does to go from one Settings to another.
type Class string

const (
	// Restart: only maxPods changes. kubelet restarts, the node is not
	// drained.
	Restart Class = "restart"
	// DrainRestart: the CPU manager policy, the reserved CPUs or the
	// topology manager change. The node is drained, its CPU and memory
	// manager state reset, kubelet restarted.
	DrainRestart Class = "drain-restart"
	// DrainReboot: hugepages or IOMMU change, as kernel arguments. The node
	// is drained and reboots.
	DrainReboot Class = "drain-reboot"
)

// Drains tells whether the node is drained (cordoned only on a single node)
// before it takes the settings.
func (c Class) Drains() bool { return c == DrainRestart || c == DrainReboot }

// ClassOf is what a node with from does to get to: the heaviest of its
// changes, "" when nothing changes.
func ClassOf(from, to Settings) Class {
	if from.IOMMU != to.IOMMU || !sameHugepages(from.Hugepages, to.Hugepages) {
		return DrainReboot
	}
	f, t := from.Kubelet, to.Kubelet
	if f.CPUManagerPolicy != t.CPUManagerPolicy || f.ReservedSystemCPUs != t.ReservedSystemCPUs ||
		f.TopologyManagerPolicy != t.TopologyManagerPolicy || f.TopologyManagerScope != t.TopologyManagerScope {
		return DrainRestart
	}
	if !samePtr(f.MaxPods, t.MaxPods) {
		return Restart
	}
	return ""
}

func sameHugepages(a, b map[string]int32) bool {
	if len(a) != len(b) {
		return false
	}
	for size, pages := range a {
		if b[size] != pages {
			return false
		}
	}
	return true
}

func samePtr(a, b *int32) bool {
	return (a == nil && b == nil) || (a != nil && b != nil && *a == *b)
}

// kubeletFields are the KubeletConfiguration fields k4all may own, in the
// order MergeKubelet appends them.
var kubeletFields = []string{"maxPods", "cpuManagerPolicy", "reservedSystemCPUs", "topologyManagerPolicy", "topologyManagerScope"}

// values is k by KubeletConfiguration field name, set fields only; maxPods
// as its decimal text.
func (k Kubelet) values() map[string]string {
	out := map[string]string{}
	if k.MaxPods != nil {
		out["maxPods"] = fmt.Sprint(*k.MaxPods)
	}
	for name, v := range map[string]string{"cpuManagerPolicy": k.CPUManagerPolicy, "reservedSystemCPUs": k.ReservedSystemCPUs,
		"topologyManagerPolicy": k.TopologyManagerPolicy, "topologyManagerScope": k.TopologyManagerScope} {
		if v != "" {
			out[name] = v
		}
	}
	return out
}

// Prior is what kubelet-config had in each field k4all owns before k4all
// first wrote it: the value's text, nil when the field was not there. A
// field is k4all's exactly when it has an entry.
type Prior map[string]*string

// MergeKubelet returns doc, kubeadm's KubeletConfiguration (data.kubelet of
// kube-system/kubelet-config), with the fields of want set, and prior with
// what each field had before k4all first wrote it. A field k4all owned
// (in prior) that want no longer sets gets back its prior value, or goes
// when it had none. Every other field keeps its value and place. changed is
// false when doc already says that; doc is then returned as it is.
func MergeKubelet(doc string, want Kubelet, prior Prior) (string, Prior, bool, error) {
	var root yaml.Node
	if err := yaml.Unmarshal([]byte(doc), &root); err != nil {
		return "", nil, false, fmt.Errorf("read the KubeletConfiguration: %w", err)
	}
	if root.Kind != yaml.DocumentNode || len(root.Content) != 1 || root.Content[0].Kind != yaml.MappingNode {
		return "", nil, false, fmt.Errorf("the KubeletConfiguration is not a YAML mapping")
	}
	m := root.Content[0]
	wantValues := want.values()
	next := Prior{}
	changed := false
	set := func(i int, field, value string) {
		switch {
		case i < 0:
			m.Content = append(m.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: field}, scalar(field, value))
			changed = true
		case m.Content[i+1].Value != value:
			m.Content[i+1] = scalar(field, value)
			changed = true
		}
	}
	for _, field := range kubeletFields {
		value, wanted := wantValues[field]
		before, owned := prior[field]
		i := keyIndex(m, field)
		switch {
		case wanted && owned:
			next[field] = before
			set(i, field, value)
		case wanted:
			next[field] = nil // the first write: keep what is there
			if i >= 0 {
				was := m.Content[i+1].Value
				next[field] = &was
			}
			set(i, field, value)
		case owned && before != nil:
			set(i, field, *before)
		case owned && i >= 0:
			m.Content = append(m.Content[:i], m.Content[i+2:]...)
			changed = true
		}
	}
	if len(next) == 0 {
		next = nil
	}
	if !changed {
		return doc, next, false, nil
	}
	var out strings.Builder
	enc := yaml.NewEncoder(&out)
	enc.SetIndent(2)
	if err := enc.Encode(&root); err != nil {
		return "", nil, false, fmt.Errorf("write the KubeletConfiguration: %w", err)
	}
	if err := enc.Close(); err != nil {
		return "", nil, false, err
	}
	return out.String(), next, true, nil
}

func keyIndex(m *yaml.Node, key string) int {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return i
		}
	}
	return -1
}

// scalar is the YAML value of a KubeletConfiguration field: maxPods an int,
// the others quoted strings ("0" stays a string).
func scalar(field, value string) *yaml.Node {
	if field == "maxPods" {
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!int", Value: value}
	}
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: value, Style: yaml.DoubleQuotedStyle}
}
