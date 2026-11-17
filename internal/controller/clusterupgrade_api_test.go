package controller

import (
	"context"
	"strings"
	"testing"

	k4allv1alpha1 "github.com/gpillon/k4all-operator/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// The UI and the operator both rely on a single ClusterUpgrade, named k4all.
func TestClusterUpgrade_OnlyOneNamedK4all(t *testing.T) {
	c := envtestClient(t)
	ctx := context.Background()

	other := &k4allv1alpha1.ClusterUpgrade{ObjectMeta: metav1.ObjectMeta{Name: "second"}}
	err := c.Create(ctx, other)
	if err == nil || !strings.Contains(err.Error(), "named k4all") {
		t.Fatalf("create ClusterUpgrade %q: err = %v, want the one-ClusterUpgrade rule", other.Name, err)
	}
}

// A ClusterUpgrade written without spec.drain still gets the drain timeout.
func TestClusterUpgrade_DrainTimeoutDefaults(t *testing.T) {
	c := envtestClient(t)
	ctx := context.Background()

	cu := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "k4all.magesgate.com/v1alpha1",
		"kind":       "ClusterUpgrade",
		"metadata":   map[string]interface{}{"name": k4allv1alpha1.ClusterUpgradeName},
		"spec":       map[string]interface{}{"targetRelease": "v2.7.0"},
	}}
	if err := c.Create(ctx, cu); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Delete(ctx, cu) })

	got := &k4allv1alpha1.ClusterUpgrade{}
	if err := c.Get(ctx, client.ObjectKeyFromObject(cu), got); err != nil {
		t.Fatal(err)
	}
	if got.Spec.Drain.TimeoutMinutes != 30 {
		t.Errorf("spec.drain.timeoutMinutes = %d, want 30", got.Spec.Drain.TimeoutMinutes)
	}
	if got.Spec.Drain.SettleMinutes == nil || *got.Spec.Drain.SettleMinutes != 10 {
		t.Errorf("spec.drain.settleMinutes = %v, want 10", got.Spec.Drain.SettleMinutes)
	}
}
