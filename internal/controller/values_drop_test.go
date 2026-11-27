package controller

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	k4allv1alpha1 "github.com/gpillon/k4all-operator/api/v1alpha1"
	"helm.sh/helm/v3/pkg/action"
	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/chartutil"
	"helm.sh/helm/v3/pkg/release"
	"helm.sh/helm/v3/pkg/repo"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// gpillon/k4all#114: the operator upgrades a Helm release without
// ReuseValues, with its own values only. A release it did not write last
// (installed by hand, restored from 1.x, upgraded by hand while Unmanaged)
// is not upgraded when that drops values of its own, until a person carries
// them in componentOverrides or accepts the drop.

const (
	takeoverChart     = "values-drop-probe"
	takeoverVersion   = "0.1.0"
	takeoverNamespace = "values-drop"
	// helmCLI is the user agent of the helm command, whose writes the API
	// server records for the field manager "Helm".
	helmCLI = "Helm/v3.17.3"
)

// takeoverRepo serves a chart repository holding the probe chart.
func takeoverRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	ch := takeoverChartFile(t)
	if _, err := chartutil.Save(ch, dir); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.FileServer(http.Dir(dir)))
	t.Cleanup(srv.Close)
	index, err := repo.IndexDirectory(dir, srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if err := index.WriteFile(filepath.Join(dir, "index.yaml"), 0o644); err != nil {
		t.Fatal(err)
	}
	return srv.URL
}

// takeoverChartFile is the probe chart: one ConfigMap, whose data are the
// chart's values "data".
func takeoverChartFile(t *testing.T) *chart.Chart {
	t.Helper()
	return &chart.Chart{
		Metadata: &chart.Metadata{APIVersion: chart.APIVersionV2, Name: takeoverChart, Version: takeoverVersion},
		Templates: []*chart.File{{Name: "templates/configmap.yaml", Data: []byte(`apiVersion: v1
kind: ConfigMap
metadata:
  name: ` + takeoverChart + `
data:
{{- range $k, $v := .Values.data }}
  {{ $k }}: {{ $v | quote }}
{{- end }}
`)}},
	}
}

// helmAs is a Helm configuration over the envtest API server whose writes
// carry userAgent.
func helmAs(t *testing.T, cfg *rest.Config, userAgent string) *action.Configuration {
	t.Helper()
	as := rest.CopyConfig(cfg)
	as.UserAgent = userAgent
	namespace := takeoverNamespace
	flags := genericclioptions.NewConfigFlags(false)
	flags.WrapConfigFn = func(*rest.Config) *rest.Config { return as }
	flags.Namespace = &namespace
	helmCfg := new(action.Configuration)
	if err := helmCfg.Init(flags, namespace, "secrets", func(string, ...interface{}) {}); err != nil {
		t.Fatal(err)
	}
	return helmCfg
}

// takeoverOnEnvtest is the probe component, Managed, over its Helm release
// as `helm install` left it with values (none installed with nil), its
// status Unmanaged, as before a switch to Managed. The ReleaseManifest's
// values are data.a=1.
func takeoverOnEnvtest(
	t *testing.T, installed map[string]interface{},
) (client.Client, *ReleaseReconciler, *action.Configuration) {
	t.Helper()
	spec := k4allv1alpha1.ComponentSpec{
		Type: k4allv1alpha1.ComponentTypeHelm, Namespace: takeoverNamespace, Version: takeoverVersion,
		Repo: takeoverRepo(t), Chart: takeoverChart,
		Values: &apiextensionsv1.JSON{Raw: []byte(`{"data":{"a":"1"}}`)},
	}
	c, r := onEnvtest(t, spec, k4allv1alpha1.ComponentStatus{
		State: k4allv1alpha1.ComponentStateUnmanaged, Message: "lifecycle not managed by operator",
		LastTransitionTime: metav1.Now(),
	})
	ensureNamespace(t, c, takeoverNamespace)
	byHand := helmAs(t, r.RestConfig, helmCLI)
	t.Cleanup(func() {
		_, _ = action.NewUninstall(byHand).Run("probe")
		_ = c.Delete(context.Background(), &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Namespace: takeoverNamespace, Name: takeoverChart}})
		_ = os.RemoveAll(filepath.Join(os.TempDir(), "k4all-helm-cache", takeoverChart))
	})
	if installed != nil {
		install := action.NewInstall(byHand)
		install.ReleaseName = "probe"
		install.Namespace = takeoverNamespace
		if _, err := install.Run(takeoverChartFile(t), installed); err != nil {
			t.Fatalf("helm install by hand: %v", err)
		}
	}
	return c, r, byHand
}

func lastRevision(t *testing.T, helmCfg *action.Configuration) *release.Release {
	t.Helper()
	last, err := helmCfg.Releases.Last("probe")
	if err != nil {
		t.Fatal(err)
	}
	return last
}

func releaseManifest(t *testing.T, c client.Client) *k4allv1alpha1.ReleaseManifest {
	t.Helper()
	rm := &k4allv1alpha1.ReleaseManifest{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: "k4all"}, rm); err != nil {
		t.Fatal(err)
	}
	return rm
}

func annotateReleaseManifest(t *testing.T, c client.Client, key, value string) {
	t.Helper()
	rm := releaseManifest(t, c)
	base := rm.DeepCopy()
	rm.SetAnnotations(map[string]string{key: value})
	if err := c.Patch(context.Background(), rm, client.MergeFrom(base)); err != nil {
		t.Fatal(err)
	}
}

func setOverride(t *testing.T, c client.Client, name, values string) {
	t.Helper()
	cc := &k4allv1alpha1.ClusterConfig{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: "k4all-cluster-config"}, cc); err != nil {
		t.Fatal(err)
	}
	if values == "" {
		delete(cc.Spec.ComponentOverrides, name)
	} else {
		if cc.Spec.ComponentOverrides == nil {
			cc.Spec.ComponentOverrides = map[string]k4allv1alpha1.ComponentOverride{}
		}
		cc.Spec.ComponentOverrides[name] = k4allv1alpha1.ComponentOverride{Values: &apiextensionsv1.JSON{Raw: []byte(values)}}
	}
	if err := c.Update(context.Background(), cc); err != nil {
		t.Fatal(err)
	}
}

// installedByHand are the values of the release installed by hand: the
// chart's data, and two values the ReleaseManifest does not set.
func installedByHand() map[string]interface{} {
	return map[string]interface{}{
		"data":     map[string]interface{}{"a": "1"},
		"extra":    map[string]interface{}{"kept": true},
		"replicas": 2,
	}
}

func expectInstalledByTheOperator(t *testing.T, c client.Client, byHand *action.Configuration) *release.Release {
	t.Helper()
	if st := probeStatus(t, c); st.State != k4allv1alpha1.ComponentStateInstalled {
		t.Fatalf("probe: %s (%s), want Installed", st.State, st.Message)
	}
	last := lastRevision(t, byHand)
	if last.Version != 2 || last.Info.Status != release.StatusDeployed {
		t.Fatalf("last revision %d %s, want revision 2 deployed by the operator", last.Version, last.Info.Status)
	}
	return last
}

func TestReconcile_ATakeoverThatWouldDropValuesIsHeld(t *testing.T) {
	c, r, byHand := takeoverOnEnvtest(t, installedByHand())

	reconcileProbe(t, r)

	st := probeStatus(t, c)
	if st.State != k4allv1alpha1.ComponentStateFailed {
		t.Fatalf("probe: %s (%s), want Failed: not taken over", st.State, st.Message)
	}
	for _, want := range []string{
		"not taking over: ", "revision 1 of the Helm release probe", "written by Helm", "extra.kept, replicas",
		"componentOverrides.probe", AllowValuesDropAnnotation + ": probe=1",
	} {
		if !strings.Contains(st.Message, want) {
			t.Errorf("message %q does not say %q", st.Message, want)
		}
	}
	if retried(st) {
		t.Errorf("a held takeover is retried with the backoff: it waits on a person")
	}
	cond := meta.FindStatusCondition(releaseManifest(t, c).Status.Conditions, valuesDropRefusedCondition)
	if cond == nil || cond.Status != metav1.ConditionTrue || !strings.HasSuffix(cond.Message, ": probe") {
		t.Errorf("%s = %+v, want True naming probe", valuesDropRefusedCondition, cond)
	}
	if last := lastRevision(t, byHand); last.Version != 1 || last.Info.Status != release.StatusDeployed {
		t.Errorf("last revision %d %s, want the release untouched at revision 1", last.Version, last.Info.Status)
	}
}

// Accepting the drop of that revision: the release is upgraded with k4all's
// values. A key written into the release's ConfigMap outside Helm (as
// cilium-config's devices by hand) stays: Helm's three-way merge removes
// only what its previous revision rendered.
func TestReconcile_ATakeoverWhoseDropIsAllowedIsApplied(t *testing.T) {
	c, r, byHand := takeoverOnEnvtest(t, installedByHand())
	reconcileProbe(t, r)
	cm := &corev1.ConfigMap{}
	key := types.NamespacedName{Namespace: takeoverNamespace, Name: takeoverChart}
	if err := c.Get(context.Background(), key, cm); err != nil {
		t.Fatal(err)
	}
	cm.Data["devices"] = "ovs-bridge"
	if err := c.Update(context.Background(), cm); err != nil {
		t.Fatal(err)
	}

	annotateReleaseManifest(t, c, AllowValuesDropAnnotation, "other=1, probe=1")
	reconcileProbe(t, r)

	last := expectInstalledByTheOperator(t, c, byHand)
	if want := map[string]interface{}{"data": map[string]interface{}{"a": "1"}}; !reflect.DeepEqual(last.Config, want) {
		t.Errorf("values %v, want k4all's %v", last.Config, want)
	}
	if meta.FindStatusCondition(releaseManifest(t, c).Status.Conditions, valuesDropRefusedCondition) != nil {
		t.Errorf("%s still set once the takeover is done", valuesDropRefusedCondition)
	}
	if err := c.Get(context.Background(), key, cm); err != nil {
		t.Fatal(err)
	}
	if cm.Data["devices"] != "ovs-bridge" || cm.Data["a"] != "1" {
		t.Errorf("ConfigMap data %v, want a=1 and the key written outside Helm kept", cm.Data)
	}
}

// The values carried in componentOverrides: nothing would go.
func TestReconcile_ATakeoverWithTheValuesCarriedIsApplied(t *testing.T) {
	c, r, byHand := takeoverOnEnvtest(t, installedByHand())
	setOverride(t, c, "probe", `{"extra":{"kept":true},"replicas":2}`)

	reconcileProbe(t, r)

	last := expectInstalledByTheOperator(t, c, byHand)
	if !reflect.DeepEqual(last.Config, map[string]interface{}{
		"data": map[string]interface{}{"a": "1"}, "extra": map[string]interface{}{"kept": true}, "replicas": float64(2),
	}) {
		t.Errorf("values %v, want the release's values carried", last.Config)
	}
}

// A release the operator wrote last is upgraded whatever its new values
// drop: a release that stops setting a value is a normal hop, never held.
// An older operator wrote with the same field manager (the same binary):
// a cluster on v0.6.0 counts as written by the operator, with no step.
func TestReconcile_AReleaseTheOperatorWroteLastIsUpgradedThoughItDropsAValue(t *testing.T) {
	c, r, byHand := takeoverOnEnvtest(t, nil)
	setOverride(t, c, "probe", `{"extra":{"kept":true}}`)
	reconcileProbe(t, r)
	if st := probeStatus(t, c); st.State != k4allv1alpha1.ComponentStateInstalled {
		t.Fatalf("fresh install: %s (%s), want Installed", st.State, st.Message)
	}

	setOverride(t, c, "probe", "")
	reconcileProbe(t, r)

	last := expectInstalledByTheOperator(t, c, byHand)
	if _, ok := last.Config["extra"]; ok {
		t.Errorf("values %v, want extra dropped by the operator's own upgrade", last.Config)
	}
}
