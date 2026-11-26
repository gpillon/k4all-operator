package controller

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	k4allv1alpha1 "github.com/gpillon/k4all-operator/api/v1alpha1"
)

// rookOn is a ClusterConfig with features.rook enabled and its
// small-cluster policy ("" is Refuse).
func rookOn(smallCluster string) k4allv1alpha1.ClusterConfig {
	return k4allv1alpha1.ClusterConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "k4all-cluster-config", Generation: 4},
		Spec: k4allv1alpha1.ClusterConfigSpec{Features: k4allv1alpha1.FeaturesConfig{
			Rook: &k4allv1alpha1.RookConfig{Enabled: true, SmallCluster: smallCluster},
		}},
	}
}

// rookCephInstalled is a ReleaseManifest whose rook-ceph component is in
// state.
func rookCephInstalled(state k4allv1alpha1.ComponentState, message string) *k4allv1alpha1.ReleaseManifest {
	return &k4allv1alpha1.ReleaseManifest{
		ObjectMeta: metav1.ObjectMeta{Name: "k4all"},
		Status: k4allv1alpha1.ReleaseManifestStatus{Components: map[string]k4allv1alpha1.ComponentStatus{
			"rook-ceph": {State: state, Message: message},
		}},
	}
}

// cephNodeObjects are n untainted nodes.
func cephNodeObjects(n int) []client.Object {
	out := make([]client.Object, 0, n)
	for i := range n {
		out = append(out, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("node-%d", i)}})
	}
	return out
}

// cephCluster is rook-ceph/rook-ceph with the status Rook writes: phase,
// message, and Ceph's health with its checks (name to message).
func cephCluster(phase, message, health string, checks map[string]string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetAPIVersion("ceph.rook.io/v1")
	u.SetKind("CephCluster")
	u.SetNamespace("rook-ceph")
	u.SetName("rook-ceph")
	status := map[string]interface{}{}
	if phase != "" {
		status["phase"] = phase
	}
	if message != "" {
		status["message"] = message
	}
	if health != "" {
		details := map[string]interface{}{}
		for name, msg := range checks {
			details[name] = map[string]interface{}{"message": msg, "severity": health}
		}
		status["ceph"] = map[string]interface{}{"health": health, "details": details}
	}
	if len(status) > 0 {
		u.Object["status"] = status
	}
	return u
}

// seedRecord is the Rook hook's record of the CephCluster it created.
func seedRecord(how string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: "rook-ceph", Name: "k4all-rook-seeds"},
		Data:       map[string]string{"cephcluster.rook-ceph": how + " 2026-11-24T00:00:00Z"},
	}
}

// rookReady computes RookReady for config over a cluster with objects.
func rookReady(t *testing.T, config k4allv1alpha1.ClusterConfig, rm *k4allv1alpha1.ReleaseManifest,
	objects ...client.Object) (metav1.Condition, bool) {
	t.Helper()
	c := fake.NewClientBuilder().WithScheme(gateScheme(t)).WithObjects(objects...).Build()
	r := &ReleaseReconciler{Client: c, Log: logr.Discard()}
	return r.rookReadyCondition(context.Background(), &config, rm)
}

func expectRookReady(t *testing.T, cond metav1.Condition, set bool, status metav1.ConditionStatus,
	reason string, fragments ...string) {
	t.Helper()
	if !set {
		t.Fatalf("no RookReady condition, want %s (%s)", status, reason)
	}
	if cond.Type != k4allv1alpha1.ConditionRookReady {
		t.Errorf("type %q, want %q", cond.Type, k4allv1alpha1.ConditionRookReady)
	}
	if cond.Status != status || cond.Reason != reason {
		t.Errorf("RookReady = %s (%s), want %s (%s): %s", cond.Status, cond.Reason, status, reason, cond.Message)
	}
	if cond.ObservedGeneration != 4 {
		t.Errorf("observedGeneration %d, want the ClusterConfig's 4", cond.ObservedGeneration)
	}
	for _, f := range fragments {
		if !strings.Contains(cond.Message, f) {
			t.Errorf("message %q\nlacks %q", cond.Message, f)
		}
	}
}

// Without features.rook, or with it off, there is no RookReady: the
// condition reports only a Rook k4all runs.
func TestRookReady_NoneWithoutRook(t *testing.T) {
	for name, rook := range map[string]*k4allv1alpha1.RookConfig{
		"no rook field": nil,
		"enabled false": {Enabled: false},
	} {
		config := rookOn("")
		config.Spec.Features.Rook = rook
		if cond, set := rookReady(t, config, rookCephInstalled(k4allv1alpha1.ComponentStateInstalled, ""),
			cephCluster("Ready", "", "HEALTH_OK", nil)); set {
			t.Errorf("%s: RookReady %s (%s), want none", name, cond.Status, cond.Reason)
		}
	}
}

// D5: below 3 nodes with Refuse (the default) k4all creates no CephCluster;
// the condition says why, and how to have one anyway.
func TestRookReady_BelowThreeNodesWithRefuse(t *testing.T) {
	cond, set := rookReady(t, rookOn(""), rookCephInstalled(k4allv1alpha1.ComponentStateInstalled, ""),
		cephNodeObjects(2)...)

	expectRookReady(t, cond, set, metav1.ConditionFalse, "NotEnoughNodes",
		"this cluster has 2", "smallCluster: Allow")
}

// Rook on, its chart still installing: nothing to report of Ceph yet.
func TestRookReady_WhileRookInstalls(t *testing.T) {
	cond, set := rookReady(t, rookOn(""),
		rookCephInstalled(k4allv1alpha1.ComponentStateInstalling, "helm release pending-install"),
		cephNodeObjects(3)...)

	expectRookReady(t, cond, set, metav1.ConditionFalse, "RookInstalling",
		"rook-ceph is Installing: helm release pending-install")
}

// Rook installed, 3 nodes, no CephCluster yet: the hook creates it at the
// next reconcile.
func TestRookReady_CephClusterNotCreatedYet(t *testing.T) {
	cond, set := rookReady(t, rookOn(""), rookCephInstalled(k4allv1alpha1.ComponentStateInstalled, ""),
		cephNodeObjects(3)...)

	expectRookReady(t, cond, set, metav1.ConditionFalse, "CephClusterPending", "rook-ceph/rook-ceph")
}

// The admin deleted the CephCluster k4all created, or found (to turn Rook
// off): k4all does not create it again, and says so.
func TestRookReady_CephClusterDeleted(t *testing.T) {
	for _, how := range []string{"created", "found"} {
		cond, set := rookReady(t, rookOn(""), rookCephInstalled(k4allv1alpha1.ComponentStateInstalled, ""),
			append(cephNodeObjects(3), seedRecord(how))...)

		expectRookReady(t, cond, set, metav1.ConditionFalse, "CephClusterDeleted",
			"which k4all "+how+", is gone", "does not create it again")
	}
}

// The CephCluster as Rook reports it: its phase, then Ceph's health. A
// HEALTH_WARN cluster serves volumes (True); the checks are in the message.
func TestRookReady_SummarizesTheCephCluster(t *testing.T) {
	for _, tc := range []struct {
		name      string
		cluster   *unstructured.Unstructured
		status    metav1.ConditionStatus
		reason    string
		fragments []string
	}{
		{"no status yet", cephCluster("", "", "", nil),
			metav1.ConditionFalse, "CephClusterProgressing", []string{"rook-ceph/rook-ceph"}},
		{"progressing", cephCluster("Progressing", "Configuring Ceph Mons", "", nil),
			metav1.ConditionFalse, "CephClusterProgressing", []string{"Progressing", "Configuring Ceph Mons"}},
		{"failure", cephCluster("Failure", "failed to create cluster: no OSD", "", nil),
			metav1.ConditionFalse, "CephClusterFailure", []string{"failed to create cluster: no OSD"}},
		{"ready, ok", cephCluster("Ready", "Cluster created successfully", "HEALTH_OK", nil),
			metav1.ConditionTrue, "CephHealthOK", []string{"Ready", "HEALTH_OK"}},
		{"ready, warn", cephCluster("Ready", "", "HEALTH_WARN", map[string]string{
			"POOL_NO_REDUNDANCY": "1 pool(s) have no replicas configured",
			"MON_DISK_LOW":       "mon a is low on available space",
		}), metav1.ConditionTrue, "CephHealthWarn", []string{
			"HEALTH_WARN", "MON_DISK_LOW: mon a is low on available space; POOL_NO_REDUNDANCY: 1 pool(s) have no replicas configured"}},
		{"ready, error", cephCluster("Ready", "", "HEALTH_ERR", map[string]string{
			"OSD_FULL": "1 full osd(s)",
		}), metav1.ConditionFalse, "CephHealthError", []string{"HEALTH_ERR", "OSD_FULL: 1 full osd(s)"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cond, set := rookReady(t, rookOn(""), rookCephInstalled(k4allv1alpha1.ComponentStateInstalled, ""),
				append(cephNodeObjects(3), seedRecord("created"), tc.cluster)...)
			expectRookReady(t, cond, set, tc.status, tc.reason, tc.fragments...)
		})
	}
}

// With smallCluster Allow on one node the CephCluster exists and Ceph
// itself warns about the single copy: True, with Ceph's warning.
func TestRookReady_SingleNodeAllowed(t *testing.T) {
	cluster := cephCluster("Ready", "", "HEALTH_WARN", map[string]string{
		"POOL_NO_REDUNDANCY": "1 pool(s) have no replicas configured",
	})
	cond, set := rookReady(t, rookOn(k4allv1alpha1.RookSmallClusterAllow),
		rookCephInstalled(k4allv1alpha1.ComponentStateInstalled, ""),
		append(cephNodeObjects(1), seedRecord("created"), cluster)...)

	expectRookReady(t, cond, set, metav1.ConditionTrue, "CephHealthWarn", "POOL_NO_REDUNDANCY")
}

// The reconciler writes the ClusterConfig's status as one merge patch, its
// conditions as a whole list: RookReady is computed at each reconcile, so a
// reconcile in which another condition changes (the CNI moves to Cilium:
// PodNetworkOverlap appears) keeps it, transition time included.
func TestReconcile_RookReadySurvivesAnotherConditionsChange(t *testing.T) {
	config := rookOn("")
	config.Generation = 0
	config.Spec.Networking.CNI.Type = "calico"
	rm := &k4allv1alpha1.ReleaseManifest{
		ObjectMeta: metav1.ObjectMeta{Name: "k4all"},
		Spec: k4allv1alpha1.ReleaseManifestSpec{Components: map[string]k4allv1alpha1.ComponentSpec{
			"probe": {Type: "unsupported", Version: "1"},
		}},
	}
	objects := append(cephNodeObjects(3), &config, rm, seedRecord("created"),
		cephCluster("Ready", "", "HEALTH_OK", nil))
	s := gateScheme(t)
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(objects...).
		WithStatusSubresource(&k4allv1alpha1.ReleaseManifest{}, &k4allv1alpha1.ClusterConfig{}).
		Build()
	cl := &cluster{t: t, c: c, r: &ReleaseReconciler{Client: c, Scheme: s, Log: logr.Discard()}}

	cl.reconcile()
	before := cl.condition(k4allv1alpha1.ConditionRookReady)
	if before == nil || before.Status != metav1.ConditionTrue || before.Reason != "CephHealthOK" {
		t.Fatalf("after the first reconcile: RookReady %+v, want True (CephHealthOK)", before)
	}
	if cl.condition(k4allv1alpha1.ConditionPodNetworkOverlap) != nil {
		t.Fatal("PodNetworkOverlap with Calico")
	}

	cc := &k4allv1alpha1.ClusterConfig{}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(&config), cc); err != nil {
		t.Fatal(err)
	}
	cc.Spec.Networking.CNI.Type = ciliumCNI
	if err := c.Update(context.Background(), cc); err != nil {
		t.Fatal(err)
	}
	cl.reconcile()

	if cl.condition(k4allv1alpha1.ConditionPodNetworkOverlap) == nil {
		t.Fatal("no PodNetworkOverlap after the move to Cilium: the test changes no other condition")
	}
	after := cl.condition(k4allv1alpha1.ConditionRookReady)
	if after == nil {
		t.Fatal("RookReady is gone after a reconcile that changed another condition")
	}
	if after.Status != before.Status || after.Reason != before.Reason ||
		!after.LastTransitionTime.Equal(&before.LastTransitionTime) {
		t.Errorf("RookReady %s (%s) since %s, want it unchanged: %s (%s) since %s",
			after.Status, after.Reason, after.LastTransitionTime, before.Status, before.Reason, before.LastTransitionTime)
	}

	cc = &k4allv1alpha1.ClusterConfig{}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(&config), cc); err != nil {
		t.Fatal(err)
	}
	cc.Spec.Features.Rook.Enabled = false
	if err := c.Update(context.Background(), cc); err != nil {
		t.Fatal(err)
	}
	cl.reconcile()
	if cond := cl.condition(k4allv1alpha1.ConditionRookReady); cond != nil {
		t.Errorf("Rook off: RookReady %s (%s), want it removed", cond.Status, cond.Reason)
	}
}

// rookReadyCluster is a cluster with Rook on, 3 nodes and a release with no
// component (nothing else asks for a sooner reconcile), plus objects.
func rookReadyCluster(t *testing.T, objects ...client.Object) *cluster {
	t.Helper()
	config := rookOn("")
	config.Generation = 0
	config.Spec.Networking.CNI.Type = calicoCNI
	rm := &k4allv1alpha1.ReleaseManifest{ObjectMeta: metav1.ObjectMeta{Name: "k4all"}}
	s := gateScheme(t)
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(append(objects, &config, rm)...).
		WithStatusSubresource(&k4allv1alpha1.ReleaseManifest{}, &k4allv1alpha1.ClusterConfig{}).
		Build()
	return &cluster{t: t, c: c, r: &ReleaseReconciler{Client: c, Scheme: s, Log: logr.Discard()}}
}

// The v2.7.0 VM round (F3): RookReady said CephClusterProgressing for
// minutes after the CephCluster was Ready and HEALTH_OK, until an unrelated
// ClusterConfig edit: nothing reconciles when Rook updates the CephCluster,
// and no watch can (Rook's CRDs come with the rook-ceph component, if
// ever). So while Rook works towards a ready cluster the reconcile comes
// back within 30 s; once RookReady is True, at the periodic resync.
func TestReconcile_ComesBackSoonWhileRookIsNotReady(t *testing.T) {
	cluster := cephCluster("Progressing", "Configuring Ceph Mons", "", nil)
	cl := rookReadyCluster(t, append(cephNodeObjects(3), seedRecord("created"), cluster)...)

	res := cl.reconcile()
	if cond := cl.condition(k4allv1alpha1.ConditionRookReady); cond == nil || cond.Reason != "CephClusterProgressing" {
		t.Fatalf("RookReady %+v, want CephClusterProgressing", cond)
	}
	if res.RequeueAfter <= 0 || res.RequeueAfter > 30*time.Second {
		t.Errorf("RequeueAfter %s while the CephCluster progresses, want at most 30s", res.RequeueAfter)
	}

	ready := cephCluster("Ready", "Cluster created successfully", "HEALTH_OK", nil)
	live := cephCluster("", "", "", nil)
	if err := cl.c.Get(context.Background(), client.ObjectKeyFromObject(live), live); err != nil {
		t.Fatal(err)
	}
	live.Object["status"] = ready.Object["status"]
	if err := cl.c.Update(context.Background(), live); err != nil {
		t.Fatal(err)
	}
	res = cl.reconcile()
	if cond := cl.condition(k4allv1alpha1.ConditionRookReady); cond == nil || cond.Status != metav1.ConditionTrue {
		t.Fatalf("RookReady %+v, want True", cond)
	}
	if res.RequeueAfter != resyncInterval {
		t.Errorf("RequeueAfter %s with RookReady True, want the resync, %s", res.RequeueAfter, resyncInterval)
	}
}

// RookReady False because of what only a person or a new node changes (too
// few nodes with smallCluster Refuse, the CephCluster k4all made deleted):
// nothing to watch for, the periodic resync is enough.
func TestReconcile_RookWaitingOnAPersonIsNotPolled(t *testing.T) {
	for name, tc := range map[string]struct {
		objects []client.Object
		reason  string
	}{
		"one node": {cephNodeObjects(1), "NotEnoughNodes"},
		"deleted":  {append(cephNodeObjects(3), seedRecord("created")), "CephClusterDeleted"},
	} {
		t.Run(name, func(t *testing.T) {
			cl := rookReadyCluster(t, tc.objects...)
			res := cl.reconcile()
			if cond := cl.condition(k4allv1alpha1.ConditionRookReady); cond == nil || cond.Reason != tc.reason {
				t.Fatalf("RookReady %+v, want %s", cond, tc.reason)
			}
			if res.RequeueAfter != resyncInterval {
				t.Errorf("RequeueAfter %s, want the resync, %s", res.RequeueAfter, resyncInterval)
			}
		})
	}
}
