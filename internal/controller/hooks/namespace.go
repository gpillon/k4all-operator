package hooks

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/metadata"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Defaults for how long and how often the cleanup of a removed component's
// namespace waits for what the removal is still deleting. Short: removals
// run before the other components, and a replacement comes after.
const (
	cleanupSettle = 30 * time.Second
	cleanupPoll   = 2 * time.Second
)

// deleteNamespaceIfEmpty deletes the namespace a removed component leaves,
// once nothing is left in it that namespaceLeftovers counts. A removal does
// not wait for what it deletes, and right after it the namespace still holds
// objects on their way out (in an e2e run, the ingress-nginx controller pod,
// listed without its owner): it checks again every poll, up to settle (0:
// the defaults). Best effort, only logged; without cfg it does nothing.
func deleteNamespaceIfEmpty(ctx context.Context, c client.Client, cfg *rest.Config, log logr.Logger,
	ns string, settle, poll time.Duration) {
	if cfg == nil {
		return
	}
	left, err := waitForLeftovers(ctx, cfg, ns, settle, poll)
	switch {
	case err != nil:
		log.Info("namespace kept: cannot tell whether it is empty", "namespace", ns, "error", err.Error())
	case len(left) > 0:
		log.Info("namespace kept: it holds objects the component did not leave", "namespace", ns, "objects", left)
	default:
		err := c.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
		if err != nil && !apierrors.IsNotFound(err) {
			log.Info("cannot delete the namespace", "namespace", ns, "error", err.Error())
		} else if err == nil {
			log.Info("deleted the namespace the component left", "namespace", ns)
		}
	}
}

// waitForLeftovers checks the namespace until nothing is left in it, or
// until the settle time, and returns the last check.
func waitForLeftovers(ctx context.Context, cfg *rest.Config, ns string, settle, poll time.Duration) ([]string, error) {
	if settle == 0 {
		settle = cleanupSettle
	}
	if poll == 0 {
		poll = cleanupPoll
	}
	deadline := time.Now().Add(settle)
	for {
		left, err := namespaceLeftovers(ctx, cfg, ns)
		if (err == nil && len(left) == 0) || time.Now().After(deadline) {
			return left, err
		}
		select {
		case <-ctx.Done():
			return left, ctx.Err()
		case <-time.After(poll):
		}
	}
}

// namespaceLeftovers lists, as "<resource>/<name>", what namespace still holds
// that nobody else would clean up. It does not count:
//   - events;
//   - what Kubernetes puts in every namespace (ConfigMap kube-root-ca.crt,
//     ServiceAccount default);
//   - objects being deleted, or owned by another object: the garbage
//     collector removes them with their owner, or their owner is counted.
//
// It reads the API server directly (discovery, metadata-only lists), so no
// informer is started for every resource type. A resource type it cannot
// discover or list makes it fail: then the namespace is not known empty.
func namespaceLeftovers(ctx context.Context, cfg *rest.Config, namespace string) ([]string, error) {
	disco, err := discovery.NewDiscoveryClientForConfig(cfg)
	if err != nil {
		return nil, err
	}
	meta, err := metadata.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	lists, err := disco.ServerPreferredNamespacedResources()
	if err != nil {
		return nil, fmt.Errorf("discover namespaced resources: %w", err)
	}
	var left []string
	for _, list := range lists {
		gv, err := schema.ParseGroupVersion(list.GroupVersion)
		if err != nil {
			return nil, err
		}
		for _, res := range list.APIResources {
			if strings.Contains(res.Name, "/") || !listable(res) || res.Name == "events" {
				continue
			}
			objs, err := meta.Resource(gv.WithResource(res.Name)).Namespace(namespace).List(ctx, metav1.ListOptions{})
			if err != nil {
				return nil, fmt.Errorf("list %s in %s: %w", res.Name, namespace, err)
			}
			for _, o := range objs.Items {
				if o.DeletionTimestamp != nil || len(o.OwnerReferences) > 0 || namespaceDefault(res.Name, o.Name) {
					continue
				}
				left = append(left, res.Name+"/"+o.Name)
			}
		}
	}
	return left, nil
}

func listable(res metav1.APIResource) bool {
	for _, v := range res.Verbs {
		if v == "list" {
			return true
		}
	}
	return false
}

// namespaceDefault is an object Kubernetes creates in every namespace.
func namespaceDefault(resource, name string) bool {
	return (resource == "configmaps" && name == "kube-root-ca.crt") ||
		(resource == "serviceaccounts" && name == "default")
}
