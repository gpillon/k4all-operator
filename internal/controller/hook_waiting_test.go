package controller

import (
	"fmt"
	"testing"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	k4allv1alpha1 "github.com/gpillon/k4all-operator/api/v1alpha1"
	"github.com/gpillon/k4all-operator/internal/controller/hooks"
	"github.com/gpillon/k4all-operator/internal/engine"
)

// A hook that waits on the cluster, not on a fault (Rook on fewer than 3
// nodes: the CephCluster comes once a third node joins), shows its component
// as waiting, not as a hook failure. The component is installed as asked: the
// release stays Ready, and the feature's own condition says what is missing.
// The wait ends at the first Sync that does not wait.
func TestReconcile_AWaitingHookShowsAsWaiting(t *testing.T) {
	probe := &syncProbe{err: hooks.Waiting{Reason: "Ceph needs 3 nodes, this cluster has 1"}}
	cl := installedProbe(t, probe, engine.InstalledMessage)

	res := cl.reconcile()

	rm := cl.manifest()
	st := rm.Status.Components["probe"]
	if st.State != k4allv1alpha1.ComponentStateInstalled || st.Message != "waiting: Ceph needs 3 nodes, this cluster has 1" {
		t.Fatalf("%s (%q), want Installed, waiting", st.State, st.Message)
	}
	if !meta.IsStatusConditionTrue(rm.Status.Conditions, "Ready") {
		t.Errorf("Ready %+v, want True: the component is installed as asked",
			meta.FindStatusCondition(rm.Status.Conditions, "Ready"))
	}
	if res.RequeueAfter != resyncInterval {
		t.Errorf("RequeueAfter %v while waiting, want the %v resync: a wait is no failure to retry", res.RequeueAfter, resyncInterval)
	}

	probe.err = nil
	cl.reconcile()

	if msg := cl.manifest().Status.Components["probe"].Message; msg != engine.InstalledMessage {
		t.Fatalf("message %q once the hook no longer waits, want %q", msg, engine.InstalledMessage)
	}
}

// A PostInstall can wait too, also wrapped in an error; it runs again at the
// next reconcile, like a failed one.
func TestReconcile_AWaitingPostInstallShowsAsWaitingAndRunsAgain(t *testing.T) {
	probe := &postInstallProbe{errs: []error{
		fmt.Errorf("pool: %w", hooks.Waiting{Reason: "no node is ready"}),
	}}
	cl := newCluster(t,
		map[string]k4allv1alpha1.ComponentSpec{"probe": {Type: "unsupported", Version: "1"}},
		map[string]k4allv1alpha1.ComponentStatus{"probe": {
			State: k4allv1alpha1.ComponentStateInstalled, InstalledVersion: "1",
			Message: hookFailedPrefix + "timed out", LastTransitionTime: metav1.Now(),
		}},
	)
	cl.r.hookReg = hooks.NewRegistry(cl.c, logr.Discard())
	cl.r.hookReg.Register("probe", probe)

	cl.reconcile()

	if msg := cl.manifest().Status.Components["probe"].Message; msg != "waiting: no node is ready" {
		t.Fatalf("message %q, want waiting", msg)
	}

	cl.reconcile()

	if msg := cl.manifest().Status.Components["probe"].Message; probe.calls != 2 || msg != engine.InstalledMessage {
		t.Fatalf("PostInstall called %d times, message %q; want twice, %q", probe.calls, msg, engine.InstalledMessage)
	}
}
