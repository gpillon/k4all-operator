package engine

import (
	"sort"

	"github.com/Masterminds/semver/v3"
	k4allv1alpha1 "github.com/gpillon/k4all-operator/api/v1alpha1"
)

// UpgradeStep returns the spec to apply now to move a component from
// installedVersion towards spec.Version, and whether it is an intermediate
// step of spec.UpgradePath: the lowest path version strictly between the two.
// Fresh installs, downgrades and versions that are not semver go straight to
// spec.Version.
func UpgradeStep(spec k4allv1alpha1.ComponentSpec, installedVersion string) (k4allv1alpha1.ComponentSpec, bool) {
	installed, err := semver.NewVersion(installedVersion)
	if err != nil {
		return spec, false
	}
	target, err := semver.NewVersion(spec.Version)
	if err != nil {
		return spec, false
	}

	type candidate struct {
		version *semver.Version
		step    k4allv1alpha1.UpgradeStep
	}
	var between []candidate
	for _, step := range spec.UpgradePath {
		v, err := semver.NewVersion(step.Version)
		if err != nil {
			continue
		}
		if v.GreaterThan(installed) && v.LessThan(target) {
			between = append(between, candidate{version: v, step: step})
		}
	}
	if len(between) == 0 {
		return spec, false
	}
	sort.Slice(between, func(i, j int) bool { return between[i].version.LessThan(between[j].version) })

	next := between[0].step
	stepSpec := spec
	stepSpec.Version = next.Version
	if next.HelmArgs != nil {
		stepSpec.HelmArgs = next.HelmArgs
	}
	if next.Values != nil {
		stepSpec.Values = next.Values
	}
	return stepSpec, true
}
