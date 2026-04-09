package engine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/go-logr/logr"
	k4allv1alpha1 "github.com/gpillon/k4all-operator/api/v1alpha1"
	"helm.sh/helm/v3/pkg/action"
	"helm.sh/helm/v3/pkg/chart/loader"
	"helm.sh/helm/v3/pkg/cli"
	"helm.sh/helm/v3/pkg/getter"
	"helm.sh/helm/v3/pkg/registry"
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
}

func NewHelmInstaller(restConfig *rest.Config, log logr.Logger) *HelmInstaller {
	cacheDir := filepath.Join(os.TempDir(), "k4all-helm-cache")
	_ = os.MkdirAll(cacheDir, 0o755)
	return &HelmInstaller{
		restConfig: restConfig,
		log:        log,
		cacheDir:   cacheDir,
	}
}

func (h *HelmInstaller) Reconcile(ctx context.Context, name string, spec k4allv1alpha1.ComponentSpec, values map[string]interface{}) error {
	cfg, err := h.actionConfig(spec.Namespace)
	if err != nil {
		return fmt.Errorf("helm action config: %w", err)
	}

	installed, currentVersion, err := h.getRelease(cfg, name)
	if err != nil {
		return err
	}

	chartPath, err := h.pullChart(ctx, spec)
	if err != nil {
		return fmt.Errorf("pull chart: %w", err)
	}

	chart, err := loader.Load(chartPath)
	if err != nil {
		return fmt.Errorf("load chart %s: %w", chartPath, err)
	}

	if !installed {
		h.log.Info("installing helm release", "name", name, "namespace", spec.Namespace, "version", spec.Version)
		install := action.NewInstall(cfg)
		install.ReleaseName = name
		install.Namespace = spec.Namespace
		install.CreateNamespace = true
		install.Wait = false
		install.Timeout = 10 * time.Minute
		_, err = install.RunWithContext(ctx, chart, values)
		return err
	}

	if currentVersion == spec.Version {
		h.log.V(1).Info("helm release already at desired version", "name", name, "version", spec.Version)
	}

	h.log.Info("upgrading helm release", "name", name, "namespace", spec.Namespace,
		"from", currentVersion, "to", spec.Version)
	upgrade := action.NewUpgrade(cfg)
	upgrade.Namespace = spec.Namespace
	upgrade.Wait = false
	upgrade.Timeout = 10 * time.Minute
	upgrade.MaxHistory = 3
	_, err = upgrade.RunWithContext(ctx, name, chart, values)
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

func (h *HelmInstaller) pullChart(ctx context.Context, spec k4allv1alpha1.ComponentSpec) (string, error) {
	if spec.Type == k4allv1alpha1.ComponentTypeHelmOCI {
		return h.pullOCI(ctx, spec)
	}
	return h.pullFromRepo(ctx, spec)
}

func (h *HelmInstaller) pullFromRepo(ctx context.Context, spec k4allv1alpha1.ComponentSpec) (string, error) {
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

func (h *HelmInstaller) pullOCI(ctx context.Context, spec k4allv1alpha1.ComponentSpec) (string, error) {
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
