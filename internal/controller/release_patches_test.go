package controller

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"testing"

	"github.com/go-logr/logr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	k4allv1alpha1 "github.com/gpillon/k4all-operator/api/v1alpha1"
	"github.com/gpillon/k4all-operator/internal/controller/hooks"
	"github.com/gpillon/k4all-operator/internal/engine"
)

// The patches of the k4all release (v2.6.7 to v2.7.0) folded into the
// objects of their manifest component before its server-side apply
// (gpillon/k4all-operator#44): one write per object and per apply. Before,
// the apply put back upstream's value of each patched field and the patch
// changed it again, two pod-template changes and two rollouts at every
// apply: virt-operator reached revision 14 after ten applies.

// releasePatchedWorkload is a Deployment or DaemonSet of upstream's manifest
// (testdata/release-patches, as published) with the release's patch of it.
type releasePatchedWorkload struct {
	component, file string
	gvk             schema.GroupVersionKind
	key             types.NamespacedName
	patch           string
	// patched tells what the patch changed is there.
	patched func(t *testing.T, obj *unstructured.Unstructured)
}

var (
	deploymentGVK = schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "Deployment"}
	daemonSetGVK  = schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "DaemonSet"}
)

var releasePatchedWorkloads = []releasePatchedWorkload{
	{
		component: "kubevirt", file: "virt-operator.yaml", gvk: deploymentGVK,
		key:   types.NamespacedName{Namespace: "kubevirt", Name: "virt-operator"},
		patch: `{"spec":{"template":{"spec":{"affinity":{"nodeAffinity":{"requiredDuringSchedulingIgnoredDuringExecution":null,"preferredDuringSchedulingIgnoredDuringExecution":[{"weight":100,"preference":{"matchExpressions":[{"key":"node-role.kubernetes.io/control-plane","operator":"Exists"}]}}]}}}}}}`,
		patched: func(t *testing.T, obj *unstructured.Unstructured) {
			affinity := []string{"spec", "template", "spec", "affinity", "nodeAffinity"}
			if _, found, _ := unstructured.NestedFieldNoCopy(obj.Object, append(affinity, "requiredDuringSchedulingIgnoredDuringExecution")...); found {
				t.Error("virt-operator still requires a control plane")
			}
			preferred, _, _ := unstructured.NestedSlice(obj.Object, append(affinity, "preferredDuringSchedulingIgnoredDuringExecution")...)
			key, _, _ := unstructured.NestedString(firstMatchExpression(preferred), "key")
			if len(preferred) != 1 || key != "node-role.kubernetes.io/control-plane" {
				t.Errorf("virt-operator prefers %v, want the control planes", preferred)
			}
			// The strategic merge keeps what the patch does not name.
			if _, found, _ := unstructured.NestedFieldNoCopy(obj.Object, "spec", "template", "spec", "affinity", "podAntiAffinity"); !found {
				t.Error("virt-operator lost upstream's podAntiAffinity")
			}
		},
	},
	{
		component: "metrics-server", file: "metrics-server.yaml", gvk: deploymentGVK,
		key:   types.NamespacedName{Namespace: "kube-system", Name: "metrics-server"},
		patch: `{"spec":{"template":{"spec":{"containers":[{"name":"metrics-server","args":["--cert-dir=/tmp","--secure-port=10250","--kubelet-preferred-address-types=InternalIP,ExternalIP,Hostname","--kubelet-use-node-status-port","--metric-resolution=15s","--kubelet-insecure-tls"]}]}}}}`,
		patched: func(t *testing.T, obj *unstructured.Unstructured) {
			c := container(t, obj, "containers", "metrics-server")
			if args, _, _ := unstructured.NestedStringSlice(c, "args"); !slices.Contains(args, "--kubelet-insecure-tls") {
				t.Errorf("metrics-server args %v, want --kubelet-insecure-tls", args)
			}
			if image, _, _ := unstructured.NestedString(c, "image"); image != "registry.k8s.io/metrics-server/metrics-server:v0.9.1" {
				t.Errorf("metrics-server image %q, want upstream's", image)
			}
		},
	},
	{
		component: "multus-cni", file: "kube-multus-ds.yaml", gvk: daemonSetGVK,
		key:   types.NamespacedName{Namespace: "kube-system", Name: "kube-multus-ds"},
		patch: `{"spec":{"template":{"spec":{"initContainers":[{"name":"install-multus-binary","image":"ghcr.io/k8snetworkplumbingwg/multus-cni:v4.3.2"}],"containers":[{"name":"kube-multus","image":"ghcr.io/k8snetworkplumbingwg/multus-cni:v4.3.2"}]}}}}`,
		patched: func(t *testing.T, obj *unstructured.Unstructured) {
			assertImage(t, container(t, obj, "initContainers", "install-multus-binary"), "ghcr.io/k8snetworkplumbingwg/multus-cni:v4.3.2")
			assertImage(t, container(t, obj, "containers", "kube-multus"), "ghcr.io/k8snetworkplumbingwg/multus-cni:v4.3.2")
		},
	},
	{
		component: "ovs-cni", file: "ovs-cni-amd64.yaml", gvk: daemonSetGVK,
		key:   types.NamespacedName{Namespace: "kube-system", Name: "ovs-cni-amd64"},
		patch: `{"spec":{"template":{"spec":{"initContainers":[{"name":"ovs-cni-plugin","image":"ghcr.io/k8snetworkplumbingwg/ovs-cni-plugin:v0.40.0"}],"containers":[{"name":"ovs-cni-marker","image":"ghcr.io/k8snetworkplumbingwg/ovs-cni-plugin:v0.40.0"}]}}}}`,
		patched: func(t *testing.T, obj *unstructured.Unstructured) {
			assertImage(t, container(t, obj, "initContainers", "ovs-cni-plugin"), "ghcr.io/k8snetworkplumbingwg/ovs-cni-plugin:v0.40.0")
			assertImage(t, container(t, obj, "containers", "ovs-cni-marker"), "ghcr.io/k8snetworkplumbingwg/ovs-cni-plugin:v0.40.0")
		},
	},
}

func firstMatchExpression(terms []interface{}) map[string]interface{} {
	if len(terms) == 0 {
		return nil
	}
	term, _ := terms[0].(map[string]interface{})
	exprs, _, _ := unstructured.NestedSlice(term, "preference", "matchExpressions")
	if len(exprs) == 0 {
		return nil
	}
	expr, _ := exprs[0].(map[string]interface{})
	return expr
}

// container is the container name of the pod template's list field
// (containers, initContainers).
func container(t *testing.T, obj *unstructured.Unstructured, field, name string) map[string]interface{} {
	t.Helper()
	list, _, _ := unstructured.NestedSlice(obj.Object, "spec", "template", "spec", field)
	for _, item := range list {
		if c, ok := item.(map[string]interface{}); ok && c["name"] == name {
			return c
		}
	}
	t.Fatalf("%s %s has no %s %s", obj.GetKind(), obj.GetName(), field, name)
	return nil
}

func assertImage(t *testing.T, c map[string]interface{}, want string) {
	t.Helper()
	if image, _, _ := unstructured.NestedString(c, "image"); image != want {
		t.Errorf("container %v: image %q, want the release's %q", c["name"], image, want)
	}
}

// spec is the manifest component of w, its manifest served by srv, with the
// release's patch.
func (w releasePatchedWorkload) spec(srv string) k4allv1alpha1.ComponentSpec {
	return k4allv1alpha1.ComponentSpec{
		Type: k4allv1alpha1.ComponentTypeManifest, Version: "v1", Sources: []string{srv + "/" + w.file},
		Patches: []k4allv1alpha1.ComponentPatch{{
			Target: k4allv1alpha1.PatchTarget{
				APIVersion: w.gvk.GroupVersion().String(), Kind: w.gvk.Kind, Name: w.key.Name, Namespace: w.key.Namespace,
			},
			Type: k4allv1alpha1.PatchTypeStrategic, Patch: w.patch,
		}},
	}
}

func (w releasePatchedWorkload) get(t *testing.T, c client.Client) *unstructured.Unstructured {
	t.Helper()
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(w.gvk)
	if err := c.Get(context.Background(), w.key, obj); err != nil {
		t.Fatalf("get %s %s: %v", w.gvk.Kind, w.key, err)
	}
	return obj
}

// onEnvtest makes w's namespace, and deletes w's object when the test ends.
func (w releasePatchedWorkload) onEnvtest(t *testing.T, c client.Client) {
	t.Helper()
	ensureNamespace(t, c, w.key.Namespace)
	t.Cleanup(func() {
		obj := &unstructured.Unstructured{}
		obj.SetGroupVersionKind(w.gvk)
		obj.SetNamespace(w.key.Namespace)
		obj.SetName(w.key.Name)
		_ = c.Delete(context.Background(), obj)
	})
}

// releasePatches serves testdata/release-patches.
func releasePatches(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.FileServer(http.Dir("testdata/release-patches")))
	t.Cleanup(srv.Close)
	return srv.URL
}

// reconcileComponent runs the engine's ReconcileComponent of the operator.
func reconcileComponent(t *testing.T, c client.Client, name string, spec k4allv1alpha1.ComponentSpec, opts ...engine.ReconcileOption) {
	t.Helper()
	st, err := engine.New(c, envtestConfig(t), logr.Discard(), "").ReconcileComponent(context.Background(), name, spec, nil, opts...)
	if err != nil {
		t.Fatalf("ReconcileComponent %s: %v", name, err)
	}
	if st.State != k4allv1alpha1.ComponentStateInstalled {
		t.Fatalf("%s is %s (%s), want Installed", name, st.State, st.Message)
	}
}

// v060Apply applies spec as k4all-operator v0.6.0 (and v0.5.x) did:
// upstream's objects server-side applied as they are, then the patches, as
// patches of the objects in the cluster.
func v060Apply(t *testing.T, c client.Client, name string, spec k4allv1alpha1.ComponentSpec, opts ...engine.ReconcileOption) {
	t.Helper()
	unpatched := spec
	unpatched.Patches = nil
	reconcileComponent(t, c, name, unpatched, opts...)
	if err := engine.NewPatchApplier(c, logr.Discard()).ApplyAll(context.Background(), spec.Patches); err != nil {
		t.Fatalf("v0.6.0's patches of %s: %v", name, err)
	}
}

// Installed, then applied twice again: the object is written once, patched,
// by k4all-operator's apply, and never changed again. Each apply of v0.6.0
// raised its generation by 2.
func TestReconcileComponent_AReleasePatchedWorkloadIsWrittenOnce(t *testing.T) {
	srv := releasePatches(t)
	c := operatorClient(t)
	for _, w := range releasePatchedWorkloads {
		t.Run(w.component, func(t *testing.T) {
			w.onEnvtest(t, c)
			for apply := 1; apply <= 3; apply++ {
				reconcileComponent(t, c, w.component, w.spec(srv))
				obj := w.get(t, c)
				if obj.GetGeneration() != 1 {
					t.Fatalf("apply %d: generation %d, want 1; managers: %v", apply, obj.GetGeneration(), managers(obj))
				}
				w.patched(t, obj)
			}
			obj := w.get(t, c)
			if fields := managedFieldsOf(t, obj, metav1.ManagedFieldsOperationUpdate); fields != nil {
				t.Errorf("k4all-operator patched %s after its apply: %v", w.key, fields)
			}
			if !owns(managedFieldsOf(t, obj, metav1.ManagedFieldsOperationApply), "spec", "template", "spec") {
				t.Errorf("the pod template is not k4all-operator's apply; managers: %v", managers(obj))
			}
		})
	}
}

// The hop from k4all v2.7.0 (operator v0.6.0) to v2.7.1: the first apply of
// the new operator finds each object as v0.6.0's apply and patch left it,
// and writes the same pod template. Nothing rolls.
func TestReconcileComponent_TheFirstApplyAfterV060RollsNothing(t *testing.T) {
	srv := releasePatches(t)
	c := operatorClient(t)
	for _, w := range releasePatchedWorkloads {
		t.Run(w.component, func(t *testing.T) {
			w.onEnvtest(t, c)
			v060Apply(t, c, w.component, w.spec(srv))
			before := w.get(t, c)
			if before.GetGeneration() != 2 {
				t.Fatalf("v0.6.0: generation %d, want 2 (its apply, then its patch): the test does not start from v0.6.0",
					before.GetGeneration())
			}
			w.patched(t, before)

			reconcileComponent(t, c, w.component, w.spec(srv))

			after := w.get(t, c)
			if after.GetGeneration() != before.GetGeneration() {
				t.Errorf("generation %d after the first apply of the new operator, want %d", after.GetGeneration(), before.GetGeneration())
			}
			wantTemplate, _, _ := unstructured.NestedMap(before.Object, "spec", "template")
			gotTemplate, _, _ := unstructured.NestedMap(after.Object, "spec", "template")
			if !reflect.DeepEqual(gotTemplate, wantTemplate) {
				t.Errorf("pod template changed:\n got %v\nwant %v", gotTemplate, wantTemplate)
			}
			w.patched(t, after)
			if !owns(managedFieldsOf(t, after, metav1.ManagedFieldsOperationApply), "spec", "template", "spec") {
				t.Errorf("the pod template is not k4all-operator's apply; managers: %v", managers(after))
			}
		})
	}
}

// A JSON patch that adds to a list goes into the object once per apply,
// not once more after it: the args hold the flag once, and the generation
// does not move.
func TestReconcileComponent_AJSONPatchIsFoldedOnce(t *testing.T) {
	srv := releasePatches(t)
	c := operatorClient(t)
	w := releasePatchedWorkloads[1] // metrics-server
	w.onEnvtest(t, c)
	spec := w.spec(srv)
	spec.Patches[0].Type = k4allv1alpha1.PatchTypeJSON
	spec.Patches[0].Patch = `[{"op":"add","path":"/spec/template/spec/containers/0/args/-","value":"--kubelet-insecure-tls"}]`

	for apply := 1; apply <= 2; apply++ {
		reconcileComponent(t, c, w.component, spec)
		obj := w.get(t, c)
		args, _, _ := unstructured.NestedStringSlice(container(t, obj, "containers", "metrics-server"), "args")
		n := 0
		for _, a := range args {
			if a == "--kubelet-insecure-tls" {
				n++
			}
		}
		if n != 1 || obj.GetGeneration() != 1 {
			t.Fatalf("apply %d: --kubelet-insecure-tls %d times, generation %d; want once, generation 1", apply, n, obj.GetGeneration())
		}
	}
}

// A patch whose target is in no manifest of the component is still applied
// after the apply, to the object in the cluster.
func TestReconcileComponent_APatchOfAnObjectOutsideTheManifests(t *testing.T) {
	c := operatorClient(t)
	ctx := context.Background()
	outside := &unstructured.Unstructured{}
	outside.SetAPIVersion("v1")
	outside.SetKind("ConfigMap")
	outside.SetNamespace("default")
	outside.SetName("made-by-an-operator")
	if err := c.Create(ctx, outside); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Delete(context.Background(), outside) })
	t.Cleanup(func() {
		probe := &unstructured.Unstructured{}
		probe.SetAPIVersion("v1")
		probe.SetKind("ConfigMap")
		probe.SetNamespace("default")
		probe.SetName("upgrade-path-probe")
		_ = c.Delete(context.Background(), probe)
	})
	spec := k4allv1alpha1.ComponentSpec{
		Type: k4allv1alpha1.ComponentTypeManifest, Version: "v1", InlineManifests: []string{probeConfigMap},
		Patches: []k4allv1alpha1.ComponentPatch{{
			Target: k4allv1alpha1.PatchTarget{APIVersion: "v1", Kind: "ConfigMap", Name: "made-by-an-operator", Namespace: "default"},
			Type:   k4allv1alpha1.PatchTypeMerge, Patch: `{"data":{"patched":"by-the-release"}}`,
		}},
	}

	reconcileComponent(t, c, "probe", spec)

	if err := c.Get(ctx, client.ObjectKeyFromObject(outside), outside); err != nil {
		t.Fatal(err)
	}
	if v, _, _ := unstructured.NestedString(outside.Object, "data", "patched"); v != "by-the-release" {
		t.Errorf("ConfigMap default/made-by-an-operator: data %v, want the patch's", outside.Object["data"])
	}
}

// The KubeVirt CR with the release's two merge patches (infra placement,
// workloadUpdateMethods) and the ClusterConfig's gates, through the
// VirtHook, as v0.6.0 left it: the first apply of the new operator changes
// nothing in it, not even its generation, and the gates stay k4all's as
// the annotations say. Hangar and the k4all addon read k4all's fields by the
// manager k4all-operator: the patched ones are still its, by its apply.
func TestReconcileComponent_TheKubeVirtCRAfterV060(t *testing.T) {
	srv := upstreamReleases(t)
	c := operatorClient(t)
	ensureNamespace(t, c, "kubevirt")
	kubeVirtCR.deleteOnCleanup(t, c)
	spec := kubeVirtComponent(srv, "v1.9.1")
	spec.Patches = []k4allv1alpha1.ComponentPatch{
		{
			Target: k4allv1alpha1.PatchTarget{APIVersion: "kubevirt.io/v1", Kind: "KubeVirt", Name: "kubevirt", Namespace: "kubevirt"},
			Type:   k4allv1alpha1.PatchTypeMerge,
			Patch:  `{"spec":{"infra":{"nodePlacement":{"affinity":{"nodeAffinity":{"preferredDuringSchedulingIgnoredDuringExecution":[{"weight":100,"preference":{"matchExpressions":[{"key":"node-role.kubernetes.io/control-plane","operator":"Exists"}]}}]}}}}}}`,
		},
		{
			Target: k4allv1alpha1.PatchTarget{APIVersion: "kubevirt.io/v1", Kind: "KubeVirt", Name: "kubevirt", Namespace: "kubevirt"},
			Type:   k4allv1alpha1.PatchTypeMerge,
			Patch:  liveMigratePatch,
		},
	}
	config := k4allv1alpha1.ClusterConfigSpec{Features: k4allv1alpha1.FeaturesConfig{Virt: k4allv1alpha1.VirtConfig{
		Enabled: true, KubeVirt: &k4allv1alpha1.KubeVirtSettings{FeatureGates: []string{"Sidecar"}, DisabledFeatureGates: []string{"Snapshot"}},
	}}}
	mutate := engine.MutateObjects(hooks.ObjectMutator(hooks.NewRegistry(c, logr.Discard()).Get("kubevirt"), config))

	v060Apply(t, c, "kubevirt", spec, mutate)
	kubeVirtCR.patchByHand(t, c, `{"spec":{"configuration":{"developerConfiguration":{"featureGates":["Sidecar","HostDevices"]}}}}`)
	before := kubeVirtCR.get(t, c)

	reconcileComponent(t, c, "kubevirt", spec, mutate)

	after := kubeVirtCR.get(t, c)
	if after.GetGeneration() != before.GetGeneration() {
		t.Errorf("generation %d after the first apply of the new operator, want %d", after.GetGeneration(), before.GetGeneration())
	}
	if !reflect.DeepEqual(after.Object["spec"], before.Object["spec"]) {
		t.Errorf("spec changed:\n got %v\nwant %v", after.Object["spec"], before.Object["spec"])
	}
	assertList(t, c, "after v0.6.0", kvGatesPath, []string{"Sidecar", "HostDevices"})
	assertList(t, c, "after v0.6.0", kvDisabledPath, []string{"Snapshot"})
	assertAnnotation(t, c, "after v0.6.0", hooks.FeatureGatesAnnotation, "Sidecar")
	assertAnnotation(t, c, "after v0.6.0", hooks.DisabledFeatureGatesAnnotation, "Snapshot")
	if got := workloadUpdateMethods(t, after); !slices.Equal(got, []string{"LiveMigrate"}) {
		t.Errorf("workloadUpdateMethods %v, want the release's [LiveMigrate]", got)
	}
	applied := managedFieldsOf(t, after, metav1.ManagedFieldsOperationApply)
	for _, path := range [][]string{
		kvGatesPath, kvDisabledPath,
		{"metadata", "annotations", hooks.FeatureGatesAnnotation},
		{"spec", "workloadUpdateStrategy", "workloadUpdateMethods"},
		{"spec", "infra", "nodePlacement", "affinity"},
	} {
		if !owns(applied, path...) {
			t.Errorf("%v is not k4all-operator's apply; managers: %v", path, managers(after))
		}
	}
}
