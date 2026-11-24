package nodesettings

import (
	"encoding/json"
	"fmt"
	"time"
)

// Record is what the operator keeps on kube-system/kubelet-config, in
// ConfigMapAnnotation: the settings the ConfigMap carries, what each kubelet
// field k4all owns had before k4all first wrote it (put back when the field
// is cleared), the settings of older hashes some node still has (to tell
// what a node must do to move on), and when the last node finished its step.
//
// The k4all-upgrade agent reads hash, hugepages and iommu: it applies a
// target only when hash names it.
type Record struct {
	Hash string `json:"hash"`
	Settings
	Prior    Prior               `json:"prior,omitempty"`
	History  map[string]Settings `json:"history,omitempty"`
	LastDone *time.Time          `json:"lastDone,omitempty"`
}

// Encode is r as the annotation's JSON.
func (r Record) Encode() string {
	data, err := json.Marshal(r)
	if err != nil { // strings, ints, maps and a time: never
		panic(err)
	}
	return string(data)
}

// ParseRecord reads the annotation.
func ParseRecord(text string) (*Record, error) {
	r := &Record{}
	if err := json.Unmarshal([]byte(text), r); err != nil {
		return nil, fmt.Errorf("read %s: %w", ConfigMapAnnotation, err)
	}
	return r, nil
}

// known is every Settings the record can name by hash.
func (r *Record) known() map[string]Settings {
	out := map[string]Settings{}
	if r == nil {
		return out
	}
	for h, s := range r.History {
		out[h] = s
	}
	if r.Hash != "" {
		out[r.Hash] = r.Settings
	}
	return out
}
