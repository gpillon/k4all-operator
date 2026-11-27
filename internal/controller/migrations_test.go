package controller

import (
	"context"
	"strconv"
	"strings"
	"testing"

	"github.com/go-logr/logr"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	k4allv1alpha1 "github.com/gpillon/k4all-operator/api/v1alpha1"
	"github.com/gpillon/k4all-operator/internal/migrations"
)

// The v2.6.0 ReleaseManifest moves ingress.nginx to ingress.traefik before
// any component is applied, and not while a node is upgrading.
func TestReconcile_MigratesTheClusterConfigForTheRelease(t *testing.T) {
	reconcile := func(t *testing.T, nodes ...client.Object) *k4allv1alpha1.ClusterConfig {
		t.Helper()
		s := gateScheme(t)
		rm := &k4allv1alpha1.ReleaseManifest{
			ObjectMeta: metav1.ObjectMeta{Name: "k4all"},
			Spec: k4allv1alpha1.ReleaseManifestSpec{Components: map[string]k4allv1alpha1.ComponentSpec{
				// Unsupported type: reconciling fails at once, without network.
				"traefik": {Type: "unsupported", Version: "1"},
				"ingress-nginx": {Type: "unsupported", Version: "1",
					Management: k4allv1alpha1.ComponentManagementRemoved},
			}},
		}
		cc := &k4allv1alpha1.ClusterConfig{
			ObjectMeta: metav1.ObjectMeta{Name: "k4all-cluster-config"},
			Spec: k4allv1alpha1.ClusterConfigSpec{
				Networking: k4allv1alpha1.NetworkingConfig{CNI: k4allv1alpha1.CNIConfig{Type: "calico"}},
				Ingress: k4allv1alpha1.IngressConfig{
					Nginx: &k4allv1alpha1.NginxIngressConfig{DedicatedIP: "192.168.6.193", IsDefault: false},
				},
			},
		}
		c := fake.NewClientBuilder().WithScheme(s).
			WithObjects(append([]client.Object{rm, cc}, nodes...)...).
			WithStatusSubresource(&k4allv1alpha1.ReleaseManifest{}, &k4allv1alpha1.ClusterConfig{}).
			Build()
		r := &ReleaseReconciler{Client: c, Scheme: s, Log: logr.Discard()}
		if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "k4all"}}); err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
		got := &k4allv1alpha1.ClusterConfig{}
		if err := c.Get(context.Background(), types.NamespacedName{Name: "k4all-cluster-config"}, got); err != nil {
			t.Fatal(err)
		}
		return got
	}

	t.Run("migrated", func(t *testing.T) {
		got := reconcile(t, node("cp1", nil))
		want := k4allv1alpha1.TraefikIngressConfig{DedicatedIP: "192.168.6.193", IsDefault: false}
		if got.Spec.Ingress.Traefik == nil || *got.Spec.Ingress.Traefik != want || got.Spec.Ingress.Nginx != nil {
			t.Errorf("ingress = nginx %+v, traefik %+v; want only traefik %+v",
				got.Spec.Ingress.Nginx, got.Spec.Ingress.Traefik, want)
		}
	})

	t.Run("not during a node upgrade", func(t *testing.T) {
		got := reconcile(t, node("cp1", map[string]string{upgradeInProgress: "v2.5.0->v2.6.0"}))
		if got.Spec.Ingress.Nginx == nil || got.Spec.Ingress.Traefik != nil {
			t.Errorf("ingress = nginx %+v, traefik %+v; want it untouched",
				got.Spec.Ingress.Nginx, got.Spec.Ingress.Traefik)
		}
	})
}

// On a real API server the migrated ClusterConfig has no ingress.nginx key
// left, not even an empty one that would read as isDefault true.
func TestMigrations_RemoveTheNginxKey(t *testing.T) {
	c := envtestClient(t)
	ctx := context.Background()
	cc := &k4allv1alpha1.ClusterConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "migrate-nginx"},
		Spec: k4allv1alpha1.ClusterConfigSpec{
			Networking: k4allv1alpha1.NetworkingConfig{CNI: k4allv1alpha1.CNIConfig{Type: "calico"}},
			Ingress: k4allv1alpha1.IngressConfig{
				Nginx: &k4allv1alpha1.NginxIngressConfig{DedicatedIP: "192.168.6.193", IsDefault: false},
			},
		},
	}
	if err := c.Create(ctx, cc); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Delete(context.Background(), cc) })
	rm := &k4allv1alpha1.ReleaseManifest{Spec: k4allv1alpha1.ReleaseManifestSpec{
		Components: map[string]k4allv1alpha1.ComponentSpec{"traefik": {Type: "helm", Version: "1"}},
	}}

	if _, err := migrations.Run(ctx, c, rm, cc); err != nil {
		t.Fatal(err)
	}

	got := &unstructured.Unstructured{}
	got.SetGroupVersionKind(k4allv1alpha1.GroupVersion.WithKind("ClusterConfig"))
	if err := c.Get(ctx, types.NamespacedName{Name: "migrate-nginx"}, got); err != nil {
		t.Fatal(err)
	}
	ingress, _, _ := unstructured.NestedMap(got.Object, "spec", "ingress")
	if _, ok := ingress["nginx"]; ok {
		t.Errorf("spec.ingress = %v, want no nginx key", ingress)
	}
	if traefik, _ := ingress["traefik"].(map[string]interface{}); traefik["dedicatedIP"] != "192.168.6.193" || traefik["isDefault"] != false {
		t.Errorf("spec.ingress.traefik = %v, want dedicatedIP 192.168.6.193 and isDefault false", ingress["traefik"])
	}
}

// At step 1/2 of the k4all v2.4.0 sync, operator v0.5.0 starts with the
// cluster's v2.2.x ReleaseManifest (ingress-nginx, no traefik): until step
// 2/2 brings Traefik, the nginx-to-traefik migration must neither convert
// nor count itself. With Traefik in and ingress-nginx Removed, it does both.
func TestMigrations_WaitForTraefikInTheReleaseManifest(t *testing.T) {
	c := envtestClient(t)
	ctx := context.Background()
	cc := &k4allv1alpha1.ClusterConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "migrate-at-v24-sync"},
		Spec: k4allv1alpha1.ClusterConfigSpec{
			Networking: k4allv1alpha1.NetworkingConfig{CNI: k4allv1alpha1.CNIConfig{Type: "calico"}},
			Ingress: k4allv1alpha1.IngressConfig{
				Nginx: &k4allv1alpha1.NginxIngressConfig{DedicatedIP: "192.168.6.193", IsDefault: false},
			},
		},
	}
	if err := c.Create(ctx, cc); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Delete(context.Background(), cc) })

	stored := func() *k4allv1alpha1.ClusterConfig {
		got := &k4allv1alpha1.ClusterConfig{}
		if err := c.Get(ctx, types.NamespacedName{Name: cc.Name}, got); err != nil {
			t.Fatal(err)
		}
		return got
	}
	manifest := func(components map[string]k4allv1alpha1.ComponentManagement) *k4allv1alpha1.ReleaseManifest {
		rm := &k4allv1alpha1.ReleaseManifest{Spec: k4allv1alpha1.ReleaseManifestSpec{
			Components: map[string]k4allv1alpha1.ComponentSpec{},
		}}
		for name, m := range components {
			rm.Spec.Components[name] = k4allv1alpha1.ComponentSpec{Type: "helm", Version: "1", Management: m}
		}
		return rm
	}

	v22 := manifest(map[string]k4allv1alpha1.ComponentManagement{
		"ingress-nginx": k4allv1alpha1.ComponentManagementManaged,
		"headlamp":      k4allv1alpha1.ComponentManagementManaged,
	})
	if changed, err := migrations.Run(ctx, c, v22, stored()); err != nil || changed {
		t.Fatalf("v2.2.x manifest: changed %t, err %v; want no change", changed, err)
	}
	got := stored()
	if got.Spec.Ingress.Nginx == nil || got.Spec.Ingress.Traefik != nil {
		t.Errorf("v2.2.x manifest: ingress = nginx %+v, traefik %+v; want it untouched", got.Spec.Ingress.Nginx, got.Spec.Ingress.Traefik)
	}
	if v, ok := got.Annotations[migrations.AppliedAnnotation]; ok {
		t.Errorf("v2.2.x manifest: %s = %q, want none", migrations.AppliedAnnotation, v)
	}

	v24 := manifest(map[string]k4allv1alpha1.ComponentManagement{
		"ingress-nginx": k4allv1alpha1.ComponentManagementRemoved,
		"traefik":       k4allv1alpha1.ComponentManagementManaged,
		"headlamp":      k4allv1alpha1.ComponentManagementManaged,
	})
	// Step 2/2 just applied: ingress-nginx is uninstalled after the migrations.
	v24.Status.Components = map[string]k4allv1alpha1.ComponentStatus{
		"ingress-nginx": {State: k4allv1alpha1.ComponentStateInstalled},
	}
	if changed, err := migrations.Run(ctx, c, v24, stored()); err != nil || !changed {
		t.Fatalf("v2.4.0 manifest: changed %t, err %v; want a change", changed, err)
	}
	got = stored()
	want := k4allv1alpha1.TraefikIngressConfig{DedicatedIP: "192.168.6.193", IsDefault: false}
	if got.Spec.Ingress.Nginx != nil || got.Spec.Ingress.Traefik == nil || *got.Spec.Ingress.Traefik != want {
		t.Errorf("v2.4.0 manifest: ingress = nginx %+v, traefik %+v; want only traefik %+v",
			got.Spec.Ingress.Nginx, got.Spec.Ingress.Traefik, want)
	}
	if v := got.Annotations[migrations.AppliedAnnotation]; v != "1" {
		t.Errorf("v2.4.0 manifest: %s = %q, want \"1\"", migrations.AppliedAnnotation, v)
	}
}

// A cluster past the v2.4.0 sync with operator v0.5.0 kept the ingress-nginx
// namespace with what the release left. Once on v0.5.1, migration 2 deletes
// it, through the same check as the removal's cleanup.
func TestMigrations_CleanTheIngressNginxNamespaceLeftByV050(t *testing.T) {
	c := envtestClient(t)
	ctx := context.Background()
	ns := "ingress-nginx"
	// The migration cleans this very name. envtest never finishes deleting
	// a namespace: a second run of the test (go test -count) finds the first
	// run's still being deleted, and cannot make it again.
	left := &corev1.Namespace{}
	if err := c.Get(ctx, client.ObjectKey{Name: ns}, left); err == nil && left.DeletionTimestamp != nil {
		t.Skip("namespace ingress-nginx is still being deleted by an earlier run of this test")
	}
	for _, o := range []client.Object{
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "ingress-nginx-admission"}},
		&coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "ingress-nginx-leader"}},
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "kube-root-ca.crt"}},
		&corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "default"}},
	} {
		if err := c.Create(ctx, o); err != nil {
			t.Fatalf("create %s: %v", o.GetName(), err)
		}
	}
	cc := &k4allv1alpha1.ClusterConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "past-v24-with-v050", Annotations: map[string]string{migrations.AppliedAnnotation: "1"}},
		Spec: k4allv1alpha1.ClusterConfigSpec{
			Networking: k4allv1alpha1.NetworkingConfig{CNI: k4allv1alpha1.CNIConfig{Type: "calico"}},
			Ingress:    k4allv1alpha1.IngressConfig{Traefik: &k4allv1alpha1.TraefikIngressConfig{IsDefault: true}},
		},
	}
	if err := c.Create(ctx, cc); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Delete(context.Background(), cc) })
	rm := &k4allv1alpha1.ReleaseManifest{
		Spec: k4allv1alpha1.ReleaseManifestSpec{Components: map[string]k4allv1alpha1.ComponentSpec{
			"traefik":       {Type: "helm", Version: "42.4.0"},
			"ingress-nginx": {Type: "helm", Version: "4.14.3", Management: k4allv1alpha1.ComponentManagementRemoved},
		}},
		Status: k4allv1alpha1.ReleaseManifestStatus{Components: map[string]k4allv1alpha1.ComponentStatus{
			"ingress-nginx": {State: k4allv1alpha1.ComponentStateRemoved},
		}},
	}

	env := migrations.Env{Client: c, RestConfig: envtestConfig(t), Log: logr.Discard()}
	if changed, err := migrations.RunWith(ctx, env, rm, cc); err != nil || !changed {
		t.Fatalf("changed %t, err %v; want a change", changed, err)
	}

	got := &corev1.Namespace{}
	err := c.Get(ctx, types.NamespacedName{Name: ns}, got)
	if err == nil && got.DeletionTimestamp == nil {
		t.Errorf("namespace %s kept, holding only what ingress-nginx left", ns)
	} else if err != nil && !apierrors.IsNotFound(err) {
		t.Fatal(err)
	}
	stored := &k4allv1alpha1.ClusterConfig{}
	if err := c.Get(ctx, types.NamespacedName{Name: cc.Name}, stored); err != nil {
		t.Fatal(err)
	}
	if v := stored.Annotations[migrations.AppliedAnnotation]; v != "2" {
		t.Errorf("%s = %q, want \"2\"", migrations.AppliedAnnotation, v)
	}
}

// An operator older than the one that ran the ClusterConfig's migrations
// (k4all#77) applies no component: they may need what it does not know.
// The ClusterConfig and the ReleaseManifest say why until the count fits.
func TestReconcile_StopsWhenTheMigrationCountIsAheadOfTheOperator(t *testing.T) {
	cl := newCluster(t,
		// Unsupported type: a reconcile of it would mark it Failed.
		map[string]k4allv1alpha1.ComponentSpec{"probe": {Type: "unsupported", Version: "v2"}},
		map[string]k4allv1alpha1.ComponentStatus{"probe": {State: k4allv1alpha1.ComponentStateInstalled, InstalledVersion: "v1"}},
	)
	setMigrationCount(t, cl, strconv.Itoa(len(migrations.All)+1))

	if res := cl.reconcile(); res.RequeueAfter <= 0 {
		t.Errorf("RequeueAfter = %v, want a later look", res.RequeueAfter)
	}

	rm := cl.manifest()
	if st := rm.Status.Components["probe"]; st.State != k4allv1alpha1.ComponentStateInstalled || st.InstalledVersion != "v1" {
		t.Errorf("probe = %s at %q (%s), want it left Installed at v1", st.State, st.InstalledVersion, st.Message)
	}
	for _, conds := range map[string][]metav1.Condition{
		"ReleaseManifest": rm.Status.Conditions, "ClusterConfig": cl.config().Status.Conditions,
	} {
		ahead := meta.FindStatusCondition(conds, "MigrationsAhead")
		if ahead == nil || ahead.Status != metav1.ConditionTrue || !strings.Contains(ahead.Message, "older than the operator that ran them") {
			t.Errorf("MigrationsAhead = %+v, want True naming the older operator", ahead)
		}
	}
	if ready := meta.FindStatusCondition(rm.Status.Conditions, "Ready"); ready == nil || ready.Reason != "MigrationsAhead" {
		t.Errorf("Ready = %+v, want reason MigrationsAhead", ready)
	}

	// once the count fits (a newer operator, or a person who checked), the
	// components go on and the condition goes
	setMigrationCount(t, cl, strconv.Itoa(len(migrations.All)))
	cl.reconcile()
	rm = cl.manifest()
	if st := rm.Status.Components["probe"]; st.State != k4allv1alpha1.ComponentStateFailed {
		t.Errorf("probe = %s, want it reconciled again (Failed: unsupported type)", st.State)
	}
	for _, conds := range [][]metav1.Condition{rm.Status.Conditions, cl.config().Status.Conditions} {
		if meta.IsStatusConditionTrue(conds, "MigrationsAhead") {
			t.Errorf("MigrationsAhead still True: %+v", conds)
		}
	}
}

func setMigrationCount(t *testing.T, cl *cluster, count string) {
	t.Helper()
	cc := cl.config()
	if cc.Annotations == nil {
		cc.Annotations = map[string]string{}
	}
	cc.Annotations[migrations.AppliedAnnotation] = count
	if err := cl.c.Update(context.Background(), cc); err != nil {
		t.Fatal(err)
	}
}

func (cl *cluster) config() *k4allv1alpha1.ClusterConfig {
	cl.t.Helper()
	cc := &k4allv1alpha1.ClusterConfig{}
	if err := cl.c.Get(context.Background(), types.NamespacedName{Name: "k4all-cluster-config"}, cc); err != nil {
		cl.t.Fatal(err)
	}
	return cc
}

// MigrationsAhead stops uninstalls too: a component that leaves the release,
// either way, stays installed until the count fits.
func TestReconcile_MigrationsAheadKeepsComponentsThatLeaveTheRelease(t *testing.T) {
	for name, goAway := range map[string]func(*k4allv1alpha1.ComponentSpec){
		"its feature is off":    func(s *k4allv1alpha1.ComponentSpec) { s.FeatureGate = gateOff },
		"management is Removed": func(s *k4allv1alpha1.ComponentSpec) { s.Management = k4allv1alpha1.ComponentManagementRemoved },
	} {
		t.Run(name, func(t *testing.T) {
			spec := k4allv1alpha1.ComponentSpec{Type: "unsupported", Version: "v1"}
			goAway(&spec)
			installed := k4allv1alpha1.ComponentStatus{State: k4allv1alpha1.ComponentStateInstalled, InstalledVersion: "v1"}
			cl := newCluster(t, map[string]k4allv1alpha1.ComponentSpec{"probe": spec},
				map[string]k4allv1alpha1.ComponentStatus{"probe": installed})
			setMigrationCount(t, cl, strconv.Itoa(len(migrations.All)+1))

			cl.reconcile()

			if st := cl.manifest().Status.Components["probe"]; st.State != installed.State || st.InstalledVersion != "v1" {
				t.Fatalf("probe = %s at %q (%s), want it left Installed at v1", st.State, st.InstalledVersion, st.Message)
			}

			setMigrationCount(t, cl, strconv.Itoa(len(migrations.All)))
			cl.reconcile()

			if st := cl.manifest().Status.Components["probe"]; st.State == installed.State {
				t.Errorf("probe still %s (%s) once the count fits, want its uninstall tried", st.State, st.Message)
			}
		})
	}
}
