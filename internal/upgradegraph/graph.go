// Package upgradegraph reads the k4all upgrade graph (upgrade-graph.json,
// published with each release) and plans the hops between two releases.
// The rules are those of k4all-upgrade on the nodes (fcos-k8s
// bootc/overlay/usr/local/bin/k4all-upgrade, plan_path): the operator plans
// the hops it asks the nodes for, so both must pick the same ones.
package upgradegraph

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/Masterminds/semver/v3"
)

// ErrNoPath means the graph has no way from the current release to the target.
var ErrNoPath = errors.New("no upgrade path")

// Release is one entry of the graph.
type Release struct {
	Version      string `json:"version"`
	Kind         string `json:"kind"` // minor | patch
	MinFrom      string `json:"minFrom"`
	Kubernetes   string `json:"kubernetes"`
	Crio         string `json:"crio"`
	Fedora       int    `json:"fedora"`
	SyncManifest bool   `json:"syncManifest,omitempty"`
	Blocked      bool   `json:"blocked,omitempty"`
	Digest       string `json:"digest,omitempty"`
	Notes        string `json:"notes,omitempty"`

	key, minFromKey *semver.Version
}

// Graph holds the releases, oldest first.
type Graph struct {
	Releases []Release
}

// versionRE is k4all-upgrade's _VERSION_RE: no build metadata, "v" optional.
var versionRE = regexp.MustCompile(`^v?\d+\.\d+\.\d+(-[0-9A-Za-z.-]+)?$`)

func parseVersion(text string) (*semver.Version, error) {
	text = strings.TrimSpace(text)
	if !versionRE.MatchString(text) {
		return nil, fmt.Errorf("not a release version: %q", text)
	}
	return semver.NewVersion(text)
}

func normalize(text string) string {
	text = strings.TrimSpace(text)
	if strings.HasPrefix(text, "v") {
		return text
	}
	return "v" + text
}

// Compare orders two release versions as semver: -1, 0 or 1.
func Compare(a, b string) (int, error) {
	ka, err := parseVersion(a)
	if err != nil {
		return 0, err
	}
	kb, err := parseVersion(b)
	if err != nil {
		return 0, err
	}
	return ka.Compare(kb), nil
}

// Parse reads upgrade-graph.json: {"releases": [...]} or a bare list.
func Parse(data []byte) (*Graph, error) {
	var doc struct {
		Releases []json.RawMessage `json:"releases"`
	}
	var entries []json.RawMessage
	if err := json.Unmarshal(data, &doc); err == nil {
		entries = doc.Releases
	} else if err := json.Unmarshal(data, &entries); err != nil {
		return nil, fmt.Errorf("parse upgrade graph: %w", err)
	}

	g := &Graph{}
	for _, raw := range entries {
		var r Release
		if err := json.Unmarshal(raw, &r); err != nil {
			return nil, fmt.Errorf("parse upgrade graph: %w", err)
		}
		if err := r.init(); err != nil {
			return nil, err
		}
		g.Releases = append(g.Releases, r)
	}
	sort.SliceStable(g.Releases, func(i, j int) bool {
		return g.Releases[i].key.LessThan(g.Releases[j].key)
	})
	return g, nil
}

func (r *Release) init() error {
	var missing []string
	for field, value := range map[string]string{
		"version": r.Version, "kind": r.Kind, "minFrom": r.MinFrom,
		"kubernetes": r.Kubernetes, "crio": r.Crio,
	} {
		if value == "" {
			missing = append(missing, field)
		}
	}
	if r.Fedora == 0 {
		missing = append(missing, "fedora")
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return fmt.Errorf("release %s is missing %s", r.Version, strings.Join(missing, ", "))
	}
	r.Version = normalize(r.Version)
	r.MinFrom = normalize(r.MinFrom)
	r.Kubernetes = strings.TrimPrefix(r.Kubernetes, "v")
	var err error
	if r.key, err = parseVersion(r.Version); err != nil {
		return err
	}
	if r.minFromKey, err = parseVersion(r.MinFrom); err != nil {
		return fmt.Errorf("release %s: minFrom: %w", r.Version, err)
	}
	return nil
}

func sameStream(a, b *semver.Version) bool {
	return a.Major() == b.Major() && a.Minor() == b.Minor()
}

func latest(releases []Release) Release {
	best := releases[0]
	for _, r := range releases[1:] {
		if r.key.GreaterThan(best.key) {
			best = r
		}
	}
	return best
}

// Plan returns the releases to visit, in order, from current to target ("" or
// "latest" for the newest one). It never skips a minor release (minFrom),
// finishes the current X.Y stream before crossing into the next one, and
// never picks a blocked release.
func (g *Graph) Plan(current, target string) ([]Release, error) {
	var usable, minors []Release
	for _, r := range g.Releases {
		if !r.Blocked {
			usable = append(usable, r)
		}
		if r.Kind == "minor" {
			minors = append(minors, r)
		}
	}
	if len(usable) == 0 {
		return nil, fmt.Errorf("%w: the upgrade graph has no usable release", ErrNoPath)
	}
	cur, err := parseVersion(current)
	if err != nil {
		return nil, err
	}
	goal, err := findGoal(usable, target)
	if err != nil {
		return nil, err
	}

	path := []Release{}
	curVersion := normalize(current)
	for cur.LessThan(goal.key) {
		next, err := nextHop(usable, minors, cur, curVersion, goal)
		if err != nil {
			return nil, err
		}
		path = append(path, next)
		cur, curVersion = next.key, next.Version
	}
	return path, nil
}

// findGoal returns the usable release target names, or the newest one.
func findGoal(usable []Release, target string) (Release, error) {
	if target == "" || target == "latest" {
		return latest(usable), nil
	}
	want, err := parseVersion(normalize(target))
	if err != nil {
		return Release{}, err
	}
	for _, r := range usable {
		if r.key.Equal(want) {
			return r, nil
		}
	}
	return Release{}, fmt.Errorf("%w: release %s is not in the upgrade graph or is blocked", ErrNoPath, normalize(target))
}

// nextHop picks the release a node at cur switches to next on the way to goal.
func nextHop(usable, minors []Release, cur *semver.Version, curVersion string, goal Release) (Release, error) {
	options := make([]Release, 0, len(usable))
	var inStream, crossing []Release
	for _, r := range usable {
		if !cur.LessThan(r.key) || r.key.GreaterThan(goal.key) || r.minFromKey.GreaterThan(cur) ||
			skipsMinor(minors, cur, r) {
			continue
		}
		options = append(options, r)
		if sameStream(r.key, cur) {
			inStream = append(inStream, r)
		} else {
			crossing = append(crossing, r)
		}
	}
	if len(options) == 0 {
		return Release{}, fmt.Errorf("%w: no release can be reached from %s on the way to %s", ErrNoPath, curVersion, goal.Version)
	}
	// Finish the stream: a newer usable release of it that no hop reaches
	// from here (its minFrom comes after a blocked patch, say) stops the
	// plan instead of being left behind.
	var newerInStream []Release
	for _, r := range usable {
		if sameStream(r.key, cur) && r.key.GreaterThan(cur) {
			newerInStream = append(newerInStream, r)
		}
	}
	if len(crossing) > 0 && len(newerInStream) > 0 && len(inStream) == 0 {
		tip := latest(newerInStream)
		return Release{}, fmt.Errorf("%w: cannot leave %s before reaching %s, the latest patch of its stream, and no hop reaches it from %s (minFrom %s)",
			ErrNoPath, curVersion, tip.Version, curVersion, tip.MinFrom)
	}
	if len(inStream) > 0 && len(crossing) > 0 {
		return latest(inStream), nil
	}
	return latest(options), nil
}

// skipsMinor tells whether a minor release lies strictly between cur and r.
func skipsMinor(minors []Release, cur *semver.Version, r Release) bool {
	for _, m := range minors {
		if cur.LessThan(m.key) && m.key.LessThan(r.key) {
			return true
		}
	}
	return false
}

// Newer returns the releases after current that are not blocked, oldest first.
func (g *Graph) Newer(current string) []Release {
	cur, err := parseVersion(current)
	if err != nil {
		return nil
	}
	var out []Release
	for _, r := range g.Releases {
		if !r.Blocked && r.key.GreaterThan(cur) {
			out = append(out, r)
		}
	}
	return out
}

// Find returns the release named version, blocked or not.
func (g *Graph) Find(version string) (Release, bool) {
	want, err := parseVersion(normalize(version))
	if err != nil {
		return Release{}, false
	}
	for _, r := range g.Releases {
		if r.key.Equal(want) {
			return r, true
		}
	}
	return Release{}, false
}
