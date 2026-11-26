package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	k4allv1alpha1 "github.com/gpillon/k4all-operator/api/v1alpha1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"
)

// The ClusterConfig manifests these tests send: the v2.7.0 spec's example
// and the file k4all v2.6.x installs.
var (
	v270Example = filepath.Join("testdata", "clusterconfig", "v2.7.0-example.yaml")
	v26Default  = filepath.Join("testdata", "clusterconfig", "v2.6-default.yaml")
	// v26Default as the operator's Go types write it back.
	v26DefaultGoUpdate = filepath.Join("testdata", "clusterconfig", "v2.6-default-go-update.yaml")
)

// clusterConfigFile reads a ClusterConfig manifest and renames it, so that
// tests sharing the API server do not collide.
func clusterConfigFile(t *testing.T, path, name string) *unstructured.Unstructured {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	u := &unstructured.Unstructured{}
	if err := yaml.Unmarshal(data, &u.Object); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	u.SetName(name)
	return u
}

// createClusterConfig creates obj and deletes it when the test ends.
func createClusterConfig(t *testing.T, c client.Client, obj client.Object) {
	t.Helper()
	if err := c.Create(context.Background(), obj); err != nil {
		t.Fatalf("create ClusterConfig %s: %v", obj.GetName(), err)
	}
	t.Cleanup(func() { _ = c.Delete(context.Background(), obj) })
}

// readClusterConfig reads ClusterConfig name back as the API server stores it.
func readClusterConfig(t *testing.T, c client.Client, name string) *unstructured.Unstructured {
	t.Helper()
	got := &unstructured.Unstructured{}
	got.SetAPIVersion("k4all.magesgate.com/v1alpha1")
	got.SetKind("ClusterConfig")
	if err := c.Get(context.Background(), client.ObjectKey{Name: name}, got); err != nil {
		t.Fatal(err)
	}
	return got
}

// viaJSON returns v as encoding/json decodes it, so that values read from YAML
// (float64 numbers) and from the API server (int64) compare equal.
func viaJSON(t *testing.T, v interface{}) interface{} {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var out interface{}
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func asYAML(v interface{}) string {
	b, _ := yaml.Marshal(v)
	return string(b)
}

// The spec's ClusterConfig, every v2.7.0 field set, is stored as sent: no
// field pruned, none added.
func TestClusterConfigAPI_V270ExampleReadsBackIdentical(t *testing.T) {
	c := envtestClient(t)
	sent := clusterConfigFile(t, v270Example, "api-v270-example")
	want := viaJSON(t, sent.Object["spec"])
	createClusterConfig(t, c, sent.DeepCopy())

	got := viaJSON(t, readClusterConfig(t, c, sent.GetName()).Object["spec"])
	if !reflect.DeepEqual(got, want) {
		t.Errorf("spec read back:\n%s\nwant the spec sent:\n%s", asYAML(got), asYAML(want))
	}
}

// The spec's example reaches Go code in the fields the spec names.
func TestClusterConfigAPI_V270ExampleDecodes(t *testing.T) {
	c := envtestClient(t)
	sent := clusterConfigFile(t, v270Example, "api-v270-decodes")
	createClusterConfig(t, c, sent)

	got := &k4allv1alpha1.ClusterConfig{}
	if err := c.Get(context.Background(), client.ObjectKey{Name: sent.GetName()}, got); err != nil {
		t.Fatal(err)
	}
	want := k4allv1alpha1.ClusterConfigSpec{
		Networking: k4allv1alpha1.NetworkingConfig{CNI: k4allv1alpha1.CNIConfig{Type: "cilium"}},
		Features: k4allv1alpha1.FeaturesConfig{
			Virt: k4allv1alpha1.VirtConfig{
				Enabled:   true,
				Emulation: "auto",
				KubeVirt: &k4allv1alpha1.KubeVirtSettings{
					FeatureGates:         []string{"Sidecar"},
					DisabledFeatureGates: []string{},
					VMStateStorageClass:  "ceph-rbd",
				},
				Hangar: &k4allv1alpha1.HangarSettings{NodeAgent: ptr.To(true)},
				Metadata: &k4allv1alpha1.MetadataSettings{
					Enabled: true,
					Scope:   k4allv1alpha1.MetadataScopeLabelled,
					Tokens: &k4allv1alpha1.MetadataTokens{
						Audiences:         []string{"k4all-metadata"},
						ExpirationSeconds: ptr.To[int64](3600),
					},
					VendorDataConfigMap: "k4all-vendor-data",
				},
			},
			Rook: &k4allv1alpha1.RookConfig{
				Storage:      &k4allv1alpha1.RookStorage{DeviceFilter: "^sdb$"},
				SmallCluster: k4allv1alpha1.RookSmallClusterRefuse,
			},
			OVSBridge: k4allv1alpha1.OVSBridgeConfig{Enabled: true},
		},
		Nodes: &k4allv1alpha1.NodesConfig{
			Kubelet: &k4allv1alpha1.KubeletSettings{
				MaxPods:               ptr.To[int32](250),
				CPUManagerPolicy:      "static",
				ReservedSystemCPUs:    "0-1",
				TopologyManagerPolicy: "single-numa-node",
				TopologyManagerScope:  "container",
			},
			Hugepages: map[string]int32{"1Gi": 4, "2Mi": 0},
			IOMMU:     ptr.To(true),
			Rollout:   &k4allv1alpha1.NodeRollout{Paused: false},
		},
		Cluster: k4allv1alpha1.ControlPlaneConfig{PodNetwork: "10.100.0.1/18"},
	}
	if !equality.Semantic.DeepEqual(got.Spec, want) {
		t.Errorf("spec decoded:\n%s\nwant:\n%s", asYAML(got.Spec), asYAML(want))
	}
}

// An upgrade from v2.6.x changes nothing until someone sets a v2.7.0 field:
// the ClusterConfig v2.6.x installed is stored as it is, and the API server
// defaults none of the new fields, not even when the operator writes it back
// with its Go types (the migrations do).
func TestClusterConfigAPI_V26ConfigGrowsNoNewField(t *testing.T) {
	c := envtestClient(t)
	ctx := context.Background()
	sent := clusterConfigFile(t, v26Default, "api-v26-default")
	want := viaJSON(t, sent.Object["spec"])
	createClusterConfig(t, c, sent.DeepCopy())

	got := viaJSON(t, readClusterConfig(t, c, sent.GetName()).Object["spec"])
	if !reflect.DeepEqual(got, want) {
		t.Errorf("v2.6.x spec read back:\n%s\nwant the spec sent:\n%s", asYAML(got), asYAML(want))
	}

	typed := &k4allv1alpha1.ClusterConfig{}
	if err := c.Get(ctx, client.ObjectKey{Name: sent.GetName()}, typed); err != nil {
		t.Fatal(err)
	}
	if err := c.Update(ctx, typed); err != nil {
		t.Fatal(err)
	}
	// The Go types drop the zero values v2.6.x wrote (as v0.5.x did), and add
	// no key of their own: not even features.rook: {}.
	updated := asJSON(t, readClusterConfig(t, c, sent.GetName()).Object["spec"])
	wantUpdated := asJSON(t, clusterConfigFile(t, v26DefaultGoUpdate, "").Object["spec"])
	if updated != wantUpdated {
		t.Errorf("v2.6.x spec after a Go client update:\n%s\nwant:\n%s", updated, wantUpdated)
	}
}

// asJSON is v as JSON with sorted keys, numbers as encoding/json decodes them.
func asJSON(t *testing.T, v interface{}) string {
	t.Helper()
	b, err := json.Marshal(viaJSON(t, v))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// minimalClusterConfig is a ClusterConfig with only the required fields, for
// tests that set one field on it.
func minimalClusterConfig(name string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "k4all.magesgate.com/v1alpha1",
		"kind":       "ClusterConfig",
		"metadata":   map[string]interface{}{"name": name},
		"spec": map[string]interface{}{
			"networking": map[string]interface{}{"cni": map[string]interface{}{"type": "calico"}},
			"features":   map[string]interface{}{},
			"cluster":    map[string]interface{}{},
		},
	}}
}

// The node-settings rollout reports in status.nodes, one entry per node.
func TestClusterConfigAPI_StatusNodes(t *testing.T) {
	c := envtestClient(t)
	ctx := context.Background()
	cc := minimalClusterConfig("api-status-nodes")
	createClusterConfig(t, c, cc)

	nodes := func(perNode ...interface{}) map[string]interface{} {
		return map[string]interface{}{
			"settingsHash": "5f2c",
			"rollout": map[string]interface{}{
				"phase":       "Rolling",
				"currentNode": "node-b",
				"message":     "draining node-b",
			},
			"perNode": perNode,
		}
	}
	nodeA := map[string]interface{}{"name": "node-a", "appliedHash": "5f2c", "phase": "Applied"}
	nodeB := map[string]interface{}{"name": "node-b", "appliedHash": "91ae", "phase": "Draining", "message": "2 pods left"}

	t.Run("read back in the Go types", func(t *testing.T) {
		live := readClusterConfig(t, c, cc.GetName())
		live.Object["status"] = map[string]interface{}{"nodes": nodes(nodeA, nodeB)}
		if err := c.Status().Update(ctx, live); err != nil {
			t.Fatal(err)
		}
		got := &k4allv1alpha1.ClusterConfig{}
		if err := c.Get(ctx, client.ObjectKey{Name: cc.GetName()}, got); err != nil {
			t.Fatal(err)
		}
		want := &k4allv1alpha1.NodesStatus{
			SettingsHash: "5f2c",
			Rollout: k4allv1alpha1.NodeRolloutStatus{
				Phase:       k4allv1alpha1.NodeRolloutRolling,
				CurrentNode: "node-b",
				Message:     "draining node-b",
			},
			PerNode: []k4allv1alpha1.NodeSettingsStatus{
				{Name: "node-a", AppliedHash: "5f2c", Phase: "Applied"},
				{Name: "node-b", AppliedHash: "91ae", Phase: "Draining", Message: "2 pods left"},
			},
		}
		if !equality.Semantic.DeepEqual(got.Status.Nodes, want) {
			t.Errorf("status.nodes decoded:\n%s\nwant:\n%s", asYAML(got.Status.Nodes), asYAML(want))
		}
	})

	t.Run("one entry per node name", func(t *testing.T) {
		live := readClusterConfig(t, c, cc.GetName())
		live.Object["status"] = map[string]interface{}{"nodes": nodes(nodeA, nodeA)}
		err := c.Status().Update(ctx, live)
		if err == nil || !strings.Contains(err.Error(), "Duplicate value") {
			t.Errorf("two status.nodes.perNode entries for node-a: err = %v, want a duplicate refused", err)
		}
	})
}

// A field path in the ClusterConfig, and a value for it.
type apiField struct {
	path  []string
	value interface{}
}

var (
	kubevirtPath  = []string{"spec", "features", "virt", "kubevirt"}
	kubeletPath   = []string{"spec", "nodes", "kubelet"}
	hugepagesPath = []string{"spec", "nodes", "hugepages"}
	metadataPath  = []string{"spec", "features", "virt", "metadata"}
	rookPath      = []string{"spec", "features", "rook"}
)

// The API server refuses the v2.7.0 settings the operator could not apply,
// and accepts the valid counterpart of each.
func TestClusterConfigAPI_Validation(t *testing.T) {
	c := envtestClient(t)
	cases := []struct {
		name  string
		field apiField
		// Part of the API server's refusal; empty when the value is valid.
		refused string
	}{
		{
			name: "a gate both on and off",
			field: apiField{kubevirtPath, map[string]interface{}{
				"featureGates": []interface{}{"Sidecar", "VSOCK"}, "disabledFeatureGates": []interface{}{"VSOCK"},
			}},
			refused: "a gate cannot be both on and off",
		},
		{
			name: "different gates on and off",
			field: apiField{kubevirtPath, map[string]interface{}{
				"featureGates": []interface{}{"Sidecar", "VSOCK"}, "disabledFeatureGates": []interface{}{"HostDevices"},
			}},
		},
		{
			name:  "gates only off",
			field: apiField{kubevirtPath, map[string]interface{}{"disabledFeatureGates": []interface{}{"VSOCK"}}},
		},
		{
			name: "HotplugVolumes and DeclarativeHotplugVolumes both on",
			field: apiField{kubevirtPath, map[string]interface{}{
				"featureGates": []interface{}{"HotplugVolumes", "Sidecar", "DeclarativeHotplugVolumes"},
			}},
			refused: "HotplugVolumes turns DeclarativeHotplugVolumes off: set one",
		},
		{
			name:  "HotplugVolumes on alone",
			field: apiField{kubevirtPath, map[string]interface{}{"featureGates": []interface{}{"HotplugVolumes"}}},
		},
		{
			name: "DeclarativeHotplugVolumes on, HotplugVolumes off",
			field: apiField{kubevirtPath, map[string]interface{}{
				"featureGates": []interface{}{"DeclarativeHotplugVolumes"}, "disabledFeatureGates": []interface{}{"HotplugVolumes"},
			}},
		},
		{
			name:    "the static CPU manager without reserved CPUs",
			field:   apiField{kubeletPath, map[string]interface{}{"cpuManagerPolicy": "static"}},
			refused: "the static CPU manager needs reservedSystemCPUs",
		},
		{
			name: "the static CPU manager with empty reserved CPUs",
			field: apiField{kubeletPath, map[string]interface{}{
				"cpuManagerPolicy": "static", "reservedSystemCPUs": "",
			}},
			refused: "the static CPU manager needs reservedSystemCPUs",
		},
		{
			name: "the static CPU manager with reserved CPUs",
			field: apiField{kubeletPath, map[string]interface{}{
				"cpuManagerPolicy": "static", "reservedSystemCPUs": "0-1",
			}},
		},
		{
			name:  "the none CPU manager without reserved CPUs",
			field: apiField{kubeletPath, map[string]interface{}{"cpuManagerPolicy": "none"}},
		},
		{
			name:  "reserved CPUs without a CPU manager policy",
			field: apiField{kubeletPath, map[string]interface{}{"reservedSystemCPUs": "0"}},
		},
		{
			name:    "hugepages of a size spelt as the kernel does",
			field:   apiField{hugepagesPath, map[string]interface{}{"1G": int64(4)}},
			refused: "hugepages page sizes are 1Gi and 2Mi",
		},
		{
			name:    "hugepages of a size x86 does not have",
			field:   apiField{hugepagesPath, map[string]interface{}{"2Mi": int64(512), "4Mi": int64(1)}},
			refused: "hugepages page sizes are 1Gi and 2Mi",
		},
		{
			name:    "a negative hugepage count",
			field:   apiField{hugepagesPath, map[string]interface{}{"2Mi": int64(-1)}},
			refused: "hugepage counts are 0 or more",
		},
		{
			name:  "hugepages of both sizes",
			field: apiField{hugepagesPath, map[string]interface{}{"1Gi": int64(2), "2Mi": int64(512)}},
		},
		{
			name:    "metadata scope spelt Labeled",
			field:   apiField{metadataPath, map[string]interface{}{"enabled": true, "scope": "Labeled"}},
			refused: `Unsupported value: "Labeled"`,
		},
		{
			name:  "metadata scope All",
			field: apiField{metadataPath, map[string]interface{}{"enabled": true, "scope": "All"}},
		},
		{
			name:    "rook smallCluster in lower case",
			field:   apiField{rookPath, map[string]interface{}{"enabled": true, "smallCluster": "allow"}},
			refused: `Unsupported value: "allow"`,
		},
		{
			name:  "rook smallCluster Allow",
			field: apiField{rookPath, map[string]interface{}{"enabled": true, "smallCluster": "Allow"}},
		},
		{
			name: "CPU manager policy capitalised",
			field: apiField{kubeletPath, map[string]interface{}{
				"cpuManagerPolicy": "Static", "reservedSystemCPUs": "0",
			}},
			refused: `Unsupported value: "Static"`,
		},
		{
			name:    "a topology manager policy kubelet does not have",
			field:   apiField{kubeletPath, map[string]interface{}{"topologyManagerPolicy": "numa"}},
			refused: `Unsupported value: "numa"`,
		},
		{
			name:    "a topology manager scope kubelet does not have",
			field:   apiField{kubeletPath, map[string]interface{}{"topologyManagerScope": "node"}},
			refused: `Unsupported value: "node"`,
		},
		{
			name: "topology manager best-effort for the pod",
			field: apiField{kubeletPath, map[string]interface{}{
				"topologyManagerPolicy": "best-effort", "topologyManagerScope": "pod",
			}},
		},
		{
			name: "metadata tokens shorter than kubelet's minimum",
			field: apiField{metadataPath, map[string]interface{}{
				"tokens": map[string]interface{}{"expirationSeconds": int64(599)},
			}},
			refused: "should be greater than or equal to 600",
		},
		{
			name: "metadata tokens of kubelet's minimum",
			field: apiField{metadataPath, map[string]interface{}{
				"tokens": map[string]interface{}{"expirationSeconds": int64(600)},
			}},
		},
		{
			name:    "maxPods below 10",
			field:   apiField{kubeletPath, map[string]interface{}{"maxPods": int64(9)}},
			refused: "should be greater than or equal to 10",
		},
		{
			name:    "maxPods above 1000",
			field:   apiField{kubeletPath, map[string]interface{}{"maxPods": int64(1001)}},
			refused: "should be less than or equal to 1000",
		},
		{
			name:  "maxPods 10",
			field: apiField{kubeletPath, map[string]interface{}{"maxPods": int64(10)}},
		},
		{
			name:  "maxPods 1000",
			field: apiField{kubeletPath, map[string]interface{}{"maxPods": int64(1000)}},
		},
		{
			name:    "more than 64 gates on",
			field:   apiField{kubevirtPath, map[string]interface{}{"featureGates": gateNames(65)}},
			refused: "must have at most 64 items",
		},
		{
			name:  "64 gates on",
			field: apiField{kubevirtPath, map[string]interface{}{"featureGates": gateNames(64)}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			obj := minimalClusterConfig("")
			obj.SetGenerateName("api-validation-")
			if err := unstructured.SetNestedField(obj.Object, tc.field.value, tc.field.path...); err != nil {
				t.Fatal(err)
			}
			err := c.Create(context.Background(), obj)
			if err == nil {
				t.Cleanup(func() { _ = c.Delete(context.Background(), obj) })
			}
			switch {
			case tc.refused == "" && err != nil:
				t.Errorf("%s = %v: refused (%v), want it accepted", strings.Join(tc.field.path, "."), tc.field.value, err)
			case tc.refused != "" && (err == nil || !strings.Contains(err.Error(), tc.refused)):
				t.Errorf("%s = %v: err = %v, want refused with %q", strings.Join(tc.field.path, "."), tc.field.value, err, tc.refused)
			}
		})
	}
}

// gateNames returns n distinct feature gate names.
func gateNames(n int) []interface{} {
	gates := make([]interface{}, n)
	for i := range gates {
		gates[i] = fmt.Sprintf("Gate%d", i)
	}
	return gates
}
