package hooks_test

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	k4allv1alpha1 "github.com/gpillon/k4all-operator/api/v1alpha1"
	"github.com/gpillon/k4all-operator/internal/controller/hooks"
	"github.com/gpillon/k4all-operator/internal/operatorscheme"
)

const (
	dashboardNodeIP      = "192.168.6.10"
	dashboardFQDN        = "dashboard.k4all.example.com"
	middlewareAnnotation = "traefik.ingress.kubernetes.io/router.middlewares"
	traefikClass         = "traefik"
)

var middlewareGVK = schema.GroupVersionKind{Group: "traefik.io", Version: "v1alpha1", Kind: "Middleware"}

// releaseManifest has the named components, Managed, and ingress-nginx
// Removed when traefik is among them, as from k4all v2.6.0.
func releaseManifest(components ...string) *k4allv1alpha1.ReleaseManifest {
	rm := &k4allv1alpha1.ReleaseManifest{
		ObjectMeta: metav1.ObjectMeta{Name: "k4all"},
		Spec:       k4allv1alpha1.ReleaseManifestSpec{Components: map[string]k4allv1alpha1.ComponentSpec{}},
	}
	for _, name := range components {
		rm.Spec.Components[name] = k4allv1alpha1.ComponentSpec{Type: "helm", Version: "1"}
	}
	if _, ok := rm.Spec.Components["traefik"]; ok {
		rm.Spec.Components["ingress-nginx"] = k4allv1alpha1.ComponentSpec{
			Type: "helm", Version: "1", Management: k4allv1alpha1.ComponentManagementRemoved,
		}
	}
	return rm
}

// redirectMiddleware is the Middleware the k4all Traefik values create.
func redirectMiddleware() *unstructured.Unstructured {
	m := &unstructured.Unstructured{}
	m.SetGroupVersionKind(middlewareGVK)
	m.SetNamespace("traefik")
	m.SetName("redirect-https")
	return m
}

// dashboard runs the headlamp post-install hook on a one-node cluster with
// objs and returns the dashboard Ingress.
func dashboard(t *testing.T, config k4allv1alpha1.ClusterConfigSpec, objs ...client.Object) *networkingv1.Ingress {
	t.Helper()
	s := operatorscheme.New()
	s.AddKnownTypeWithName(middlewareGVK, &unstructured.Unstructured{})
	s.AddKnownTypeWithName(middlewareGVK.GroupVersion().WithKind("MiddlewareList"), &unstructured.UnstructuredList{})
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "cp1"},
		Status: corev1.NodeStatus{Addresses: []corev1.NodeAddress{
			{Type: corev1.NodeInternalIP, Address: dashboardNodeIP},
		}},
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(append(objs, node)...).Build()

	hook := hooks.NewRegistry(c, logr.Discard()).Get("headlamp")
	if err := hook.PostInstall(context.Background(), k4allv1alpha1.ComponentSpec{Namespace: "headlamp"}, config); err != nil {
		t.Fatal(err)
	}
	ing := &networkingv1.Ingress{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "headlamp", Name: "headlamp"}, ing); err != nil {
		t.Fatal(err)
	}
	return ing
}

func traefikOn(ip string) k4allv1alpha1.ClusterConfigSpec {
	return k4allv1alpha1.ClusterConfigSpec{Ingress: k4allv1alpha1.IngressConfig{
		Traefik: &k4allv1alpha1.TraefikIngressConfig{DedicatedIP: ip, IsDefault: true},
	}}
}

func hosts(ing *networkingv1.Ingress) []string {
	out := make([]string, 0, len(ing.Spec.Rules))
	for _, r := range ing.Spec.Rules {
		out = append(out, r.Host)
	}
	return out
}

func class(ing *networkingv1.Ingress) string {
	if ing.Spec.IngressClassName == nil {
		return ""
	}
	return *ing.Spec.IngressClassName
}

func nginxAnnotations(ing *networkingv1.Ingress) []string {
	var out []string
	for key := range ing.Annotations {
		if strings.HasPrefix(key, "nginx.ingress.kubernetes.io/") {
			out = append(out, key)
		}
	}
	return out
}

func TestHeadlampHook_DashboardOnNginxBeforeTraefik(t *testing.T) {
	ing := dashboard(t, k4allv1alpha1.ClusterConfigSpec{Ingress: k4allv1alpha1.IngressConfig{
		Nginx: &k4allv1alpha1.NginxIngressConfig{IsDefault: true},
	}}, releaseManifest("ingress-nginx"))

	if class(ing) != "nginx" {
		t.Errorf("ingressClassName = %q, want nginx", class(ing))
	}
	if want := []string{"dashboard." + dashboardNodeIP + ".nip.io"}; !reflect.DeepEqual(hosts(ing), want) {
		t.Errorf("hosts = %v, want %v", hosts(ing), want)
	}
}

func TestHeadlampHook_DashboardOnTraefik(t *testing.T) {
	for name, tc := range map[string]struct{ dedicatedIP, host string }{
		"dedicated IP": {"192.168.6.193", "dashboard.192.168.6.193.nip.io"},
		"node ports":   {"", "dashboard." + dashboardNodeIP + ".nip.io"},
	} {
		t.Run(name, func(t *testing.T) {
			ing := dashboard(t, traefikOn(tc.dedicatedIP), releaseManifest("traefik", "headlamp"), redirectMiddleware())

			if class(ing) != traefikClass {
				t.Errorf("ingressClassName = %q, want traefik", class(ing))
			}
			if want := []string{tc.host}; !reflect.DeepEqual(hosts(ing), want) {
				t.Errorf("hosts = %v, want %v", hosts(ing), want)
			}
			if v := ing.Annotations[middlewareAnnotation]; v != "traefik-redirect-https@kubernetescrd" {
				t.Errorf("%s = %q, want traefik-redirect-https@kubernetescrd", middlewareAnnotation, v)
			}
			if got := nginxAnnotations(ing); len(got) > 0 {
				t.Errorf("nginx annotations %v left on a traefik Ingress", got)
			}
		})
	}
}

// With a dedicated IP ingress-nginx is not on the node's ports: the dashboard
// host names the IP that serves it, as update-routes.sh does.
func TestHeadlampHook_DashboardOnTheNginxDedicatedIP(t *testing.T) {
	ing := dashboard(t, k4allv1alpha1.ClusterConfigSpec{Ingress: k4allv1alpha1.IngressConfig{
		Nginx: &k4allv1alpha1.NginxIngressConfig{DedicatedIP: "192.168.6.193", IsDefault: false},
	}}, releaseManifest("ingress-nginx"))

	if want := []string{"dashboard.192.168.6.193.nip.io"}; !reflect.DeepEqual(hosts(ing), want) {
		t.Errorf("hosts = %v, want %v", hosts(ing), want)
	}
}

// Traefik drops a router whose middleware does not exist: no redirect
// rather than no dashboard.
func TestHeadlampHook_NoRedirectWithoutTheMiddleware(t *testing.T) {
	ing := dashboard(t, traefikOn(""), releaseManifest("traefik", "headlamp"))

	if class(ing) != traefikClass {
		t.Errorf("ingressClassName = %q, want traefik", class(ing))
	}
	if v, ok := ing.Annotations[middlewareAnnotation]; ok {
		t.Errorf("%s = %q without the Middleware", middlewareAnnotation, v)
	}
}

// update-routes.sh on the host adds the FQDN rule to the same Ingress: the
// hook only manages its own nip.io rule, the class and its annotations.
func TestHeadlampHook_KeepsWhatItDoesNotOwn(t *testing.T) {
	nip := "dashboard." + dashboardNodeIP + ".nip.io"
	nginx := "nginx"
	existing := &networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{Namespace: "headlamp", Name: "headlamp", Annotations: map[string]string{
			"nginx.ingress.kubernetes.io/force-ssl-redirect": "true",
			"example.com/keep": "x",
		}},
		Spec: networkingv1.IngressSpec{
			IngressClassName: &nginx,
			Rules:            []networkingv1.IngressRule{{Host: nip}, {Host: dashboardFQDN}},
			TLS:              []networkingv1.IngressTLS{{Hosts: []string{dashboardFQDN}, SecretName: "dashboard-tls"}},
		},
	}

	ing := dashboard(t, traefikOn(""), releaseManifest("traefik", "headlamp"), redirectMiddleware(), existing)

	if want := []string{nip, dashboardFQDN}; !reflect.DeepEqual(hosts(ing), want) {
		t.Errorf("hosts = %v, want %v", hosts(ing), want)
	}
	if len(ing.Spec.TLS) != 1 || ing.Spec.TLS[0].SecretName != "dashboard-tls" {
		t.Errorf("tls = %+v, want the host's entry kept", ing.Spec.TLS)
	}
	if ing.Annotations["example.com/keep"] != "x" {
		t.Errorf("annotations = %v, want example.com/keep kept", ing.Annotations)
	}
	if class(ing) != traefikClass || len(nginxAnnotations(ing)) > 0 || ing.Annotations[middlewareAnnotation] == "" {
		t.Errorf("class %q, annotations %v: want traefik, the redirect and no nginx ones", class(ing), ing.Annotations)
	}
	if rule := ing.Spec.Rules[0]; rule.HTTP == nil || len(rule.HTTP.Paths) != 1 ||
		rule.HTTP.Paths[0].Backend.Service == nil || rule.HTTP.Paths[0].Backend.Service.Name != "headlamp" {
		t.Errorf("rule %s = %+v, want the headlamp backend", nip, rule)
	}
}

// When the ingress IP changes, the hook's rule moves with it, in place.
func TestHeadlampHook_MovesItsRuleWhenTheIPChanges(t *testing.T) {
	old := "dashboard." + dashboardNodeIP + ".nip.io"
	existing := &networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{Namespace: "headlamp", Name: "headlamp", Annotations: map[string]string{
			hooks.DashboardHostAnnotation: old,
		}},
		Spec: networkingv1.IngressSpec{Rules: []networkingv1.IngressRule{{Host: old}, {Host: dashboardFQDN}}},
	}

	ing := dashboard(t, traefikOn("192.168.6.193"), releaseManifest("traefik", "headlamp"), redirectMiddleware(), existing)

	if want := []string{"dashboard.192.168.6.193.nip.io", dashboardFQDN}; !reflect.DeepEqual(hosts(ing), want) {
		t.Errorf("hosts = %v, want %v", hosts(ing), want)
	}
	if v := ing.Annotations[hooks.DashboardHostAnnotation]; v != "dashboard.192.168.6.193.nip.io" {
		t.Errorf("%s = %q, want the new host", hooks.DashboardHostAnnotation, v)
	}
}
