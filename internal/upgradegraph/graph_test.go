package upgradegraph

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"
)

// The cases of plan_path in fcos-k8s bootc/tests/upgrade/test_graph.py: the
// operator plans the hops it asks k4all-upgrade for, so both must agree.

type entry = map[string]interface{}

func release(version, kind, minFrom, kubernetes, crio string, fedora int, extra ...interface{}) entry {
	e := entry{"version": version, "kind": kind, "minFrom": minFrom,
		"kubernetes": kubernetes, "crio": crio, "fedora": fedora}
	for i := 0; i+1 < len(extra); i += 2 {
		e[extra[i].(string)] = extra[i+1]
	}
	return e
}

// ladder is the release ladder agreed for the 2.x migration (conftest.py LADDER).
var ladder = []entry{
	release("v2.2.4", "patch", "v2.0.0-beta1", "1.35.6", "1.32", 43),
	release("v2.3.0", "minor", "v2.2.4", "1.35.10", "1.35", 43),
	release("v2.4.0", "minor", "v2.3.0", "1.35.10", "1.35", 43, "syncManifest", true),
	release("v2.5.0", "minor", "v2.4.0", "1.35.10", "1.35", 45),
	release("v2.6.0", "minor", "v2.5.0", "1.35.10", "1.35", 45, "syncManifest", true),
	release("v2.7.0", "minor", "v2.6.0", "1.36.6", "1.36", 45),
	release("v2.8.0", "minor", "v2.7.0", "1.36.6", "1.36", 45, "syncManifest", true),
	release("v2.9.0", "minor", "v2.8.0", "1.37.2", "1.37", 45),
}

// graph parses entries as upgrade-graph.json, which the CI publishes.
func graph(t *testing.T, entries []entry) *Graph {
	t.Helper()
	doc, err := json.Marshal(map[string]interface{}{"schema": 1, "image": "ghcr.io/gpillon/k4all-bootc", "releases": entries})
	if err != nil {
		t.Fatal(err)
	}
	g, err := Parse(doc)
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func plan(t *testing.T, g *Graph, current, target string) []string {
	t.Helper()
	path, err := g.Plan(current, target)
	if err != nil {
		t.Fatalf("Plan(%s, %q): %v", current, target, err)
	}
	versions := []string{}
	for _, r := range path {
		versions = append(versions, r.Version)
	}
	return versions
}

func expectPath(t *testing.T, got []string, want ...string) {
	t.Helper()
	if want == nil {
		want = []string{}
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("path = %v, want %v", got, want)
	}
}

func TestPlan_OldReleaseWalksEveryMinor(t *testing.T) {
	expectPath(t, plan(t, graph(t, ladder), "v2.2.4", ""),
		"v2.3.0", "v2.4.0", "v2.5.0", "v2.6.0", "v2.7.0", "v2.8.0", "v2.9.0")
}

func TestPlan_StopsAtTheRequestedTarget(t *testing.T) {
	expectPath(t, plan(t, graph(t, ladder), "v2.2.4", "v2.4.0"), "v2.3.0", "v2.4.0")
}

func TestPlan_IsEmptyWhenUpToDate(t *testing.T) {
	expectPath(t, plan(t, graph(t, ladder), "v2.9.0", ""))
}

func TestPlan_PatchesInsideAStreamAreSkipped(t *testing.T) {
	g := graph(t, []entry{
		release("v2.3.0", "minor", "v2.2.4", "1.35.10", "1.35", 43),
		release("v2.3.1", "patch", "v2.3.0", "1.35.11", "1.35", 43),
		release("v2.3.2", "patch", "v2.3.0", "1.35.12", "1.35", 43),
	})
	expectPath(t, plan(t, g, "v2.3.0", ""), "v2.3.2")
}

func TestPlan_StreamIsFinishedBeforeCrossingAMinor(t *testing.T) {
	g := graph(t, []entry{
		release("v2.6.0", "minor", "v2.5.0", "1.35.10", "1.35", 45),
		release("v2.6.1", "patch", "v2.6.0", "1.35.11", "1.35", 45),
		release("v2.7.0", "minor", "v2.6.0", "1.36.6", "1.36", 45),
	})
	expectPath(t, plan(t, g, "v2.6.0", ""), "v2.6.1", "v2.7.0")
}

func TestPlan_BlockedReleaseIsNeverPicked(t *testing.T) {
	g := graph(t, []entry{
		release("v2.3.0", "minor", "v2.2.4", "1.35.10", "1.35", 43),
		release("v2.3.1", "patch", "v2.3.0", "1.35.11", "1.35", 43, "blocked", true),
	})
	expectPath(t, plan(t, g, "v2.3.0", ""))
	if _, err := g.Plan("v2.3.0", "v2.3.1"); !errors.Is(err, ErrNoPath) {
		t.Errorf("Plan to blocked v2.3.1: err = %v, want ErrNoPath", err)
	}
}

func TestPlan_NoPathFromAReleaseOlderThanEveryMinFrom(t *testing.T) {
	if _, err := graph(t, ladder[1:]).Plan("v2.2.3", ""); !errors.Is(err, ErrNoPath) {
		t.Errorf("Plan from v2.2.3 without the bridge: err = %v, want ErrNoPath", err)
	}
}

func TestPlan_BridgeIsReachableFromAny2x(t *testing.T) {
	expectPath(t, plan(t, graph(t, ladder), "v2.0.0-beta1", "v2.2.4"), "v2.2.4")
}

func TestCompare_PutsPrereleasesFirst(t *testing.T) {
	ordered := []string{"v2.0.0-beta1", "v2.0.0-beta2", "v2.0.0", "v2.0.1-beta.0", "v2.2.3", "v2.2.10", "v2.10.0"}
	for i := 0; i+1 < len(ordered); i++ {
		if c, err := Compare(ordered[i], ordered[i+1]); err != nil || c >= 0 {
			t.Errorf("Compare(%s, %s) = %d, %v; want < 0", ordered[i], ordered[i+1], c, err)
		}
	}
}
