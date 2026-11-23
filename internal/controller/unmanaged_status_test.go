package controller

import (
	"testing"
	"time"

	k4allv1alpha1 "github.com/gpillon/k4all-operator/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// An Unmanaged component keeps the time it became Unmanaged, so a reconcile
// that changes nothing leaves the status as it is (the API server then skips
// the write). Restored 1.x clusters have a dozen Unmanaged components, and
// every reconcile rewrote their status.
func TestReconcile_UnmanagedComponentKeepsItsTransitionTime(t *testing.T) {
	since := metav1.NewTime(time.Now().Add(-time.Hour).Truncate(time.Second))
	cl := newCluster(t,
		map[string]k4allv1alpha1.ComponentSpec{"probe": {
			Type: "unsupported", Version: "1", Management: k4allv1alpha1.ComponentManagementUnmanaged,
		}},
		map[string]k4allv1alpha1.ComponentStatus{"probe": {
			State: k4allv1alpha1.ComponentStateUnmanaged, Message: "lifecycle not managed by operator",
			LastTransitionTime: since,
		}},
	)

	cl.reconcile()
	st := cl.manifest().Status.Components["probe"]
	if st.State != k4allv1alpha1.ComponentStateUnmanaged || !st.LastTransitionTime.Equal(&since) {
		t.Errorf("%s since %s, want Unmanaged since %s", st.State, st.LastTransitionTime, since)
	}
}

// A component that turns Unmanaged gets the time it did.
func TestReconcile_ComponentTurningUnmanagedRecordsWhen(t *testing.T) {
	since := metav1.NewTime(time.Now().Add(-time.Hour).Truncate(time.Second))
	cl := newCluster(t,
		map[string]k4allv1alpha1.ComponentSpec{"probe": {
			Type: "unsupported", Version: "1", Management: k4allv1alpha1.ComponentManagementUnmanaged,
		}},
		map[string]k4allv1alpha1.ComponentStatus{"probe": {
			State: k4allv1alpha1.ComponentStateInstalled, InstalledVersion: "1", LastTransitionTime: since,
		}},
	)

	cl.reconcile()

	st := cl.manifest().Status.Components["probe"]
	if st.State != k4allv1alpha1.ComponentStateUnmanaged || !since.Before(&st.LastTransitionTime) {
		t.Errorf("%s since %s, want Unmanaged since now", st.State, st.LastTransitionTime)
	}
}
