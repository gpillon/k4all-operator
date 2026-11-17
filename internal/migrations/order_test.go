package migrations

import (
	"context"
	"testing"

	k4allv1alpha1 "github.com/gpillon/k4all-operator/api/v1alpha1"
)

// Migrations run in order: one not due yet holds back the ones after it.
func TestApply_StopsAtTheFirstMigrationNotDue(t *testing.T) {
	var ran []string
	step := func(name string, due bool) Migration {
		return Migration{
			Name: name,
			Due:  func(*k4allv1alpha1.ReleaseManifest) bool { return due },
			Apply: func(*k4allv1alpha1.ClusterConfigSpec) {
				ran = append(ran, name)
			},
		}
	}
	cc := clusterConfig(k4allv1alpha1.IngressConfig{}, nil)
	c := newClient(t, cc)

	changed, err := apply(context.Background(), c, []Migration{step("first", true), step("second", false), step("third", true)}, v26(), cc)
	if err != nil {
		t.Fatal(err)
	}

	if !changed || len(ran) != 1 || ran[0] != "first" {
		t.Errorf("ran %v (changed %t), want [first]", ran, changed)
	}
	if v := cc.Annotations[AppliedAnnotation]; v != "1" {
		t.Errorf("%s = %q, want \"1\"", AppliedAnnotation, v)
	}
}
