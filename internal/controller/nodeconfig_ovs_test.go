package controller

import (
	"context"
	"testing"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	k4allv1alpha1 "github.com/gpillon/k4all-operator/api/v1alpha1"
)

// reconcileNodeConfig runs one NodeConfig reconcile of a node, with the
// ClusterConfig asking for the OVS bridge or not. nmstate is read on the
// envtest API server, which has no nmstate CRDs: as a cluster where nmstate
// is not installed yet.
func reconcileNodeConfig(t *testing.T, ovsBridge bool) ctrl.Result {
	t.Helper()
	s := gateScheme(t)
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "ovs-node"}}
	cc := &k4allv1alpha1.ClusterConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "k4all-cluster-config"},
		Spec: k4allv1alpha1.ClusterConfigSpec{
			Networking: k4allv1alpha1.NetworkingConfig{CNI: k4allv1alpha1.CNIConfig{Type: "calico"}},
			Features:   k4allv1alpha1.FeaturesConfig{OVSBridge: k4allv1alpha1.OVSBridgeConfig{Enabled: ovsBridge}},
		},
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(node, cc).
		WithStatusSubresource(&k4allv1alpha1.NodeConfig{}).Build()
	r := &NodeConfigReconciler{Client: c, Scheme: s, Log: logr.Discard(), APIReader: envtestClient(t)}

	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "ovs-node"}})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	return res
}

// The bridge waits for nmstate (its CRDs, then the node's NodeNetworkState):
// the reconcile comes back for it, instead of waiting for the next change of
// the Node, up to five minutes without one.
func TestNodeConfig_ComesBackForTheOVSBridge(t *testing.T) {
	if res := reconcileNodeConfig(t, true); res.RequeueAfter == 0 {
		t.Errorf("ovsBridge enabled, nmstate missing: no requeue")
	}
	if res := reconcileNodeConfig(t, false); res.RequeueAfter != 0 {
		t.Errorf("ovsBridge disabled: requeue after %s", res.RequeueAfter)
	}
}
