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
	"helm.sh/helm/v3/pkg/action"
	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/release"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// runningProbe is the hook of the "probe" component: it reports the version
// the component runs, as the KubeVirt and CDI hooks read it from their CR.
type runningProbe struct {
	hooks.BaseHook
	version string
	ready   bool
	err     error
}

func (p *runningProbe) RunningVersion(context.Context) (string, bool, error) {
	return p.version, p.ready, p.err
}

// The probe's release version and the upgradePath step before it.
const (
	probeRelease = "v1.16.0"
	probeStep    = "v1.15.0"
)

// probeWithUpgradePath is a manifest component at v1.16.0 whose upgradePath goes
// through v1.15.0.
func probeWithUpgradePath() k4allv1alpha1.ComponentSpec {
	return k4allv1alpha1.ComponentSpec{
		Type:            k4allv1alpha1.ComponentTypeManifest,
		Version:         probeRelease,
		InlineManifests: []string{probeConfigMap},
		UpgradePath:     []k4allv1alpha1.UpgradeStep{{Version: probeStep}},
	}
}

// switchedToManagedOnEnvtest is a cluster where "probe" was Unmanaged (restored from
// k4all 1.x) and has just been switched to Managed: the status has no
// installedVersion. The probe hook reports what runs.
func switchedToManagedOnEnvtest(t *testing.T, running *runningProbe) (client.Client, *ReleaseReconciler) {
	t.Helper()
	c, r := onEnvtest(t, probeWithUpgradePath(), k4allv1alpha1.ComponentStatus{
		State: k4allv1alpha1.ComponentStateUnmanaged, LastTransitionTime: metav1.Now(),
	})
	r.hookReg = hooks.NewRegistry(c, logr.Discard())
	r.hookReg.Register("probe", running)
	return c, r
}

func reconcileProbe(t *testing.T, r *ReleaseReconciler) ctrl.Result {
	t.Helper()
	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "k4all"}})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestReconcile_ComponentSwitchedToManagedFollowsItsUpgradePathFromTheRunningVersion(t *testing.T) {
	c, r := switchedToManagedOnEnvtest(t, &runningProbe{version: "v1.14.0", ready: true})

	reconcileProbe(t, r)

	st := probeStatus(t, c)
	if st.InstalledVersion != probeStep || st.State != k4allv1alpha1.ComponentStateUpgrading {
		t.Fatalf("%s at %q (%s), want Upgrading at the step v1.15.0: v1.14.0 runs", st.State, st.InstalledVersion, st.Message)
	}
}

// A manifest step is only applied: the next one waits until the component
// runs it, or KubeVirt would get v1.8 while still rolling out v1.7.
func TestReconcile_NextStepWaitsUntilTheStepRuns(t *testing.T) {
	running := &runningProbe{version: "v1.14.0", ready: true}
	c, r := switchedToManagedOnEnvtest(t, running)
	reconcileProbe(t, r) // applies the step v1.15.0

	res := reconcileProbe(t, r) // v1.14.0 still runs
	st := probeStatus(t, c)
	if st.InstalledVersion != probeStep || st.State != k4allv1alpha1.ComponentStateUpgrading {
		t.Fatalf("%s at %q (%s), want still Upgrading at v1.15.0 while v1.14.0 runs", st.State, st.InstalledVersion, st.Message)
	}
	if res.RequeueAfter <= 0 || res.RequeueAfter > 30*time.Second {
		t.Errorf("waiting on a step should come back soon, got RequeueAfter %v", res.RequeueAfter)
	}

	running.version, running.ready = probeStep, false // rolling out
	reconcileProbe(t, r)
	if st := probeStatus(t, c); st.InstalledVersion != probeStep {
		t.Fatalf("installedVersion %q while v1.15.0 is not ready, want v1.15.0", st.InstalledVersion)
	}

	running.ready = true
	reconcileProbe(t, r)
	st = probeStatus(t, c)
	if st.InstalledVersion != probeRelease || st.State != k4allv1alpha1.ComponentStateInstalled {
		t.Fatalf("%s at %q (%s), want Installed at v1.16.0 once v1.15.0 runs", st.State, st.InstalledVersion, st.Message)
	}
}

// KubeVirt and CDI cannot go back a version: a component switched to
// Managed that is newer than the release is left as it is, and says so.
func TestReconcile_ComponentSwitchedToManagedNewerThanTheReleaseIsNotDowngraded(t *testing.T) {
	c, r := switchedToManagedOnEnvtest(t, &runningProbe{version: "v1.17.0", ready: true})

	reconcileProbe(t, r)

	st := probeStatus(t, c)
	if st.State != k4allv1alpha1.ComponentStateFailed || st.InstalledVersion != "" {
		t.Fatalf("%s at %q (%s), want Failed with no installedVersion: v1.17.0 runs, the release has v1.16.0",
			st.State, st.InstalledVersion, st.Message)
	}
	for _, v := range []string{"v1.17.0", probeRelease} {
		if !strings.Contains(st.Message, v) {
			t.Errorf("message %q should name %s", st.Message, v)
		}
	}
}

// Not knowing what runs is no reason to jump to the release's version.
func TestReconcile_SwitchToManagedStopsWhenTheRunningVersionCannotBeRead(t *testing.T) {
	c, r := switchedToManagedOnEnvtest(t, &runningProbe{err: errors.New("the server is currently unable to handle the request")})

	reconcileProbe(t, r)

	st := probeStatus(t, c)
	if st.State != k4allv1alpha1.ComponentStateFailed || st.InstalledVersion != "" {
		t.Fatalf("%s at %q (%s), want Failed with no installedVersion", st.State, st.InstalledVersion, st.Message)
	}
	if !strings.Contains(st.Message, "unable to handle the request") {
		t.Errorf("message %q should carry the error", st.Message)
	}
}

// On an upgrade step a read error keeps the component waiting on its step: as Failed,
// the next reconcile would apply the following step without waiting.
func TestReconcile_ReadErrorOnAnUpgradeStepKeepsWaitingOnTheStep(t *testing.T) {
	running := &runningProbe{version: "v1.14.0", ready: true}
	c, r := switchedToManagedOnEnvtest(t, running)
	reconcileProbe(t, r) // applies the step v1.15.0

	running.err = errors.New("connection refused")
	reconcileProbe(t, r)
	if msg := probeStatus(t, c).Message; !strings.Contains(msg, "connection refused") {
		t.Errorf("message %q should carry the error", msg)
	}
	running.version, running.err = "v1.14.0", nil
	reconcileProbe(t, r)

	st := probeStatus(t, c)
	if st.InstalledVersion != probeStep || st.State != k4allv1alpha1.ComponentStateUpgrading {
		t.Fatalf("%s at %q (%s), want still Upgrading at v1.15.0: v1.14.0 runs", st.State, st.InstalledVersion, st.Message)
	}
}

// The upgrade path starts from a healthy component: one still rolling out (or
// degraded) gets nothing until it is ready.
func TestReconcile_SwitchToManagedWaitsUntilTheRunningVersionIsReady(t *testing.T) {
	running := &runningProbe{version: "v1.14.0"}
	c, r := switchedToManagedOnEnvtest(t, running)

	res := reconcileProbe(t, r)
	st := probeStatus(t, c)
	if st.State != k4allv1alpha1.ComponentStatePending || st.InstalledVersion != "" {
		t.Fatalf("%s at %q (%s), want Pending with no installedVersion while v1.14.0 is not ready",
			st.State, st.InstalledVersion, st.Message)
	}
	if res.RequeueAfter <= 0 || res.RequeueAfter > 30*time.Second {
		t.Errorf("waiting should come back soon, got RequeueAfter %v", res.RequeueAfter)
	}

	running.ready = true
	reconcileProbe(t, r)
	if st := probeStatus(t, c); st.InstalledVersion != probeStep {
		t.Fatalf("installedVersion %q once v1.14.0 is ready, want the step v1.15.0", st.InstalledVersion)
	}
}

// Nothing running is a fresh install: straight to the release's version.
func TestReconcile_ComponentSwitchedToManagedNotRunningInstallsTheReleaseVersion(t *testing.T) {
	c, r := switchedToManagedOnEnvtest(t, &runningProbe{})

	reconcileProbe(t, r)

	st := probeStatus(t, c)
	if st.InstalledVersion != probeRelease || st.State != k4allv1alpha1.ComponentStateInstalled {
		t.Fatalf("%s at %q (%s), want Installed at v1.16.0", st.State, st.InstalledVersion, st.Message)
	}
}

// Helm components report the chart of their Helm release. The chart repository is
// unreachable here, so the step the reconciler picked shows in the failure.
func TestReconcile_HelmComponentSwitchedToManagedFollowsItsUpgradePathFromItsHelmRelease(t *testing.T) {
	spec := k4allv1alpha1.ComponentSpec{
		Type: k4allv1alpha1.ComponentTypeHelm, Namespace: "default", Version: probeRelease,
		Repo: "http://127.0.0.1:1", Chart: "probe",
		UpgradePath: []k4allv1alpha1.UpgradeStep{{Version: probeStep}},
	}
	c, r := onEnvtest(t, spec, k4allv1alpha1.ComponentStatus{
		State: k4allv1alpha1.ComponentStateUnmanaged, LastTransitionTime: metav1.Now(),
	})
	deployHelmRelease(t, r.RestConfig, "default", "probe", "v1.14.0")

	reconcileProbe(t, r)

	st := probeStatus(t, c)
	if st.State != k4allv1alpha1.ComponentStateFailed || !strings.Contains(st.Message, "upgrade step v1.15.0") {
		t.Fatalf("%s (%s), want the failed step v1.15.0: the release runs chart v1.14.0", st.State, st.Message)
	}
}

// deployHelmRelease stores a deployed revision of a release, as `helm install` would.
func deployHelmRelease(t *testing.T, cfg *rest.Config, namespace, name, chartVersion string) {
	t.Helper()
	flags := genericclioptions.NewConfigFlags(false)
	flags.WrapConfigFn = func(*rest.Config) *rest.Config { return cfg }
	flags.Namespace = &namespace
	helmCfg := new(action.Configuration)
	if err := helmCfg.Init(flags, namespace, "secrets", func(string, ...interface{}) {}); err != nil {
		t.Fatal(err)
	}
	rel := &release.Release{
		Name: name, Namespace: namespace, Version: 1,
		Info:  &release.Info{Status: release.StatusDeployed},
		Chart: &chart.Chart{Metadata: &chart.Metadata{Name: name, Version: chartVersion}},
	}
	if err := helmCfg.Releases.Create(rel); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = helmCfg.Releases.Delete(name, 1) })
}

// The status write after a step can be lost (operator restart, API server
// down): the component then runs a later version than the status records,
// and the upgrade path goes on from what runs.
func TestReconcile_UpgradePathGoesOnFromARunningVersionPastTheRecordedStep(t *testing.T) {
	c, r := onEnvtest(t, probeWithUpgradePath(), k4allv1alpha1.ComponentStatus{
		State: k4allv1alpha1.ComponentStateUpgrading, InstalledVersion: "v1.14.0", LastTransitionTime: metav1.Now(),
	})
	r.hookReg = hooks.NewRegistry(c, logr.Discard())
	r.hookReg.Register("probe", &runningProbe{version: probeStep, ready: true})

	reconcileProbe(t, r)

	st := probeStatus(t, c)
	if st.InstalledVersion != probeRelease || st.State != k4allv1alpha1.ComponentStateInstalled {
		t.Fatalf("%s at %q (%s), want Installed at %s: %s runs", st.State, st.InstalledVersion, st.Message, probeRelease, probeStep)
	}
}

// A step that never becomes ready (a degraded KubeVirt) keeps the component
// waiting: the ReleaseManifest is not Ready meanwhile, and says on what.
func TestReconcile_WaitingOnAStepIsNotReady(t *testing.T) {
	c, r := switchedToManagedOnEnvtest(t, &runningProbe{version: "v1.14.0"})

	reconcileProbe(t, r)

	rm := &k4allv1alpha1.ReleaseManifest{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: "k4all"}, rm); err != nil {
		t.Fatal(err)
	}
	ready := meta.FindStatusCondition(rm.Status.Conditions, "Ready")
	if ready == nil || ready.Status != metav1.ConditionFalse || !strings.Contains(ready.Message, "probe") {
		t.Fatalf("Ready = %+v, want False naming probe, which waits", ready)
	}
}
