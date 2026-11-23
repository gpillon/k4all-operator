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
func (h *HelmInstaller) Reconcile(ctx context.Context, name string, spec k4allv1alpha1.ComponentSpec, values map[string]interface{}, wait bool) error {
	cfg, err := h.actionConfig(spec.Namespace)
	if err != nil {
		return fmt.Errorf("helm action config: %w", err)
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
	if err := h.recoverPendingRelease(cfg, name); err != nil {
		return err
	}
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

// recoverPendingRelease marks failed a last revision still pending-install,
// pending-upgrade or pending-rollback, so that the install or upgrade can go
// on. Only the operator's leader installs and upgrades these releases, one
// reconcile at a time, and it calls Helm synchronously: a revision pending
// when a reconcile starts was left by an operator process that stopped in the
// middle of the operation (deleted by k4all's CRI-O bridge handover on a
// fresh install, OOM-killed, or rebooted with its node). Helm would refuse
// every later install or upgrade with "another operation is in progress".
// Marking it failed is what Helm itself does to an operation that fails: the
// upgrade then goes on from the last deployed revision, or from this one when
// none was ever deployed. Nothing is uninstalled.
func (h *HelmInstaller) recoverPendingRelease(cfg *action.Configuration, name string) error {
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
	h.log.Info("helm release left pending by an operator that stopped during the operation: marking it failed",
		"name", name, "revision", last.Version, "status", last.Info.Status)
	last.Info.Description = fmt.Sprintf("%s left by an operator that stopped during it; marked failed by k4all-operator",
		last.Info.Status)
	last.Info.Status = release.StatusFailed
	if err := cfg.Releases.Update(last); err != nil {
		return fmt.Errorf("mark the pending revision %d of helm release %s failed: %w", last.Version, name, err)
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
