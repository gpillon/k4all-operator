package engine

import (
	"context"
	goerrors "errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/go-logr/logr"
	k4allv1alpha1 "github.com/gpillon/k4all-operator/api/v1alpha1"
	"helm.sh/helm/v3/pkg/action"
	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/chart/loader"
	"helm.sh/helm/v3/pkg/cli"
	"helm.sh/helm/v3/pkg/getter"
	"helm.sh/helm/v3/pkg/registry"
	"helm.sh/helm/v3/pkg/release"
	"helm.sh/helm/v3/pkg/repo"
	"helm.sh/helm/v3/pkg/storage/driver"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/client-go/rest"
)

// HelmInstaller manages Helm chart installations using the Helm Go SDK.
type HelmInstaller struct {
	restConfig *rest.Config
	log        logr.Logger
	cacheDir   string
	// configFor builds the Helm action configuration of a namespace.
	configFor func(namespace string) (*action.Configuration, error)
	// fieldManager is the field manager of this operator's Helm writes, and
	// revisionWriter tells which field manager wrote a revision of a release
	// (checkTakeover).
	fieldManager   string
	revisionWriter func(ctx context.Context, namespace string, rel *release.Release) (string, error)
}

func NewHelmInstaller(restConfig *rest.Config, log logr.Logger) *HelmInstaller {
	cacheDir := filepath.Join(os.TempDir(), "k4all-helm-cache")
	_ = os.MkdirAll(cacheDir, 0o755)
	h := &HelmInstaller{
		restConfig: restConfig,
		log:        log,
		cacheDir:   cacheDir,
	}
	h.configFor = h.actionConfig
	h.fieldManager = fieldManagerOf(restConfig)
	h.revisionWriter = secretCreator(restConfig)
	return h
}

// RunningVersion returns the chart version of the Helm release's last
// deployed revision: what runs, also after a failed upgrade. A release with
// no deployed revision (none, or a failed first install) is not installed.
// A deployed revision is ready: helm --wait waited for it.
func (h *HelmInstaller) RunningVersion(_ context.Context, name, namespace string) (string, bool, error) {
	cfg, err := h.configFor(namespace)
	if err != nil {
		return "", false, fmt.Errorf("helm action config: %w", err)
	}
	rel, err := cfg.Releases.Deployed(name)
	if goerrors.Is(err, driver.ErrReleaseNotFound) || goerrors.Is(err, driver.ErrNoDeployedReleases) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("helm release %s/%s: %w", namespace, name, err)
	}
	return rel.Chart.Metadata.Version, true, nil
}

// Reconcile installs or upgrades the release. With wait it returns only once
// the release's resources are ready (used for intermediate upgradePath steps).
// A release this operator did not write last is upgraded only when values
// drop none of its values, or allowValuesDrop allows it (checkTakeover);
// nothing is done to it otherwise, not even the recovery of a pending
// revision.
func (h *HelmInstaller) Reconcile(
	ctx context.Context, name string, spec k4allv1alpha1.ComponentSpec, values map[string]interface{}, wait bool,
	allowValuesDrop func(revision int) bool,
) error {
	cfg, err := h.actionConfig(spec.Namespace)
	if err != nil {
		return fmt.Errorf("helm action config: %w", err)
	}
	if err := h.checkTakeover(ctx, cfg, name, spec.Namespace, values, allowValuesDrop); err != nil {
		return err
	}
	if err := h.recoverPendingRelease(cfg, name, time.Now()); err != nil {
		return err
	}

	chartPath, err := h.pullChart(spec)
	if err != nil {
		return fmt.Errorf("pull chart: %w", err)
	}

	ch, err := loader.Load(chartPath)
	if err != nil {
		return fmt.Errorf("load chart %s: %w", chartPath, err)
	}

	return h.installOrUpgrade(ctx, cfg, name, spec, ch, values, wait)
}

// installOrUpgrade installs the release, or upgrades it when it exists.
func (h *HelmInstaller) installOrUpgrade(
	ctx context.Context, cfg *action.Configuration, name string, spec k4allv1alpha1.ComponentSpec,
	ch *chart.Chart, values map[string]interface{}, wait bool,
) error {
	installed, currentVersion, err := h.getRelease(cfg, name)
	if err != nil {
		return err
	}

	if !installed {
		h.log.Info("installing helm release", "name", name, "namespace", spec.Namespace, "version", spec.Version)
		install := action.NewInstall(cfg)
		install.ReleaseName = name
		install.Namespace = spec.Namespace
		install.CreateNamespace = true
		install.Wait = wait
		install.Timeout = 10 * time.Minute
		_, err = install.RunWithContext(ctx, ch, values)
		return err
	}

	if currentVersion == spec.Version {
		h.log.V(1).Info("helm release already at desired version", "name", name, "version", spec.Version)
	}

	h.log.Info("upgrading helm release", "name", name, "namespace", spec.Namespace,
		"from", currentVersion, "to", spec.Version)
	upgrade := action.NewUpgrade(cfg)
	upgrade.Namespace = spec.Namespace
	upgrade.Wait = wait
	upgrade.Timeout = 10 * time.Minute
	upgrade.MaxHistory = 3
	_, err = upgrade.RunWithContext(ctx, name, ch, values)
	return err
}

func (h *HelmInstaller) Uninstall(ctx context.Context, name, namespace string) error {
	cfg, err := h.actionConfig(namespace)
	if err != nil {
		return err
	}

	installed, _, err := h.getRelease(cfg, name)
	if err != nil {
		return err
	}
	if !installed {
		return nil
	}

	h.log.Info("uninstalling helm release", "name", name, "namespace", namespace)
	uninstall := action.NewUninstall(cfg)
	uninstall.Timeout = 5 * time.Minute
	_, err = uninstall.Run(name)
	return err
}

// ErrHelmReleaseBusy says a release's last revision is pending and too recent
// to be taken as left by a stopped operator: an operation may still hold it.
var ErrHelmReleaseBusy = goerrors.New("a helm operation on the release is in progress")

// pendingReleaseGrace is how old a pending revision must be before the
// operator takes it as left by an operator process that stopped in the middle
// of the operation. Only the leader runs Helm on these releases, one
// reconcile at a time and synchronously; the grace covers a previous leader
// still finishing its last call during a hand-over.
const pendingReleaseGrace = time.Minute

// recoverPendingRelease clears a last revision left pending-install,
// pending-upgrade or pending-rollback by an operator process that stopped in
// the middle of it (deleted by k4all's CRI-O bridge handover on a fresh
// install, OOM-killed, or rebooted with its node), on which Helm refuses every
// install or upgrade with "another operation is in progress". Nothing is
// uninstalled: an uninstall would delete what the operation created, CRDs
// and their objects included.
//   - No deployed revision (a first install): only the stuck release record is
//     deleted; the install that follows adopts the objects that already carry
//     this release's Helm ownership metadata.
//   - A deployed revision: the release is rolled back to it; the upgrade that
//     follows goes on from there.
//
// A pending revision younger than pendingReleaseGrace is left alone and
// reported with ErrHelmReleaseBusy.
func (h *HelmInstaller) recoverPendingRelease(cfg *action.Configuration, name string, now time.Time) error {
	last, err := cfg.Releases.Last(name)
	if goerrors.Is(err, driver.ErrReleaseNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("helm release %s: %w", name, err)
	}
	if last.Info == nil || !last.Info.Status.IsPending() {
		return nil
	}
	if age := now.Sub(last.Info.LastDeployed.Time); age < pendingReleaseGrace {
		return fmt.Errorf("helm release %s: revision %d is %s since %s: %w",
			name, last.Version, last.Info.Status, age.Round(time.Second), ErrHelmReleaseBusy)
	}

	deployed, err := cfg.Releases.Deployed(name)
	switch {
	case goerrors.Is(err, driver.ErrNoDeployedReleases) || goerrors.Is(err, driver.ErrReleaseNotFound):
		h.log.Info("helm release left pending by an operator that stopped during the operation: "+
			"deleting its stuck record, the install adopts what it created",
			"name", name, "revision", last.Version, "status", last.Info.Status)
		if _, err := cfg.Releases.Delete(name, last.Version); err != nil {
			return fmt.Errorf("delete the stuck revision %d of helm release %s: %w", last.Version, name, err)
		}
		return nil
	case err != nil:
		return fmt.Errorf("helm release %s: %w", name, err)
	}

	h.log.Info("helm release left pending by an operator that stopped during the operation: "+
		"rolling it back to its last deployed revision",
		"name", name, "revision", last.Version, "status", last.Info.Status, "deployed", deployed.Version)
	// Helm's own state for an operation that did not finish: the history
	// then shows what happened, and the rollback is the last revision.
	last.Info.Status = release.StatusFailed
	last.Info.Description = "left pending by an operator that stopped during it; marked failed by k4all-operator"
	if err := cfg.Releases.Update(last); err != nil {
		return fmt.Errorf("mark the stuck revision %d of helm release %s failed: %w", last.Version, name, err)
	}
	rollback := action.NewRollback(cfg)
	rollback.Version = deployed.Version
	rollback.Timeout = 10 * time.Minute
	rollback.MaxHistory = 3
	if err := rollback.Run(name); err != nil {
		return fmt.Errorf("roll back helm release %s to revision %d: %w", name, deployed.Version, err)
	}
	return nil
}

func (h *HelmInstaller) getRelease(cfg *action.Configuration, name string) (installed bool, version string, err error) {
	status := action.NewStatus(cfg)
	rel, err := status.Run(name)
	if err != nil {
		if err == driver.ErrReleaseNotFound {
			return false, "", nil
		}
		return false, "", err
	}
	return true, rel.Chart.Metadata.Version, nil
}

// pullChart downloads and untars the chart into the cache dir. Helm v3 pull
// actions do not accept a context, so the download cannot be cancelled.
func (h *HelmInstaller) pullChart(spec k4allv1alpha1.ComponentSpec) (string, error) {
	if spec.Type == k4allv1alpha1.ComponentTypeHelmOCI {
		return h.pullOCI(spec)
	}
	return h.pullFromRepo(spec)
}

func (h *HelmInstaller) pullFromRepo(spec k4allv1alpha1.ComponentSpec) (string, error) {
	repoEntry := &repo.Entry{
		Name: sanitizeRepoName(spec.Repo),
		URL:  spec.Repo,
	}

	settings := cli.New()
	settings.RepositoryCache = h.cacheDir
	settings.RepositoryConfig = filepath.Join(h.cacheDir, "repositories.yaml")

	chartRepo, err := repo.NewChartRepository(repoEntry, getter.All(settings))
	if err != nil {
		return "", fmt.Errorf("create chart repository: %w", err)
	}
	chartRepo.CachePath = h.cacheDir

	_, err = chartRepo.DownloadIndexFile()
	if err != nil {
		return "", fmt.Errorf("download repo index: %w", err)
	}

	pull := action.NewPullWithOpts(action.WithConfig(&action.Configuration{}))
	pull.RepoURL = spec.Repo
	pull.Version = spec.Version
	pull.DestDir = h.cacheDir
	pull.Settings = settings
	pull.Untar = true
	pull.UntarDir = h.cacheDir

	chartDir := filepath.Join(h.cacheDir, spec.Chart)
	_ = os.RemoveAll(chartDir)
	h.cleanTgzFiles(spec.Chart)

	_, err = pull.Run(spec.Chart)
	if err != nil {
		return "", fmt.Errorf("pull chart %s: %w", spec.Chart, err)
	}
	return chartDir, nil
}

func (h *HelmInstaller) pullOCI(spec k4allv1alpha1.ComponentSpec) (string, error) {
	ref := spec.Repo
	if !strings.HasPrefix(ref, "oci://") {
		ref = "oci://" + ref
	}

	registryClient, err := registry.NewClient(
		registry.ClientOptEnableCache(true),
	)
	if err != nil {
		return "", fmt.Errorf("create OCI registry client: %w", err)
	}

	pull := action.NewPullWithOpts(action.WithConfig(&action.Configuration{}))
	pull.Version = spec.Version
	pull.DestDir = h.cacheDir
	pull.Settings = cli.New()
	pull.SetRegistryClient(registryClient)
	pull.Untar = true
	pull.UntarDir = h.cacheDir

	chartName := filepath.Base(spec.Repo)
	chartDir := filepath.Join(h.cacheDir, chartName)
	_ = os.RemoveAll(chartDir)
	h.cleanTgzFiles(chartName)

	_, err = pull.Run(ref)
	if err != nil {
		return "", fmt.Errorf("pull OCI chart %s: %w", ref, err)
	}

	return chartDir, nil
}

func (h *HelmInstaller) actionConfig(namespace string) (*action.Configuration, error) {
	flags := genericclioptions.NewConfigFlags(false)
	flags.WrapConfigFn = func(_ *rest.Config) *rest.Config {
		return h.restConfig
	}
	flags.Namespace = &namespace

	cfg := new(action.Configuration)
	if err := cfg.Init(flags, namespace, "secrets", func(format string, v ...interface{}) {
		h.log.V(1).Info(fmt.Sprintf(format, v...))
	}); err != nil {
		return nil, err
	}
	return cfg, nil
}

func (h *HelmInstaller) cleanTgzFiles(chartName string) {
	pattern := filepath.Join(h.cacheDir, chartName+"-*.tgz")
	matches, _ := filepath.Glob(pattern)
	for _, m := range matches {
		_ = os.Remove(m)
	}
}

func sanitizeRepoName(url string) string {
	name := strings.TrimPrefix(url, "https://")
	name = strings.TrimPrefix(name, "http://")
	name = strings.NewReplacer("/", "-", ".", "-", ":", "-").Replace(name)
	return name
}
