package hooks

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"

	k4allv1alpha1 "github.com/gpillon/k4all-operator/api/v1alpha1"
	"github.com/gpillon/k4all-operator/internal/engine"
)

// HangarNodeAgentHook removes Hangar's node agent (features.virt.hangar.
// nodeAgent false, or virt off) without the namespace it shares with
// Hangar: the ConfigMap hangar-passthrough there is Hangar's (the PCI
// devices given to VMs), and a RoleBinding of hangar-hardware-admin the
// admin's (R7). Deleting every object of the manifest, as for any
// component, would delete the namespace hangar-system and them with it.
type HangarNodeAgentHook struct {
	BaseHook
	client client.Client
	log    logr.Logger
	// restConfig reaches the API server directly, for the namespace check.
	restConfig *rest.Config
	// How long and how often the namespace check waits for what the removal
	// is still deleting; 0 means the defaults (tests shorten them).
	settle, poll time.Duration
}

func NewHangarNodeAgentHook(c client.Client, log logr.Logger) *HangarNodeAgentHook {
	return &HangarNodeAgentHook{client: c, log: log}
}

// The label and name of the agent's script ConfigMap, which kustomize names
// after its content (hangar-node-agent-scripts-<hash>).
const (
	hangarNodeAgentLabel   = "hangar-node-agent"
	hangarNodeAgentScripts = "hangar-node-agent-scripts"
)

// Remove deletes the objects of the agent's manifest (the release's file at
// the component's version) but its Namespace, and the script ConfigMaps of
// earlier versions; then the namespace, once nothing k4all did not create
// is left in it. Without the manifest it deletes nothing and fails: the
// component stays, and the next reconcile tries again.
func (h *HangarNodeAgentHook) Remove(ctx context.Context, spec k4allv1alpha1.ComponentSpec, _ k4allv1alpha1.ClusterConfigSpec) error {
	objects, err := engine.ManifestObjects(ctx, spec)
	if err != nil {
		return fmt.Errorf("read the node agent's manifest: %w", err)
	}
	var namespaces []string
	var errs []error
	for i := len(objects) - 1; i >= 0; i-- {
		obj := &objects[i]
		if obj.GetKind() == "Namespace" {
			namespaces = append(namespaces, obj.GetName())
			continue
		}
		err := h.client.Delete(ctx, obj, client.PropagationPolicy(metav1.DeletePropagationBackground))
		if err != nil && !apierrors.IsNotFound(err) {
			errs = append(errs, fmt.Errorf("delete %s %s/%s: %w", obj.GetKind(), obj.GetNamespace(), obj.GetName(), err))
		}
	}
	for _, ns := range namespaces {
		if err := h.deleteEarlierScripts(ctx, ns); err != nil {
			errs = append(errs, err)
		}
	}
	if err := errors.Join(errs...); err != nil {
		return err
	}
	for _, ns := range namespaces {
		deleteNamespaceIfEmpty(ctx, h.client, h.restConfig, h.log, ns, h.settle, h.poll)
	}
	return nil
}

// deleteEarlierScripts deletes the agent's script ConfigMaps of other
// versions in ns: those with its label and name.
func (h *HangarNodeAgentHook) deleteEarlierScripts(ctx context.Context, ns string) error {
	list := &corev1.ConfigMapList{}
	if err := h.client.List(ctx, list, client.InNamespace(ns),
		client.MatchingLabels{"app.kubernetes.io/name": hangarNodeAgentLabel}); err != nil {
		return fmt.Errorf("list the node agent's ConfigMaps in %s: %w", ns, err)
	}
	for i := range list.Items {
		cm := &list.Items[i]
		if !strings.HasPrefix(cm.Name, hangarNodeAgentScripts) {
			continue
		}
		if err := h.client.Delete(ctx, cm); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete ConfigMap %s/%s: %w", ns, cm.Name, err)
		}
	}
	return nil
}
