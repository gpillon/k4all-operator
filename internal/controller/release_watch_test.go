package controller

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/config"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	k4allv1alpha1 "github.com/gpillon/k4all-operator/api/v1alpha1"
)

// countingClient counts the ReleaseManifest reads of a ReleaseReconciler:
// every reconcile reads it.
type countingClient struct {
	client.Client
	reads atomic.Int64
}

func (c *countingClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if _, ok := obj.(*k4allv1alpha1.ReleaseManifest); ok {
		c.reads.Add(1)
	}
	return c.Client.Get(ctx, key, obj, opts...)
}

// settled waits until the count has not moved for quiet, and returns it.
func (c *countingClient) settled(t *testing.T, quiet time.Duration) int64 {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	last := c.reads.Load()
	for time.Now().Before(deadline) {
		time.Sleep(quiet)
		now := c.reads.Load()
		if now == last {
			return now
		}
		last = now
	}
	t.Fatal("the reconciles never stopped")
	return 0
}

// The ClusterConfig's status does not start a reconcile, its spec does:
// every reconcile writes the status (lastReconcileTime), and one that ran
// into the next second started the next at once, so a component that failed
// slowly (a Helm timeout) was tried again and again with no backoff.
func TestReleaseReconciler_WatchesTheClusterConfigSpecNotItsStatus(t *testing.T) {
	cfg := envtestConfig(t)
	s := gateScheme(t)
	c, err := client.New(cfg, client.Options{Scheme: s})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	cc := &k4allv1alpha1.ClusterConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "k4all-cluster-config"},
		Spec: k4allv1alpha1.ClusterConfigSpec{
			Networking: k4allv1alpha1.NetworkingConfig{CNI: k4allv1alpha1.CNIConfig{Type: "calico"}},
		},
	}
	rm := &k4allv1alpha1.ReleaseManifest{
		ObjectMeta: metav1.ObjectMeta{Name: "k4all"},
		Spec: k4allv1alpha1.ReleaseManifestSpec{Components: map[string]k4allv1alpha1.ComponentSpec{"probe": {
			Type: k4allv1alpha1.ComponentTypeManifest, Version: "v1", InlineManifests: []string{probeConfigMap},
		}}},
	}
	for _, obj := range []client.Object{cc, rm} {
		if err := c.Create(ctx, obj); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = c.Delete(context.Background(), obj) })
	}
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:     s,
		Metrics:    metricsserver.Options{BindAddress: "0"},
		Controller: config.Controller{SkipNameValidation: ptr.To(true)},
	})
	if err != nil {
		t.Fatal(err)
	}
	counting := &countingClient{Client: mgr.GetClient()}
	r := &ReleaseReconciler{Client: counting, Scheme: s, RestConfig: cfg, Log: logr.Discard()}
	if err := r.SetupWithManager(mgr); err != nil {
		t.Fatal(err)
	}
	go func() { _ = mgr.Start(ctx) }()

	before := counting.settled(t, 2*time.Second)
	if before == 0 {
		t.Fatal("no reconcile at start")
	}

	// Another controller's condition, then the reconciler's own field.
	if err := c.Get(ctx, client.ObjectKeyFromObject(cc), cc); err != nil {
		t.Fatal(err)
	}
	meta.SetStatusCondition(&cc.Status.Conditions, metav1.Condition{
		Type: "NodeSettingsRollout", Status: metav1.ConditionTrue, Reason: "Done", Message: "all nodes",
	})
	cc.Status.LastReconcileTime = metav1.NewTime(time.Now().Add(-time.Hour))
	if err := c.Status().Update(ctx, cc); err != nil {
		t.Fatal(err)
	}
	if after := counting.settled(t, 2*time.Second); after != before {
		t.Fatalf("%d ReleaseManifest reads after a ClusterConfig status change, want no reconcile", after-before)
	}

	if err := c.Get(ctx, client.ObjectKeyFromObject(cc), cc); err != nil {
		t.Fatal(err)
	}
	cc.Spec.Features.ArgoCD.Enabled = true
	if err := c.Update(ctx, cc); err != nil {
		t.Fatal(err)
	}
	if after := counting.settled(t, 2*time.Second); after == before {
		t.Fatal("no reconcile after a ClusterConfig spec change")
	}
}
