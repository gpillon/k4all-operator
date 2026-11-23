package controller

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	k4allv1alpha1 "github.com/gpillon/k4all-operator/api/v1alpha1"
	"github.com/gpillon/k4all-operator/internal/controller/hooks"
	"github.com/gpillon/k4all-operator/internal/engine"
)

// syncProbe is the hook of the "probe" component: a Syncer that records its
// calls, asks to run again after `after`, and fails with err.
type syncProbe struct {
	hooks.BaseHook
	after time.Duration
	err   error
	calls []string
}

func (p *syncProbe) PostInstall(context.Context, k4allv1alpha1.ComponentSpec, k4allv1alpha1.ClusterConfigSpec) error {
	p.calls = append(p.calls, "PostInstall")
	return nil
}

func (p *syncProbe) Sync(context.Context, k4allv1alpha1.ComponentSpec, k4allv1alpha1.ClusterConfigSpec) (time.Duration, error) {
	p.calls = append(p.calls, "Sync")
	return p.after, p.err
}

// installedProbe is a fake cluster whose "probe" component is Installed, with
// the syncProbe as its hook.
func installedProbe(t *testing.T, probe *syncProbe, message string) *cluster {
	t.Helper()
	cl := newCluster(t,
		map[string]k4allv1alpha1.ComponentSpec{"probe": {Type: "unsupported", Version: "1"}},
		map[string]k4allv1alpha1.ComponentStatus{"probe": {
			State: k4allv1alpha1.ComponentStateInstalled, InstalledVersion: "1", Message: message,
		}},
	)
	cl.r.hookReg = hooks.NewRegistry(cl.c, logr.Discard())
	cl.r.hookReg.Register("probe", probe)
	return cl
}

// KubeVirt's emulation "auto" is only known once virt-handler runs: the hook's
// Sync runs on every reconcile of an installed component, not only when the
// component is applied, and the reconcile comes back when it asks to.
func TestReconcile_SyncRunsWhileTheComponentIsUpToDate(t *testing.T) {
	probe := &syncProbe{after: 30 * time.Second}
	cl := installedProbe(t, probe, engine.InstalledMessage)

	for _, step := range []string{"adopted", "at its applied hash"} {
		res := cl.reconcile()
		if st := cl.manifest().Status.Components["probe"].State; st != k4allv1alpha1.ComponentStateInstalled {
			t.Fatalf("%s: state %q, want Installed (not applied again)", step, st)
		}
		if res.RequeueAfter != 30*time.Second {
			t.Errorf("%s: RequeueAfter %v, want the 30s Sync asked for", step, res.RequeueAfter)
		}
	}
	if got := strings.Join(probe.calls, " "); got != "Sync Sync" {
		t.Fatalf("hook calls %q, want Sync at each reconcile and no PostInstall", got)
	}

	probe.after = 0
	if res := cl.reconcile(); res.RequeueAfter != resyncInterval {
		t.Errorf("Sync asks for nothing: RequeueAfter %v, want the periodic %v", res.RequeueAfter, resyncInterval)
	}
}

func TestReconcile_SyncRunsAfterPostInstall(t *testing.T) {
	spec := k4allv1alpha1.ComponentSpec{
		Type: k4allv1alpha1.ComponentTypeManifest, Version: "v1", InlineManifests: []string{probeConfigMap},
	}
	c, r := onEnvtest(t, spec, k4allv1alpha1.ComponentStatus{State: k4allv1alpha1.ComponentStatePending})
	probe := &syncProbe{after: 30 * time.Second}
	r.hookReg = hooks.NewRegistry(c, logr.Discard())
	r.hookReg.Register("probe", probe)

	res := reconcileProbe(t, r)

	if st := probeStatus(t, c); st.State != k4allv1alpha1.ComponentStateInstalled {
		t.Fatalf("probe %s (%s), want Installed", st.State, st.Message)
	}
	if got := strings.Join(probe.calls, " "); got != "PostInstall Sync" {
		t.Fatalf("hook calls %q, want PostInstall then Sync", got)
	}
	if res.RequeueAfter != 30*time.Second {
		t.Errorf("RequeueAfter %v, want the 30s Sync asked for", res.RequeueAfter)
	}
}

// A Sync failure shows in the component's message like a PostInstall one, and
// the next Sync that succeeds clears it.
func TestReconcile_SyncFailureShowsInTheStatusUntilASyncSucceeds(t *testing.T) {
	probe := &syncProbe{err: errors.New("KubeVirt CR unreadable")}
	cl := installedProbe(t, probe, engine.InstalledMessage)

	cl.reconcile()
	st := cl.manifest().Status.Components["probe"]
	if st.State != k4allv1alpha1.ComponentStateInstalled || st.Message != hookFailedPrefix+"KubeVirt CR unreadable" {
		t.Fatalf("%s (%q), want Installed with the Sync failure", st.State, st.Message)
	}

	probe.err = nil
	cl.reconcile()
	if msg := cl.manifest().Status.Components["probe"].Message; msg != engine.InstalledMessage {
		t.Fatalf("message %q after a Sync that succeeded, want %q", msg, engine.InstalledMessage)
	}
}

// A PostInstall failure recorded by an earlier reconcile is cleared by the
// Sync that redoes the hook's work; other messages are left alone.
func TestReconcile_SyncClearsOnlyHookFailures(t *testing.T) {
	for message, want := range map[string]string{
		hookFailedPrefix + "timed out waiting for KubeVirt CR": engine.InstalledMessage,
		"adopted from k4all 1.x":                               "adopted from k4all 1.x",
	} {
		cl := installedProbe(t, &syncProbe{}, message)
		cl.reconcile()
		if got := cl.manifest().Status.Components["probe"].Message; got != want {
			t.Errorf("message %q: after a Sync that succeeded %q, want %q", message, got, want)
		}
	}
}
