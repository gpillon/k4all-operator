package hooks

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	k4allv1alpha1 "github.com/gpillon/k4all-operator/api/v1alpha1"
)

// The node agent's manifest as Hangar v0.1.0's GitHub Release ships it
// (hangar-node-agent.yaml: `kubectl kustomize distribution/node-agent`).
var hangarNodeAgentBundle = filepath.Join("testdata", "upstream", "hangar", "v0.1.0", "hangar-node-agent.yaml")

// hangarNodeAgentRelease serves the bundle with its namespace renamed ns, as
// GitHub serves the release asset: <version>/hangar-node-agent.yaml. envtest
// never finishes deleting a namespace, so each test has its own.
func hangarNodeAgentRelease(t *testing.T, ns string) (k4allv1alpha1.ComponentSpec, string) {
	t.Helper()
	raw, err := os.ReadFile(hangarNodeAgentBundle)
	if err != nil {
		t.Fatal(err)
	}
	bundle := strings.ReplaceAll(string(raw), "hangar-system", ns)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v0.1.0/hangar-node-agent.yaml" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(bundle))
	}))
	t.Cleanup(srv.Close)
	return k4allv1alpha1.ComponentSpec{
		Type: k4allv1alpha1.ComponentTypeManifest, Version: "v0.1.0", FeatureGate: "virt",
		Source: srv.URL + "/{{ version }}/hangar-node-agent.yaml",
	}, bundle
}

// installHangarNodeAgent creates the objects of the bundle, as an apply of
// the component left them.
func installHangarNodeAgent(t *testing.T, c client.Client, bundle string) {
	t.Helper()
	for _, doc := range strings.Split(bundle, "\n---") {
		obj := newObject("", "", "", "")
		if err := yaml.Unmarshal([]byte(doc), &obj.Object); err != nil {
			t.Fatal(err)
		}
		if obj.GetKind() == "" {
			continue
		}
		if err := c.Create(context.Background(), obj); err != nil {
			t.Fatalf("create %s %s: %v", obj.GetKind(), obj.GetName(), err)
		}
	}
}

func hangarNodeAgentHook(t *testing.T, c client.Client) *HangarNodeAgentHook {
	t.Helper()
	_, cfg := envtestClient(t)
	h := NewHangarNodeAgentHook(c, logr.Discard())
	h.restConfig, h.settle, h.poll = cfg, 2*time.Second, 200*time.Millisecond
	return h
}

// The agent's own objects (the DaemonSet, its ServiceAccount and script
// ConfigMap, the Role for host devices) go; with nothing else in it, so
// does the namespace.
func TestHangarNodeAgentRemoval_DeletesTheAgentAndItsNamespace(t *testing.T) {
	c, _ := envtestClient(t)
	ns := "hangar-agent-alone"
	spec, bundle := hangarNodeAgentRelease(t, ns)
	installHangarNodeAgent(t, c, bundle)

	if err := hangarNodeAgentHook(t, c).Remove(context.Background(), spec, k4allv1alpha1.ClusterConfigSpec{}); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	for _, obj := range []client.Object{
		&appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "hangar-node-agent"}},
		&corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "hangar-node-agent"}},
		&rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "hangar-hardware-admin"}},
	} {
		if exists(t, c, obj) {
			t.Errorf("%T %s left", obj, obj.GetName())
		}
	}
	if scripts := agentScripts(t, c, ns); len(scripts) != 0 {
		t.Errorf("script ConfigMaps left: %v", scripts)
	}
	if !deleting(t, c, ns) {
		t.Errorf("namespace %s kept, holding only what the agent left", ns)
	}
}

// R7: hangar-passthrough is Hangar's (the PCI devices given to VMs, one key
// per host), and an admin's RoleBinding of hangar-hardware-admin is the
// admin's. Removing the agent (nodeAgent false, or virt off) keeps them and
// their namespace: turning the agent on again finds the devices as they
// were.
func TestHangarNodeAgentRemoval_KeepsTheNamespaceWithHangarsConfigMap(t *testing.T) {
	c, _ := envtestClient(t)
	ns := "hangar-agent-passthrough"
	spec, bundle := hangarNodeAgentRelease(t, ns)
	installHangarNodeAgent(t, c, bundle)
	passthrough := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "hangar-passthrough",
			Labels: map[string]string{"app.kubernetes.io/part-of": "hangar"}},
		Data: map[string]string{"node-1": "0000:00:02.0\n"},
	}
	binding := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "hardware-admins"},
		RoleRef:    rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "Role", Name: "hangar-hardware-admin"},
		Subjects:   []rbacv1.Subject{{APIGroup: "rbac.authorization.k8s.io", Kind: "Group", Name: "ops"}},
	}
	for _, obj := range []client.Object{passthrough, binding} {
		if err := c.Create(context.Background(), obj); err != nil {
			t.Fatal(err)
		}
	}

	if err := hangarNodeAgentHook(t, c).Remove(context.Background(), spec, k4allv1alpha1.ClusterConfigSpec{}); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	if deleting(t, c, ns) {
		t.Errorf("namespace %s deleted with hangar-passthrough in it", ns)
	}
	for _, obj := range []client.Object{passthrough, binding} {
		if !exists(t, c, obj) {
			t.Errorf("%T %s deleted", obj, obj.GetName())
		}
	}
	if exists(t, c, &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "hangar-node-agent"}}) {
		t.Error("the agent's DaemonSet left")
	}
}

// kustomize names the script ConfigMap after its content: one left by an
// earlier version of the agent goes too, by its label and name.
func TestHangarNodeAgentRemoval_DeletesTheScriptsOfEarlierVersions(t *testing.T) {
	c, _ := envtestClient(t)
	ns := "hangar-agent-older"
	spec, bundle := hangarNodeAgentRelease(t, ns)
	installHangarNodeAgent(t, c, bundle)
	older := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "hangar-node-agent-scripts-5h2f8m9k7c",
		Labels: map[string]string{"app.kubernetes.io/name": "hangar-node-agent", "app.kubernetes.io/part-of": "hangar"}}}
	if err := c.Create(context.Background(), older); err != nil {
		t.Fatal(err)
	}

	if err := hangarNodeAgentHook(t, c).Remove(context.Background(), spec, k4allv1alpha1.ClusterConfigSpec{}); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	if scripts := agentScripts(t, c, ns); len(scripts) != 0 {
		t.Errorf("script ConfigMaps left: %v", scripts)
	}
	if !deleting(t, c, ns) {
		t.Errorf("namespace %s kept, holding only what the agent left", ns)
	}
}

// An object the component adds as an inline manifest (a release's own
// addition to Hangar's file) is the component's too: it goes with the rest.
func TestHangarNodeAgentRemoval_DeletesItsInlineObjects(t *testing.T) {
	c, _ := envtestClient(t)
	ns := "hangar-agent-inline"
	spec, bundle := hangarNodeAgentRelease(t, ns)
	installHangarNodeAgent(t, c, bundle)
	inline := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "k4all-agent-settings"}}
	if err := c.Create(context.Background(), inline); err != nil {
		t.Fatal(err)
	}
	spec.InlineManifests = []string{"apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: k4all-agent-settings\n  namespace: " + ns + "\n"}

	if err := hangarNodeAgentHook(t, c).Remove(context.Background(), spec, k4allv1alpha1.ClusterConfigSpec{}); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	if exists(t, c, inline) {
		t.Error("the component's inline ConfigMap left")
	}
	if !deleting(t, c, ns) {
		t.Errorf("namespace %s kept, holding only what the component left", ns)
	}
}

// Without its manifest the hook cannot tell the agent's objects: nothing is
// deleted, and the error keeps the component for the next reconcile.
func TestHangarNodeAgentRemoval_KeepsEverythingWithoutItsManifest(t *testing.T) {
	c, _ := envtestClient(t)
	ns := "hangar-agent-offline"
	spec, bundle := hangarNodeAgentRelease(t, ns)
	installHangarNodeAgent(t, c, bundle)
	spec.Version = "v9.9.9" // not served

	if err := hangarNodeAgentHook(t, c).Remove(context.Background(), spec, k4allv1alpha1.ClusterConfigSpec{}); err == nil {
		t.Fatal("Remove without the manifest succeeded")
	}
	if !exists(t, c, &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "hangar-node-agent"}}) {
		t.Error("the DaemonSet was deleted")
	}
}

// The registry gives hangar-node-agent this hook, as a Remover.
func TestHangarNodeAgent_HasARemover(t *testing.T) {
	c, _ := envtestClient(t)
	if _, ok := NewRegistry(c, logr.Discard()).Get("hangar-node-agent").(Remover); !ok {
		t.Error("hangar-node-agent has no hook that removes it")
	}
}

// agentScripts lists the ConfigMaps of the agent's scripts in ns.
func agentScripts(t *testing.T, c client.Client, ns string) []string {
	t.Helper()
	list := &corev1.ConfigMapList{}
	if err := c.List(context.Background(), list, client.InNamespace(ns)); err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, cm := range list.Items {
		if strings.HasPrefix(cm.Name, "hangar-node-agent-scripts") && cm.DeletionTimestamp == nil {
			out = append(out, cm.Name)
		}
	}
	return out
}
