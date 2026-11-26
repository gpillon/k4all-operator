package controller

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/go-logr/logr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	k4allv1alpha1 "github.com/gpillon/k4all-operator/api/v1alpha1"
	"github.com/gpillon/k4all-operator/internal/controller/hooks"
	"github.com/gpillon/k4all-operator/internal/engine"
)

// A Failed component is tried again 30 s after it failed, then 1, 2 and 4
// minutes after that, then every 5 minutes: the next try counted from when it
// went Failed, so nothing but its status says where it is.
func TestRetryAfter_BacksOffFromTheFailure(t *testing.T) {
	for _, tc := range []struct{ failedFor, want time.Duration }{
		{0, 30 * time.Second},
		{10 * time.Second, 20 * time.Second},
		{30 * time.Second, time.Minute},           // tried at 30 s: next at 1m30
		{89 * time.Second, time.Second},           // the 1m30 try is due
		{90 * time.Second, 2 * time.Minute},       // tried at 1m30: next at 3m30
		{210 * time.Second, 4 * time.Minute},      // tried at 3m30: next at 7m30
		{450 * time.Second, 5 * time.Minute},      // tried at 7m30: every 5 minutes from there
		{451 * time.Second, 299 * time.Second},    // still 12m30
		{time.Hour, 150 * time.Second},            // 1h: tried at 57m30, next at 1h2m30
		{-10 * time.Second, 30 * time.Second},     // a clock behind the stored time
		{365 * 24 * time.Hour, 150 * time.Second}, // never more than 5 minutes
	} {
		if got := retryAfter(tc.failedFor); got != tc.want {
			t.Errorf("Failed for %v: retry in %v, want %v", tc.failedFor, got, tc.want)
		}
	}
}

// requeueNear fails the test unless got is want, give or take the second
// the stored lastTransitionTime drops and the test's own run time.
func requeueNear(t *testing.T, step string, got, want time.Duration) {
	t.Helper()
	if got > want || got < want-3*time.Second {
		t.Errorf("%s: RequeueAfter %v, want about %v", step, got, want)
	}
}

// setTransition sets the component's lastTransitionTime to ago before now.
func (cl *cluster) setTransition(name string, ago time.Duration) metav1.Time {
	cl.t.Helper()
	rm := cl.manifest()
	st := rm.Status.Components[name]
	st.LastTransitionTime = metav1.NewTime(time.Now().Add(-ago))
	rm.Status.Components[name] = st
	if err := cl.c.Status().Update(context.Background(), rm); err != nil {
		cl.t.Fatal(err)
	}
	return cl.manifest().Status.Components[name].LastTransitionTime
}

// A component that fails to apply is tried again soon, not at the 5-minute
// resync (Calico failing on DNS at first boot left a cluster waiting 13
// minutes), and less often the longer it fails, whatever the error. Its
// lastTransitionTime stays the time it went Failed.
func TestReconcile_AFailedComponentIsRetriedWithBackoff(t *testing.T) {
	// "unsupported": the apply fails at once, without network access.
	cl := newCluster(t,
		map[string]k4allv1alpha1.ComponentSpec{"probe": {Type: "unsupported", Version: "1"}},
		map[string]k4allv1alpha1.ComponentStatus{"probe": {State: k4allv1alpha1.ComponentStatePending}},
	)

	requeueNear(t, "just failed", cl.reconcile().RequeueAfter, 30*time.Second)
	if st := cl.state("probe"); st != k4allv1alpha1.ComponentStateFailed {
		t.Fatalf("state %s, want Failed", st)
	}

	for _, tc := range []struct{ failedFor, want time.Duration }{
		{40 * time.Second, 50 * time.Second},
		{100 * time.Second, 110 * time.Second},
		{time.Hour, 150 * time.Second},
	} {
		since := cl.setTransition("probe", tc.failedFor)
		res := cl.reconcile()
		st := cl.manifest().Status.Components["probe"]
		if st.State != k4allv1alpha1.ComponentStateFailed || !st.LastTransitionTime.Equal(&since) {
			t.Fatalf("failed for %v: %s since %v, want Failed since %v", tc.failedFor, st.State, st.LastTransitionTime, since)
		}
		requeueNear(t, "failed for "+tc.failedFor.String(), res.RequeueAfter, tc.want)
	}
}

// The same for a failed uninstall: tried again soon, with the same backoff.
func TestReconcile_AFailedUninstallIsRetriedWithBackoff(t *testing.T) {
	cl := newCluster(t,
		map[string]k4allv1alpha1.ComponentSpec{"probe": {Type: "unsupported", Version: "1", FeatureGate: "rook"}},
		map[string]k4allv1alpha1.ComponentStatus{"probe": {
			State: k4allv1alpha1.ComponentStateInstalled, InstalledVersion: "1",
		}},
	)

	requeueNear(t, "just failed", cl.reconcile().RequeueAfter, 30*time.Second)
	if st := cl.state("probe"); st != k4allv1alpha1.ComponentStateFailed {
		t.Fatalf("state %s, want Failed", st)
	}

	since := cl.setTransition("probe", 40*time.Second)
	res := cl.reconcile()
	if st := cl.manifest().Status.Components["probe"]; st.State != k4allv1alpha1.ComponentStateFailed ||
		!st.LastTransitionTime.Equal(&since) {
		t.Fatalf("%s since %v, want Failed since %v", st.State, st.LastTransitionTime, since)
	}
	requeueNear(t, "failed for 40s", res.RequeueAfter, 50*time.Second)
}

// A refused downgrade waits on a person (the allow-downgrade annotation,
// which starts a reconcile): no backoff for it.
func TestReconcile_ARefusedDowngradeIsNotRetried(t *testing.T) {
	cl := newCluster(t,
		map[string]k4allv1alpha1.ComponentSpec{"probe": {Type: "unsupported", Version: "1.0.0"}},
		map[string]k4allv1alpha1.ComponentStatus{"probe": {
			State: k4allv1alpha1.ComponentStateInstalled, InstalledVersion: "2.0.0",
		}},
	)

	if res := cl.reconcile(); res.RequeueAfter != resyncInterval {
		t.Fatalf("RequeueAfter %v with a downgrade refused, want the %v resync", res.RequeueAfter, resyncInterval)
	}
}

// postInstallProbe is the hook of the "probe" component: its PostInstall
// returns the next of errs (nil once they are used up) and counts its calls.
type postInstallProbe struct {
	hooks.BaseHook
	errs  []error
	calls int
}

func (p *postInstallProbe) PostInstall(context.Context, k4allv1alpha1.ComponentSpec, k4allv1alpha1.ClusterConfigSpec) error {
	p.calls++
	if len(p.errs) == 0 {
		return nil
	}
	err := p.errs[0]
	p.errs = p.errs[1:]
	return err
}

// A PostInstall that failed is run again, with the backoff of a failure,
// until it succeeds: it used to wait for the next change of the applied hash
// (MetalLB's pool, missing for good when its webhook was slow to start).
// Then it runs only after an apply again.
func TestReconcile_AFailedPostInstallIsRetried(t *testing.T) {
	probe := &postInstallProbe{errs: []error{errors.New("MetalLB webhook not ready")}}
	cl := newCluster(t,
		map[string]k4allv1alpha1.ComponentSpec{"probe": {Type: "unsupported", Version: "1"}},
		map[string]k4allv1alpha1.ComponentStatus{"probe": {
			State: k4allv1alpha1.ComponentStateInstalled, InstalledVersion: "1",
			Message: hookFailedPrefix + "MetalLB webhook not ready", LastTransitionTime: metav1.Now(),
		}},
	)
	cl.r.hookReg = hooks.NewRegistry(cl.c, logr.Discard())
	cl.r.hookReg.Register("probe", probe)

	res := cl.reconcile()

	st := cl.manifest().Status.Components["probe"]
	if probe.calls != 1 || st.State != k4allv1alpha1.ComponentStateInstalled ||
		st.Message != hookFailedPrefix+"MetalLB webhook not ready" {
		t.Fatalf("PostInstall called %d times; %s (%s), want once, Installed with the failure", probe.calls, st.State, st.Message)
	}
	requeueNear(t, "PostInstall failed again", res.RequeueAfter, 30*time.Second)

	res = cl.reconcile()

	if msg := cl.manifest().Status.Components["probe"].Message; probe.calls != 2 || msg != engine.InstalledMessage {
		t.Fatalf("PostInstall called %d times, message %q; want twice, %q", probe.calls, msg, engine.InstalledMessage)
	}
	if res.RequeueAfter != resyncInterval {
		t.Errorf("RequeueAfter %v once PostInstall succeeded, want the %v resync", res.RequeueAfter, resyncInterval)
	}

	cl.reconcile()
	if probe.calls != 2 {
		t.Errorf("PostInstall called %d times, want no call once it succeeded", probe.calls)
	}
}
