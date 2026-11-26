package hooks

import (
	"errors"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	k4allv1alpha1 "github.com/gpillon/k4all-operator/api/v1alpha1"
)

// Rook's pods tolerate no taint by default: a node whose taints keep pods
// away cannot hold a mon or an OSD. A node that is only not ready, or
// cordoned for an upgrade, still counts: the plan is for the cluster's
// lifetime, not for this minute.
func TestCephNodes(t *testing.T) {
	node := func(taints ...corev1.Taint) corev1.Node {
		return corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n"}, Spec: corev1.NodeSpec{Taints: taints}}
	}
	for _, tc := range []struct {
		name  string
		nodes []corev1.Node
		want  int
	}{
		{name: "untainted control plane and workers", nodes: []corev1.Node{node(), node(), node()}, want: 3},
		{name: "tainted control plane", want: 2, nodes: []corev1.Node{
			node(corev1.Taint{Key: "node-role.kubernetes.io/control-plane", Effect: corev1.TaintEffectNoSchedule}),
			node(), node(),
		}},
		{name: "NoExecute taint", want: 1, nodes: []corev1.Node{
			node(corev1.Taint{Key: "dedicated", Value: "gpu", Effect: corev1.TaintEffectNoExecute}), node(),
		}},
		{name: "PreferNoSchedule taint", want: 1, nodes: []corev1.Node{
			node(corev1.Taint{Key: "dedicated", Effect: corev1.TaintEffectPreferNoSchedule}),
		}},
		{name: "not ready, unreachable or cordoned", want: 3, nodes: []corev1.Node{
			node(corev1.Taint{Key: "node.kubernetes.io/not-ready", Effect: corev1.TaintEffectNoExecute}),
			node(corev1.Taint{Key: "node.kubernetes.io/unreachable", Effect: corev1.TaintEffectNoSchedule}),
			node(corev1.Taint{Key: "node.kubernetes.io/unschedulable", Effect: corev1.TaintEffectNoSchedule}),
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := cephNodes(tc.nodes); got != tc.want {
				t.Errorf("cephNodes = %d, want %d", got, tc.want)
			}
		})
	}
}

// The CephCluster and vm-pool defaults k4all creates, by node count. Rook
// places one mon per node (allowMultiplePerNode false) and vm-pool one
// replica per host, so the counts never exceed the nodes.
func TestPlanRook(t *testing.T) {
	refuse := RookSettings{SmallCluster: k4allv1alpha1.RookSmallClusterRefuse}
	allow := RookSettings{SmallCluster: k4allv1alpha1.RookSmallClusterAllow}
	for _, tc := range []struct {
		name     string
		nodes    int
		settings RookSettings
		want     rookPlan
		warns    bool
		refused  bool
	}{
		{name: "1 node, refuse", nodes: 1, settings: refuse, refused: true},
		{name: "2 nodes, refuse", nodes: 2, settings: refuse, refused: true},
		{name: "3 nodes", nodes: 3, settings: refuse, want: rookPlan{Mons: 3, Mgrs: 2, PoolSize: 3}},
		{name: "4 nodes: an even mon count adds no quorum", nodes: 4, settings: refuse, want: rookPlan{Mons: 3, Mgrs: 2, PoolSize: 3}},
		{name: "5 nodes", nodes: 5, settings: refuse, want: rookPlan{Mons: 5, Mgrs: 2, PoolSize: 3}},
		{name: "9 nodes: 5 mons at most", nodes: 9, settings: refuse, want: rookPlan{Mons: 5, Mgrs: 2, PoolSize: 3}},
		{name: "1 node, allowed", nodes: 1, settings: allow, want: rookPlan{Mons: 1, Mgrs: 1, PoolSize: 1}, warns: true},
		{name: "2 nodes, allowed: 2 mons would need both for quorum", nodes: 2, settings: allow,
			want: rookPlan{Mons: 1, Mgrs: 2, PoolSize: 2}, warns: true},
		{name: "3 nodes, allowed", nodes: 3, settings: allow, want: rookPlan{Mons: 3, Mgrs: 2, PoolSize: 3}},
		{name: "pool size override", nodes: 5, settings: RookSettings{SmallCluster: k4allv1alpha1.RookSmallClusterRefuse, PoolSize: 2},
			want: rookPlan{Mons: 5, Mgrs: 2, PoolSize: 2}, warns: true},
		{name: "pool size above the node count", nodes: 3, settings: RookSettings{SmallCluster: k4allv1alpha1.RookSmallClusterRefuse, PoolSize: 4},
			refused: true},
		{name: "no node", nodes: 0, settings: allow, refused: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := planRook(tc.nodes, tc.settings)
			if tc.refused {
				var refusal rookRefusal
				if !errors.As(err, &refusal) {
					t.Fatalf("planRook = %+v, %v; want a refusal", got, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("planRook: %v", err)
			}
			if (got.Warning != "") != tc.warns {
				t.Errorf("warning %q, want one: %t", got.Warning, tc.warns)
			}
			got.Warning = ""
			if got != tc.want {
				t.Errorf("planRook = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// The ClusterConfig's creation inputs: storage.deviceFilter and smallCluster
// (empty is Refuse, D5). Without features.rook, or without the fields, the
// defaults: every blank device, nothing below 3 nodes.
func TestRookSettingsFrom(t *testing.T) {
	rook := func(r *k4allv1alpha1.RookConfig) k4allv1alpha1.ClusterConfigSpec {
		return k4allv1alpha1.ClusterConfigSpec{Features: k4allv1alpha1.FeaturesConfig{Rook: r}}
	}
	refuse := k4allv1alpha1.RookSmallClusterRefuse
	for _, tc := range []struct {
		name   string
		config k4allv1alpha1.ClusterConfigSpec
		want   RookSettings
	}{
		{"no rook field", rook(nil), RookSettings{SmallCluster: refuse}},
		{"enabled only", rook(&k4allv1alpha1.RookConfig{Enabled: true}), RookSettings{SmallCluster: refuse}},
		{"storage without a filter", rook(&k4allv1alpha1.RookConfig{Enabled: true, Storage: &k4allv1alpha1.RookStorage{}}),
			RookSettings{SmallCluster: refuse}},
		{"device filter", rook(&k4allv1alpha1.RookConfig{Enabled: true,
			Storage: &k4allv1alpha1.RookStorage{DeviceFilter: "^nvme[01]n1$"}}),
			RookSettings{DeviceFilter: "^nvme[01]n1$", SmallCluster: refuse}},
		{"Refuse", rook(&k4allv1alpha1.RookConfig{Enabled: true, SmallCluster: refuse}), RookSettings{SmallCluster: refuse}},
		{"Allow", rook(&k4allv1alpha1.RookConfig{Enabled: true, SmallCluster: k4allv1alpha1.RookSmallClusterAllow}),
			RookSettings{SmallCluster: k4allv1alpha1.RookSmallClusterAllow}},
	} {
		if got := rookSettingsFrom(tc.config); got != tc.want {
			t.Errorf("%s: %+v, want %+v", tc.name, got, tc.want)
		}
	}
}

// The CephCluster k4all creates: the lab's (ai/hangar/lab/06-rook), with the
// counts of the plan, only aes256k cephx keys (CVE-2025-30156), and no mgr
// module list: "rook", the lab's old entry, is disabled by Rook 1.21 on
// Ceph 20 anyway.
func TestCephClusterObject(t *testing.T) {
	plan := rookPlan{Mons: 3, Mgrs: 2, PoolSize: 3}
	cluster := cephClusterObject(plan, RookSettings{}, "quay.io/ceph/ceph:v20.2.4")

	if cluster.GetNamespace() != "rook-ceph" || cluster.GetName() != "rook-ceph" {
		t.Errorf("CephCluster %s/%s, want rook-ceph/rook-ceph", cluster.GetNamespace(), cluster.GetName())
	}
	for path, want := range map[string]interface{}{
		"spec.cephVersion.image":                     "quay.io/ceph/ceph:v20.2.4",
		"spec.dataDirHostPath":                       "/var/lib/rook",
		"spec.mon.count":                             int64(3),
		"spec.mon.allowMultiplePerNode":              false,
		"spec.mgr.count":                             int64(2),
		"spec.storage.useAllNodes":                   true,
		"spec.storage.useAllDevices":                 true,
		"spec.disruptionManagement.managePodBudgets": true,
	} {
		if got := fieldAt(t, cluster.Object, path); got != want {
			t.Errorf("%s = %#v, want %#v", path, got, want)
		}
	}
	ciphers, _, _ := unstructured.NestedStringSlice(cluster.Object, "spec", "security", "cephx", "allowedCiphers")
	if len(ciphers) != 1 || ciphers[0] != "aes256k" {
		t.Errorf("allowedCiphers %v, want [aes256k]", ciphers)
	}
	if _, found, _ := unstructured.NestedFieldNoCopy(cluster.Object, "spec", "mgr", "modules"); found {
		t.Error("spec.mgr.modules is set, want no module list")
	}
	if _, found, _ := unstructured.NestedFieldNoCopy(cluster.Object, "spec", "storage", "deviceFilter"); found {
		t.Error("deviceFilter set without a filter in the settings")
	}

	filtered := cephClusterObject(plan, RookSettings{DeviceFilter: "^sdb$"}, "quay.io/ceph/ceph:v20.2.4")
	for path, want := range map[string]interface{}{
		"spec.storage.useAllDevices": false,
		"spec.storage.deviceFilter":  "^sdb$",
	} {
		if got := fieldAt(t, filtered.Object, path); got != want {
			t.Errorf("with a device filter: %s = %#v, want %#v", path, got, want)
		}
	}
}

// fieldAt reads a field of an unstructured object by its dotted path.
func fieldAt(t *testing.T, obj map[string]interface{}, path string) interface{} {
	t.Helper()
	v, found, err := unstructured.NestedFieldNoCopy(obj, strings.Split(path, ".")...)
	if err != nil || !found {
		t.Fatalf("%s: found %t, %v", path, found, err)
	}
	return v
}
