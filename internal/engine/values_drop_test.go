package engine

import (
	"context"
	goerrors "errors"
	"reflect"
	"strings"
	"testing"

	"helm.sh/helm/v3/pkg/release"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
)

func TestDroppedValues(t *testing.T) {
	for _, tc := range []struct {
		name             string
		running, applied map[string]interface{}
		want             []string
	}{
		{
			name:    "a nested value left out",
			running: map[string]interface{}{"a": 1, "b": map[string]interface{}{"c": true, "d": "x"}},
			applied: map[string]interface{}{"a": 1, "b": map[string]interface{}{"d": "x"}},
			want:    []string{"b.c"},
		},
		{
			name:    "a value changed",
			running: map[string]interface{}{"routingMode": "tunnel"},
			applied: map[string]interface{}{"routingMode": "native"},
			want:    []string{"routingMode"},
		},
		{
			name:    "values only k4all sets are not listed",
			running: map[string]interface{}{"a": 1},
			applied: map[string]interface{}{"a": 1, "b": map[string]interface{}{"c": true}},
		},
		{
			name:    "numbers compare as Helm stores them",
			running: map[string]interface{}{"port": float64(6443), "replicas": int64(1)},
			applied: map[string]interface{}{"port": 6443, "replicas": 1.0},
		},
		{
			name:    "a list is one value",
			running: map[string]interface{}{"devices": []interface{}{"ovs-bridge", "eth1"}},
			applied: map[string]interface{}{"devices": []string{"ovs-bridge"}},
			want:    []string{"devices"},
		},
		{
			name:    "the same list",
			running: map[string]interface{}{"devices": []interface{}{"ovs-bridge"}},
			applied: map[string]interface{}{"devices": []string{"ovs-bridge"}},
		},
		{
			name:    "a map replaced by a value",
			running: map[string]interface{}{"hubble": map[string]interface{}{"relay": map[string]interface{}{"enabled": true}}},
			applied: map[string]interface{}{"hubble": false},
			want:    []string{"hubble.relay.enabled"},
		},
		{
			name:    "an empty map is no value",
			running: map[string]interface{}{"podAnnotations": map[string]interface{}{}},
			applied: map[string]interface{}{},
		},
		{
			name:    "a null, which removes a chart default, is a value",
			running: map[string]interface{}{"resources": nil},
			applied: map[string]interface{}{},
			want:    []string{"resources"},
		},
		{
			name:    "a dot in a key is escaped",
			running: map[string]interface{}{"labels": map[string]interface{}{"app.kubernetes.io/name": "x"}},
			applied: map[string]interface{}{},
			want:    []string{`labels.app\.kubernetes\.io/name`},
		},
		{
			name:    "sorted",
			running: map[string]interface{}{"z": 1, "a": 1, "m": map[string]interface{}{"b": 1, "a": 1}},
			applied: map[string]interface{}{},
			want:    []string{"a", "m.a", "m.b", "z"},
		},
		{name: "a release without values", applied: map[string]interface{}{"a": 1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := DroppedValues(tc.running, tc.applied); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("DroppedValues = %v, want %v", got, tc.want)
			}
		})
	}
}

// operatorManager is the field manager the operator's Helm writes carry in
// these tests, helmCLI the helm command's.
const operatorManager, helmCLI = "manager", "Helm"

// takeoverOf is a HelmInstaller over a release whose revisions were written
// by the given field managers, the operator's being operatorManager.
func takeoverOf(t *testing.T, writers map[int]string, revisions ...*release.Release) *HelmInstaller {
	t.Helper()
	h := helmWith(t, revisions...)
	h.fieldManager = operatorManager
	h.revisionWriter = func(_ context.Context, _ string, rel *release.Release) (string, error) {
		return writers[rel.Version], nil
	}
	return h
}

func withValues(rel *release.Release, values map[string]interface{}) *release.Release {
	rel.Config = values
	return rel
}

func checkCertManager(t *testing.T, h *HelmInstaller, values map[string]interface{}, allowed func(int) bool) error {
	t.Helper()
	return h.checkTakeover(context.Background(), testConfig(t, h), "cert-manager", "cert-manager", values, allowed)
}

// A release installed by hand with values k4all does not set: held, with
// what would go.
func TestCheckTakeover_AReleaseWrittenByHandWithExtraValuesIsHeld(t *testing.T) {
	h := takeoverOf(t, map[int]string{1: operatorManager, 2: helmCLI},
		certManagerRelease(1, "v1.20.5", release.StatusSuperseded),
		withValues(certManagerRelease(2, "v1.20.5", release.StatusDeployed),
			map[string]interface{}{"crds": map[string]interface{}{"enabled": true}, "replicaCount": 2}))

	err := checkCertManager(t, h, map[string]interface{}{"crds": map[string]interface{}{"enabled": true}}, nil)

	var dropped *ValuesDropError
	if !goerrors.As(err, &dropped) {
		t.Fatalf("checkTakeover = %v, want a ValuesDropError", err)
	}
	if dropped.Revision != 2 || dropped.WrittenBy != helmCLI || !reflect.DeepEqual(dropped.Paths, []string{"replicaCount"}) {
		t.Errorf("got %+v, want revision 2, written by Helm, replicaCount", dropped)
	}
}

// The person accepts the drop of that revision: the apply goes on.
func TestCheckTakeover_AnAllowedRevisionIsTakenOver(t *testing.T) {
	h := takeoverOf(t, map[int]string{1: helmCLI},
		withValues(certManagerRelease(1, "v1.20.5", release.StatusDeployed), map[string]interface{}{"replicaCount": 2}))

	if err := checkCertManager(t, h, nil, func(rev int) bool { return rev == 1 }); err != nil {
		t.Errorf("checkTakeover = %v, want none: revision 1 may lose its values", err)
	}
	if err := checkCertManager(t, h, nil, func(rev int) bool { return rev == 3 }); err == nil {
		t.Errorf("checkTakeover passed, want it held: only revision 3 is allowed")
	}
}

// The values carried in componentOverrides: nothing would go.
func TestCheckTakeover_ValuesCarriedOverAreTakenOver(t *testing.T) {
	h := takeoverOf(t, map[int]string{1: helmCLI},
		withValues(certManagerRelease(1, "v1.20.5", release.StatusDeployed), map[string]interface{}{"replicaCount": 2}))

	if err := checkCertManager(t, h, map[string]interface{}{"replicaCount": 2, "crds": true}, nil); err != nil {
		t.Errorf("checkTakeover = %v, want none", err)
	}
}

// A release the operator wrote last is upgraded whatever its new values
// drop: a release that stops setting a value is a normal hop. This holds for
// a revision an older operator wrote: the same binary, the same manager.
func TestCheckTakeover_AReleaseTheOperatorWroteLastIsNeverHeld(t *testing.T) {
	for _, status := range []release.Status{release.StatusDeployed, release.StatusFailed} {
		h := takeoverOf(t, map[int]string{1: helmCLI, 2: operatorManager},
			withValues(certManagerRelease(1, "v1.20.5", release.StatusSuperseded), map[string]interface{}{"a": 1}),
			withValues(certManagerRelease(2, "v1.21.3", status), map[string]interface{}{"replicaCount": 2}))

		if err := checkCertManager(t, h, map[string]interface{}{}, nil); err != nil {
			t.Errorf("last revision %s by the operator: checkTakeover = %v, want none", status, err)
		}
	}
}

// Who wrote it cannot be told (the managedFields were cleared): held like a
// release written by hand, since a hold is undone with the annotation and a
// silent drop is not.
func TestCheckTakeover_AnUnknownWriterIsHeld(t *testing.T) {
	h := takeoverOf(t, map[int]string{},
		withValues(certManagerRelease(1, "v1.20.5", release.StatusDeployed), map[string]interface{}{"replicaCount": 2}))

	var dropped *ValuesDropError
	if err := checkCertManager(t, h, nil, nil); !goerrors.As(err, &dropped) {
		t.Fatalf("checkTakeover = %v, want a ValuesDropError", err)
	}
}

func TestCheckTakeover_NoReleaseIsNoTakeover(t *testing.T) {
	h := takeoverOf(t, nil)

	if err := checkCertManager(t, h, map[string]interface{}{"a": 1}, nil); err != nil {
		t.Errorf("checkTakeover = %v, want none: a fresh install", err)
	}
}

// The writer read fails: nothing is applied, rather than a guess.
func TestCheckTakeover_AnUnreadableWriterIsAnError(t *testing.T) {
	h := takeoverOf(t, nil, withValues(certManagerRelease(1, "v1.20.5", release.StatusDeployed), map[string]interface{}{"a": 1}))
	h.revisionWriter = func(context.Context, string, *release.Release) (string, error) {
		return "", goerrors.New("connection refused")
	}

	if err := checkCertManager(t, h, map[string]interface{}{"a": 1}, nil); err == nil {
		t.Errorf("checkTakeover passed, want an error")
	}
}

func TestCreator_IsTheManagerThatOwnsTheType(t *testing.T) {
	entries := []metav1.ManagedFieldsEntry{
		{Manager: "manager", Operation: metav1.ManagedFieldsOperationUpdate,
			FieldsV1: &metav1.FieldsV1{Raw: []byte(`{"f:data":{"f:release":{}},"f:metadata":{"f:labels":{"f:status":{}}}}`)}},
		{Manager: helmCLI, Operation: metav1.ManagedFieldsOperationUpdate,
			FieldsV1: &metav1.FieldsV1{Raw: []byte(`{"f:data":{".":{}},"f:metadata":{"f:labels":{"f:name":{}}},"f:type":{}}`)}},
	}
	if got := creator(entries); got != helmCLI {
		t.Errorf("creator = %q, want Helm: it created the Secret, the operator only marked it", got)
	}
	if got := creator(nil); got != "" {
		t.Errorf("creator without managedFields = %q, want none", got)
	}
}

func TestFieldManagerOf_IsTheUserAgentUpToItsFirstSlash(t *testing.T) {
	if got := fieldManagerOf(&rest.Config{UserAgent: "Helm/v3.17.3"}); got != helmCLI {
		t.Errorf("fieldManagerOf = %q, want Helm", got)
	}
	want, _, _ := strings.Cut(rest.DefaultKubernetesUserAgent(), "/")
	if got := fieldManagerOf(&rest.Config{}); got != want {
		t.Errorf("fieldManagerOf without a user agent = %q, want client-go's default %q", got, want)
	}
}
