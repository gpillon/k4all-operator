package controller

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/config"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	k4allv1alpha1 "github.com/gpillon/k4all-operator/api/v1alpha1"
	"github.com/gpillon/k4all-operator/internal/controller/hooks"
	"github.com/gpillon/k4all-operator/internal/metadata"
)

var mdNamespaceSeq int

// ctrlConfigSkipNameValidation lets each test's manager have its own
// controller named "metadata".
func ctrlConfigSkipNameValidation() config.Controller {
	return config.Controller{SkipNameValidation: ptr.To(true)}
}

// mdEnv is the shared envtest with KubeVirt's VirtualMachine kind, a fresh
// namespace (labels: its PodSecurity) and a MetadataReconciler with the
// settings given, reading as the operator does (no Secret cache).
func mdEnv(t *testing.T, nsLabels map[string]string, md *k4allv1alpha1.MetadataSettings) (client.Client, *MetadataReconciler, string, *record.FakeRecorder) {
	t.Helper()
	virtWorkloadAPIs(t)
	c := envtestClient(t)
	mdNamespaceSeq++
	ns := fmt.Sprintf("md-%d-%d", time.Now().UnixNano()%100000, mdNamespaceSeq)
	createAll(t, c, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns, Labels: nsLabels}})
	rec := record.NewFakeRecorder(100)
	r := &MetadataReconciler{
		Client: c, Reader: c, APIReader: c, Recorder: rec, Log: logr.Discard(),
		Settings: func(context.Context) (hooks.MetadataSettings, error) {
			var spec k4allv1alpha1.ClusterConfigSpec
			spec.Features.Virt.Enabled = true
			spec.Features.Virt.Metadata = md
			return hooks.MetadataSettingsFrom(spec), nil
		},
	}
	return c, r, ns, rec
}

var mdOn = &k4allv1alpha1.MetadataSettings{Enabled: true}

// testVM is VM name in ns, labelled for metadata, with the template spec.
func testVM(ns, name string, labels map[string]string, spec map[string]any) *unstructured.Unstructured {
	vm := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "kubevirt.io/v1", "kind": "VirtualMachine",
		"metadata": map[string]any{"name": name, "namespace": ns},
		"spec": map[string]any{
			"runStrategy": "Always",
			"template": map[string]any{
				"metadata": map[string]any{"labels": toAny(labels)},
				"spec":     spec,
			},
		},
	}}
	return vm
}

func toAny(m map[string]string) map[string]any {
	out := map[string]any{}
	for k, v := range m {
		out[k] = v
	}
	return out
}

func noCloud(fields map[string]any) map[string]any {
	return map[string]any{
		"domain":  map[string]any{"devices": map[string]any{}},
		"volumes": []any{map[string]any{"name": "cloudinit", "cloudInitNoCloud": fields}},
	}
}

func reconcileVM(t *testing.T, r *MetadataReconciler, ns, name string) ctrl.Result {
	t.Helper()
	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: name}})
	if err != nil {
		t.Fatalf("reconcile %s/%s: %v", ns, name, err)
	}
	return res
}

func mdSecret(t *testing.T, c client.Client, ns, vm string) *corev1.Secret {
	t.Helper()
	s := &corev1.Secret{}
	err := c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: metadata.SecretName(vm)}, s)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func events(rec *record.FakeRecorder) []string {
	var out []string
	for {
		select {
		case e := <-rec.Events:
			out = append(out, e)
		default:
			return out
		}
	}
}

func optIn() map[string]string {
	return map[string]string{metadata.LabelOptIn: metadata.OptInEnabled}
}

// The VM's Secret has what the server serves, from the VM's own spec: its
// UID as instance-id, the hostname, the user-data of its cloud-init
// volume, the SSH keys of its accessCredentials. It goes with the VM.
func TestMetadataSecret_FromTheVMSpec(t *testing.T) {
	c, r, ns, _ := mdEnv(t, nil, mdOn)
	keys := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "web-keys", Namespace: ns},
		Data: map[string][]byte{
			"alice": []byte("ssh-ed25519 AAAAone alice@laptop\n"),
			"team":  []byte("ssh-rsa AAAAtwo bob\n\n# old key\nssh-ed25519 AAAAthree carol\n"),
		},
	}
	spec := noCloud(map[string]any{"userData": "#cloud-config\nruncmd: [[touch, /ran]]\n"})
	spec["hostname"] = "web-1"
	spec["accessCredentials"] = []any{map[string]any{"sshPublicKey": map[string]any{
		"source":            map[string]any{"secret": map[string]any{"secretName": "web-keys"}},
		"propagationMethod": map[string]any{"qemuGuestAgent": map[string]any{"users": []any{"fedora"}}},
	}}}
	vm := testVM(ns, "web", optIn(), spec)
	createAll(t, c, keys, vm)

	if res := reconcileVM(t, r, ns, "web"); res.RequeueAfter <= 0 || res.RequeueAfter > 15*time.Minute {
		t.Fatalf("requeue %v: the referenced Secrets are read again from time to time", res.RequeueAfter)
	}
	s := mdSecret(t, c, ns, "web")
	if s == nil {
		t.Fatal("no Secret")
	}
	want := map[string]string{
		metadata.KeyInstanceID: string(vm.GetUID()),
		metadata.KeyName:       "web",
		metadata.KeyHostname:   "web-1",
		metadata.KeyUserData:   "#cloud-config\nruncmd: [[touch, /ran]]\n",
		metadata.KeySSHKeys:    "ssh-ed25519 AAAAone alice@laptop\nssh-rsa AAAAtwo bob\nssh-ed25519 AAAAthree carol\n",
	}
	for k, v := range want {
		if string(s.Data[k]) != v {
			t.Errorf("%s = %q, want %q", k, s.Data[k], v)
		}
	}
	if len(s.Data) != len(want) {
		t.Errorf("keys %v", s.Data)
	}
	if s.Type != metadata.SecretType || s.Labels["app.kubernetes.io/managed-by"] != metadataFieldOwner ||
		s.Labels[metadataVMLabel] != "web" {
		t.Fatalf("type %q labels %v", s.Type, s.Labels)
	}
	if len(s.OwnerReferences) != 1 || s.OwnerReferences[0].UID != vm.GetUID() || s.OwnerReferences[0].Kind != "VirtualMachine" ||
		s.OwnerReferences[0].Controller == nil || !*s.OwnerReferences[0].Controller {
		t.Fatalf("owner references %+v", s.OwnerReferences)
	}
	if _, ok := s.Annotations[metadataMessageAnnotation]; ok {
		t.Fatalf("message on a complete Secret: %v", s.Annotations)
	}
	for _, m := range s.ManagedFields {
		if m.Manager != "k4all-operator" {
			t.Fatalf("written by %q", m.Manager)
		}
	}
}

func TestMetadataSecret_UserDataSources(t *testing.T) {
	c, r, ns, _ := mdEnv(t, nil, mdOn)
	createAll(t, c,
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "ud-lower", Namespace: ns},
			Data: map[string][]byte{"userdata": []byte("#!/bin/sh\necho lower\n")}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "ud-camel", Namespace: ns},
			Data: map[string][]byte{"userData": []byte("#!/bin/sh\necho camel\n")}},
	)
	cases := map[string]struct {
		spec map[string]any
		want string
	}{
		"base64": {noCloud(map[string]any{"userDataBase64": base64.StdEncoding.EncodeToString([]byte("#cloud-config\n{}\n"))}),
			"#cloud-config\n{}\n"},
		"secretRef userdata": {noCloud(map[string]any{"secretRef": map[string]any{"name": "ud-lower"}}), "#!/bin/sh\necho lower\n"},
		"secretRef userData": {noCloud(map[string]any{"secretRef": map[string]any{"name": "ud-camel"}}), "#!/bin/sh\necho camel\n"},
		"configDrive": {map[string]any{"volumes": []any{map[string]any{"name": "cd",
			"cloudInitConfigDrive": map[string]any{"userData": "#cloud-config\nconfig-drive: true\n"}}}},
			"#cloud-config\nconfig-drive: true\n"},
	}
	i := 0
	for name, tc := range cases {
		i++
		vm := fmt.Sprintf("vm-%d", i)
		createAll(t, c, testVM(ns, vm, optIn(), tc.spec))
		reconcileVM(t, r, ns, vm)
		if s := mdSecret(t, c, ns, vm); s == nil || string(s.Data[metadata.KeyUserData]) != tc.want {
			t.Errorf("%s: secret %+v", name, s)
		}
	}
	// No cloud-init volume: no user-data, no key.
	createAll(t, c, testVM(ns, "bare", optIn(), map[string]any{"domain": map[string]any{}}))
	reconcileVM(t, r, ns, "bare")
	if s := mdSecret(t, c, ns, "bare"); s == nil || s.Data[metadata.KeyUserData] != nil || s.Data[metadata.KeySSHKeys] != nil {
		t.Fatalf("bare VM: %+v", s)
	}
}

// The instance-id is the VM's UID, not the VMI's: the same at every boot,
// so cloud-init does its first boot (SSH host keys) once.
func TestMetadataSecret_InstanceIDStaysAcrossRestarts(t *testing.T) {
	c, r, ns, _ := mdEnv(t, nil, mdOn)
	vm := testVM(ns, "web", optIn(), noCloud(map[string]any{"userData": "#cloud-config\n"}))
	createAll(t, c, vm)
	reconcileVM(t, r, ns, "web")
	first := string(mdSecret(t, c, ns, "web").Data[metadata.KeyInstanceID])

	// a restart: a new VMI (new UID), the VM's status moves
	for _, uid := range []string{"vmi-uid-1", "vmi-uid-2"} {
		vmi := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "kubevirt.io/v1", "kind": "VirtualMachineInstance",
			"metadata": map[string]any{"name": "web", "namespace": ns, "labels": map[string]any{"kubevirt.io/created-by": uid}},
		}}
		createAll(t, c, vmi)
		reconcileVM(t, r, ns, "web")
		if got := string(mdSecret(t, c, ns, "web").Data[metadata.KeyInstanceID]); got != first || got != string(vm.GetUID()) {
			t.Fatalf("instance-id %q, want the VM's UID %q", got, vm.GetUID())
		}
		if err := deleteNow(c, vmi); err != nil {
			t.Fatal(err)
		}
	}
}

func TestMetadataSecret_FollowsTheVM(t *testing.T) {
	c, r, ns, _ := mdEnv(t, nil, mdOn)
	vm := testVM(ns, "web", optIn(), noCloud(map[string]any{"userData": "#cloud-config\nv: 1\n"}))
	createAll(t, c, vm)
	reconcileVM(t, r, ns, "web")
	rv := mdSecret(t, c, ns, "web").ResourceVersion

	// nothing changed: nothing written
	reconcileVM(t, r, ns, "web")
	if got := mdSecret(t, c, ns, "web").ResourceVersion; got != rv {
		t.Fatal("the Secret was written again with nothing changed")
	}

	ctx := context.Background()
	if err := c.Get(ctx, client.ObjectKeyFromObject(vm), vm); err != nil {
		t.Fatal(err)
	}
	if err := unstructured.SetNestedSlice(vm.Object, []any{map[string]any{"name": "cloudinit",
		"cloudInitNoCloud": map[string]any{"userData": "#cloud-config\nv: 2\n"}}}, "spec", "template", "spec", "volumes"); err != nil {
		t.Fatal(err)
	}
	if err := c.Update(ctx, vm); err != nil {
		t.Fatal(err)
	}
	reconcileVM(t, r, ns, "web")
	if got := string(mdSecret(t, c, ns, "web").Data[metadata.KeyUserData]); got != "#cloud-config\nv: 2\n" {
		t.Fatalf("user-data %q", got)
	}

	// out of scope (label gone): the Secret goes
	if err := c.Get(ctx, client.ObjectKeyFromObject(vm), vm); err != nil {
		t.Fatal(err)
	}
	unstructured.RemoveNestedField(vm.Object, "spec", "template", "metadata", "labels")
	if err := c.Update(ctx, vm); err != nil {
		t.Fatal(err)
	}
	reconcileVM(t, r, ns, "web")
	if s := mdSecret(t, c, ns, "web"); s != nil {
		t.Fatal("the Secret of a VM out of scope stays")
	}
	// a VM deleted: nothing to do (the Secret's owner reference takes it)
	if err := deleteNow(c, vm); err != nil {
		t.Fatal(err)
	}
	reconcileVM(t, r, ns, "web")
}

// A user-data Secret that is not there: the VM gets its identity and keys
// without user-data, and its Secret and an Event say what is missing (for
// Hangar's VM page); it is read again soon, and once there, served.
func TestMetadataSecret_AMissingUserDataSecretIsSaid(t *testing.T) {
	c, r, ns, rec := mdEnv(t, nil, mdOn)
	createAll(t, c, testVM(ns, "web", optIn(), noCloud(map[string]any{"secretRef": map[string]any{"name": "web-ud"}})))
	res := reconcileVM(t, r, ns, "web")
	s := mdSecret(t, c, ns, "web")
	if s == nil || s.Data[metadata.KeyInstanceID] == nil || s.Data[metadata.KeyUserData] != nil {
		t.Fatalf("secret %+v", s)
	}
	msg := s.Annotations[metadataMessageAnnotation]
	if !strings.Contains(msg, `user-data Secret "web-ud" not found`) {
		t.Fatalf("message %q", msg)
	}
	if res.RequeueAfter <= 0 || res.RequeueAfter > time.Minute {
		t.Fatalf("requeue %v", res.RequeueAfter)
	}
	ev := events(rec)
	if len(ev) != 1 || !strings.HasPrefix(ev[0], "Warning MetadataIncomplete") || !strings.Contains(ev[0], "web-ud") {
		t.Fatalf("events %v", ev)
	}
	// the same again: no second Event
	reconcileVM(t, r, ns, "web")
	if ev := events(rec); len(ev) != 0 {
		t.Fatalf("events again %v", ev)
	}

	createAll(t, c, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "web-ud", Namespace: ns},
		Data: map[string][]byte{"userdata": []byte("#cloud-config\n")}})
	reconcileVM(t, r, ns, "web")
	s = mdSecret(t, c, ns, "web")
	if string(s.Data[metadata.KeyUserData]) != "#cloud-config\n" {
		t.Fatalf("user-data %q", s.Data[metadata.KeyUserData])
	}
	if _, ok := s.Annotations[metadataMessageAnnotation]; ok {
		t.Fatalf("message stays: %v", s.Annotations)
	}
}

func TestMetadataSecret_MissingKeySecretIsSaid(t *testing.T) {
	c, r, ns, _ := mdEnv(t, nil, mdOn)
	spec := map[string]any{"accessCredentials": []any{map[string]any{"sshPublicKey": map[string]any{
		"source": map[string]any{"secret": map[string]any{"secretName": "gone"}},
	}}}}
	createAll(t, c, testVM(ns, "web", optIn(), spec))
	reconcileVM(t, r, ns, "web")
	if msg := mdSecret(t, c, ns, "web").Annotations[metadataMessageAnnotation]; !strings.Contains(msg, `SSH key Secret "gone" not found`) {
		t.Fatalf("message %q", msg)
	}
}

func TestMetadataSecret_Scope(t *testing.T) {
	c, r, ns, _ := mdEnv(t, nil, mdOn)
	createAll(t, c, testVM(ns, "plain", nil, map[string]any{}))
	reconcileVM(t, r, ns, "plain")
	if mdSecret(t, c, ns, "plain") != nil {
		t.Fatal("scope Labelled: an unlabelled VM got a Secret")
	}

	c, r, ns, _ = mdEnv(t, nil, &k4allv1alpha1.MetadataSettings{Enabled: true, Scope: k4allv1alpha1.MetadataScopeAll})
	createAll(t, c, testVM(ns, "plain", nil, map[string]any{}),
		testVM(ns, "out", map[string]string{metadata.LabelOptIn: metadata.OptInDisabled}, map[string]any{}))
	reconcileVM(t, r, ns, "plain")
	reconcileVM(t, r, ns, "out")
	if mdSecret(t, c, ns, "plain") == nil {
		t.Fatal("scope All: no Secret")
	}
	if mdSecret(t, c, ns, "out") != nil {
		t.Fatal("scope All: a VM labelled disabled got a Secret")
	}
}

// Where PodSecurity forbids NET_ADMIN the policy does not inject (D8): a
// VM that asked for metadata gets an Event saying why (Hangar shows it),
// once, and no Secret.
func TestMetadataSecret_ABaselineNamespaceGetsAnEventInstead(t *testing.T) {
	for _, level := range []string{"baseline", "restricted"} {
		c, r, ns, rec := mdEnv(t, map[string]string{"pod-security.kubernetes.io/enforce": level}, mdOn)
		createAll(t, c, testVM(ns, "web", optIn(), map[string]any{}))
		reconcileVM(t, r, ns, "web")
		reconcileVM(t, r, ns, "web")
		if mdSecret(t, c, ns, "web") != nil {
			t.Fatalf("%s: a Secret", level)
		}
		ev := events(rec)
		if len(ev) != 1 || !strings.HasPrefix(ev[0], "Warning MetadataNotServed") || !strings.Contains(ev[0], level) ||
			!strings.Contains(ev[0], "cloud-init disk") {
			t.Fatalf("%s: events %v", level, ev)
		}
	}
}

// A Secret of that name that k4all did not make is left alone (R7).
func TestMetadataSecret_LeavesAnotherSecretOfTheName(t *testing.T) {
	c, r, ns, rec := mdEnv(t, nil, mdOn)
	mine := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: metadata.SecretName("web"), Namespace: ns},
		Data: map[string][]byte{"mine": []byte("x")}}
	createAll(t, c, mine, testVM(ns, "web", optIn(), map[string]any{}))
	reconcileVM(t, r, ns, "web")
	s := mdSecret(t, c, ns, "web")
	if string(s.Data["mine"]) != "x" || len(s.Data) != 1 {
		t.Fatalf("the user's Secret was written: %v", s.Data)
	}
	if ev := events(rec); len(ev) != 1 || !strings.Contains(ev[0], "not k4all's") {
		t.Fatalf("events %v", ev)
	}
	// out of scope: still not deleted
	r.Settings = func(context.Context) (hooks.MetadataSettings, error) { return hooks.MetadataSettings{}, nil }
	reconcileVM(t, r, ns, "web")
	if mdSecret(t, c, ns, "web") == nil {
		t.Fatal("the user's Secret was deleted")
	}
}

func TestMetadataSecret_GoneWhenMetadataIsOff(t *testing.T) {
	c, r, ns, _ := mdEnv(t, nil, mdOn)
	createAll(t, c, testVM(ns, "web", optIn(), map[string]any{}))
	reconcileVM(t, r, ns, "web")
	r.Settings = func(context.Context) (hooks.MetadataSettings, error) { return hooks.MetadataSettings{}, nil }
	reconcileVM(t, r, ns, "web")
	if mdSecret(t, c, ns, "web") != nil {
		t.Fatal("metadata off: the Secret stays")
	}
}

// With the manager: the VM watch starts once KubeVirt serves VMs (an
// operator may start before KubeVirt), and a new VM gets its Secret.
func TestMetadataController_StartsWatchingWhenKubeVirtArrives(t *testing.T) {
	cfg := envtestConfig(t)
	c := envtestClient(t)
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme: gateScheme(t), Metrics: metricsserver.Options{BindAddress: "0"}, HealthProbeBindAddress: "0",
		Controller: ctrlConfigSkipNameValidation(),
	})
	if err != nil {
		t.Fatal(err)
	}
	r := &MetadataReconciler{
		Client: mgr.GetClient(), Reader: mgr.GetCache(), APIReader: mgr.GetAPIReader(),
		Recorder: record.NewFakeRecorder(100), Log: logr.Discard(), APIPollInterval: 200 * time.Millisecond,
		Settings: func(context.Context) (hooks.MetadataSettings, error) {
			var spec k4allv1alpha1.ClusterConfigSpec
			spec.Features.Virt.Enabled = true
			spec.Features.Virt.Metadata = mdOn
			return hooks.MetadataSettingsFrom(spec), nil
		},
	}
	if err := r.SetupWithManager(mgr); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- mgr.Start(ctx) }()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("manager: %v", err)
		}
	}()
	// The controller asks for the VirtualMachine kind until it is there.
	time.Sleep(time.Second)

	virtWorkloadAPIs(t)
	ns := fmt.Sprintf("md-watch-%d", time.Now().UnixNano()%100000)
	createAll(t, c, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
	vm := testVM(ns, "web", optIn(), noCloud(map[string]any{"userData": "#cloud-config\n"}))
	err = wait.PollUntilContextTimeout(context.Background(), 200*time.Millisecond, 30*time.Second, true,
		func(ctx context.Context) (bool, error) {
			if vm.GetUID() == "" {
				// the CRD may need a moment to be served
				return c.Create(ctx, vm) == nil, nil
			}
			s := &corev1.Secret{}
			err := c.Get(ctx, types.NamespacedName{Namespace: ns, Name: metadata.SecretName("web")}, s)
			return err == nil && string(s.Data[metadata.KeyInstanceID]) == string(vm.GetUID()), nil
		})
	if err != nil {
		t.Fatalf("no Secret for the VM: %v", err)
	}
	if err := deleteNow(c, vm); err != nil {
		t.Fatal(err)
	}
}
