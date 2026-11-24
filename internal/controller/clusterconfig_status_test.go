package controller

import (
	"context"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	k4allv1alpha1 "github.com/gpillon/k4all-operator/api/v1alpha1"
)

// nodeSettingsStatus is what the node-settings controller writes on the
// ClusterConfig: status.nodes and its NodeSettingsRollout condition.
func nodeSettingsStatus(st *k4allv1alpha1.ClusterConfigStatus, phase string) {
	st.Nodes = &k4allv1alpha1.NodesStatus{
		SettingsHash: "hash-" + phase,
		Rollout:      k4allv1alpha1.NodeRolloutStatus{Phase: phase, CurrentNode: "w-1", Message: "rollout " + phase},
		PerNode:      []k4allv1alpha1.NodeSettingsStatus{{Name: "w-1", Phase: phase}},
	}
	meta.SetStatusCondition(&st.Conditions, metav1.Condition{
		Type: k4allv1alpha1.ConditionNodeSettingsRollout, Status: metav1.ConditionFalse,
		Reason: phase, Message: "rollout " + phase,
	})
}

// The release reconciler writes only what it owns of the ClusterConfig
// status: its fields and its condition types. What the node-settings
// controller wrote while a release reconcile ran (status.nodes, the
// NodeSettingsRollout condition) stays: the reconcile read the ClusterConfig
// before that write, and used to patch its whole copy of the status back,
// conditions list and all. Its own conditions still update, and go when they
// no longer apply.
func TestReconcile_TheClusterConfigStatusKeepsWhatOtherControllersWrite(t *testing.T) {
	c, r := onEnvtest(t, k4allv1alpha1.ComponentSpec{
		Type: k4allv1alpha1.ComponentTypeManifest, Version: "v1", InlineManifests: []string{probeConfigMap},
	}, k4allv1alpha1.ComponentStatus{})
	ctx := context.Background()
	cc := &k4allv1alpha1.ClusterConfig{}
	if err := c.Get(ctx, client.ObjectKey{Name: "k4all-cluster-config"}, cc); err != nil {
		t.Fatal(err)
	}
	// Before the reconcile: the node-settings controller's status, and two
	// conditions of the release reconciler that no longer apply (no
	// migration ahead, the CNI is not Cilium).
	nodeSettingsStatus(&cc.Status, k4allv1alpha1.NodeRolloutIdle)
	for _, stale := range []string{migrationsAheadCondition, k4allv1alpha1.ConditionPodNetworkOverlap} {
		meta.SetStatusCondition(&cc.Status.Conditions, metav1.Condition{
			Type: stale, Status: metav1.ConditionTrue, Reason: stale, Message: "stale",
		})
	}
	if err := c.Status().Update(ctx, cc); err != nil {
		t.Fatal(err)
	}
	// During the reconcile, once it has read the ClusterConfig, the
	// node-settings controller moves on.
	watching, err := client.NewWithWatch(envtestConfig(t), client.Options{Scheme: gateScheme(t)})
	if err != nil {
		t.Fatal(err)
	}
	moved := false
	r.Client = interceptor.NewClient(watching, interceptor.Funcs{
		List: func(ctx context.Context, cl client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if err := cl.List(ctx, list, opts...); err != nil {
				return err
			}
			if _, ok := list.(*k4allv1alpha1.ClusterConfigList); ok && !moved {
				moved = true
				latest := &k4allv1alpha1.ClusterConfig{}
				if err := c.Get(ctx, client.ObjectKey{Name: "k4all-cluster-config"}, latest); err != nil {
					t.Fatal(err)
				}
				nodeSettingsStatus(&latest.Status, k4allv1alpha1.NodeRolloutRolling)
				if err := c.Status().Update(ctx, latest); err != nil {
					t.Fatal(err)
				}
			}
			return nil
		},
	})

	reconcileProbe(t, r)

	if !moved {
		t.Fatal("the reconcile never listed the ClusterConfig")
	}
	got := &k4allv1alpha1.ClusterConfig{}
	if err := c.Get(ctx, client.ObjectKey{Name: "k4all-cluster-config"}, got); err != nil {
		t.Fatal(err)
	}
	if got.Status.Nodes == nil || got.Status.Nodes.Rollout.Phase != k4allv1alpha1.NodeRolloutRolling ||
		got.Status.Nodes.SettingsHash != "hash-Rolling" {
		t.Errorf("status.nodes %+v, want the node-settings controller's latest (Rolling)", got.Status.Nodes)
	}
	if rollout := meta.FindStatusCondition(got.Status.Conditions, k4allv1alpha1.ConditionNodeSettingsRollout); rollout == nil ||
		rollout.Reason != k4allv1alpha1.NodeRolloutRolling {
		t.Errorf("NodeSettingsRollout %+v, want the node-settings controller's latest (Rolling)", rollout)
	}
	reconciled := meta.FindStatusCondition(got.Status.Conditions, "Reconciled")
	if reconciled == nil || reconciled.Status != metav1.ConditionTrue || !strings.HasPrefix(reconciled.Message, "CNI=calico") {
		t.Errorf("Reconciled %+v, want True from this reconcile", reconciled)
	}
	for _, stale := range []string{migrationsAheadCondition, k4allv1alpha1.ConditionPodNetworkOverlap} {
		if cond := meta.FindStatusCondition(got.Status.Conditions, stale); cond != nil {
			t.Errorf("%s %+v kept, want it removed: it no longer applies", stale, cond)
		}
	}
	if got.Status.ActiveCNI != "calico" || got.Status.ObservedGeneration != got.Generation || got.Status.LastReconcileTime.IsZero() {
		t.Errorf("activeCNI %q, observedGeneration %d (generation %d), lastReconcileTime %v: want this reconcile's",
			got.Status.ActiveCNI, got.Status.ObservedGeneration, got.Generation, got.Status.LastReconcileTime)
	}
}

// What is copied from the reconcile's status onto the server's: its own
// fields and condition types (set, or removed when it has none), nothing of
// status.nodes or of a condition type it does not own, even from a stale copy.
func TestClusterConfigStatus_OnlyTheReleaseReconcilersOwnPartsAreCopied(t *testing.T) {
	var server k4allv1alpha1.ClusterConfigStatus
	nodeSettingsStatus(&server, k4allv1alpha1.NodeRolloutRolling)
	meta.SetStatusCondition(&server.Conditions, metav1.Condition{
		Type: "SomethingElse", Status: metav1.ConditionTrue, Reason: "Other", Message: "another controller's",
	})
	meta.SetStatusCondition(&server.Conditions, metav1.Condition{
		Type: k4allv1alpha1.ConditionRookReady, Status: metav1.ConditionFalse, Reason: "Waiting", Message: "old",
	})

	var own k4allv1alpha1.ClusterConfigStatus
	nodeSettingsStatus(&own, k4allv1alpha1.NodeRolloutIdle) // a stale copy: not the reconciler's to write
	own.ActiveCNI = ciliumCNI
	meta.SetStatusCondition(&own.Conditions, metav1.Condition{
		Type: k4allv1alpha1.ConditionPodNetworkOverlap, Status: metav1.ConditionFalse, Reason: "Clear", Message: "clear",
	})

	copyReleaseStatus(own, &server)

	if server.Nodes.Rollout.Phase != k4allv1alpha1.NodeRolloutRolling {
		t.Errorf("status.nodes %+v, want the server's", server.Nodes)
	}
	if rollout := meta.FindStatusCondition(server.Conditions, k4allv1alpha1.ConditionNodeSettingsRollout); rollout.Reason != k4allv1alpha1.NodeRolloutRolling {
		t.Errorf("NodeSettingsRollout %+v, want the server's", rollout)
	}
	if other := meta.FindStatusCondition(server.Conditions, "SomethingElse"); other == nil {
		t.Error("a condition type the release reconciler does not own was dropped")
	}
	if rook := meta.FindStatusCondition(server.Conditions, k4allv1alpha1.ConditionRookReady); rook != nil {
		t.Errorf("RookReady %+v kept, want it removed: the reconcile did not set it", rook)
	}
	if overlap := meta.FindStatusCondition(server.Conditions, k4allv1alpha1.ConditionPodNetworkOverlap); overlap == nil ||
		overlap.Reason != "Clear" {
		t.Errorf("PodNetworkOverlap %+v, want the reconcile's", overlap)
	}
	if server.ActiveCNI != ciliumCNI {
		t.Errorf("activeCNI %q, want the reconcile's", server.ActiveCNI)
	}
}
