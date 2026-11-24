package controller

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	k4allv1alpha1 "github.com/gpillon/k4all-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// A pod that declares no controller (`kubectl run`, a `kubectl debug
// node/...` pod left running): a drain deletes it and nothing recreates it.
// Up to v0.5.7 the operator evicted it without a word. Now it does not cordon
// the node while it runs one, and names it, as `kubectl drain` without
// --force refuses it (k4all#99).
func TestClusterUpgrade_DoesNotCordonANodeThatRunsABarePod(t *testing.T) {
	b := newUpgradeTestbed(t,
		upgradeNode("cu-cp", "bootstrap", "v2.5.0"),
		upgradeNode("cu-w", "worker", "v2.5.0"))
	b.reconcile()
	debug := b.barePod("debug-x")
	app := b.pod("app")
	b.targetNextRelease()

	cu := b.reconcile()
	n := b.node("cu-cp")
	if n.Spec.Unschedulable || n.Annotations[k4allv1alpha1.NodeUpgradeCordonedAnnotation] != "" || n.Labels[drainingLabel] != "" {
		t.Fatalf("cu-cp unschedulable=%v cordoned=%q labels=%v, want it left alone",
			n.Spec.Unschedulable, n.Annotations[k4allv1alpha1.NodeUpgradeCordonedAnnotation], n.Labels)
	}
	if b.evicted(debug) || b.evicted(app) {
		t.Fatalf("evicted debug-x (%v) or app (%v) before any cordon", b.evicted(debug), b.evicted(app))
	}
	want := "cu-cp runs pods that declare no controller: default/debug-x (a drain would delete them"
	if !strings.Contains(cu.Status.Message, want) || strings.Contains(cu.Status.Message, "default/app") {
		t.Errorf("message = %q, want %q in it and app left out", cu.Status.Message, want)
	}
	if cu.Status.Phase != k4allv1alpha1.ClusterUpgradeProgressing {
		t.Errorf("phase = %s, want Progressing (waiting, not Failed)", cu.Status.Phase)
	}

	// The admin deletes the pod: the upgrade goes on.
	if err := b.c.Delete(context.Background(), debug, client.GracePeriodSeconds(0)); err != nil {
		t.Fatal(err)
	}
	b.reconcile()
	if n := b.node("cu-cp"); !n.Spec.Unschedulable {
		t.Errorf("cu-cp not cordoned once the bare pod is gone")
	}
}

// A node an older operator cordoned before it could look: its bare pod is not
// evicted either. The drain names it and waits for it, until its timeout.
func TestClusterUpgrade_DrainDoesNotEvictABarePod(t *testing.T) {
	b := newUpgradeTestbed(t,
		upgradeNode("cu-cp", "bootstrap", "v2.5.0", k4allv1alpha1.NodeUpgradeCordonedAnnotation, "true"),
		upgradeNode("cu-w", "worker", "v2.5.0"))
	debug := b.barePod("debug-x")
	app := b.pod("app")
	time.Sleep(1100 * time.Millisecond) // creation times have a one-second resolution
	cu := &k4allv1alpha1.ClusterUpgrade{ObjectMeta: metav1.ObjectMeta{Name: k4allv1alpha1.ClusterUpgradeName},
		Spec: k4allv1alpha1.ClusterUpgradeSpec{TargetRelease: nextRelease}}
	if err := b.c.Create(context.Background(), cu); err != nil {
		t.Fatal(err)
	}

	got := b.reconcile()
	if !b.evicted(app) {
		t.Errorf("app was not evicted")
	}
	if b.evicted(debug) {
		t.Errorf("debug-x was evicted: nothing recreates it")
	}
	if msg := got.Status.Nodes[0].Message; !strings.Contains(msg, "not evicted, they declare no controller: default/debug-x") {
		t.Errorf("cu-cp message = %q, want debug-x named", msg)
	}
}

func TestFindBarePods(t *testing.T) {
	self := types.NamespacedName{Namespace: "db", Name: "operator"}
	controller := metav1.OwnerReference{Kind: "ReplicaSet", Name: "rs", Controller: ptrTo(true)}
	pods := []corev1.Pod{
		testPod("bare-1", "n1", nil),
		testPod("bare-pending", "n1", nil, func(p *corev1.Pod) { p.Status.Phase = corev1.PodPending }),
		testPod("owner-not-controller", "n1", nil, func(p *corev1.Pod) {
			p.OwnerReferences = []metav1.OwnerReference{{Kind: "ConfigMap", Name: "cm"}}
		}),
		testPod("bare-2", "n2", nil),
		testPod("managed", "n1", nil, func(p *corev1.Pod) { p.OwnerReferences = []metav1.OwnerReference{controller} }),
		testPod("daemonset", "n1", nil, func(p *corev1.Pod) {
			p.OwnerReferences = []metav1.OwnerReference{{Kind: "DaemonSet", Name: "ds"}}
		}),
		testPod("mirror", "n1", nil, func(p *corev1.Pod) {
			p.Annotations = map[string]string{corev1.MirrorPodAnnotationKey: "x"}
		}),
		testPod("done", "n1", nil, func(p *corev1.Pod) { p.Status.Phase = corev1.PodSucceeded }),
		testPod("failed", "n1", nil, func(p *corev1.Pod) { p.Status.Phase = corev1.PodFailed }),
		testPod("terminating", "n1", nil, func(p *corev1.Pod) { p.DeletionTimestamp = &metav1.Time{Time: time.Now()} }),
		testPod("operator", "n1", nil),
		testPod("unscheduled", "", nil),
	}
	want := map[string][]string{
		"n1": {"db/bare-1", "db/bare-pending", "db/owner-not-controller"},
		"n2": {"db/bare-2"},
	}
	if got := findBarePods(pods, self); !reflect.DeepEqual(got, want) {
		t.Errorf("findBarePods = %v, want %v", got, want)
	}
}
