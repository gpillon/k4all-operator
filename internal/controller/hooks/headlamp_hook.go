package hooks

import (
	"context"
	"fmt"
	"net"
	"strings"

	"github.com/go-logr/logr"
	k4allv1alpha1 "github.com/gpillon/k4all-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// headlampDefaultNamespace is used when the component spec does not set a namespace.
	headlampDefaultNamespace = "headlamp"
	// headlampReleaseName is the name of the Helm-managed ServiceAccount, Service and Ingress.
	headlampReleaseName = "headlamp"
	// headlampAdminName is the name of the operator-managed admin ServiceAccount and ClusterRoleBinding.
	headlampAdminName = "headlamp-admin"
)

// HeadlampHook handles post-install configuration for the Headlamp dashboard:
//   - Creates admin ServiceAccount + ClusterRoleBinding
//   - Creates long-lived token Secret
//   - Publishes the dashboard on dashboard.<ingress IP>.nip.io, through
//     Traefik from k4all v2.6.0 and NGINX before (ensureIngress)
type HeadlampHook struct {
	BaseHook
	client client.Client
	log    logr.Logger
}

func NewHeadlampHook(c client.Client, log logr.Logger) *HeadlampHook {
	return &HeadlampHook{client: c, log: log}
}

func (h *HeadlampHook) PostInstall(ctx context.Context, spec k4allv1alpha1.ComponentSpec, config k4allv1alpha1.ClusterConfigSpec) error {
	ns := spec.Namespace
	if ns == "" {
		ns = headlampDefaultNamespace
	}

	if err := h.ensureAdminSA(ctx, ns); err != nil {
		return fmt.Errorf("headlamp admin SA: %w", err)
	}

	if err := h.ensureClusterRoleBinding(ctx, ns); err != nil {
		return fmt.Errorf("headlamp cluster role binding: %w", err)
	}

	if err := h.ensurePodClusterRoleBinding(ctx, ns); err != nil {
		return fmt.Errorf("headlamp pod cluster role binding: %w", err)
	}

	if err := h.ensureTokenSecret(ctx, ns); err != nil {
		return fmt.Errorf("headlamp token secret: %w", err)
	}

	if err := h.ensureIngress(ctx, ns, config); err != nil {
		return fmt.Errorf("headlamp ingress: %w", err)
	}

	h.log.Info("headlamp post-install complete", "namespace", ns)
	return nil
}

func (h *HeadlampHook) ensureAdminSA(ctx context.Context, namespace string) error {
	sa := &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      headlampAdminName,
			Namespace: namespace,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "k4all-operator",
			},
		},
	}

	existing := &corev1.ServiceAccount{}
	if err := h.client.Get(ctx, types.NamespacedName{Name: sa.Name, Namespace: sa.Namespace}, existing); err != nil {
		if errors.IsNotFound(err) {
			return h.client.Create(ctx, sa)
		}
		return err
	}
	return nil
}

func (h *HeadlampHook) ensureClusterRoleBinding(ctx context.Context, namespace string) error {
	desired := &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name: headlampAdminName,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "k4all-operator",
			},
		},
		RoleRef: rbacv1.RoleRef{
			APIGroup: "rbac.authorization.k8s.io",
			Kind:     "ClusterRole",
			Name:     "cluster-admin",
		},
		Subjects: []rbacv1.Subject{
			{
				Kind:      "ServiceAccount",
				Name:      headlampAdminName,
				Namespace: namespace,
			},
		},
	}

	existing := &rbacv1.ClusterRoleBinding{}
	if err := h.client.Get(ctx, types.NamespacedName{Name: desired.Name}, existing); err != nil {
		if errors.IsNotFound(err) {
			return h.client.Create(ctx, desired)
		}
		return err
	}

	if existing.RoleRef.Name != "cluster-admin" {
		if err := h.client.Delete(ctx, existing); err != nil {
			return err
		}
		return h.client.Create(ctx, desired)
	}

	found := false
	for _, s := range existing.Subjects {
		if s.Kind == "ServiceAccount" && s.Name == headlampAdminName && s.Namespace == namespace {
			found = true
			break
		}
	}
	if !found {
		existing.Subjects = desired.Subjects
		return h.client.Update(ctx, existing)
	}
	return nil
}

// ensurePodClusterRoleBinding grants cluster-admin to the Helm-managed "headlamp"
// ServiceAccount used by the pod, so in-cluster mode has full visibility.
func (h *HeadlampHook) ensurePodClusterRoleBinding(ctx context.Context, namespace string) error {
	desired := &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name: "headlamp-pod-admin",
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "k4all-operator",
			},
		},
		RoleRef: rbacv1.RoleRef{
			APIGroup: "rbac.authorization.k8s.io",
			Kind:     "ClusterRole",
			Name:     "cluster-admin",
		},
		Subjects: []rbacv1.Subject{
			{
				Kind:      "ServiceAccount",
				Name:      headlampReleaseName,
				Namespace: namespace,
			},
		},
	}

	existing := &rbacv1.ClusterRoleBinding{}
	if err := h.client.Get(ctx, types.NamespacedName{Name: desired.Name}, existing); err != nil {
		if errors.IsNotFound(err) {
			return h.client.Create(ctx, desired)
		}
		return err
	}
	return nil
}

func (h *HeadlampHook) ensureTokenSecret(ctx context.Context, namespace string) error {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "headlamp-admin-token",
			Namespace: namespace,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "k4all-operator",
			},
			Annotations: map[string]string{
				"kubernetes.io/service-account.name": headlampAdminName,
			},
		},
		Type: corev1.SecretTypeServiceAccountToken,
	}

	existing := &corev1.Secret{}
	if err := h.client.Get(ctx, types.NamespacedName{Name: secret.Name, Namespace: secret.Namespace}, existing); err != nil {
		if errors.IsNotFound(err) {
			return h.client.Create(ctx, secret)
		}
		return err
	}
	return nil
}

const headlampDefaultPort = 4466

// DashboardHostAnnotation on the dashboard Ingress names the rule the hook
// manages; update-routes.sh on the host adds others to the same Ingress.
const DashboardHostAnnotation = "k4all.magesgate.com/dashboard-host"

// Settings of the dashboard Ingress on Traefik (k4all v2.6.0 and later).
const (
	traefikMiddlewaresAnnotation = "traefik.ingress.kubernetes.io/router.middlewares"
	// dashboardRedirect is the Middleware traefik/redirect-https that the
	// k4all Traefik values create: HTTP to HTTPS, permanent.
	dashboardRedirect     = "traefik-redirect-https@kubernetescrd"
	nginxAnnotationPrefix = "nginx.ingress.kubernetes.io/"
)

var middlewareGVK = schema.GroupVersionKind{Group: "traefik.io", Version: "v1alpha1", Kind: "Middleware"}

// ensureIngress publishes the dashboard on dashboard.<ingress IP>.nip.io:
// through Traefik once the ReleaseManifest has it, through NGINX before.
// It manages that rule, the class and its own annotations; other rules, the
// tls entries and other annotations on the Ingress are left as they are.
func (h *HeadlampHook) ensureIngress(ctx context.Context, namespace string, config k4allv1alpha1.ClusterConfigSpec) error {
	ip, err := h.detectClusterIP(ctx)
	if err != nil {
		h.log.Error(err, "cannot detect cluster IP for headlamp ingress, skipping")
		return nil
	}
	onTraefik, err := h.traefikActive(ctx)
	if err != nil {
		return err
	}
	redirect := false
	if onTraefik {
		if dedicated := traefikConfig(config.Ingress).DedicatedIP; dedicated != "" {
			ip = dedicated
		}
		if redirect, err = h.redirectMiddlewareExists(ctx); err != nil {
			return err
		}
	}
	host := fmt.Sprintf("dashboard.%s.nip.io", ip)

	ing := &networkingv1.Ingress{}
	err = h.client.Get(ctx, types.NamespacedName{Name: headlampReleaseName, Namespace: namespace}, ing)
	create := errors.IsNotFound(err)
	if err != nil && !create {
		return err
	}
	if create {
		ing = &networkingv1.Ingress{ObjectMeta: metav1.ObjectMeta{
			Name:      headlampReleaseName,
			Namespace: namespace,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by":  "k4all-operator",
				"k4all.magesgate.com/dashboard": "true",
			},
		}}
	}
	if ing.Annotations == nil {
		ing.Annotations = map[string]string{}
	}

	className := "nginx"
	if onTraefik {
		className = "traefik"
		for key := range ing.Annotations {
			if strings.HasPrefix(key, nginxAnnotationPrefix) {
				delete(ing.Annotations, key)
			}
		}
		if redirect {
			ing.Annotations[traefikMiddlewaresAnnotation] = dashboardRedirect
		} else if ing.Annotations[traefikMiddlewaresAnnotation] == dashboardRedirect {
			delete(ing.Annotations, traefikMiddlewaresAnnotation)
		}
	} else {
		ing.Annotations[nginxAnnotationPrefix+"backend-protocol"] = "HTTP"
	}
	ing.Spec.IngressClassName = &className
	ing.Spec.Rules = withDashboardRule(ing.Spec.Rules, ing.Annotations[DashboardHostAnnotation], host)
	ing.Annotations[DashboardHostAnnotation] = host

	if create {
		h.log.Info("creating headlamp ingress", "host", host, "class", className)
		return h.client.Create(ctx, ing)
	}
	return h.client.Update(ctx, ing)
}

// withDashboardRule puts the dashboard rule for host in place of the one the
// hook wrote before (owned), or of one already for host, or else after the
// other rules.
func withDashboardRule(rules []networkingv1.IngressRule, owned, host string) []networkingv1.IngressRule {
	pathType := networkingv1.PathTypePrefix
	rule := networkingv1.IngressRule{
		Host: host,
		IngressRuleValue: networkingv1.IngressRuleValue{HTTP: &networkingv1.HTTPIngressRuleValue{
			Paths: []networkingv1.HTTPIngressPath{{
				Path:     "/",
				PathType: &pathType,
				Backend: networkingv1.IngressBackend{Service: &networkingv1.IngressServiceBackend{
					Name: headlampReleaseName,
					Port: networkingv1.ServiceBackendPort{Number: headlampDefaultPort},
				}},
			}},
		}},
	}
	out := make([]networkingv1.IngressRule, 0, len(rules)+1)
	placed := false
	for _, r := range rules {
		if r.Host != host && (owned == "" || r.Host != owned) {
			out = append(out, r)
			continue
		}
		if !placed {
			out = append(out, rule)
			placed = true
		}
	}
	if !placed {
		out = append(out, rule)
	}
	return out
}

// traefikActive says whether the ReleaseManifest installs Traefik.
func (h *HeadlampHook) traefikActive(ctx context.Context) (bool, error) {
	manifests := &k4allv1alpha1.ReleaseManifestList{}
	if err := h.client.List(ctx, manifests); err != nil {
		return false, fmt.Errorf("list ReleaseManifests: %w", err)
	}
	for _, rm := range manifests.Items {
		if c, ok := rm.Spec.Components["traefik"]; ok && c.Management != k4allv1alpha1.ComponentManagementRemoved {
			return true, nil
		}
	}
	return false, nil
}

// redirectMiddlewareExists: Traefik drops a router whose middleware is
// missing, so the dashboard only uses the redirect when it is there.
func (h *HeadlampHook) redirectMiddlewareExists(ctx context.Context) (bool, error) {
	m := &unstructured.Unstructured{}
	m.SetGroupVersionKind(middlewareGVK)
	err := h.client.Get(ctx, types.NamespacedName{Namespace: "traefik", Name: "redirect-https"}, m)
	switch {
	case err == nil:
		return true, nil
	case errors.IsNotFound(err) || meta.IsNoMatchError(err):
		h.log.Info("no Middleware traefik/redirect-https: the dashboard has no HTTPS redirect")
		return false, nil
	}
	return false, fmt.Errorf("read Middleware traefik/redirect-https: %w", err)
}

func (h *HeadlampHook) Cleanup(ctx context.Context, _ string, _ k4allv1alpha1.ClusterConfigSpec) error {
	ns := headlampDefaultNamespace
	h.log.Info("cleaning up headlamp resources", "namespace", ns)

	secret := &corev1.Secret{}
	secret.Name = "headlamp-admin-token"
	secret.Namespace = ns
	if err := h.client.Delete(ctx, secret); err != nil && !errors.IsNotFound(err) {
		h.log.V(1).Info("headlamp secret delete (best-effort)", "error", err)
	}

	sa := &corev1.ServiceAccount{}
	sa.Name = headlampAdminName
	sa.Namespace = ns
	if err := h.client.Delete(ctx, sa); err != nil && !errors.IsNotFound(err) {
		h.log.V(1).Info("headlamp SA delete (best-effort)", "error", err)
	}

	crb := &rbacv1.ClusterRoleBinding{}
	crb.Name = headlampAdminName
	if err := h.client.Delete(ctx, crb); err != nil && !errors.IsNotFound(err) {
		h.log.V(1).Info("headlamp CRB delete (best-effort)", "error", err)
	}

	podCrb := &rbacv1.ClusterRoleBinding{}
	podCrb.Name = "headlamp-pod-admin"
	if err := h.client.Delete(ctx, podCrb); err != nil && !errors.IsNotFound(err) {
		h.log.V(1).Info("headlamp pod CRB delete (best-effort)", "error", err)
	}

	ingress := &networkingv1.Ingress{}
	ingress.Name = headlampReleaseName
	ingress.Namespace = ns
	if err := h.client.Delete(ctx, ingress); err != nil && !errors.IsNotFound(err) {
		h.log.V(1).Info("headlamp ingress delete (best-effort)", "error", err)
	}

	h.log.Info("headlamp cleanup complete")
	return nil
}

func (h *HeadlampHook) detectClusterIP(ctx context.Context) (string, error) {
	nodes := &corev1.NodeList{}
	if err := h.client.List(ctx, nodes); err != nil {
		return "", fmt.Errorf("list nodes: %w", err)
	}
	for _, node := range nodes.Items {
		for _, addr := range node.Status.Addresses {
			if addr.Type == corev1.NodeInternalIP {
				if ip := net.ParseIP(addr.Address); ip != nil {
					return addr.Address, nil
				}
			}
		}
	}
	return "", fmt.Errorf("no node with InternalIP found")
}
