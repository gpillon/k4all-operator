package engine

import (
	"reflect"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/yaml"

	k4allv1alpha1 "github.com/gpillon/k4all-operator/api/v1alpha1"
)

const twoContainers = `apiVersion: apps/v1
kind: Deployment
metadata:
  name: web
  namespace: default
spec:
  template:
    spec:
      affinity:
        nodeAffinity:
          requiredDuringSchedulingIgnoredDuringExecution:
            nodeSelectorTerms:
            - matchExpressions:
              - {key: node-role.kubernetes.io/control-plane, operator: Exists}
      containers:
      - name: app
        image: app:v1
        args: [--a]
      - name: sidecar
        image: sidecar:v1
`

func decode(t *testing.T, manifest string) *unstructured.Unstructured {
	t.Helper()
	obj := &unstructured.Unstructured{}
	if err := yaml.Unmarshal([]byte(manifest), &obj.Object); err != nil {
		t.Fatal(err)
	}
	return obj
}

func patchOf(kind, name, namespace string, typ k4allv1alpha1.PatchType, patch string) k4allv1alpha1.ComponentPatch {
	return k4allv1alpha1.ComponentPatch{
		Target: k4allv1alpha1.PatchTarget{Kind: kind, Name: name, Namespace: namespace},
		Type:   typ, Patch: patch,
	}
}

// Each patch type, as the API server applies it to the object.
func TestPatchObject(t *testing.T) {
	for _, tc := range []struct {
		name     string
		manifest string
		patch    k4allv1alpha1.ComponentPatch
		want     string
	}{
		{
			name:     "strategic: containers merge by name, null deletes",
			manifest: twoContainers,
			patch: patchOf("Deployment", "web", "default", k4allv1alpha1.PatchTypeStrategic,
				`{"spec":{"template":{"spec":{"affinity":{"nodeAffinity":{"requiredDuringSchedulingIgnoredDuringExecution":null}},"containers":[{"name":"app","image":"app:v2"}]}}}}`),
			want: `apiVersion: apps/v1
kind: Deployment
metadata: {name: web, namespace: default}
spec:
  template:
    spec:
      affinity: {nodeAffinity: {}}
      containers:
      - {name: app, image: "app:v2", args: [--a]}
      - {name: sidecar, image: "sidecar:v1"}
`,
		},
		{
			name:     "merge: a list is replaced",
			manifest: twoContainers,
			patch: patchOf("Deployment", "web", "default", k4allv1alpha1.PatchTypeMerge,
				`{"spec":{"template":{"spec":{"affinity":null,"containers":[{"name":"app","image":"app:v2"}]}}}}`),
			want: `apiVersion: apps/v1
kind: Deployment
metadata: {name: web, namespace: default}
spec:
  template:
    spec:
      containers:
      - {name: app, image: "app:v2"}
`,
		},
		{
			name:     "json: added to a list",
			manifest: twoContainers,
			patch: patchOf("Deployment", "web", "default", k4allv1alpha1.PatchTypeJSON,
				`[{"op":"add","path":"/spec/template/spec/containers/0/args/-","value":"--b"},{"op":"remove","path":"/spec/template/spec/affinity"}]`),
			want: `apiVersion: apps/v1
kind: Deployment
metadata: {name: web, namespace: default}
spec:
  template:
    spec:
      containers:
      - {name: app, image: "app:v1", args: [--a, --b]}
      - {name: sidecar, image: "sidecar:v1"}
`,
		},
		{
			name: "strategic on a CR, which the scheme lacks: a merge patch",
			manifest: `apiVersion: kubevirt.io/v1
kind: KubeVirt
metadata: {name: kubevirt, namespace: kubevirt}
spec:
  workloadUpdateStrategy: {}
  configuration: {developerConfiguration: {featureGates: [A]}}
`,
			patch: patchOf("KubeVirt", "kubevirt", "kubevirt", k4allv1alpha1.PatchTypeStrategic,
				`{"spec":{"workloadUpdateStrategy":{"workloadUpdateMethods":["LiveMigrate"]},"configuration":{"developerConfiguration":{"featureGates":["B"]}}}}`),
			want: `apiVersion: kubevirt.io/v1
kind: KubeVirt
metadata: {name: kubevirt, namespace: kubevirt}
spec:
  workloadUpdateStrategy: {workloadUpdateMethods: [LiveMigrate]}
  configuration: {developerConfiguration: {featureGates: [B]}}
`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			obj := decode(t, tc.manifest)
			if err := patchObject(obj, tc.patch, clientgoscheme.Scheme); err != nil {
				t.Fatal(err)
			}
			if want := decode(t, tc.want); !reflect.DeepEqual(obj.Object, want.Object) {
				t.Errorf("patched:\n got %v\nwant %v", obj.Object, want.Object)
			}
		})
	}
}

func TestPatchObject_InvalidJSON(t *testing.T) {
	obj := decode(t, twoContainers)
	p := patchOf("Deployment", "web", "default", k4allv1alpha1.PatchTypeMerge, `spec: {}`)
	if err := patchObject(obj, p, clientgoscheme.Scheme); err == nil {
		t.Error("a patch that is not JSON applied, want an error (PatchApplier refuses it too)")
	}
}

// A patch goes into the object its target names; the others stay for after
// the apply.
func TestPatchFolder(t *testing.T) {
	cdi := decode(t, "apiVersion: cdi.kubevirt.io/v1beta1\nkind: CDI\nmetadata: {name: cdi}\nspec: {}\n")
	cdi.SetNamespace("cdi") // filled in from the component's namespace: isNamespaced does not know CDI
	web := decode(t, twoContainers)
	patches := []k4allv1alpha1.ComponentPatch{
		patchOf("CDI", "cdi", "", k4allv1alpha1.PatchTypeMerge, `{"spec":{"config":{"x":"y"}}}`),
		patchOf("Deployment", "web", "default", k4allv1alpha1.PatchTypeMerge, `{"metadata":{"labels":{"patched":"1"}}}`),
		patchOf("Deployment", "web", "other", k4allv1alpha1.PatchTypeMerge, `{}`),
		patchOf("Deployment", "api", "default", k4allv1alpha1.PatchTypeMerge, `{}`),
		patchOf("DaemonSet", "web", "default", k4allv1alpha1.PatchTypeMerge, `{}`),
		{Target: k4allv1alpha1.PatchTarget{APIVersion: "apps/v1beta2", Kind: "Deployment", Name: "web", Namespace: "default"},
			Type: k4allv1alpha1.PatchTypeMerge, Patch: `{}`},
	}
	f := newPatchFolder(patches, clientgoscheme.Scheme)
	if err := f.fold(cdi, ""); err != nil {
		t.Fatal(err)
	}
	if err := f.fold(web, "default"); err != nil {
		t.Fatal(err)
	}

	if v, _, _ := unstructured.NestedString(cdi.Object, "spec", "config", "x"); v != "y" {
		t.Errorf("CDI: spec %v, want its patch, whose target has no namespace", cdi.Object["spec"])
	}
	if web.GetLabels()["patched"] != "1" {
		t.Errorf("Deployment default/web: labels %v, want its patch", web.GetLabels())
	}
	if got, want := f.unfolded(), patches[2:]; !reflect.DeepEqual(got, want) {
		t.Errorf("unfolded %v, want %v", got, want)
	}
}
