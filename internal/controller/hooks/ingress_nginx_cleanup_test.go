package hooks

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	k4allv1alpha1 "github.com/gpillon/k4all-operator/api/v1alpha1"
)

var (
	envOnce sync.Once
	testEnv *envtest.Environment
	envCfg  *rest.Config
	envErr  error
)

// envtestClient is a client of an envtest API server shared by the package.
func envtestClient(t *testing.T) (client.Client, *rest.Config) {
	t.Helper()
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS not set: run through `make test`")
	}
	envOnce.Do(func() {
		testEnv = &envtest.Environment{}
		envCfg, envErr = testEnv.Start()
	})
	if envErr != nil {
		t.Fatalf("start envtest: %v", envErr)
	}
	c, err := client.New(envCfg, client.Options{})
	if err != nil {
		t.Fatal(err)
	}
	return c, envCfg
}

func TestMain(m *testing.M) {
	code := m.Run()
	if testEnv != nil {
		_ = testEnv.Stop()
	}
	os.Exit(code)
}

// leftNamespace is what helm uninstall of ingress-nginx leaves in its
// namespace (VM 951), plus what Kubernetes puts in every namespace and the
// controller's pods that the garbage collector removes after the uninstall.
func leftNamespace(t *testing.T, c client.Client, ns string) {
	t.Helper()
	ctx := context.Background()
	owner := []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "ReplicaSet", Name: "gone", UID: "gone-uid"}}
	objs := []client.Object{
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "ingress-nginx-admission"}},
		&coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "ingress-nginx-leader"}},
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "kube-root-ca.crt"}},
		&corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "default"}},
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "ingress-nginx-controller-x", OwnerReferences: owner},
			Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "x"}}},
		},
		&corev1.Event{
			ObjectMeta:     metav1.ObjectMeta{Namespace: ns, Name: "ev"},
			InvolvedObject: corev1.ObjectReference{Kind: "Pod", Namespace: ns, Name: "ingress-nginx-controller-x"},
		},
	}
	for _, o := range objs {
		if err := c.Create(ctx, o); err != nil {
			t.Fatalf("create %T %s: %v", o, o.GetName(), err)
		}
	}
}

// deleting tells whether the namespace is gone or being deleted (envtest has
// no namespace controller to finish).
func deleting(t *testing.T, c client.Client, ns string) bool {
	t.Helper()
	got := &corev1.Namespace{}
	err := c.Get(context.Background(), types.NamespacedName{Name: ns}, got)
	if apierrors.IsNotFound(err) {
		return true
	}
	if err != nil {
		t.Fatal(err)
	}
	return got.DeletionTimestamp != nil
}

func TestIngressNginxCleanup_DeletesTheNamespaceItLeft(t *testing.T) {
	c, cfg := envtestClient(t)
	ns := "ingress-nginx-left"
	leftNamespace(t, c, ns)
	h := &IngressNginxHook{client: c, log: logr.Discard(), restConfig: cfg, namespace: ns, settle: 2 * time.Second, poll: 200 * time.Millisecond}

	if err := h.Cleanup(context.Background(), "ingress-nginx", k4allv1alpha1.ClusterConfigSpec{}); err != nil {
		t.Fatal(err)
	}
	if !deleting(t, c, ns) {
		t.Errorf("namespace %s kept, holding only what ingress-nginx left", ns)
	}
}

func TestIngressNginxCleanup_KeepsANamespaceWithOtherObjects(t *testing.T) {
	c, cfg := envtestClient(t)
	ns := "ingress-nginx-shared"
	leftNamespace(t, c, ns)
	mine := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "mine"}}
	if err := c.Create(context.Background(), mine); err != nil {
		t.Fatal(err)
	}
	h := &IngressNginxHook{client: c, log: logr.Discard(), restConfig: cfg, namespace: ns, settle: 2 * time.Second, poll: 200 * time.Millisecond}

	if err := h.Cleanup(context.Background(), "ingress-nginx", k4allv1alpha1.ClusterConfigSpec{}); err != nil {
		t.Fatal(err)
	}
	if deleting(t, c, ns) {
		t.Errorf("namespace %s deleted with ConfigMap mine in it", ns)
	}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(mine), &corev1.ConfigMap{}); err != nil {
		t.Errorf("ConfigMap mine: %v", err)
	}
	err := c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: "ingress-nginx-admission"}, &corev1.Secret{})
	if !apierrors.IsNotFound(err) {
		t.Errorf("secret ingress-nginx-admission still there (err %v)", err)
	}
}

// A Cleanup error would leave ingress-nginx Failed and stop the host's sync:
// the namespace is best effort, only logged.
func TestIngressNginxCleanup_NeverFailsTheRemoval(t *testing.T) {
	c, _ := envtestClient(t)
	h := &IngressNginxHook{client: c, log: logr.Discard(), restConfig: &rest.Config{Host: "http://127.0.0.1:1"}, namespace: "ingress-nginx-none",
		settle: time.Second, poll: 200 * time.Millisecond}

	if err := h.Cleanup(context.Background(), "ingress-nginx", k4allv1alpha1.ClusterConfigSpec{}); err != nil {
		t.Errorf("Cleanup: %v, want nil", err)
	}
}

// helm uninstall does not wait for what it deletes: right after it, the
// namespace still holds objects on their way out. In the e2e run the
// ingress-nginx controller pod was still listed without an owner; it was
// gone a moment later. The cleanup waits for them before it decides.
func TestIngressNginxCleanup_WaitsForWhatTheUninstallIsDeleting(t *testing.T) {
	c, cfg := envtestClient(t)
	ns := "ingress-nginx-going"
	leftNamespace(t, c, ns)
	going := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "ingress-nginx-controller-5sgxq"},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "x"}}},
	}
	if err := c.Create(context.Background(), going); err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(time.Second)
		_ = c.Delete(context.Background(), going, client.GracePeriodSeconds(0))
	}()
	h := &IngressNginxHook{client: c, log: logr.Discard(), restConfig: cfg, namespace: ns,
		settle: 10 * time.Second, poll: 200 * time.Millisecond}

	if err := h.Cleanup(context.Background(), "ingress-nginx", k4allv1alpha1.ClusterConfigSpec{}); err != nil {
		t.Fatal(err)
	}
	if !deleting(t, c, ns) {
		t.Errorf("namespace %s kept: the pod on its way out was counted", ns)
	}
}
