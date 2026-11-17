package controller

import (
	"context"
	"testing"
	"time"

	"github.com/go-logr/logr"
	batchv1 "k8s.io/api/batch/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	k4allv1alpha1 "github.com/gpillon/k4all-operator/api/v1alpha1"
	"github.com/gpillon/k4all-operator/internal/controller/hooks"
)

// A static-pod component left alone because its manifest belongs to someone
// else stays so until its spec changes: no Job every reconcile.
func TestReconcile_LeavesAForeignStaticPodAlone(t *testing.T) {
	s := gateScheme(t)
	kubeVIP := k4allv1alpha1.ComponentSpec{Type: k4allv1alpha1.ComponentTypeStaticPod, Version: "v1.3.1",
		Image: "ghcr.io/kube-vip/kube-vip:v1.3.1", Namespace: "kube-system"}
	cc := &k4allv1alpha1.ClusterConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "k4all-cluster-config"},
		Spec: k4allv1alpha1.ClusterConfigSpec{
			Networking: k4allv1alpha1.NetworkingConfig{CNI: k4allv1alpha1.CNIConfig{Type: "calico"}},
			Cluster:    k4allv1alpha1.ControlPlaneConfig{HA: k4allv1alpha1.HAConfig{Type: "kubevip"}},
		},
	}
	r := &ReleaseReconciler{Scheme: s, Log: logr.Discard()}
	r.hookReg = hooks.NewRegistry(nil, logr.Discard())
	rm := &k4allv1alpha1.ReleaseManifest{
		ObjectMeta: metav1.ObjectMeta{Name: "k4all"},
		Spec:       k4allv1alpha1.ReleaseManifestSpec{Components: map[string]k4allv1alpha1.ComponentSpec{"kube-vip": kubeVIP}},
		Status: k4allv1alpha1.ReleaseManifestStatus{Components: map[string]k4allv1alpha1.ComponentStatus{"kube-vip": {
			State:       k4allv1alpha1.ComponentStateUnmanaged,
			Message:     "kube-vip: its static pod manifest was not written by the k4all-operator: left alone",
			AppliedHash: r.appliedHash("kube-vip", kubeVIP, cc.Spec),
		}}},
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(rm, cc).
		WithStatusSubresource(&k4allv1alpha1.ReleaseManifest{}, &k4allv1alpha1.ClusterConfig{}).Build()
	r.Client, r.hookReg = c, nil

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: "k4all"}}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	jobs := &batchv1.JobList{}
	if err := c.List(ctx, jobs); err != nil {
		t.Fatal(err)
	}
	if len(jobs.Items) > 0 {
		t.Errorf("Job %s started for a static pod left alone", jobs.Items[0].Name)
	}
	got := &k4allv1alpha1.ReleaseManifest{}
	if err := c.Get(ctx, types.NamespacedName{Name: "k4all"}, got); err != nil {
		t.Fatal(err)
	}
	if st := got.Status.Components["kube-vip"].State; st != k4allv1alpha1.ComponentStateUnmanaged {
		t.Errorf("kube-vip state = %s, want Unmanaged", st)
	}
}
