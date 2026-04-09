package hooks

import (
	"context"
	"fmt"

	"github.com/go-logr/logr"
	k4allv1alpha1 "github.com/gpillon/k4all-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// TopolvmHook handles pre-install tasks for TopoLVM:
//   - Labels the topolvm-system and kube-system namespaces with topolvm.io/webhook=ignore
//
// Mirrors the logic from setup-topolvm.sh.
type TopolvmHook struct {
	BaseHook
	client client.Client
	log    logr.Logger
}

func NewTopolvmHook(c client.Client, log logr.Logger) *TopolvmHook {
	return &TopolvmHook{client: c, log: log}
}

func (h *TopolvmHook) PreInstall(ctx context.Context, _ string, _ k4allv1alpha1.ComponentSpec, _ k4allv1alpha1.ClusterConfigSpec, values map[string]interface{}) (map[string]interface{}, error) {
	ns := "topolvm-system"

	if err := h.ensureNamespace(ctx, ns); err != nil {
		return values, fmt.Errorf("ensure namespace %s: %w", ns, err)
	}

	if err := h.labelNamespace(ctx, ns); err != nil {
		return values, fmt.Errorf("label namespace %s: %w", ns, err)
	}

	if err := h.labelNamespace(ctx, "kube-system"); err != nil {
		h.log.V(1).Info("failed to label kube-system (non-fatal)", "error", err)
	}

	return values, nil
}

func (h *TopolvmHook) ensureNamespace(ctx context.Context, name string) error {
	ns := &corev1.Namespace{}
	if err := h.client.Get(ctx, types.NamespacedName{Name: name}, ns); err != nil {
		if !errors.IsNotFound(err) {
			return err
		}
		ns = &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name: name,
				Labels: map[string]string{
					"topolvm.io/webhook": "ignore",
				},
			},
		}
		return h.client.Create(ctx, ns)
	}
	return nil
}

func (h *TopolvmHook) labelNamespace(ctx context.Context, name string) error {
	ns := &corev1.Namespace{}
	if err := h.client.Get(ctx, types.NamespacedName{Name: name}, ns); err != nil {
		return err
	}

	if ns.Labels == nil {
		ns.Labels = make(map[string]string)
	}

	if ns.Labels["topolvm.io/webhook"] == "ignore" {
		return nil
	}

	ns.Labels["topolvm.io/webhook"] = "ignore"
	return h.client.Update(ctx, ns)
}
