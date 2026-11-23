package engine

import (
	"slices"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestMergeAdditive(t *testing.T) {
	hotplug := [][2]string{{"HotplugVolumes", "DeclarativeHotplugVolumes"}}
	for _, tc := range []struct {
		name           string
		live, upstream []string
		exclusive      [][2]string
		want           []string
	}{
		{name: "the live entries when upstream sets none",
			live: []string{"HostDevices", "Snapshot"}, upstream: []string{},
			want: []string{"HostDevices", "Snapshot"}},
		{name: "upstream's entries when the list is empty",
			live: nil, upstream: []string{"HonorWaitForFirstConsumer"},
			want: []string{"HonorWaitForFirstConsumer"}},
		{name: "the live entries first, then upstream's missing ones",
			live:     []string{"DataVolumeClaimAdoption", "HonorWaitForFirstConsumer"},
			upstream: []string{"HonorWaitForFirstConsumer", "WebhookPvcRendering"},
			want:     []string{"DataVolumeClaimAdoption", "HonorWaitForFirstConsumer", "WebhookPvcRendering"}},
		{name: "an entry removed by hand that upstream does not set stays out",
			live: []string{"DataVolumeClaimAdoption"}, upstream: []string{"HonorWaitForFirstConsumer"},
			want: []string{"DataVolumeClaimAdoption", "HonorWaitForFirstConsumer"}},
		{name: "each entry once",
			live: []string{"Snapshot", "Snapshot", "Sidecar"}, upstream: []string{"Sidecar", "VSOCK", "VSOCK"},
			want: []string{"Snapshot", "Sidecar", "VSOCK"}},
		{name: "upstream's HotplugVolumes left out when the CR has DeclarativeHotplugVolumes",
			live: []string{"DeclarativeHotplugVolumes"}, upstream: []string{"HotplugVolumes"}, exclusive: hotplug,
			want: []string{"DeclarativeHotplugVolumes"}},
		{name: "upstream's DeclarativeHotplugVolumes left out when the CR has HotplugVolumes",
			live: []string{"HotplugVolumes"}, upstream: []string{"DeclarativeHotplugVolumes"}, exclusive: hotplug,
			want: []string{"HotplugVolumes"}},
		{name: "a pair set by hand is the admin's",
			live: []string{"HotplugVolumes", "DeclarativeHotplugVolumes"}, upstream: []string{}, exclusive: hotplug,
			want: []string{"HotplugVolumes", "DeclarativeHotplugVolumes"}},
		{name: "upstream's entry of a pair when the CR has neither",
			live: []string{"Snapshot"}, upstream: []string{"DeclarativeHotplugVolumes"}, exclusive: hotplug,
			want: []string{"Snapshot", "DeclarativeHotplugVolumes"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := mergeAdditive(tc.live, tc.upstream, tc.exclusive); !slices.Equal(got, tc.want) {
				t.Errorf("mergeAdditive(%v, %v) = %v, want %v", tc.live, tc.upstream, got, tc.want)
			}
		})
	}
}

// Upstream's CRs, as published (kubevirt-cr.yaml v1.9.1, cdi-cr.yaml v1.66.1).
const (
	upstreamKubeVirt = `apiVersion: kubevirt.io/v1
kind: KubeVirt
metadata:
  name: kubevirt
  namespace: kubevirt
spec:
  certificateRotateStrategy: {}
  configuration:
    developerConfiguration:
      featureGates: []
    imagePullPolicy: IfNotPresent
  customizeComponents: {}
  imagePullPolicy: IfNotPresent
  workloadUpdateStrategy: {}
`
	upstreamCDI = `apiVersion: cdi.kubevirt.io/v1beta1
kind: CDI
metadata:
  name: cdi
spec:
  config:
    featureGates:
    - HonorWaitForFirstConsumer
  imagePullPolicy: IfNotPresent
`
)

func decodeOne(t *testing.T, doc string) *unstructured.Unstructured {
	t.Helper()
	objs, err := decodeMultiDoc([]byte(doc))
	if err != nil || len(objs) != 1 {
		t.Fatalf("decode: %v (%d objects)", err, len(objs))
	}
	return &objs[0]
}

// withList is obj with the string list at path set to entries, or removed
// when entries is nil.
func withList(t *testing.T, obj *unstructured.Unstructured, entries []string, path ...string) *unstructured.Unstructured {
	t.Helper()
	obj = obj.DeepCopy()
	if entries == nil {
		unstructured.RemoveNestedField(obj.Object, path...)
		return obj
	}
	if err := unstructured.SetNestedStringSlice(obj.Object, entries, path...); err != nil {
		t.Fatal(err)
	}
	return obj
}

func TestKeepLiveEntries(t *testing.T) {
	kvGates := []string{"spec", "configuration", "developerConfiguration", "featureGates"}
	cdiGates := []string{"spec", "config", "featureGates"}
	kubeVirt, cdi := decodeOne(t, upstreamKubeVirt), decodeOne(t, upstreamCDI)

	for _, tc := range []struct {
		name      string
		obj, live *unstructured.Unstructured
		path      []string
		want      []string // nil: the field absent
	}{
		{name: "fresh install: upstream's list as it is",
			obj: kubeVirt, live: nil, path: kvGates, want: []string{}},
		{name: "KubeVirt: the gates set on the cluster",
			obj: kubeVirt, live: withList(t, kubeVirt, []string{"HostDevices", "Snapshot"}, kvGates...),
			path: kvGates, want: []string{"HostDevices", "Snapshot"}},
		{name: "CDI: the gates set on the cluster, then upstream's",
			obj: cdi, live: withList(t, cdi, []string{"WebhookPvcRendering", "DataVolumeClaimAdoption"}, cdiGates...),
			path: cdiGates, want: []string{"WebhookPvcRendering", "DataVolumeClaimAdoption", "HonorWaitForFirstConsumer"}},
		{name: "the list removed from the CR: upstream's",
			obj: cdi, live: withList(t, cdi, nil, cdiGates...),
			path: cdiGates, want: []string{"HonorWaitForFirstConsumer"}},
		// The operator owns the list after its first apply: leaving it out of
		// the apply would make the API server delete it.
		{name: "upstream stops setting the list: the cluster's stays",
			obj: withList(t, kubeVirt, nil, kvGates...), live: withList(t, kubeVirt, []string{"Sidecar"}, kvGates...),
			path: kvGates, want: []string{"Sidecar"}},
		{name: "set on neither side: left out",
			obj: withList(t, kubeVirt, nil, kvGates...), live: withList(t, kubeVirt, nil, kvGates...),
			path: kvGates, want: nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			obj := tc.obj.DeepCopy()
			if err := keepLiveEntries(obj, tc.live); err != nil {
				t.Fatal(err)
			}
			got, found, err := unstructured.NestedStringSlice(obj.Object, tc.path...)
			if err != nil {
				t.Fatal(err)
			}
			if tc.want == nil {
				if found {
					t.Errorf("list = %v, want it left out", got)
				}
				return
			}
			if !found || !slices.Equal(got, tc.want) {
				t.Errorf("list = %v (set: %v), want %v", got, found, tc.want)
			}
		})
	}
}

// keepLiveEntries touches only the lists it knows, and nothing else of the
// object: what upstream does not set stays out of the apply.
func TestKeepLiveEntries_OnlyTheKnownLists(t *testing.T) {
	kubeVirt := decodeOne(t, upstreamKubeVirt)
	live := kubeVirt.DeepCopy()
	if err := unstructured.SetNestedField(live.Object, int64(1), "spec", "infra", "replicas"); err != nil {
		t.Fatal(err)
	}
	obj := kubeVirt.DeepCopy()
	if err := keepLiveEntries(obj, live); err != nil {
		t.Fatal(err)
	}
	if _, found, _ := unstructured.NestedFieldNoCopy(obj.Object, "spec", "infra"); found {
		t.Error("spec.infra copied from the live object into the apply")
	}

	// Same kind, another group: not KubeVirt's list.
	other := withList(t, kubeVirt, []string{}, "spec", "configuration", "developerConfiguration", "featureGates")
	other.SetAPIVersion("example.com/v1")
	otherLive := withList(t, other, []string{"Snapshot"}, "spec", "configuration", "developerConfiguration", "featureGates")
	if err := keepLiveEntries(other, otherLive); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := unstructured.NestedStringSlice(other.Object, "spec", "configuration", "developerConfiguration", "featureGates"); len(got) != 0 {
		t.Errorf("example.com/v1 KubeVirt: list = %v, want upstream's", got)
	}
}
