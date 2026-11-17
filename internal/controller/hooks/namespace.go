package hooks

import (
	"context"
	"fmt"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/metadata"
	"k8s.io/client-go/rest"
)

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
