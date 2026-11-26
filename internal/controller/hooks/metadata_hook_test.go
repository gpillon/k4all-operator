package hooks

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/yaml"

	k4allv1alpha1 "github.com/gpillon/k4all-operator/api/v1alpha1"
	"github.com/gpillon/k4all-operator/internal/metadata"
)

var update = flag.Bool("update", false, "rewrite the golden files of testdata/metadata")

const mdImage = "ghcr.io/gpillon/k4all-metadata:v0.6.0"

var (
	podsGVR = schema.GroupVersionResource{Version: "v1", Resource: "pods"}
	vmisGVR = schema.GroupVersionResource{Group: "kubevirt.io", Version: "v1", Resource: "virtualmachineinstances"}
)

func mdConfig(md *k4allv1alpha1.MetadataSettings) k4allv1alpha1.ClusterConfigSpec {
	var c k4allv1alpha1.ClusterConfigSpec
	c.Features.Virt.Enabled = true
	c.Features.Virt.Metadata = md
	return c
}

func mdObjects(t *testing.T, md *k4allv1alpha1.MetadataSettings) map[string]*unstructured.Unstructured {
	t.Helper()
	objs, err := metadataObjects(mdImage, MetadataSettingsFrom(mdConfig(md)))
	if err != nil {
		t.Fatal(err)
	}
	m := map[string]*unstructured.Unstructured{}
	for _, o := range objs {
		m[o.GetKind()+"/"+o.GetName()] = o
	}
	return m
}

// --- what is rendered --------------------------------------------------------

func golden(t *testing.T, name string, objs []*unstructured.Unstructured) {
	t.Helper()
	docs := make([]string, 0, len(objs))
	for _, o := range objs {
		raw, err := yaml.Marshal(o.Object)
		if err != nil {
			t.Fatal(err)
		}
		docs = append(docs, string(raw))
	}
	got := strings.Join(docs, "---\n")
	path := filepath.Join("testdata", "metadata", name)
	if *update {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (go test ./internal/controller/hooks -run Golden -update writes it)", err)
	}
	if got != string(want) {
		t.Fatalf("%s differs from what is rendered (-update rewrites it):\n%s", path, got)
	}
}

// The objects of a ClusterConfig with metadata enabled and nothing else
// set: scope Labelled, the default token.
func TestMetadataObjects_GoldenLabelledDefaults(t *testing.T) {
	objs, err := metadataObjects(mdImage, MetadataSettingsFrom(mdConfig(&k4allv1alpha1.MetadataSettings{Enabled: true})))
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "labelled-defaults.yaml", objs)
}

// Scope All, two audiences, a longer lifetime, vendor-data.
func TestMetadataObjects_GoldenAllAudiencesVendorData(t *testing.T) {
	objs, err := metadataObjects(mdImage, MetadataSettingsFrom(mdConfig(&k4allv1alpha1.MetadataSettings{
		Enabled: true, Scope: k4allv1alpha1.MetadataScopeAll,
		Tokens: &k4allv1alpha1.MetadataTokens{
			Audiences: []string{"k4all-metadata", "https://vault.example.com"}, ExpirationSeconds: ptr.To[int64](7200),
		},
		VendorDataConfigMap: "k4all-vendor-data",
	})))
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "all-audiences-vendor.yaml", objs)
}

func TestMetadataObjects_FailurePolicyIgnoreAndOwnedByK4all(t *testing.T) {
	for key, o := range mdObjects(t, &k4allv1alpha1.MetadataSettings{Enabled: true}) {
		if o.GetAPIVersion() != "admissionregistration.k8s.io/v1" {
			t.Errorf("%s: %s", key, o.GetAPIVersion())
		}
		if o.GetLabels()["app.kubernetes.io/managed-by"] != "k4all-operator" {
			t.Errorf("%s: labels %v", key, o.GetLabels())
		}
		if o.GetKind() == "MutatingAdmissionPolicy" {
			if fp, _, _ := unstructured.NestedString(o.Object, "spec", "failurePolicy"); fp != "Ignore" {
				t.Errorf("%s: failurePolicy %q", key, fp)
			}
		}
	}
}

// --- what a VM's pod becomes -------------------------------------------------

// launcherPod is a virt-launcher pod of VM "web" as virt-controller creates
// it (KubeVirt 1.9.1, masquerade binding), after the ServiceAccount plugin.
func launcherPod(labels map[string]string) *corev1.Pod {
	l := map[string]string{
		"kubevirt.io":            "virt-launcher",
		"kubevirt.io/created-by": "0fde2733-7537-483a-9d44-c212db9b510c",
		"vm.kubevirt.io/name":    "web",
		"vmi.kubevirt.io/id":     "web",
	}
	for k, v := range labels {
		if v == "" {
			delete(l, k)
			continue
		}
		l[k] = v
	}
	return &corev1.Pod{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Pod"},
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "virt-launcher-web-", Namespace: "shop", Labels: l,
			Annotations: map[string]string{
				"kubevirt.io/domain":                          "web",
				"istio.io/reroute-virtual-interfaces":         "k6t-eth0",
				"traffic.sidecar.istio.io/kubevirtInterfaces": "k6t-eth0",
			},
		},
		Spec: corev1.PodSpec{
			AutomountServiceAccountToken: ptr.To(false),
			ServiceAccountName:           "default",
			InitContainers: []corev1.Container{{
				Name: "guest-console-log", Image: "quay.io/kubevirt/virt-launcher:v1.9.1",
				RestartPolicy: ptr.To(corev1.ContainerRestartPolicyAlways),
			}},
			Containers: []corev1.Container{{Name: "compute", Image: "quay.io/kubevirt/virt-launcher:v1.9.1"}},
			Volumes: []corev1.Volume{
				{Name: "private", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
				{Name: "public", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
			},
		},
	}
}

// namespace is the namespace "shop" of the test pods, with labels.
func namespace(labels map[string]string) *corev1.Namespace {
	return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "shop", Labels: labels}}
}

// admitPod runs the pod policy of md on pod in ns.
func admitPod(t *testing.T, md *k4allv1alpha1.MetadataSettings, pod *corev1.Pod, ns *corev1.Namespace) (*corev1.Pod, bool) {
	t.Helper()
	objs := mdObjects(t, md)
	out, changed := admit(t, objs["MutatingAdmissionPolicy/"+MetadataPolicy], objs["MutatingAdmissionPolicyBinding/"+MetadataPolicy],
		pod, podsGVR, ns)
	return out.(*corev1.Pod), changed
}

var labelled = map[string]string{metadata.LabelOptIn: metadata.OptInEnabled}

func container(t *testing.T, cs []corev1.Container, name string) corev1.Container {
	t.Helper()
	for _, c := range cs {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("no container %s in %v", name, cs)
	return corev1.Container{}
}

func volume(t *testing.T, pod *corev1.Pod, name string) corev1.Volume {
	t.Helper()
	for _, v := range pod.Spec.Volumes {
		if v.Name == name {
			return v
		}
	}
	t.Fatalf("no volume %s", name)
	return corev1.Volume{}
}

func mounts(c corev1.Container) map[string]string {
	m := map[string]string{}
	for _, vm := range c.VolumeMounts {
		if !vm.ReadOnly {
			m[vm.Name] = "WRITABLE " + vm.MountPath
			continue
		}
		m[vm.Name] = vm.MountPath
	}
	return m
}

// A guaranteed-QoS VM (dedicated CPUs) must stay guaranteed: every injected
// container has limits equal to its requests.
func wantGuaranteed(t *testing.T, c corev1.Container) {
	t.Helper()
	for _, r := range []corev1.ResourceName{corev1.ResourceCPU, corev1.ResourceMemory} {
		req, lim := c.Resources.Requests[r], c.Resources.Limits[r]
		if req.IsZero() || req.Cmp(lim) != 0 {
			t.Fatalf("%s: %s request %s, limit %s", c.Name, r, req.String(), lim.String())
		}
	}
}

func TestPodPolicy_InjectsTheRedirectAndTheServer(t *testing.T) {
	pod, changed := admitPod(t, &k4allv1alpha1.MetadataSettings{Enabled: true}, launcherPod(labelled), namespace(nil))
	if !changed {
		t.Fatal("not injected")
	}
	init := pod.Spec.InitContainers
	if len(init) != 3 || init[0].Name != "guest-console-log" || init[1].Name != metadata.RedirectContainer ||
		init[2].Name != metadata.ServerContainer {
		t.Fatalf("init containers: %v", init)
	}
	wantRedirect(t, init[1])
	wantServer(t, init[2])
	wantVolumes(t, pod)
	// What KubeVirt set stays: no API token, the namespace's default SA.
	if *pod.Spec.AutomountServiceAccountToken || pod.Spec.ServiceAccountName != "default" || len(pod.Spec.Containers) != 1 {
		t.Fatalf("pod spec: %+v", pod.Spec)
	}
}

// The redirect: root with NET_ADMIN only, container_t (no SELinux type,
// not privileged), no volume.
func wantRedirect(t *testing.T, redirect corev1.Container) {
	t.Helper()
	if redirect.Image != mdImage || strings.Join(redirect.Command, " ") != "/metadata-server -redirect" ||
		redirect.RestartPolicy != nil {
		t.Fatalf("redirect: %+v", redirect)
	}
	sc := redirect.SecurityContext
	if *sc.RunAsUser != 0 || *sc.RunAsNonRoot || *sc.AllowPrivilegeEscalation || !*sc.ReadOnlyRootFilesystem ||
		len(sc.Capabilities.Add) != 1 || sc.Capabilities.Add[0] != "NET_ADMIN" || sc.Capabilities.Drop[0] != "ALL" ||
		sc.Privileged != nil || sc.SELinuxOptions != nil {
		t.Fatalf("redirect security context: %+v", sc)
	}
	if len(redirect.VolumeMounts) != 0 {
		t.Fatalf("redirect mounts: %v", redirect.VolumeMounts)
	}
	wantGuaranteed(t, redirect)
}

// The server: a native sidecar, uid 107, nothing more, an exec probe.
func wantServer(t *testing.T, server corev1.Container) {
	t.Helper()
	if server.Image != mdImage || strings.Join(server.Command, " ") != "/metadata-server" ||
		server.RestartPolicy == nil || *server.RestartPolicy != corev1.ContainerRestartPolicyAlways {
		t.Fatalf("server: %+v", server)
	}
	sc := server.SecurityContext
	if *sc.RunAsUser != 107 || *sc.RunAsGroup != 107 || !*sc.RunAsNonRoot || *sc.AllowPrivilegeEscalation ||
		!*sc.ReadOnlyRootFilesystem || len(sc.Capabilities.Add) != 0 || sc.Capabilities.Drop[0] != "ALL" ||
		sc.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Fatalf("server security context: %+v", sc)
	}
	wantGuaranteed(t, server)
	// No TCP or HTTP probe: KubeVirt hands every port of the pod to the guest.
	if server.LivenessProbe == nil || server.LivenessProbe.Exec == nil ||
		strings.Join(server.LivenessProbe.Exec.Command, " ") != "/metadata-server -probe" ||
		server.ReadinessProbe != nil || server.StartupProbe != nil {
		t.Fatalf("server probes: %+v %+v %+v", server.LivenessProbe, server.ReadinessProbe, server.StartupProbe)
	}
	want := map[string]string{
		"k4all-md-tokens": metadata.TokenDir, "k4all-md-pod": metadata.PodDir, "k4all-md-vm": metadata.VMDir,
	}
	if got := mounts(server); !reflect.DeepEqual(got, want) {
		t.Fatalf("server mounts %v, want %v", got, want)
	}
}

// The volumes: the token, the downward API, the VM's Secret (optional).
func wantVolumes(t *testing.T, pod *corev1.Pod) {
	t.Helper()
	tokens := volume(t, pod, "k4all-md-tokens").Projected
	if tokens == nil || len(tokens.Sources) != 1 {
		t.Fatalf("token volume: %+v", tokens)
	}
	sat := tokens.Sources[0].ServiceAccountToken
	if sat.Audience != "k4all-metadata" || *sat.ExpirationSeconds != 3600 || sat.Path != metadata.TokenFile("k4all-metadata") {
		t.Fatalf("token: %+v", sat)
	}
	paths := map[string]string{}
	for _, item := range volume(t, pod, "k4all-md-pod").DownwardAPI.Items {
		paths[item.Path] = item.FieldRef.FieldPath
	}
	if !reflect.DeepEqual(paths, map[string]string{
		"labels": "metadata.labels", "annotations": "metadata.annotations", "namespace": "metadata.namespace",
	}) {
		t.Fatalf("downward API: %v", paths)
	}
	vm := volume(t, pod, "k4all-md-vm").Secret
	if vm.SecretName != "k4all-md-web" || vm.Optional == nil || !*vm.Optional {
		t.Fatalf("VM secret volume: %+v", vm)
	}
}

func TestPodPolicy_ScopeLabelledNeedsTheLabel(t *testing.T) {
	ns := namespace(nil)
	if _, changed := admitPod(t, &k4allv1alpha1.MetadataSettings{Enabled: true}, launcherPod(nil), ns); changed {
		t.Fatal("an unlabelled VM was injected")
	}
	for _, v := range []string{"true", metadata.OptInDisabled} {
		if _, changed := admitPod(t, &k4allv1alpha1.MetadataSettings{Enabled: true},
			launcherPod(map[string]string{metadata.LabelOptIn: v}), ns); changed {
			t.Fatalf("label %q injected", v)
		}
	}
}

func TestPodPolicy_ScopeAllServesEveryVMButTheOptedOut(t *testing.T) {
	all := &k4allv1alpha1.MetadataSettings{Enabled: true, Scope: k4allv1alpha1.MetadataScopeAll}
	ns := namespace(nil)
	if _, changed := admitPod(t, all, launcherPod(nil), ns); !changed {
		t.Fatal("scope All did not inject an unlabelled VM")
	}
	if _, changed := admitPod(t, all, launcherPod(map[string]string{metadata.LabelOptIn: metadata.OptInDisabled}), ns); changed {
		t.Fatal("scope All injected a VM labelled disabled")
	}
	// never a pod that is not a VM's
	other := launcherPod(nil)
	other.Labels["kubevirt.io"] = "virt-exportserver"
	if _, changed := admitPod(t, all, other, ns); changed {
		t.Fatal("scope All injected a pod that is not a virt-launcher")
	}
}

// The redirect needs NET_ADMIN, which PodSecurity baseline (and restricted)
// refuses: injected there, the VM could not start. Only enforce counts.
func TestPodPolicy_SkipsNamespacesEnforcingBaselineOrRestricted(t *testing.T) {
	for level, want := range map[string]bool{"baseline": false, "restricted": false, "privileged": true, "": true} {
		labels := map[string]string{"pod-security.kubernetes.io/warn": "restricted", "pod-security.kubernetes.io/audit": "baseline"}
		if level != "" {
			labels["pod-security.kubernetes.io/enforce"] = level
		}
		_, changed := admitPod(t, &k4allv1alpha1.MetadataSettings{Enabled: true}, launcherPod(labelled), namespace(labels))
		if changed != want {
			t.Errorf("enforce %q: injected %v, want %v", level, changed, want)
		}
	}
}

// Without the masquerade binding (bridge on the pod network, secondary
// networks only) guest traffic never reaches the pod's k6t-eth0 NAT.
func TestPodPolicy_OnlyWithTheMasqueradeBinding(t *testing.T) {
	pod := launcherPod(labelled)
	delete(pod.Annotations, "istio.io/reroute-virtual-interfaces")
	delete(pod.Annotations, "traffic.sidecar.istio.io/kubevirtInterfaces")
	if _, changed := admitPod(t, &k4allv1alpha1.MetadataSettings{Enabled: true}, pod, namespace(nil)); changed {
		t.Fatal("injected without masquerade")
	}
	pod = launcherPod(labelled)
	delete(pod.Annotations, "istio.io/reroute-virtual-interfaces")
	if _, changed := admitPod(t, &k4allv1alpha1.MetadataSettings{Enabled: true}, pod, namespace(nil)); !changed {
		t.Fatal("not injected with the older annotation only")
	}
}

func TestPodPolicy_TheVMsServiceAccountFromItsLabel(t *testing.T) {
	pod, changed := admitPod(t, &k4allv1alpha1.MetadataSettings{Enabled: true},
		launcherPod(map[string]string{metadata.LabelOptIn: metadata.OptInEnabled, metadata.LabelServiceAccount: "shop-web"}),
		namespace(nil))
	if !changed || pod.Spec.ServiceAccountName != "shop-web" || *pod.Spec.AutomountServiceAccountToken {
		t.Fatalf("service account %q, automount %v", pod.Spec.ServiceAccountName, *pod.Spec.AutomountServiceAccountToken)
	}
}

func TestPodPolicy_ATokenPerAudience(t *testing.T) {
	pod, _ := admitPod(t, &k4allv1alpha1.MetadataSettings{Enabled: true, Tokens: &k4allv1alpha1.MetadataTokens{
		Audiences: []string{"k4all-metadata", "https://vault.example.com"}, ExpirationSeconds: ptr.To[int64](7200),
	}}, launcherPod(labelled), namespace(nil))
	sources := volume(t, pod, "k4all-md-tokens").Projected.Sources
	if len(sources) != 2 {
		t.Fatalf("sources: %+v", sources)
	}
	for i, aud := range []string{"k4all-metadata", "https://vault.example.com"} {
		sat := sources[i].ServiceAccountToken
		if sat.Audience != aud || *sat.ExpirationSeconds != 7200 || sat.Path != metadata.TokenFile(aud) {
			t.Fatalf("source %d: %+v", i, sat)
		}
	}
}

func TestPodPolicy_VendorDataConfigMap(t *testing.T) {
	pod, _ := admitPod(t, &k4allv1alpha1.MetadataSettings{Enabled: true, VendorDataConfigMap: "k4all-vendor-data"},
		launcherPod(labelled), namespace(nil))
	cm := volume(t, pod, "k4all-md-vendor").ConfigMap
	if cm.Name != "k4all-vendor-data" || cm.Optional == nil || !*cm.Optional {
		t.Fatalf("vendor-data volume: %+v", cm)
	}
	if got := mounts(container(t, pod.Spec.InitContainers, metadata.ServerContainer)); got["k4all-md-vendor"] != metadata.VendorDir {
		t.Fatalf("mounts: %v", got)
	}
	pod, _ = admitPod(t, &k4allv1alpha1.MetadataSettings{Enabled: true}, launcherPod(labelled), namespace(nil))
	for _, v := range pod.Spec.Volumes {
		if v.Name == "k4all-md-vendor" {
			t.Fatal("a vendor-data volume without a vendorDataConfigMap")
		}
	}
}

// A VirtualMachineInstance without a VM has no per-VM Secret.
func TestPodPolicy_AStandaloneVMIHasNoSecretVolume(t *testing.T) {
	pod, changed := admitPod(t, &k4allv1alpha1.MetadataSettings{Enabled: true},
		launcherPod(map[string]string{metadata.LabelOptIn: metadata.OptInEnabled, "vm.kubevirt.io/name": ""}),
		namespace(nil))
	if !changed {
		t.Fatal("not injected")
	}
	for _, v := range pod.Spec.Volumes {
		if v.Name == "k4all-md-vm" {
			t.Fatal("a Secret volume for a VMI without a VM")
		}
	}
	if _, ok := mounts(container(t, pod.Spec.InitContainers, metadata.ServerContainer))["k4all-md-vm"]; ok {
		t.Fatal("a mount of the missing Secret volume")
	}
}

// A pod that has the containers or a volume of k4all's names is left alone:
// adding them again would make the pod invalid, and the VM unable to start.
func TestPodPolicy_NeverTwiceNorOverAClash(t *testing.T) {
	md := &k4allv1alpha1.MetadataSettings{Enabled: true}
	ns := namespace(nil)
	once, _ := admitPod(t, md, launcherPod(labelled), ns)
	if _, changed := admitPod(t, md, once, ns); changed {
		t.Fatal("injected twice")
	}
	clash := launcherPod(labelled)
	clash.Spec.Volumes = append(clash.Spec.Volumes, corev1.Volume{Name: "k4all-md-pod"})
	if _, changed := admitPod(t, md, clash, ns); changed {
		t.Fatal("injected over a volume of the same name")
	}
}

// A pod KubeVirt made without init containers or volumes still gets them.
func TestPodPolicy_APodWithoutInitContainersOrVolumes(t *testing.T) {
	pod := launcherPod(labelled)
	pod.Spec.InitContainers, pod.Spec.Volumes = nil, nil
	out, changed := admitPod(t, &k4allv1alpha1.MetadataSettings{Enabled: true}, pod, namespace(nil))
	if !changed || len(out.Spec.InitContainers) != 2 || len(out.Spec.Volumes) != 3 {
		t.Fatalf("init %v volumes %v", out.Spec.InitContainers, out.Spec.Volumes)
	}
}

// --- the VMI's chassis asset tag ----------------------------------------------

func vmi(labels map[string]string, domain map[string]any) *unstructured.Unstructured {
	if domain == nil {
		domain = map[string]any{}
	}
	if _, ok := domain["devices"]; !ok {
		domain["devices"] = map[string]any{
			"interfaces": []any{map[string]any{"name": "default", "masquerade": map[string]any{}}},
		}
	}
	u := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "kubevirt.io/v1", "kind": "VirtualMachineInstance",
		"metadata": map[string]any{"name": "web", "namespace": "shop"},
		"spec":     map[string]any{"domain": domain},
	}}
	u.SetLabels(labels)
	return u
}

func admitVMI(t *testing.T, md *k4allv1alpha1.MetadataSettings, obj *unstructured.Unstructured, ns *corev1.Namespace) string {
	t.Helper()
	asset, _, _ := unstructured.NestedString(admitVMIObject(t, md, obj, ns).Object, "spec", "domain", "chassis", "asset")
	return asset
}

func admitVMIObject(t *testing.T, md *k4allv1alpha1.MetadataSettings, obj *unstructured.Unstructured,
	ns *corev1.Namespace) *unstructured.Unstructured {
	t.Helper()
	objs := mdObjects(t, md)
	out, _ := admit(t, objs["MutatingAdmissionPolicy/"+MetadataVMIPolicy], objs["MutatingAdmissionPolicyBinding/"+MetadataVMIPolicy],
		obj, vmisGVR, ns)
	return out.(*unstructured.Unstructured)
}

// vmOwned is a VMI the VM of UID vmUID started (its VMI UID: vmiUID), as
// KubeVirt's VM controller creates it: a controller ownerReference.
func vmOwned(vmUID, vmiUID string, domain map[string]any) *unstructured.Unstructured {
	u := vmi(labelled, domain)
	u.SetUID(types.UID(vmiUID))
	u.SetOwnerReferences([]metav1.OwnerReference{{
		APIVersion: "kubevirt.io/v1", Kind: "VirtualMachine", Name: "web", UID: types.UID(vmUID),
		Controller: ptr.To(true), BlockOwnerDeletion: ptr.To(true),
	}})
	return u
}

// macs are the macAddress of each interface of a VMI ("" without one).
func macs(t *testing.T, u *unstructured.Unstructured) []string {
	t.Helper()
	ifaces, _, _ := unstructured.NestedSlice(u.Object, "spec", "domain", "devices", "interfaces")
	out := make([]string, 0, len(ifaces))
	for _, i := range ifaces {
		mac, _ := i.(map[string]any)["macAddress"].(string)
		out = append(out, mac)
	}
	return out
}

const (
	vmA = "d4793485-2698-4d0d-a537-62431b51cc7e"
	vmB = "7f570bd6-4deb-4be7-b03b-cfad6320d6b0"
	// macA is vmA's NIC's MAC: 02 and the UID's first 5 bytes.
	macA = "02:d4:79:34:85:26"
)

// KubeVirt gives a NIC without macAddress a new MAC at each start, and a
// guest's network config (cloud-init's of the first boot) names the MAC:
// on a persistent disk the NIC is down after a restart. A served VM's
// masquerade NIC gets a MAC of its own VM's UID instead: locally
// administered (02:), the UID's first 5 bytes, the same at every start
// (the VMI's UID changes at each).
func TestVMIPolicy_AStableMACFromTheVMsUID(t *testing.T) {
	md := &k4allv1alpha1.MetadataSettings{Enabled: true}
	ns := namespace(nil)
	first := macs(t, admitVMIObject(t, md, vmOwned(vmA, "vmi-uid-1", nil), ns))
	again := macs(t, admitVMIObject(t, md, vmOwned(vmA, "vmi-uid-2", nil), ns))
	other := macs(t, admitVMIObject(t, md, vmOwned(vmB, "vmi-uid-3", nil), ns))
	if first[0] != macA {
		t.Fatalf("MAC %q, want %s", first[0], macA)
	}
	if again[0] != first[0] {
		t.Fatalf("the same VM restarted got %q, then %q", first[0], again[0])
	}
	if other[0] != "02:7f:57:0b:d6:4d" || other[0] == first[0] {
		t.Fatalf("another VM got %q", other[0])
	}
}

// A MAC in the VM's spec (Hangar sets one at create and clone, or
// KubeMacPool) is the VM's: never touched.
func TestVMIPolicy_AnExplicitMACIsNeverTouched(t *testing.T) {
	domain := map[string]any{"devices": map[string]any{"interfaces": []any{
		map[string]any{"name": "default", "masquerade": map[string]any{}, "macAddress": "02:aa:bb:cc:dd:ee"},
	}}}
	if got := macs(t, admitVMIObject(t, &k4allv1alpha1.MetadataSettings{Enabled: true}, vmOwned(vmA, "v1", domain), namespace(nil))); got[0] != "02:aa:bb:cc:dd:ee" {
		t.Fatalf("MAC %q", got[0])
	}
}

// Only the masquerade NIC: a bridged one keeps KubeVirt's MAC.
func TestVMIPolicy_TheMACOnlyOnTheMasqueradeNIC(t *testing.T) {
	domain := map[string]any{"devices": map[string]any{"interfaces": []any{
		map[string]any{"name": "vlan6", "bridge": map[string]any{}},
		map[string]any{"name": "default", "masquerade": map[string]any{}},
	}}}
	got := macs(t, admitVMIObject(t, &k4allv1alpha1.MetadataSettings{Enabled: true}, vmOwned(vmA, "v1", domain), namespace(nil)))
	if got[0] != "" || got[1] != macA {
		t.Fatalf("MACs %q", got)
	}
}

// A VMI without a VM has no UID that outlives it: KubeVirt's MAC stays.
// So does one whose owner is not its controlling VM.
func TestVMIPolicy_ABareVMIIsLeftAlone(t *testing.T) {
	md := &k4allv1alpha1.MetadataSettings{Enabled: true}
	bare := admitVMIObject(t, md, vmi(labelled, nil), namespace(nil))
	if got := macs(t, bare); got[0] != "" {
		t.Fatalf("a bare VMI got MAC %q", got[0])
	}
	if asset, _, _ := unstructured.NestedString(bare.Object, "spec", "domain", "chassis", "asset"); asset != metadata.ChassisAsset {
		t.Fatalf("a bare VMI's asset %q: it is served, it needs the tag", asset)
	}
	notController := vmOwned(vmA, "v1", nil)
	refs := notController.GetOwnerReferences()
	refs[0].Controller = nil
	notController.SetOwnerReferences(refs)
	if got := macs(t, admitVMIObject(t, md, notController, namespace(nil))); got[0] != "" {
		t.Fatalf("a VMI its VM does not control got MAC %q", got[0])
	}
}

// The MAC goes where the policy serves, as the asset tag.
func TestVMIPolicy_TheMACOnlyWhereThePodIsServed(t *testing.T) {
	md := &k4allv1alpha1.MetadataSettings{Enabled: true}
	unlabelled := vmOwned(vmA, "v1", nil)
	unlabelled.SetLabels(nil)
	if got := macs(t, admitVMIObject(t, md, unlabelled, namespace(nil))); got[0] != "" {
		t.Fatalf("unlabelled VMI got MAC %q", got[0])
	}
	baseline := namespace(map[string]string{"pod-security.kubernetes.io/enforce": "baseline"})
	if got := macs(t, admitVMIObject(t, md, vmOwned(vmA, "v1", nil), baseline)); got[0] != "" {
		t.Fatalf("baseline namespace: MAC %q", got[0])
	}
}

// The policy sets "OpenStack Nova" only where the VM's spec has no asset
// tag; one set by the user stays (the metadata controller puts a Warning
// Event on the VM), and the MAC is set all the same.
func TestVMIPolicy_NeverReplacesTheUsersAssetTag(t *testing.T) {
	out := admitVMIObject(t, &k4allv1alpha1.MetadataSettings{Enabled: true},
		vmOwned(vmA, "v1", map[string]any{"chassis": map[string]any{"asset": "ACME-42"}}), namespace(nil))
	if asset, _, _ := unstructured.NestedString(out.Object, "spec", "domain", "chassis", "asset"); asset != "ACME-42" {
		t.Fatalf("the user's asset became %q", asset)
	}
	if got := macs(t, out); got[0] != macA {
		t.Fatalf("MAC %q", got[0])
	}
}

// Without the asset tag cloud-init's ds-identify never looks for an
// OpenStack metadata service (prototype: no request arrives).
func TestVMIPolicy_DefaultsTheChassisAssetTag(t *testing.T) {
	md := &k4allv1alpha1.MetadataSettings{Enabled: true}
	ns := namespace(nil)
	if got := admitVMI(t, md, vmi(labelled, nil), ns); got != metadata.ChassisAsset {
		t.Fatalf("no chassis: asset %q", got)
	}
	withChassis := vmi(labelled, map[string]any{"chassis": map[string]any{"manufacturer": "ACME"}})
	if got := admitVMI(t, md, withChassis, ns); got != metadata.ChassisAsset {
		t.Fatalf("chassis without an asset: asset %q", got)
	}
	if m, _, _ := unstructured.NestedString(withChassis.Object, "spec", "domain", "chassis", "manufacturer"); m != "ACME" {
		t.Fatal("the chassis lost its manufacturer")
	}
	if got := admitVMI(t, md, vmi(labelled, map[string]any{"chassis": map[string]any{"asset": "mine"}}), ns); got != "mine" {
		t.Fatalf("an asset set by the user became %q", got)
	}
}

// The tag goes only where the pod gets the service: a tag without it makes
// cloud-init wait for a server that is not there.
func TestVMIPolicy_OnlyWhereThePodIsServed(t *testing.T) {
	md := &k4allv1alpha1.MetadataSettings{Enabled: true}
	if got := admitVMI(t, md, vmi(nil, nil), namespace(nil)); got != "" {
		t.Fatalf("unlabelled VMI tagged %q", got)
	}
	if got := admitVMI(t, md, vmi(labelled, nil), namespace(
		map[string]string{"pod-security.kubernetes.io/enforce": "baseline"})); got != "" {
		t.Fatalf("baseline namespace: tagged %q", got)
	}
	bridged := vmi(labelled, map[string]any{"devices": map[string]any{
		"interfaces": []any{map[string]any{"name": "vlan6", "bridge": map[string]any{}}},
	}})
	if got := admitVMI(t, md, bridged, namespace(nil)); got != "" {
		t.Fatalf("bridge-only VMI tagged %q", got)
	}
	if got := admitVMI(t, &k4allv1alpha1.MetadataSettings{Enabled: true, Scope: k4allv1alpha1.MetadataScopeAll},
		vmi(nil, nil), namespace(nil)); got != metadata.ChassisAsset {
		t.Fatalf("scope All: asset %q", got)
	}
	// No interface listed: KubeVirt adds its default one after the
	// policy, with the cluster's default binding (bridge unless set).
	if got := admitVMI(t, md, vmi(labelled, map[string]any{"devices": map[string]any{}}), namespace(nil)); got != "" {
		t.Fatalf("no interface listed: tagged %q", got)
	}
}

// --- settings ------------------------------------------------------------------

func TestMetadataSettings_Defaults(t *testing.T) {
	s := MetadataSettingsFrom(mdConfig(&k4allv1alpha1.MetadataSettings{Enabled: true}))
	if s.Scope != k4allv1alpha1.MetadataScopeLabelled || len(s.Audiences) != 1 || s.Audiences[0] != metadata.DefaultAudience ||
		s.ExpirationSeconds != metadata.DefaultExpirationSeconds || s.VendorDataConfigMap != "" || !s.Enabled {
		t.Fatalf("%+v", s)
	}
	if s := MetadataSettingsFrom(mdConfig(nil)); s.Enabled {
		t.Fatalf("absent metadata is enabled: %+v", s)
	}
	off := mdConfig(&k4allv1alpha1.MetadataSettings{Enabled: true})
	off.Features.Virt.Enabled = false
	if s := MetadataSettingsFrom(off); s.Enabled {
		t.Fatalf("metadata without virt is enabled: %+v", s)
	}
}

// --- the hook -------------------------------------------------------------------

func mdSpec() k4allv1alpha1.ComponentSpec {
	return k4allv1alpha1.ComponentSpec{
		Type: k4allv1alpha1.ComponentTypeManifest, Version: "v0.6.0",
		Image: "ghcr.io/gpillon/k4all-metadata:{{ version }}",
	}
}

type applied struct {
	key, owner string
	force      bool
	image      string
}

// recordingClient records the server-side applies and deletes of a hook.
func recordingClient(t *testing.T, patchErr error) (client.Client, *[]applied, *[]string) {
	t.Helper()
	var applies []applied
	var deletes []string
	c := interceptor.NewClient(fake.NewClientBuilder().Build(), interceptor.Funcs{
		Patch: func(_ context.Context, _ client.WithWatch, obj client.Object, p client.Patch, opts ...client.PatchOption) error {
			if p.Type() != "application/apply-patch+yaml" {
				t.Fatalf("patch type %s, want a server-side apply", p.Type())
			}
			po := &client.PatchOptions{}
			po.ApplyOptions(opts)
			u := obj.(*unstructured.Unstructured)
			image := ""
			if strings.Contains(fmt.Sprint(u.Object), mdImage) {
				image = mdImage
			}
			applies = append(applies, applied{key: u.GetKind() + "/" + u.GetName(), owner: po.FieldManager,
				force: po.Force != nil && *po.Force, image: image})
			return patchErr
		},
		Delete: func(_ context.Context, _ client.WithWatch, obj client.Object, _ ...client.DeleteOption) error {
			u := obj.(*unstructured.Unstructured)
			deletes = append(deletes, u.GetKind()+"/"+u.GetName())
			return apierrors.NewNotFound(schema.GroupResource{}, u.GetName())
		},
	})
	return c, &applies, &deletes
}

func TestMetadataHook_SyncAppliesThePoliciesAsK4all(t *testing.T) {
	c, applies, _ := recordingClient(t, nil)
	h := NewMetadataHook(c, logr.Discard())
	if _, err := h.Sync(context.Background(), mdSpec(), mdConfig(&k4allv1alpha1.MetadataSettings{Enabled: true})); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"MutatingAdmissionPolicy/k4all-metadata", "MutatingAdmissionPolicyBinding/k4all-metadata",
		"MutatingAdmissionPolicy/k4all-metadata-vmi", "MutatingAdmissionPolicyBinding/k4all-metadata-vmi",
	}
	if len(*applies) != len(want) {
		t.Fatalf("applied %v", *applies)
	}
	for i, a := range *applies {
		if a.key != want[i] || a.owner != "k4all-operator" || !a.force {
			t.Fatalf("apply %d: %+v, want %s by k4all-operator, forced", i, a, want[i])
		}
	}
	// the component's image, its {{ version }} filled in
	if (*applies)[0].image != mdImage {
		t.Fatalf("the pod policy does not run %s", mdImage)
	}
}

func TestMetadataHook_AComponentWithoutImageFails(t *testing.T) {
	c, applies, _ := recordingClient(t, nil)
	spec := mdSpec()
	spec.Image = ""
	_, err := NewMetadataHook(c, logr.Discard()).Sync(context.Background(), spec,
		mdConfig(&k4allv1alpha1.MetadataSettings{Enabled: true}))
	if err == nil || len(*applies) != 0 {
		t.Fatalf("err %v, applies %v", err, *applies)
	}
}

// A token the API server accepts, served to a guest, would let it call the
// API as its ServiceAccount: such an audience is refused, and the policy
// in place stays as it was.
func TestMetadataHook_RefusesTheAPIServersAudience(t *testing.T) {
	for _, aud := range []string{"https://kubernetes.default.svc.cluster.local", "https://kubernetes.default.svc", "kubernetes"} {
		c, applies, _ := recordingClient(t, nil)
		_, err := NewMetadataHook(c, logr.Discard()).Sync(context.Background(), mdSpec(),
			mdConfig(&k4allv1alpha1.MetadataSettings{Enabled: true, Tokens: &k4allv1alpha1.MetadataTokens{
				Audiences: []string{"k4all-metadata", aud},
			}}))
		if err == nil || !strings.Contains(err.Error(), aud) || len(*applies) != 0 {
			t.Fatalf("%s: err %v, applies %v", aud, err, *applies)
		}
	}
}

func TestMetadataHook_SaysWhenTheAPIServerHasNoMutatingAdmissionPolicy(t *testing.T) {
	c, _, _ := recordingClient(t, &meta.NoKindMatchError{
		GroupKind: schema.GroupKind{Group: "admissionregistration.k8s.io", Kind: "MutatingAdmissionPolicy"},
	})
	_, err := NewMetadataHook(c, logr.Discard()).Sync(context.Background(), mdSpec(),
		mdConfig(&k4allv1alpha1.MetadataSettings{Enabled: true}))
	if err == nil || !strings.Contains(err.Error(), "Kubernetes 1.36") {
		t.Fatalf("err %v", err)
	}
}

// Turning metadata (or virt) off deletes the policies: pods created after
// get nothing; running VMs keep their server until they restart.
func TestMetadataHook_CleanupDeletesThePolicies(t *testing.T) {
	c, _, deletes := recordingClient(t, nil)
	if err := NewMetadataHook(c, logr.Discard()).Cleanup(context.Background(), "k4all-metadata",
		k4allv1alpha1.ClusterConfigSpec{}); err != nil {
		t.Fatal(err)
	}
	want := "MutatingAdmissionPolicyBinding/k4all-metadata MutatingAdmissionPolicy/k4all-metadata " +
		"MutatingAdmissionPolicyBinding/k4all-metadata-vmi MutatingAdmissionPolicy/k4all-metadata-vmi"
	if got := strings.Join(*deletes, " "); got != want {
		t.Fatalf("deleted %s", got)
	}
}

func TestMetadataHook_IsRegistered(t *testing.T) {
	if _, ok := NewRegistry(fake.NewClientBuilder().Build(), logr.Discard()).Get(MetadataComponent).(*MetadataHook); !ok {
		t.Fatal("k4all-metadata has no MetadataHook")
	}
}
