package migrations

import (
	"context"
	"errors"
	"reflect"
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

	changed, err := apply(context.Background(), Env{Client: c}, []Migration{step("first", true), step("second", false), step("third", true)}, v26(), cc)
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

// The ClusterConfig counts the migrations that ran by their place in All
// (AppliedAnnotation): every cluster's count points into this list. Moving or
// removing an entry would make each count name another migration. Appending
// one adds its name here; any other change to this list is a bug.
func TestAll_KeepsItsNamesAndOrder(t *testing.T) {
	want := []string{
		"nginx-to-traefik",        // 1, k4all v2.4.0
		"ingress-nginx-namespace", // 2, operator v0.5.1
	}
	got := make([]string, 0, len(All))
	for _, m := range All {
		got = append(got, m.Name)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("migrations.All = %q, want %q: only ever append to it", got, want)
	}
}

// A count higher than the list: an older operator after a newer one ran
// migrations it does not know. Nothing runs, and the caller hears of it.
func TestApply_ACountAheadOfTheListIsAnError(t *testing.T) {
	ran := false
	list := []Migration{{
		Name:  "only",
		Due:   func(*k4allv1alpha1.ReleaseManifest) bool { return true },
		Apply: func(*k4allv1alpha1.ClusterConfigSpec) { ran = true },
	}}
	cc := clusterConfig(k4allv1alpha1.IngressConfig{}, map[string]string{AppliedAnnotation: "3"})
	c := newClient(t, cc)

	changed, err := apply(context.Background(), Env{Client: c}, list, v26(), cc)

	var ahead *AheadError
	if !errors.As(err, &ahead) || ahead.Applied != 3 || ahead.Known != 1 {
		t.Fatalf("err = %v, want an AheadError with 3 applied and 1 known", err)
	}
	if changed || ran || cc.Annotations[AppliedAnnotation] != "3" {
		t.Errorf("changed %t, ran %t, count %q: want nothing touched", changed, ran, cc.Annotations[AppliedAnnotation])
	}
}
