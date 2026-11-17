package engine

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	k4allv1alpha1 "github.com/gpillon/k4all-operator/api/v1alpha1"
)

// The k4all-bootc kube-vip manifest, as the host writes it: args, env and
// volumes the operator's static-pod type cannot express.
const hostKubeVIP = `apiVersion: v1
kind: Pod
metadata:
  name: kube-vip
  namespace: kube-system
spec:
  containers:
  - name: kube-vip
    image: ghcr.io/kube-vip/kube-vip:v1.3.1
    args: ["manager"]
    env:
    - name: address
      value: 192.168.6.192
`

// runNodeScript runs a static-pod Job script against dir as the node's
// /etc/kubernetes/manifests and returns the termination message it wrote.
func runNodeScript(t *testing.T, script, dir string) string {
	t.Helper()
	termLog := filepath.Join(t.TempDir(), "termination-log")
	cmd := exec.Command("sh", "-c", script)
	cmd.Env = append(os.Environ(), "MANIFESTS="+dir, "TERMINATION_LOG="+termLog)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("script failed: %v\n%s", err, out)
	}
	msg, _ := os.ReadFile(termLog)
	return strings.TrimSpace(string(msg))
}

func kubeVIPSpec() k4allv1alpha1.ComponentSpec {
	return k4allv1alpha1.ComponentSpec{Type: k4allv1alpha1.ComponentTypeStaticPod, Image: "ghcr.io/kube-vip/kube-vip:v1.3.1"}
}

// The operator writes and removes only the manifests it wrote: on VMs
// 954-956 it replaced the host's kube-vip manifest with a bare Pod, and the
// VIP went away.
func TestStaticPod_LeavesAManifestItDidNotWrite(t *testing.T) {
	s := &StaticPodManager{}
	for name, script := range map[string]string{
		"write":  s.writeScript("kube-vip", s.generateStaticPodManifest("kube-vip", kubeVIPSpec())),
		"remove": s.removeScript("kube-vip"),
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "kube-vip.yaml")
			if err := os.WriteFile(path, []byte(hostKubeVIP), 0o600); err != nil {
				t.Fatal(err)
			}

			msg := runNodeScript(t, script, dir)

			got, err := os.ReadFile(path)
			if err != nil || string(got) != hostKubeVIP {
				t.Errorf("the host's manifest changed (err %v):\n%s", err, got)
			}
			if msg != foreignManifestMark {
				t.Errorf("termination message = %q, want %q", msg, foreignManifestMark)
			}
		})
	}
}

func TestStaticPod_WritesAndRemovesItsOwnManifest(t *testing.T) {
	s := &StaticPodManager{}
	dir := t.TempDir()
	path := filepath.Join(dir, "kube-vip.yaml")
	manifest := s.generateStaticPodManifest("kube-vip", kubeVIPSpec())

	if msg := runNodeScript(t, s.writeScript("kube-vip", manifest), dir); msg != "" {
		t.Errorf("termination message = %q writing a new manifest", msg)
	}
	if got, _ := os.ReadFile(path); strings.TrimSpace(string(got)) != strings.TrimSpace(manifest) {
		t.Errorf("manifest written:\n%s\nwant:\n%s", got, manifest)
	}
	// Its own manifest: rewritten, then removed.
	if msg := runNodeScript(t, s.writeScript("kube-vip", manifest), dir); msg != "" {
		t.Errorf("termination message = %q rewriting its manifest", msg)
	}
	if msg := runNodeScript(t, s.removeScript("kube-vip"), dir); msg != "" {
		t.Errorf("termination message = %q removing its manifest", msg)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("manifest still there after remove: %v", err)
	}
}

// A component whose static-pod manifest belongs to someone else is reported
// Unmanaged with the reason, in the status only, not as a failure: the
// host's sync would stop on a Failed component.
func TestStaticPod_ForeignManifestIsReportedUnmanaged(t *testing.T) {
	st := &k4allv1alpha1.ComponentStatus{}
	err := staticPodResult(st, "kube-vip", errForeignManifest)
	if err != nil {
		t.Fatalf("error %v, want none", err)
	}
	if st.State != k4allv1alpha1.ComponentStateUnmanaged || !strings.Contains(st.Message, "not written by the k4all-operator") {
		t.Errorf("status = %s %q, want Unmanaged with the reason", st.State, st.Message)
	}
}
