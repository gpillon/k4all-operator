package migrations

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	k4allv1alpha1 "github.com/gpillon/k4all-operator/api/v1alpha1"
)

const configName = "k4all-cluster-config"

// manifest is a ReleaseManifest with the given component managements.
func manifest(components map[string]k4allv1alpha1.ComponentManagement) *k4allv1alpha1.ReleaseManifest {
	rm := &k4allv1alpha1.ReleaseManifest{
		ObjectMeta: metav1.ObjectMeta{Name: "k4all"},
		Spec:       k4allv1alpha1.ReleaseManifestSpec{Components: map[string]k4allv1alpha1.ComponentSpec{}},
	}
	for name, m := range components {
		rm.Spec.Components[name] = k4allv1alpha1.ComponentSpec{Type: "helm", Version: "1", Management: m}
	}
	return rm
}

// v26 is the reconcile right after the v2.4.0 sync: Traefik in,
// ingress-nginx Removed but not uninstalled yet (its namespace migration,
// the next one, is not due yet).
func v26() *k4allv1alpha1.ReleaseManifest {
	rm := manifest(map[string]k4allv1alpha1.ComponentManagement{
		"traefik":       k4allv1alpha1.ComponentManagementManaged,
		"ingress-nginx": k4allv1alpha1.ComponentManagementRemoved,
	})
	rm.Status.Components = map[string]k4allv1alpha1.ComponentStatus{
		"ingress-nginx": {State: k4allv1alpha1.ComponentStateInstalled},
	}
	return rm
}

func clusterConfig(ingress k4allv1alpha1.IngressConfig, annotations map[string]string) *k4allv1alpha1.ClusterConfig {
	return &k4allv1alpha1.ClusterConfig{
		ObjectMeta: metav1.ObjectMeta{Name: configName, Annotations: annotations},
		Spec: k4allv1alpha1.ClusterConfigSpec{
			Networking: k4allv1alpha1.NetworkingConfig{CNI: k4allv1alpha1.CNIConfig{Type: "calico"}},
			Ingress:    ingress,
		},
	}
}

func newClient(t *testing.T, cc *k4allv1alpha1.ClusterConfig) client.Client {
	t.Helper()
	s := runtime.NewScheme()
	if err := k4allv1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return fake.NewClientBuilder().WithScheme(s).WithObjects(cc).Build()
}

// run runs the migrations on cc and returns what is stored afterwards.
func run(t *testing.T, rm *k4allv1alpha1.ReleaseManifest, cc *k4allv1alpha1.ClusterConfig) (*k4allv1alpha1.ClusterConfig, bool) {
	t.Helper()
	c := newClient(t, cc)
	inMemory := &k4allv1alpha1.ClusterConfig{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: configName}, inMemory); err != nil {
		t.Fatal(err)
	}
	changed, err := Run(context.Background(), c, rm, inMemory)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	stored := &k4allv1alpha1.ClusterConfig{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: configName}, stored); err != nil {
		t.Fatal(err)
	}
	if inMemory.ResourceVersion != stored.ResourceVersion {
		t.Errorf("Run left the caller's ClusterConfig at resourceVersion %s, stored is %s",
			inMemory.ResourceVersion, stored.ResourceVersion)
	}
	return stored, changed
}

func TestRun_MovesNginxSettingsToTraefikOnceTraefikReplacesNginx(t *testing.T) {
	for name, tc := range map[string]struct {
		rm    *k4allv1alpha1.ReleaseManifest
		count string
	}{
		"ingress-nginx Removed": {v26(), "1"},
		// Gone from the manifest and uninstalled: the namespace migration runs too.
		"ingress-nginx gone": {manifest(map[string]k4allv1alpha1.ComponentManagement{
			"traefik": k4allv1alpha1.ComponentManagementManaged,
		}), "2"},
	} {
		t.Run(name, func(t *testing.T) {
			rm := tc.rm
			got, changed := run(t, rm, clusterConfig(k4allv1alpha1.IngressConfig{
				Nginx: &k4allv1alpha1.NginxIngressConfig{DedicatedIP: "192.168.6.193", IsDefault: false},
			}, nil))

			if !changed {
				t.Error("Run reported no change")
			}
			want := k4allv1alpha1.TraefikIngressConfig{DedicatedIP: "192.168.6.193", IsDefault: false}
			if got.Spec.Ingress.Traefik == nil || *got.Spec.Ingress.Traefik != want {
				t.Errorf("ingress.traefik = %+v, want %+v", got.Spec.Ingress.Traefik, want)
			}
			if got.Spec.Ingress.Nginx != nil {
				t.Errorf("ingress.nginx = %+v, want it removed", got.Spec.Ingress.Nginx)
			}
			if v := got.Annotations[AppliedAnnotation]; v != tc.count {
				t.Errorf("%s = %q, want %q", AppliedAnnotation, v, tc.count)
			}
		})
	}
}

func TestRun_KeepsAnIngressTraefikAlreadySet(t *testing.T) {
	got, _ := run(t, v26(), clusterConfig(k4allv1alpha1.IngressConfig{
		Nginx:   &k4allv1alpha1.NginxIngressConfig{DedicatedIP: "192.168.6.193", IsDefault: true},
		Traefik: &k4allv1alpha1.TraefikIngressConfig{DedicatedIP: "192.168.6.194", IsDefault: false},
	}, nil))

	want := k4allv1alpha1.TraefikIngressConfig{DedicatedIP: "192.168.6.194", IsDefault: false}
	if got.Spec.Ingress.Traefik == nil || *got.Spec.Ingress.Traefik != want {
		t.Errorf("ingress.traefik = %+v, want %+v", got.Spec.Ingress.Traefik, want)
	}
	if got.Spec.Ingress.Nginx != nil {
		t.Errorf("ingress.nginx = %+v, want it removed", got.Spec.Ingress.Nginx)
	}
}

func TestRun_WaitsWhileNginxIsTheController(t *testing.T) {
	for name, rm := range map[string]*k4allv1alpha1.ReleaseManifest{
		"no traefik": manifest(map[string]k4allv1alpha1.ComponentManagement{
			"ingress-nginx": k4allv1alpha1.ComponentManagementManaged,
		}),
		"both managed": manifest(map[string]k4allv1alpha1.ComponentManagement{
			"ingress-nginx": k4allv1alpha1.ComponentManagementManaged,
			"traefik":       k4allv1alpha1.ComponentManagementManaged,
		}),
		"traefik Removed": manifest(map[string]k4allv1alpha1.ComponentManagement{
			"traefik": k4allv1alpha1.ComponentManagementRemoved,
		}),
	} {
		t.Run(name, func(t *testing.T) {
			got, changed := run(t, rm, clusterConfig(k4allv1alpha1.IngressConfig{
				Nginx: &k4allv1alpha1.NginxIngressConfig{DedicatedIP: "192.168.6.193", IsDefault: true},
			}, nil))

			if changed {
				t.Error("Run reported a change")
			}
			if got.Spec.Ingress.Nginx == nil || got.Spec.Ingress.Traefik != nil {
				t.Errorf("ingress = %+v, want it untouched", got.Spec.Ingress)
			}
			if v, ok := got.Annotations[AppliedAnnotation]; ok {
				t.Errorf("%s = %q, want none", AppliedAnnotation, v)
			}
		})
	}
}

// A migration runs once: an ingress.nginx written after it stays.
func TestRun_RunsOnce(t *testing.T) {
	got, changed := run(t, v26(), clusterConfig(k4allv1alpha1.IngressConfig{
		Nginx:   &k4allv1alpha1.NginxIngressConfig{DedicatedIP: "192.168.6.193", IsDefault: true},
		Traefik: &k4allv1alpha1.TraefikIngressConfig{IsDefault: true},
	}, map[string]string{AppliedAnnotation: "1"}))

	if changed {
		t.Error("Run reported a change")
	}
	if got.Spec.Ingress.Nginx == nil {
		t.Error("ingress.nginx removed again")
	}
}

func TestRun_RecordsAMigrationWithNothingToMove(t *testing.T) {
	got, changed := run(t, v26(), clusterConfig(k4allv1alpha1.IngressConfig{}, nil))

	if !changed {
		t.Error("Run reported no change")
	}
	if got.Spec.Ingress.Nginx != nil || got.Spec.Ingress.Traefik != nil {
		t.Errorf("ingress = %+v, want it empty", got.Spec.Ingress)
	}
	if v := got.Annotations[AppliedAnnotation]; v != "1" {
		t.Errorf("%s = %q, want \"1\"", AppliedAnnotation, v)
	}
}

func TestRun_RefusesAnUnreadableCount(t *testing.T) {
	cc := clusterConfig(k4allv1alpha1.IngressConfig{}, map[string]string{AppliedAnnotation: "one"})
	c := newClient(t, cc)
	if _, err := Run(context.Background(), c, v26(), cc); err == nil {
		t.Error("Run accepted migrations: \"one\"")
	}
}
