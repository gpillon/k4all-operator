package controller

import (
	"context"
	"testing"

	k4allv1alpha1 "github.com/gpillon/k4all-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// operatorPod creates the operator's own pod, bound to node.
func (b *upgradeTestbed) operatorPod(name, node string) *corev1.Pod {
	b.t.Helper()
	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: name},
		Spec:       corev1.PodSpec{NodeName: node, Containers: []corev1.Container{{Name: "manager", Image: "k4all-operator"}}},
	}
	if err := b.c.Create(context.Background(), p); err != nil {
		b.t.Fatal(err)
	}
	b.t.Cleanup(func() { _ = b.c.Delete(context.Background(), p, client.GracePeriodSeconds(0)) })
	return p
}

// k4all#97: the ClusterUpgrade drains the node that runs the operator. Once
// every other pod is gone the operator evicts its own pod, before asking for
// the hop, so the leader is not on the node when it reboots. The next
// leader, elsewhere, waits for the old pod to go and asks for the hop.
func TestClusterUpgrade_OperatorLeavesTheNodeItDrainsBeforeItsHop(t *testing.T) {
	b := newUpgradeTestbed(t,
		upgradeNode("cu-cp", "bootstrap", "v2.5.0"),
		upgradeNode("cu-w", "worker", "v2.5.0"))
	b.r.Lock = &NodeLock{}
	self := b.operatorPod("k4all-operator", "cu-cp")
	b.reconcile()
	b.targetNextRelease()

	b.reconcile() // cordon cu-cp
	b.reconcile() // nothing else on it: the operator evicts itself
	if !b.evicted(self) {
		t.Fatalf("the operator's pod was not evicted from the node it drained")
	}
	if got := b.node("cu-cp").Annotations[k4allv1alpha1.NodeUpgradeTargetAnnotation]; got != "" {
		t.Fatalf("upgrade-target = %q before the operator left the node", got)
	}

	// Until it is gone, the old leader starts nothing more.
	b.reconcile()
	if got := b.node("cu-cp").Annotations[k4allv1alpha1.NodeUpgradeTargetAnnotation]; got != "" {
		t.Fatalf("upgrade-target = %q from the leader that is leaving", got)
	}

	// The next leader runs on cu-w; the old pod is still terminating there.
	b.operatorPod("k4all-operator-next", "cu-w")
	b.r = &ClusterUpgradeReconciler{Client: b.c, Scheme: b.c.Scheme(), APIReader: b.c, Graphs: b.r.Graphs,
		Self: types.NamespacedName{Namespace: "default", Name: "k4all-operator-next"}, Lock: &NodeLock{}}
	b.reconcile()
	if got := b.node("cu-cp").Annotations[k4allv1alpha1.NodeUpgradeTargetAnnotation]; got != "" {
		t.Fatalf("upgrade-target = %q while the old operator pod is still on the node", got)
	}
	if err := b.c.Delete(context.Background(), self, client.GracePeriodSeconds(0)); err != nil {
		t.Fatal(err)
	}
	b.reconcile()
	if got := b.node("cu-cp").Annotations[k4allv1alpha1.NodeUpgradeTargetAnnotation]; got != nextRelease {
		t.Errorf("upgrade-target = %q once the old pod is gone, want %s", got, nextRelease)
	}
}
