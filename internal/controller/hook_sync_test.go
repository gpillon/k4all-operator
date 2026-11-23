package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	k4allv1alpha1 "github.com/gpillon/k4all-operator/api/v1alpha1"
	"github.com/gpillon/k4all-operator/internal/controller/hooks"
)

// syncProbe is the hook of the "probe" component: a Syncer that records its
// calls and asks to run again after `after`.
type syncProbe struct {
	hooks.BaseHook
	after time.Duration
	calls []string
}

func (p *syncProbe) PostInstall(context.Context, k4allv1alpha1.ComponentSpec, k4allv1alpha1.ClusterConfigSpec) error {
	p.calls = append(p.calls, "PostInstall")
	return nil
}

func (p *syncProbe) Sync(context.Context, k4allv1alpha1.ComponentSpec, k4allv1alpha1.ClusterConfigSpec) (time.Duration, error) {
	p.calls = append(p.calls, "Sync")
	return p.after, nil
}

// KubeVirt's emulation "auto" is only known once virt-handler runs: the hook's
// Sync runs on every reconcile of an installed component, not only when the
// component is applied, and the reconcile comes back when it asks to.
func TestReconcile_SyncRunsWhileTheComponentIsUpToDate(t *testing.T) {
	cl := newCluster(t,
		map[string]k4allv1alpha1.ComponentSpec{"probe": {Type: "unsupported", Version: "1"}},
		map[string]k4allv1alpha1.ComponentStatus{"probe": {State: k4allv1alpha1.ComponentStateInstalled, InstalledVersion: "1"}},
	)
	probe := &syncProbe{after: 30 * time.Second}
	cl.r.hookReg = hooks.NewRegistry(cl.c, logr.Discard())
	cl.r.hookReg.Register("probe", probe)

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
	if res := cl.reconcile(); res.RequeueAfter != 5*time.Minute {
		t.Errorf("Sync asks for nothing: RequeueAfter %v, want the periodic 5m", res.RequeueAfter)
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
