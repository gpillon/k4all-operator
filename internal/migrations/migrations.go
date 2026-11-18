// Package migrations moves the ClusterConfig forward when a release needs it.
// Migrations are numbered by their place in All, run in that order, once
// each, and only forward: there is no way back but a backup. The
// ReleaseReconciler runs them before it touches any component.
package migrations

import (
	"context"
	"fmt"
	"strconv"

	"github.com/go-logr/logr"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"

	k4allv1alpha1 "github.com/gpillon/k4all-operator/api/v1alpha1"
)

// AppliedAnnotation on the ClusterConfig counts the migrations of All that
// have run. It is written in the same update as the spec they change.
const AppliedAnnotation = "k4all.magesgate.com/migrations"

// Migration moves the cluster forward for a release: the ClusterConfig spec
// (Apply), objects of the cluster (Act), or both.
type Migration struct {
	Name string
	// Due says the ReleaseManifest has reached the release that needs it.
	Due func(rm *k4allv1alpha1.ReleaseManifest) bool
	// Apply changes the spec in place. Optional.
	Apply func(spec *k4allv1alpha1.ClusterConfigSpec)
	// Act changes the cluster, best effort: it logs what it cannot do, and
	// the migration counts as done anyway. Optional.
	Act func(ctx context.Context, env Env, rm *k4allv1alpha1.ReleaseManifest)
}

// Env is what Act works with.
type Env struct {
	Client client.Client
	// RestConfig reaches the API server directly (discovery, metadata
	// lists). Without it an Act that needs it does nothing.
	RestConfig *rest.Config
	Log        logr.Logger
}

// All the migrations, in order. Only ever append.
var All = []Migration{
	nginxToTraefik,        // 1, k4all v2.4.0
	ingressNginxNamespace, // 2, operator v0.5.1, for clusters past the v2.4.0 sync with v0.5.0
}

// Run applies to config, in order, the migrations of All it has not had yet,
// up to the first one rm is not due for, and saves config with them. It
// reports whether config changed.
func Run(ctx context.Context, c client.Client, rm *k4allv1alpha1.ReleaseManifest, config *k4allv1alpha1.ClusterConfig) (bool, error) {
	return RunWith(ctx, Env{Client: c, Log: logr.Discard()}, rm, config)
}

// RunWith is Run with what the migrations that act on the cluster need.
func RunWith(ctx context.Context, env Env, rm *k4allv1alpha1.ReleaseManifest, config *k4allv1alpha1.ClusterConfig) (bool, error) {
	return apply(ctx, env, All, rm, config)
}

func apply(ctx context.Context, env Env, list []Migration, rm *k4allv1alpha1.ReleaseManifest, config *k4allv1alpha1.ClusterConfig) (bool, error) {
	c := env.Client
	applied := 0
	if v, ok := config.Annotations[AppliedAnnotation]; ok {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return false, fmt.Errorf("ClusterConfig %s: %s is %q, not a count", config.Name, AppliedAnnotation, v)
		}
		applied = n
	}
	done := applied
	for done < len(list) && list[done].Due(rm) {
		if m := list[done]; m.Apply != nil {
			m.Apply(&config.Spec)
		}
		if m := list[done]; m.Act != nil {
			m.Act(ctx, env, rm)
		}
		done++
	}
	if done == applied {
		return false, nil
	}
	if config.Annotations == nil {
		config.Annotations = map[string]string{}
	}
	config.Annotations[AppliedAnnotation] = strconv.Itoa(done)
	if err := c.Update(ctx, config); err != nil {
		return false, fmt.Errorf("save ClusterConfig after migration %s: %w", list[done-1].Name, err)
	}
	return true, nil
}
