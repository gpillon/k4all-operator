package controller

import (
	"context"
	"strings"
	"testing"

	"github.com/go-logr/logr"
	k4allv1alpha1 "github.com/gpillon/k4all-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// The host tool k4all-upgrade puts this annotation on its own Node from STAGE
// until COMMIT or HOLD (fcos-k8s ai/migrazioni/contratto-host-operator.md §5.1).
const upgradeInProgress = "k4all.magesgate.com/upgrade-in-progress"

func gateScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := k4allv1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return s
}

// reconcileOnce runs one Reconcile over a cluster with a single component of
// an unsupported type: reconciling it fails immediately, without any network
// access, so its status shows whether components were touched at all.
func reconcileOnce(t *testing.T, nodes ...client.Object) (*k4allv1alpha1.ReleaseManifest, ctrl.Result) {
	t.Helper()
	s := gateScheme(t)
	rm := &k4allv1alpha1.ReleaseManifest{
		ObjectMeta: metav1.ObjectMeta{Name: "k4all"},
		Spec: k4allv1alpha1.ReleaseManifestSpec{
			Components: map[string]k4allv1alpha1.ComponentSpec{
				"probe": {Type: "unsupported", Version: "1"},
			},
		},
	}
	cc := &k4allv1alpha1.ClusterConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "k4all-cluster-config"},
		Spec: k4allv1alpha1.ClusterConfigSpec{
			Networking: k4allv1alpha1.NetworkingConfig{CNI: k4allv1alpha1.CNIConfig{Type: "calico"}},
		},
	}
	c := fake.NewClientBuilder().WithScheme(s).
		WithObjects(append([]client.Object{rm, cc}, nodes...)...).
		WithStatusSubresource(&k4allv1alpha1.ReleaseManifest{}, &k4allv1alpha1.ClusterConfig{}).
		Build()
	r := &ReleaseReconciler{Client: c, Scheme: s, Log: logr.Discard()}

	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "k4all"}})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	got := &k4allv1alpha1.ReleaseManifest{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: "k4all"}, got); err != nil {
		t.Fatal(err)
	}
	return got, res
}

func node(name string, annotations map[string]string) *corev1.Node {
	return &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, Annotations: annotations}}
}

func TestReconcile_UpgradeInProgressPausesComponents(t *testing.T) {
	t.Run("without the annotation components are reconciled", func(t *testing.T) {
		rm, _ := reconcileOnce(t, node("cp1", nil))
		if st := rm.Status.Components["probe"].State; st != k4allv1alpha1.ComponentStateFailed {
			t.Fatalf("probe state = %q, want %q (test setup no longer exercises the engine)", st, k4allv1alpha1.ComponentStateFailed)
		}
	})

	t.Run("an annotated node pauses every component", func(t *testing.T) {
		rm, res := reconcileOnce(t,
			node("cp1", nil),
			node("worker1", map[string]string{upgradeInProgress: "v2.2.4->v2.3.0"}),
		)

		if st, ok := rm.Status.Components["probe"]; ok {
			t.Errorf("probe was reconciled during an upgrade: %+v", st)
		}
		ready := meta.FindStatusCondition(rm.Status.Conditions, "Ready")
		if ready == nil || ready.Status != metav1.ConditionFalse || ready.Reason != "UpgradeInProgress" {
			t.Fatalf("Ready condition = %+v, want False/UpgradeInProgress", ready)
		}
		if !strings.Contains(ready.Message, "worker1") || !strings.Contains(ready.Message, "v2.2.4->v2.3.0") {
			t.Errorf("Ready message %q should name the node and the hop", ready.Message)
		}
		if res.RequeueAfter == 0 {
			t.Errorf("paused reconcile must requeue to notice the end of the upgrade")
		}
	})
}
