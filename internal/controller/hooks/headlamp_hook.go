package hooks

import (
	"context"
	"fmt"
	"net"

	"github.com/go-logr/logr"
	k4allv1alpha1 "github.com/gpillon/k4all-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// HeadlampHook handles post-install configuration for the Headlamp dashboard:
//   - Creates admin ServiceAccount + ClusterRoleBinding
//   - Creates long-lived token Secret
//   - Creates an Ingress resource so the dashboard is reachable via the cluster IP
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
		ns = "headlamp"
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

	if err := h.ensureIngress(ctx, ns, spec); err != nil {
		return fmt.Errorf("headlamp ingress: %w", err)
	}

	h.log.Info("headlamp post-install complete", "namespace", ns)
	return nil
}

func (h *HeadlampHook) ensureAdminSA(ctx context.Context, namespace string) error {
	sa := &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "headlamp-admin",
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
			Name: "headlamp-admin",
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
				Name:      "headlamp-admin",
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
		if s.Kind == "ServiceAccount" && s.Name == "headlamp-admin" && s.Namespace == namespace {
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
				Name:      "headlamp",
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
				"kubernetes.io/service-account.name": "headlamp-admin",
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

func (h *HeadlampHook) ensureIngress(ctx context.Context, namespace string, _ k4allv1alpha1.ComponentSpec) error {
	clusterIP, err := h.detectClusterIP(ctx)
	if err != nil {
		h.log.Error(err, "cannot detect cluster IP for headlamp ingress, skipping")
		return nil
	}

	host := fmt.Sprintf("dashboard.%s.nip.io", clusterIP)
	ingressClassName := "nginx"
	pathType := networkingv1.PathTypePrefix

	desired := &networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "headlamp",
			Namespace: namespace,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by":  "k4all-operator",
				"k4all.magesgate.com/dashboard": "true",
			},
			Annotations: map[string]string{
				"nginx.ingress.kubernetes.io/backend-protocol": "HTTP",
			},
		},
		Spec: networkingv1.IngressSpec{
			IngressClassName: &ingressClassName,
			Rules: []networkingv1.IngressRule{
				{
					Host: host,
					IngressRuleValue: networkingv1.IngressRuleValue{
						HTTP: &networkingv1.HTTPIngressRuleValue{
							Paths: []networkingv1.HTTPIngressPath{
								{
									Path:     "/",
									PathType: &pathType,
									Backend: networkingv1.IngressBackend{
										Service: &networkingv1.IngressServiceBackend{
											Name: "headlamp",
											Port: networkingv1.ServiceBackendPort{Number: headlampDefaultPort},
										},
									},
								},
							},
						},
					},
				},
			},
		},
	}

	existing := &networkingv1.Ingress{}
	if err := h.client.Get(ctx, types.NamespacedName{Name: desired.Name, Namespace: desired.Namespace}, existing); err != nil {
		if errors.IsNotFound(err) {
			h.log.Info("creating headlamp ingress", "host", host)
			return h.client.Create(ctx, desired)
		}
		return err
	}

	existing.Spec = desired.Spec
	existing.Annotations = desired.Annotations
	return h.client.Update(ctx, existing)
}

func (h *HeadlampHook) Cleanup(ctx context.Context, _ string, _ k4allv1alpha1.ClusterConfigSpec) error {
	ns := "headlamp"
	h.log.Info("cleaning up headlamp resources", "namespace", ns)

	secret := &corev1.Secret{}
	secret.Name = "headlamp-admin-token"
	secret.Namespace = ns
	if err := h.client.Delete(ctx, secret); err != nil && !errors.IsNotFound(err) {
		h.log.V(1).Info("headlamp secret delete (best-effort)", "error", err)
	}

	sa := &corev1.ServiceAccount{}
	sa.Name = "headlamp-admin"
	sa.Namespace = ns
	if err := h.client.Delete(ctx, sa); err != nil && !errors.IsNotFound(err) {
		h.log.V(1).Info("headlamp SA delete (best-effort)", "error", err)
	}

	crb := &rbacv1.ClusterRoleBinding{}
	crb.Name = "headlamp-admin"
	if err := h.client.Delete(ctx, crb); err != nil && !errors.IsNotFound(err) {
		h.log.V(1).Info("headlamp CRB delete (best-effort)", "error", err)
	}

	podCrb := &rbacv1.ClusterRoleBinding{}
	podCrb.Name = "headlamp-pod-admin"
	if err := h.client.Delete(ctx, podCrb); err != nil && !errors.IsNotFound(err) {
		h.log.V(1).Info("headlamp pod CRB delete (best-effort)", "error", err)
	}

	ingress := &networkingv1.Ingress{}
	ingress.Name = "headlamp"
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
