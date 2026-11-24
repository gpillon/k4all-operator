package hooks

import (
	"context"
	"errors"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	k4allv1alpha1 "github.com/gpillon/k4all-operator/api/v1alpha1"
)

// RookCluster is what the cluster has of the Ceph cluster the Rook hook
// creates for features.rook, for the ClusterConfig's RookReady condition.
type RookCluster struct {
	// Name is the CephCluster's namespace/name.
	Name string
	// CephCluster is that CephCluster; nil when the cluster has none, or
	// not Rook's CRDs yet.
	CephCluster *unstructured.Unstructured
	// Seeded is "created" or "found" once the hook created or found the
	// CephCluster (its seed record); empty before.
	Seeded string
	// Refusal is why the hook does not create the CephCluster with the
	// nodes the cluster has now (below 3 nodes with smallCluster Refuse);
	// empty when it would. Read only while there is no CephCluster, and the
	// hook has none on record.
	Refusal string
}

// ReadRookCluster reads the CephCluster k4all creates, the Rook hook's
// record of it and, while there is none, whether the hook would create it.
func ReadRookCluster(ctx context.Context, c client.Client, config k4allv1alpha1.ClusterConfigSpec) (RookCluster, error) {
	state := RookCluster{Name: rookNamespace + "/" + cephClusterName}
	cluster := &unstructured.Unstructured{}
	cluster.SetAPIVersion(cephAPIVersion)
	cluster.SetKind(cephClusterKind)
	err := c.Get(ctx, types.NamespacedName{Namespace: rookNamespace, Name: cephClusterName}, cluster)
	switch {
	case err == nil:
		state.CephCluster = cluster
	case !apierrors.IsNotFound(err) && !meta.IsNoMatchError(err):
		return state, fmt.Errorf("get CephCluster %s: %w", state.Name, err)
	}

	record := &corev1.ConfigMap{}
	err = c.Get(ctx, types.NamespacedName{Namespace: rookNamespace, Name: seedRecordName}, record)
	switch {
	case err == nil:
		state.Seeded, _, _ = strings.Cut(record.Data[seedKey(cephClusterKind, cephClusterName)], " ")
	case !apierrors.IsNotFound(err):
		return state, fmt.Errorf("get the seed record %s/%s: %w", rookNamespace, seedRecordName, err)
	}
	if state.CephCluster != nil || state.Seeded != "" {
		return state, nil
	}

	nodes := &corev1.NodeList{}
	if err := c.List(ctx, nodes); err != nil {
		return state, fmt.Errorf("list nodes: %w", err)
	}
	var refusal rookRefusal
	if _, err := planRook(cephNodes(nodes.Items), rookSettingsFrom(config)); errors.As(err, &refusal) {
		state.Refusal = refusal.Error()
	}
	return state, nil
}
