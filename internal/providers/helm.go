package providers

import (
	"bytes"
	"context"
	"crypto/sha256"
	stderrors "errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"io"
	"log/slog"

	"github.com/hjames9/kraze/internal/color"
	"github.com/hjames9/kraze/internal/config"
	"go.yaml.in/yaml/v3"
	"helm.sh/helm/v4/pkg/action"
	"helm.sh/helm/v4/pkg/chart/loader"
	helmloader "helm.sh/helm/v4/pkg/chart/v2/loader"
	"helm.sh/helm/v4/pkg/cli"
	"helm.sh/helm/v4/pkg/getter"
	"helm.sh/helm/v4/pkg/helmpath"
	"helm.sh/helm/v4/pkg/kube"
	"helm.sh/helm/v4/pkg/registry"
	ri "helm.sh/helm/v4/pkg/release"
	rcommon "helm.sh/helm/v4/pkg/release/common"
	repov1 "helm.sh/helm/v4/pkg/repo/v1"
	"helm.sh/helm/v4/pkg/storage/driver"
	apiextv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

// HelmProvider implements the Provider interface for Helm charts
type HelmProvider struct {
	opts       *ProviderOptions
	restConfig *rest.Config
	settings   *cli.EnvSettings

	// Where the provider reaches the cluster; nil builds them from
	// restConfig. Tests set them.
	actionConfigFor func(namespace string) (*action.Configuration, error)
	dynamicClient   func() (dynamic.Interface, error)
	waitClients     func() (*kubeClients, error)
}

// NewHelmProvider creates a new Helm provider
func NewHelmProvider(opts *ProviderOptions) (*HelmProvider, error) {
	settings := cli.New()

	// Get REST config from our kubeconfig
	restConfig, err := getRESTConfigFromKubeconfig(opts.KubeConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to get REST config: %w", err)
	}

	return &HelmProvider{
		opts:       opts,
		restConfig: restConfig,
		settings:   settings,
	}, nil
}

// actionConfig returns the Helm action configuration for a namespace
func (helm *HelmProvider) actionConfig(namespace string) (*action.Configuration, error) {
	if helm.actionConfigFor != nil {
		return helm.actionConfigFor(namespace)
	}
	return helm.getActionConfig(namespace)
}

// getActionConfig creates an action.Configuration for a specific namespace
// This ensures Helm stores release metadata in the correct namespace
func (helm *HelmProvider) getActionConfig(namespace string) (*action.Configuration, error) {
	var logHandler slog.Handler
	if helm.opts.Verbose {
		logHandler = slog.Default().Handler()
	} else {
		logHandler = slog.NewTextHandler(io.Discard, nil)
	}
	actionConfig := action.NewConfiguration(action.ConfigurationSetLogger(logHandler))

	// Create a REST client getter with our config
	restGetter := &restClientGetter{
		config:    helm.restConfig,
		namespace: namespace,
	}

	// Initialize action config with the target namespace
	// This ensures Helm stores release secrets in the same namespace as the chart
	if err := actionConfig.Init(restGetter, namespace, os.Getenv("HELM_DRIVER")); err != nil {
		return nil, fmt.Errorf("failed to initialize Helm action config: %w", err)
	}

	// Initialize registry client for OCI support
	registryClient, err := registry.NewClient(
		registry.ClientOptDebug(helm.opts.Verbose),
		registry.ClientOptCredentialsFile(helm.settings.RegistryConfig),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create registry client: %w", err)
	}
	actionConfig.RegistryClient = registryClient

	return actionConfig, nil
}

// Install installs or upgrades a Helm chart (idempotent)
func (helm *HelmProvider) Install(ctx context.Context, service *config.ServiceConfig) error {
	// Get action config for this service's namespace
	actionConfig, err := helm.actionConfig(service.GetNamespace())
	if err != nil {
		return err
	}

	// Check if release already exists
	histClient := action.NewHistory(actionConfig)
	histClient.Max = 1
	_, err = histClient.Run(service.Name)
	releaseExists := err == nil

	// Get chart path - download if remote
	chartPath, cleanupChart, err := helm.getChartPath(ctx, service)
	if err != nil {
		return fmt.Errorf("failed to get chart: %w", err)
	}
	defer cleanupChart()

	chart, err := loader.Load(chartPath)
	if err != nil {
		return fmt.Errorf("failed to load chart: %w", err)
	}

	// Load values
	values, err := helm.loadValues(service)
	if err != nil {
		return fmt.Errorf("failed to load values: %w", err)
	}

	var rel ri.Releaser

	if releaseExists {
		// Upgrade existing release
		upgradeClient := helm.newUpgradeClient(actionConfig, service)

		if !helm.opts.Quiet {
			fmt.Printf("Upgrading Helm chart '%s' in namespace '%s'...\n", service.Name, service.GetNamespace())
		}
		rel, err = upgradeClient.RunWithContext(ctx, service.Name, chart, values)
		if err != nil {
			return fmt.Errorf("failed to upgrade chart: %w", err)
		}
		if !helm.opts.Quiet {
			fmt.Printf("%s Chart '%s' upgraded successfully\n", color.Checkmark(), service.Name)
		}
	} else {
		// Install new release
		installClient := helm.newInstallClient(actionConfig, service)

		if !helm.opts.Quiet {
			fmt.Printf("Installing Helm chart '%s' in namespace '%s'...\n", service.Name, service.GetNamespace())
		}
		rel, err = installClient.RunWithContext(ctx, chart, values)
		if err != nil {
			return fmt.Errorf("failed to install chart: %w", err)
		}
		if !helm.opts.Quiet {
			fmt.Printf("%s Chart '%s' installed successfully\n", color.Checkmark(), service.Name)
		}
	}

	var manifest string
	if rel != nil {
		if acc, accErr := ri.NewAccessor(rel); accErr == nil {
			manifest = acc.Manifest()
		}
	}

	// Wait for resources to be ready using our shared wait logic
	if helm.opts.Wait && manifest != "" {
		clients, err := helm.kubeClients()
		if err != nil {
			return fmt.Errorf("failed waiting for resources: %w", err)
		}
		// Resources without a namespace are in the release's
		if err := waitForManifests(ctx, clients, manifest, service.GetNamespace(), helm.opts); err != nil {
			return fmt.Errorf("failed waiting for resources: %w", err)
		}
	}

	return nil
}

// newUpgradeClient configures a Helm upgrade of service's release.
//
// kraze makes the cluster match its config, so upgrades always apply
// server-side and force conflicts (`helm upgrade --server-side=true
// --force-conflicts`): kraze becomes the owner of every field its chart sets,
// and changes made outside it (kubectl edit, another controller) to those
// fields are overwritten.
//
// Helm names its field manager after the binary. Client-side upgrades by
// kraze (every release from before kraze used Helm v4) own fields as manager
// "kraze" with an Update operation, a different owner from kraze's
// server-side applies. When such a release switched to server-side apply
// through anything but kraze (the helm CLI, say), Helm didn't migrate them,
// and every later upgrade changing one failed with "conflicts with "kraze"".
// Forcing once isn't enough: it takes over only the fields that conflicted.
//
// Helm's default ("auto") would keep a client-side release client-side, and
// Helm rejects forcing conflicts there. Always applying server-side moves
// such a release over on its next upgrade; Helm migrates kraze's client-side
// ownership as it does.
func (helm *HelmProvider) newUpgradeClient(actionConfig *action.Configuration, service *config.ServiceConfig) *action.Upgrade {
	upgradeClient := action.NewUpgrade(actionConfig)
	upgradeClient.Namespace = service.GetNamespace()
	upgradeClient.WaitStrategy = kube.HookOnlyStrategy
	upgradeClient.WaitForJobs = false
	upgradeClient.ServerSideApply = "true"
	upgradeClient.ForceConflicts = true
	upgradeClient.PostRenderer = configChecksumPostRenderer{}
	upgradeClient.PostRenderStrategy = action.PostRenderStrategyNoHooks

	if helm.opts.Timeout != "" {
		timeout, err := time.ParseDuration(helm.opts.Timeout)
		if err == nil {
			upgradeClient.Timeout = timeout
		}
	}

	if service.Version != "" {
		upgradeClient.Version = service.Version
	}
	return upgradeClient
}

// newInstallClient configures a Helm install of service's release. An install
// can adopt objects that already exist, so it forces conflicts for the same
// reason upgrades do (see newUpgradeClient).
func (helm *HelmProvider) newInstallClient(actionConfig *action.Configuration, service *config.ServiceConfig) *action.Install {
	installClient := action.NewInstall(actionConfig)
	installClient.ReleaseName = service.Name
	installClient.Namespace = service.GetNamespace()
	installClient.CreateNamespace = service.ShouldCreateNamespace()
	installClient.WaitStrategy = kube.HookOnlyStrategy
	installClient.WaitForJobs = false
	installClient.ForceConflicts = true
	installClient.PostRenderer = configChecksumPostRenderer{}
	installClient.PostRenderStrategy = action.PostRenderStrategyNoHooks

	if helm.opts.Timeout != "" {
		timeout, err := time.ParseDuration(helm.opts.Timeout)
		if err == nil {
			installClient.Timeout = timeout
		}
	}

	if service.Version != "" {
		installClient.Version = service.Version
	}
	return installClient
}

// Uninstall removes a Helm release
func (helm *HelmProvider) Uninstall(ctx context.Context, service *config.ServiceConfig) error {
	// Get action config for this service's namespace
	actionConfig, err := helm.actionConfig(service.GetNamespace())
	if err != nil {
		return err
	}

	client := action.NewUninstall(actionConfig)

	if helm.opts.Timeout != "" {
		timeout, err := time.ParseDuration(helm.opts.Timeout)
		if err == nil {
			client.Timeout = timeout
		}
	}

	// Determine if we should keep CRDs based on precedence:
	// 1. CLI flag (--keep-crds) has highest priority
	// 2. Per-service config (keep_crds in YAML)
	// 3. Default is false (delete CRDs for clean slate)
	keepCRDs := false
	if helm.opts.KeepCRDs {
		// CLI flag overrides everything
		keepCRDs = true
	} else if service.KeepCRDs != nil {
		// Per-service config
		keepCRDs = *service.KeepCRDs
	}
	// else default is false (delete CRDs)

	// Set KeepHistory to false to not keep release history
	client.KeepHistory = false
	// DisableHooks is false by default (run hooks)
	client.DisableHooks = false
	client.WaitStrategy = kube.HookOnlyStrategy

	if !helm.opts.Quiet {
		fmt.Printf("Uninstalling Helm release '%s' from namespace '%s'...\n", service.Name, service.GetNamespace())
	}

	// Show CRD behavior if verbose or if CRDs will be deleted
	if !keepCRDs {
		if helm.opts.Verbose {
			fmt.Printf("[HELM] CRDs will be deleted (use --keep-crds to preserve)\n")
		}
	} else {
		if helm.opts.Verbose {
			fmt.Printf("[HELM] CRDs will be preserved\n")
		}
	}

	// Get the release info before uninstalling to find CRDs
	var releaseCRDs []string
	if !keepCRDs {
		// Get release to find associated CRDs
		statusClient := action.NewStatus(actionConfig)
		relRaw, err := statusClient.Run(service.Name)
		if err == nil && relRaw != nil {
			if acc, accErr := ri.NewAccessor(relRaw); accErr == nil {
				if m := acc.Manifest(); m != "" {
					// Parse manifest to find CRDs
					releaseCRDs = helm.extractCRDsFromManifest(m)
				}
			}
		}
	}

	// Uninstall the release
	_, err = client.Run(service.Name)
	if err != nil {
		return fmt.Errorf("failed to uninstall chart: %w", err)
	}

	if !helm.opts.Quiet {
		fmt.Printf("%s Release '%s' uninstalled successfully\n", color.Checkmark(), service.Name)
	}

	// Delete CRDs if requested
	if !keepCRDs && len(releaseCRDs) > 0 {
		if helm.opts.Verbose {
			fmt.Printf("[HELM] Deleting %d CRD(s)...\n", len(releaseCRDs))
		}
		if err := helm.deleteCRDs(ctx, releaseCRDs); err != nil {
			fmt.Printf("%s Warning: Failed to delete some CRDs: %v\n", color.Warning(), err)
		} else if helm.opts.Verbose {
			fmt.Printf("[HELM] CRDs deleted successfully\n")
		}
	}

	return nil
}

// IsReleaseNotFound reports whether err is Helm's error for a release that
// doesn't exist, as Uninstall returns for one already removed (e.g. by deleting
// its namespace). Helm returns it only when listing the release's records
// succeeded and found none; failing to list them (unauthorized, unreachable) is
// a different error.
func IsReleaseNotFound(err error) bool {
	return stderrors.Is(err, driver.ErrReleaseNotFound)
}

// Status returns the status of a Helm release
func (helm *HelmProvider) Status(ctx context.Context, service *config.ServiceConfig) (*ServiceStatus, error) {
	// Get action config for this service's namespace
	actionConfig, err := helm.actionConfig(service.GetNamespace())
	if err != nil {
		return nil, err
	}

	client := action.NewStatus(actionConfig)

	relRaw, err := client.Run(service.Name)
	if err != nil {
		return &ServiceStatus{
			Name:      service.Name,
			Installed: false,
			Ready:     false,
			Message:   fmt.Sprintf("Not installed: %v", err),
		}, nil
	}

	acc, err := ri.NewAccessor(relRaw)
	if err != nil {
		return nil, fmt.Errorf("failed to read release status: %w", err)
	}

	status := &ServiceStatus{
		Name:      service.Name,
		Installed: true,
		Ready:     acc.Status() == string(rcommon.StatusDeployed),
		Message:   acc.Status(),
	}

	// A release is deployed once its objects are applied, so its workloads
	// are checked too (pods that can't pull their image)
	if status.Ready {
		status.markUnready(helm.unreadyWorkloads(ctx, acc.Manifest(), service.GetNamespace()))
	}

	return status, nil
}

// unreadyWorkloads names the release's workloads that aren't ready
func (helm *HelmProvider) unreadyWorkloads(ctx context.Context, manifest, namespace string) ([]string, error) {
	clients, err := helm.kubeClients()
	if err != nil {
		return nil, err
	}
	objs, err := parseManifestsYAML(manifest)
	if err != nil {
		return nil, err
	}
	return unreadyWorkloads(ctx, clients.dynamic, clients.mapper, objs, namespace, true)
}

// kubeClients returns the clients for the release's objects
func (helm *HelmProvider) kubeClients() (*kubeClients, error) {
	if helm.waitClients != nil {
		return helm.waitClients()
	}
	return newKubeClients(helm.opts.KubeConfig)
}

// IsInstalled checks if a Helm release is installed
func (helm *HelmProvider) IsInstalled(ctx context.Context, service *config.ServiceConfig) (bool, error) {
	// Get action config for this service's namespace
	actionConfig, err := helm.actionConfig(service.GetNamespace())
	if err != nil {
		return false, err
	}

	client := action.NewList(actionConfig)
	client.Filter = service.Name

	releases, err := client.Run()
	if err != nil {
		return false, fmt.Errorf("failed to list releases: %w", err)
	}

	for _, relRaw := range releases {
		acc, accErr := ri.NewAccessor(relRaw)
		if accErr != nil {
			continue
		}
		if acc.Name() == service.Name {
			return true, nil
		}
	}

	return false, nil
}

// getChartPath returns the local path to a chart, downloading it if
// necessary. The caller must call cleanup once the chart is loaded: an HTTP
// repository's chart is pulled into a temp dir it removes.
func (helm *HelmProvider) getChartPath(ctx context.Context, service *config.ServiceConfig) (path string, cleanup func(), err error) {
	noCleanup := func() {}

	// Local chart - return the local path directly
	if service.IsLocalChart() {
		return service.Path, noCleanup, nil
	}

	// Remote chart - need to download it
	if service.IsRemoteChart() {
		// For OCI registries, use LocateChart (it handles OCI natively, into
		// Helm's cache)
		if config.IsOCIURL(service.Repo) {
			// Get properly initialized action config with registry client
			actionConfig, err := helm.actionConfig(service.GetNamespace())
			if err != nil {
				return "", noCleanup, fmt.Errorf("failed to get action config for OCI chart: %w", err)
			}

			client := action.NewInstall(actionConfig)
			if service.Version != "" {
				client.Version = service.Version
			}
			chartRef := fmt.Sprintf("%s/%s", service.Repo, service.Chart)
			path, err := client.LocateChart(chartRef, helm.settings)
			return path, noCleanup, err
		}

		// For HTTP/HTTPS repositories, we need to pull the chart using the SDK
		if config.IsHTTPURL(service.Repo) {
			return helm.pullHTTPChart(service)
		}

		// Fallback: try direct path reference
		return service.Path, noCleanup, nil
	}

	return "", noCleanup, fmt.Errorf("unable to determine chart path: service must have either 'path' (for local) or 'repo' and 'chart' (for remote)")
}

// pullHTTPChart downloads a chart from an HTTP/HTTPS repository into a temp
// dir, which cleanup removes. Every run used to leave one behind.
func (helm *HelmProvider) pullHTTPChart(service *config.ServiceConfig) (path string, cleanup func(), err error) {
	tmpDir, err := os.MkdirTemp("", "kraze-helm-")
	if err != nil {
		return "", func() {}, fmt.Errorf("failed to create temp directory: %w", err)
	}
	cleanup = func() { _ = os.RemoveAll(tmpDir) }

	path, err = helm.pullHTTPChartInto(service, tmpDir)
	if err != nil {
		cleanup()
		return "", func() {}, err
	}
	return path, cleanup, nil
}

// pullHTTPChartInto downloads a chart from an HTTP/HTTPS repository into tmpDir using Helm SDK
func (helm *HelmProvider) pullHTTPChartInto(service *config.ServiceConfig, tmpDir string) (string, error) {
	// Add the repository first so Pull can find it, refreshing its index when
	// the requested chart version is not already cached
	repoName, err := helm.addHTTPRepositoryForChart(service.Repo, service.Chart, service.Version)
	if err != nil {
		return "", fmt.Errorf("failed to add repository: %w", err)
	}

	// Build the chart reference using the repo name
	chartRef := fmt.Sprintf("%s/%s", repoName, service.Chart)

	// Create Pull action
	actionConfig := action.NewConfiguration()

	// Initialize registry client for OCI support (for consistency)
	registryClient, err := registry.NewClient(
		registry.ClientOptDebug(helm.opts.Verbose),
		registry.ClientOptCredentialsFile(helm.settings.RegistryConfig),
	)
	if err != nil {
		return "", fmt.Errorf("failed to create registry client: %w", err)
	}
	actionConfig.RegistryClient = registryClient

	pull := action.NewPull(action.WithConfig(actionConfig))
	pull.Settings = helm.settings
	pull.DestDir = tmpDir
	pull.Untar = true

	// Set version if specified
	if service.Version != "" {
		pull.Version = service.Version
	}

	// Pull the chart
	if helm.opts.Verbose {
		fmt.Printf("Pulling chart '%s' from repository '%s'...\n", service.Chart, repoName)
	}

	_, err = pull.Run(chartRef)
	if err != nil {
		return "", fmt.Errorf("failed to pull chart: %w", err)
	}

	// Find the extracted chart directory
	// When Untar=true, helm extracts to destdir/chartname/
	entries, err := os.ReadDir(tmpDir)
	if err != nil {
		return "", fmt.Errorf("failed to read temp directory: %w", err)
	}

	if len(entries) == 0 {
		return "", fmt.Errorf("no chart found in temp directory")
	}

	// Look for a directory (untarred chart) or .tgz file
	var chartPath string
	for _, entry := range entries {
		if entry.IsDir() {
			// Found the chart directory
			chartPath = filepath.Join(tmpDir, entry.Name())
			break
		} else if strings.HasSuffix(entry.Name(), ".tgz") {
			// Found a .tgz file
			chartPath = filepath.Join(tmpDir, entry.Name())
			break
		}
	}

	if chartPath == "" {
		return "", fmt.Errorf("could not find chart in temp directory: %s", tmpDir)
	}

	if helm.opts.Verbose {
		fmt.Printf("Chart downloaded to: %s\n", chartPath)
	}

	return chartPath, nil
}

// addHTTPRepositoryForChart adds an HTTP(S) Helm repository and returns its
// name, refreshing the cached repository index when it cannot already satisfy
// chartName at version.
//
// Helm's downloader resolves 'reponame/chart' purely from the index cached on
// disk, so a repository added on an earlier run keeps resolving against a
// stale snapshot: bumping a chart version in kraze.yml then fails with
// "not found in <repo> index. (try 'helm repo update')". Refreshing here keeps
// `kraze up` self-sufficient. An exact version already present in the cache
// needs no refresh, which avoids re-downloading large indexes on every run.
func (helm *HelmProvider) addHTTPRepositoryForChart(repoURL, chartName, version string) (string, error) {
	// Get the repository file path from settings
	repoFile := helm.settings.RepositoryConfig

	// Ensure the directory exists
	repoDir := filepath.Dir(repoFile)
	if err := os.MkdirAll(repoDir, 0750); err != nil {
		return "", fmt.Errorf("failed to create repository directory: %w", err)
	}

	// Load existing repositories or create new file
	var file *repov1.File
	if _, err := os.Stat(repoFile); os.IsNotExist(err) {
		file = repov1.NewFile()
	} else {
		var err error
		file, err = repov1.LoadFile(repoFile)
		if err != nil {
			return "", fmt.Errorf("failed to load repository file: %w", err)
		}
	}

	repoName := repoNameFor(file, repoURL)
	chartRepo := &repov1.Entry{
		Name: repoName,
		URL:  repoURL,
	}

	known := file.Has(repoName)
	if known {
		// Reuse the stored entry so credentials and TLS settings configured for
		// this repository survive the refresh.
		if existing := file.Get(repoName); existing != nil {
			chartRepo = existing
		}
		if helm.opts.Verbose {
			fmt.Printf("Repository '%s' already exists\n", repoName)
		}
	}

	// A cached index that already contains the exact requested version is
	// authoritative - there is nothing newer we need from the repository.
	if known && helm.cachedIndexHasVersion(repoName, chartName, version) {
		if helm.opts.Verbose {
			fmt.Printf("Using cached index for repository '%s' (%s %s)\n", repoName, chartName, version)
		}
		return repoName, nil
	}

	getters := getter.All(helm.settings)
	chartRepoClient, err := repov1.NewChartRepository(chartRepo, getters)
	if err != nil {
		return "", fmt.Errorf("failed to create chart repository: %w", err)
	}

	// NewChartRepository defaults CachePath to helmpath.CachePath("repository"),
	// which diverges from settings.RepositoryCache when HELM_REPOSITORY_CACHE is
	// set. The downloader reads the latter, so write the index where it looks.
	chartRepoClient.CachePath = helm.settings.RepositoryCache

	// Download index file
	if helm.opts.Verbose {
		if known {
			fmt.Printf("Refreshing Helm repository index '%s' (%s)\n", repoName, repoURL)
		} else {
			fmt.Printf("Adding Helm repository '%s' (%s)\n", repoName, repoURL)
		}
	}

	if _, err := chartRepoClient.DownloadIndexFile(); err != nil {
		// A repository we already know about with a usable cached index should
		// not break offline runs just because the refresh failed.
		if known && helm.hasCachedIndex(repoName) {
			if !helm.opts.Quiet {
				fmt.Printf("Warning: failed to refresh index for repository '%s', using cached index: %v\n", repoName, err)
			}
			return repoName, nil
		}
		return "", fmt.Errorf("failed to download repository index: %w", err)
	}

	// A known repository is already recorded; rewriting its entry would be a
	// no-op at best and could drop fields we did not round-trip.
	if !known {
		file.Update(chartRepo)

		if err := file.WriteFile(repoFile, 0644); err != nil {
			return "", fmt.Errorf("failed to write repository file: %w", err)
		}

		if helm.opts.Verbose {
			fmt.Printf("Repository '%s' added successfully\n", repoName)
		}
	}

	return repoName, nil
}

// cachedIndexPath returns the path Helm's downloader reads when resolving a
// chart from the named repository.
func (helm *HelmProvider) cachedIndexPath(repoName string) string {
	return filepath.Join(helm.settings.RepositoryCache, helmpath.CacheIndexFile(repoName))
}

// hasCachedIndex reports whether a cached index exists for the repository.
func (helm *HelmProvider) hasCachedIndex(repoName string) bool {
	_, err := os.Stat(helm.cachedIndexPath(repoName))
	return err == nil
}

// cachedIndexHasVersion reports whether the cached index already lists an exact
// match for chartName at version. Version ranges and an unset version both mean
// "resolve the newest match", which requires a fresh index, so they never match.
func (helm *HelmProvider) cachedIndexHasVersion(repoName, chartName, version string) bool {
	if chartName == "" || version == "" {
		return false
	}

	index, err := repov1.LoadIndexFile(helm.cachedIndexPath(repoName))
	if err != nil {
		return false
	}

	for _, chartVersion := range index.Entries[chartName] {
		if chartVersion.Version == version {
			return true
		}
	}

	return false
}

// NewHelmProviderForPacking creates a minimal HelmProvider that can pull charts
// without a live Kubernetes cluster. Suitable for use by the pack command.
func NewHelmProviderForPacking(verbose bool) *HelmProvider {
	return &HelmProvider{
		settings: cli.New(),
		opts: &ProviderOptions{
			Verbose: verbose,
		},
	}
}

// PullChartToDir downloads a remote Helm chart (OCI or HTTPS) as a .tgz file
// to destDir. Returns the absolute path to the downloaded .tgz.
// Does not require a live Kubernetes cluster.
func (helm *HelmProvider) PullChartToDir(service *config.ServiceConfig, destDir string) (string, error) {
	if !service.IsRemoteChart() {
		return "", fmt.Errorf("service %q is not a remote Helm chart", service.Name)
	}

	// Build a minimal action config: registry client only, no kubeconfig.
	actionConfig := action.NewConfiguration()
	registryClient, err := registry.NewClient(
		registry.ClientOptDebug(helm.opts.Verbose),
		registry.ClientOptCredentialsFile(helm.settings.RegistryConfig),
	)
	if err != nil {
		return "", fmt.Errorf("failed to create registry client: %w", err)
	}
	actionConfig.RegistryClient = registryClient

	pull := action.NewPull(action.WithConfig(actionConfig))
	pull.Settings = helm.settings
	pull.DestDir = destDir
	pull.Untar = false
	if service.Version != "" {
		pull.Version = service.Version
	}

	var chartRef string
	if config.IsOCIURL(service.Repo) {
		chartRef = fmt.Sprintf("%s/%s", service.Repo, service.Chart)
	} else if config.IsHTTPURL(service.Repo) {
		repoName, err := helm.addHTTPRepositoryForChart(service.Repo, service.Chart, service.Version)
		if err != nil {
			return "", fmt.Errorf("failed to add repository %s: %w", service.Repo, err)
		}
		chartRef = fmt.Sprintf("%s/%s", repoName, service.Chart)
	} else {
		return "", fmt.Errorf("unsupported repo URL: %s", service.Repo)
	}

	if helm.opts.Verbose {
		fmt.Printf("Pulling chart %q to %s\n", chartRef, destDir)
	}

	if _, err = pull.Run(chartRef); err != nil {
		return "", fmt.Errorf("failed to pull chart %q: %w", chartRef, err)
	}

	// Find the downloaded .tgz file in destDir.
	entries, err := os.ReadDir(destDir)
	if err != nil {
		return "", fmt.Errorf("failed to read %s: %w", destDir, err)
	}
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".tgz") {
			return filepath.Join(destDir, e.Name()), nil
		}
	}
	return "", fmt.Errorf("no .tgz found in %s after pulling %q", destDir, chartRef)
}

// generateRepoName generates a unique repository name from URL
func generateRepoName(repoURL string) string {
	// Remove protocol
	name := strings.TrimPrefix(repoURL, "https://")
	name = strings.TrimPrefix(name, "http://")
	// Replace everything but letters, digits and hyphens: a name is used as
	// the first part of "<name>/<chart>", where a ":" (a port) makes Helm
	// read the reference as a URL ("invalid chart URL format")
	name = repoNameInvalidChars.ReplaceAllString(name, "-")
	// Trim trailing hyphens
	name = strings.Trim(name, "-")

	// If name is too long, hash it
	if len(name) > 50 {
		hash := sha256.Sum256([]byte(repoURL))
		name = fmt.Sprintf("repo-%x", hash)[:16]
	}

	return name
}

var repoNameInvalidChars = regexp.MustCompile(`[^A-Za-z0-9-]`)

// repoNameFor returns the repositories.yaml name for repoURL: its generated
// name, unless an entry by that name is for another URL (the name isn't
// unique: http:// and https:// give the same one), whose repository would
// otherwise serve this chart
func repoNameFor(file *repov1.File, repoURL string) string {
	name := generateRepoName(repoURL)
	existing := file.Get(name)
	if existing == nil || strings.TrimSuffix(existing.URL, "/") == strings.TrimSuffix(repoURL, "/") {
		return name
	}
	hash := sha256.Sum256([]byte(repoURL))
	return fmt.Sprintf("%s-%x", name, hash[:4])
}

// loadValues loads the service's values (see LoadHelmValues)
func (helm *HelmProvider) loadValues(service *config.ServiceConfig) (map[string]interface{}, error) {
	if helm.opts.Verbose {
		switch files := service.Values.Files(); {
		case service.ValuesInline != "":
			fmt.Printf("Loading inline values...\n")
		case len(files) == 1:
			fmt.Printf("Loading values from: %s\n", files[0])
		case len(files) > 1:
			fmt.Printf("Loading and merging values from %d file(s): %s\n", len(files), strings.Join(files, ", "))
		}
	}
	return LoadHelmValues(service)
}

// LoadHelmValues returns the values kraze passes to Helm for a service: its
// values_inline, or else its values files merged in order (later files win,
// maps merge key by key). Both are read with Helm's own loader and merged with
// Helm's own merge, so they mean what `helm install -f` makes of them: a
// YAML 1.1 parser (`enabled: no` is false, not the string "no", which a
// template treats as true) whose dates stay strings (a time.Time made Helm
// panic copying the values). Image detection uses it too, so it renders
// charts with the same values as the install.
func LoadHelmValues(service *config.ServiceConfig) (map[string]interface{}, error) {
	if service.ValuesInline != "" {
		values, err := helmloader.LoadValues(strings.NewReader(service.ValuesInline))
		if err != nil {
			return nil, fmt.Errorf("failed to parse inline values: %w", err)
		}
		return values, nil
	}
	values := make(map[string]interface{})
	for _, valuesFile := range service.Values.Files() {
		data, err := os.ReadFile(valuesFile)
		if err != nil {
			return nil, fmt.Errorf("failed to read values file %s: %w", valuesFile, err)
		}
		fileValues, err := helmloader.LoadValues(bytes.NewReader(data))
		if err != nil {
			return nil, fmt.Errorf("failed to parse values file %s: %w", valuesFile, err)
		}
		values = helmloader.MergeMaps(values, fileValues)
	}
	return values, nil
}

// restClientGetter implements RESTClientGetter interface
type restClientGetter struct {
	config          *rest.Config
	namespace       string
	discoveryClient discovery.CachedDiscoveryInterface
	restMapper      meta.RESTMapper
}

func (rest *restClientGetter) ToRESTConfig() (*rest.Config, error) {
	return rest.config, nil
}

func (rest *restClientGetter) ToDiscoveryClient() (discovery.CachedDiscoveryInterface, error) {
	if rest.discoveryClient != nil {
		return rest.discoveryClient, nil
	}

	// Create discovery client
	discoveryClient, err := discovery.NewDiscoveryClientForConfig(rest.config)
	if err != nil {
		return nil, err
	}

	// Wrap with memory-based caching
	rest.discoveryClient = memory.NewMemCacheClient(discoveryClient)
	return rest.discoveryClient, nil
}

func (rest *restClientGetter) ToRESTMapper() (meta.RESTMapper, error) {
	if rest.restMapper != nil {
		return rest.restMapper, nil
	}

	// Get discovery client first
	discoveryClient, err := rest.ToDiscoveryClient()
	if err != nil {
		return nil, err
	}

	// Create REST mapper
	mapper := restmapper.NewDeferredDiscoveryRESTMapper(discoveryClient)
	rest.restMapper = mapper
	return mapper, nil
}

func (rest *restClientGetter) ToRawKubeConfigLoader() clientcmd.ClientConfig {
	return &simpleClientConfig{namespace: rest.namespace}
}

// simpleClientConfig implements clientcmd.ClientConfig interface
type simpleClientConfig struct {
	namespace string
}

func (cfg *simpleClientConfig) RawConfig() (clientcmdapi.Config, error) {
	return clientcmdapi.Config{}, fmt.Errorf("not implemented")
}

func (cfg *simpleClientConfig) ClientConfig() (*rest.Config, error) {
	return nil, fmt.Errorf("not implemented")
}

func (cfg *simpleClientConfig) Namespace() (string, bool, error) {
	return cfg.namespace, false, nil
}

func (cfg *simpleClientConfig) ConfigAccess() clientcmd.ConfigAccess {
	return nil
}

// extractCRDsFromManifest returns the names of the CRDs in a Helm release
// manifest. It parses the documents: a line scan missed names in flow style
// (metadata: {name: x}), so those CRDs were never deleted.
func (helm *HelmProvider) extractCRDsFromManifest(manifest string) []string {
	objs, err := parseManifestsYAML(manifest)
	if err != nil {
		if helm.opts.Verbose {
			fmt.Printf("[HELM] Failed to parse the release manifest for CRDs: %v\n", err)
		}
		return nil
	}
	var crds []string
	for _, obj := range objs {
		gvk := obj.GroupVersionKind()
		if gvk.Group == apiextv1.GroupName && gvk.Kind == "CustomResourceDefinition" && obj.GetName() != "" {
			crds = append(crds, obj.GetName())
		}
	}
	return crds
}

// deleteCRDs deletes the specified CRDs from the cluster
func (helm *HelmProvider) deleteCRDs(ctx context.Context, crdNames []string) error {
	if len(crdNames) == 0 {
		return nil
	}

	newClient := helm.dynamicClient
	if newClient == nil {
		newClient = func() (dynamic.Interface, error) { return dynamic.NewForConfig(helm.restConfig) }
	}
	dynamicClient, err := newClient()
	if err != nil {
		return fmt.Errorf("failed to create dynamic client: %w", err)
	}

	// CRDs use the apiextensions.k8s.io/v1 API
	crdGVR := apiextv1.SchemeGroupVersion.WithResource("customresourcedefinitions")

	var errs []string
	for _, crdName := range crdNames {
		if helm.opts.Verbose {
			fmt.Printf("[HELM] Deleting CRD: %s\n", crdName)
		}

		err := dynamicClient.Resource(crdGVR).Delete(ctx, crdName, metav1.DeleteOptions{})
		if err != nil {
			if errors.IsNotFound(err) {
				// CRD already deleted, ignore
				if helm.opts.Verbose {
					fmt.Printf("[HELM] CRD %s already deleted\n", crdName)
				}
			} else {
				errs = append(errs, fmt.Sprintf("%s: %v", crdName, err))
			}
		}
	}

	if len(errs) > 0 {
		return fmt.Errorf("failed to delete CRDs: %s", strings.Join(errs, "; "))
	}

	return nil
}

// calculateConfigChecksum calculates a checksum of all ConfigMaps and Secrets in a manifest
// Returns empty string if no ConfigMaps or Secrets are found
func calculateConfigChecksum(manifest string) (string, error) {
	if manifest == "" {
		return "", nil
	}

	var configData []string
	err := forEachManifestDocument(manifest, func(doc map[string]interface{}) {
		configData = append(configData, configDataOf(doc)...)
	})
	if err != nil {
		return "", err
	}
	return hashConfigData(configData), nil
}

// configDataOf returns the content of doc that the config checksum covers:
// its configChecksumFields as YAML if it's a ConfigMap or Secret, else nothing
func configDataOf(doc map[string]interface{}) []string {
	kind, _ := doc["kind"].(string)
	if kind != "ConfigMap" && kind != "Secret" {
		return nil
	}

	var configData []string
	for _, field := range configChecksumFields {
		if value, ok := doc[field]; ok {
			// Convert to YAML string for consistent hashing
			if valueBytes, err := yaml.Marshal(value); err == nil {
				configData = append(configData, string(valueBytes))
			}
		}
	}
	return configData
}

// hashConfigData returns the SHA-256 of configData combined, or an empty
// string when there is none
func hashConfigData(configData []string) string {
	if len(configData) == 0 {
		return ""
	}
	hash := sha256.Sum256([]byte(strings.Join(configData, "\n")))
	return fmt.Sprintf("%x", hash)
}

// configChecksumPostRenderer sets the configChecksumAnnotation on the pod
// template of every Deployment, StatefulSet and DaemonSet in a release, to a
// checksum of the release's ConfigMaps and Secrets, so a config change rolls
// them out. It runs as Helm renders, so the annotation is in what Helm
// applies: patching it on after an install changed every pod template a
// second time, and a single-replica hostNetwork Deployment (MetalLB's
// frr-k8s status cleaner) deadlocked on a one-node cluster, its new pod
// unschedulable while the old one held the host port.
//
// It is run with PostRenderStrategyNoHooks, so it sees the release manifest
// without hooks: the same documents the checksum covered when it was patched
// on, in the same order once Secrets are put ahead of ConfigMaps (Helm's
// install order), so upgrading kraze doesn't roll out every release.
type configChecksumPostRenderer struct{}

// workloadKinds are the kinds whose pod template gets the config checksum
var workloadKinds = map[string]bool{"Deployment": true, "StatefulSet": true, "DaemonSet": true}

func (configChecksumPostRenderer) Run(rendered *bytes.Buffer) (*bytes.Buffer, error) {
	var docs []*yaml.Node
	decoder := yaml.NewDecoder(bytes.NewReader(rendered.Bytes()))
	for {
		var doc yaml.Node
		err := decoder.Decode(&doc)
		if stderrors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("failed to parse manifest: %w", err)
		}
		docs = append(docs, &doc)
	}

	var secretData, configMapData []string
	var workloads []*yaml.Node
	for _, doc := range docs {
		var fields map[string]interface{}
		if doc.Decode(&fields) != nil || fields == nil {
			continue
		}
		switch kind, _ := fields["kind"].(string); {
		case kind == "Secret":
			secretData = append(secretData, configDataOf(fields)...)
		case kind == "ConfigMap":
			configMapData = append(configMapData, configDataOf(fields)...)
		case workloadKinds[kind]:
			workloads = append(workloads, doc)
		}
	}

	checksum := hashConfigData(append(secretData, configMapData...))
	if checksum == "" || len(workloads) == 0 {
		return rendered, nil
	}
	for _, doc := range workloads {
		annotations := mappingAt(doc.Content[0], "spec", "template", "metadata", "annotations")
		if annotations == nil {
			return nil, fmt.Errorf("failed to set %s: a workload's pod template isn't a mapping", configChecksumAnnotation)
		}
		setMappingValue(annotations, configChecksumAnnotation, checksum)
	}

	var out bytes.Buffer
	encoder := yaml.NewEncoder(&out)
	encoder.SetIndent(2)
	for _, doc := range docs {
		if err := encoder.Encode(doc); err != nil {
			return nil, fmt.Errorf("failed to write manifest: %w", err)
		}
	}
	if err := encoder.Close(); err != nil {
		return nil, fmt.Errorf("failed to write manifest: %w", err)
	}
	return &out, nil
}

// mappingAt returns the mapping at path under node, creating any part of it
// that is missing or null, or nil if something along it isn't a mapping
func mappingAt(node *yaml.Node, path ...string) *yaml.Node {
	for _, key := range path {
		if node.Kind != yaml.MappingNode {
			return nil
		}
		var child *yaml.Node
		for i := 0; i+1 < len(node.Content); i += 2 {
			if node.Content[i].Value == key {
				child = node.Content[i+1]
				break
			}
		}
		if child == nil {
			child = &yaml.Node{}
			node.Content = append(node.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, child)
		}
		if child.Kind == 0 || (child.Kind == yaml.ScalarNode && child.Tag == "!!null") {
			*child = yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		}
		node = child
	}
	if node.Kind != yaml.MappingNode {
		return nil
	}
	return node
}

// setMappingValue sets key to the string value in mapping, replacing any
// value it has
func setMappingValue(mapping *yaml.Node, key, value string) {
	// Quoted, so a checksum of all digits doesn't read back as a number
	valueNode := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Style: yaml.DoubleQuotedStyle, Value: value}
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == key {
			mapping.Content[i+1] = valueNode
			return
		}
	}
	mapping.Content = append(mapping.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, valueNode)
}

// forEachManifestDocument calls fn with each YAML document of a rendered
// manifest that is a mapping. A document of another shape is skipped (the
// decoder has moved past it). A syntax error stops with an error: the decoder
// can't resume after one and would return it forever.
func forEachManifestDocument(manifest string, fn func(doc map[string]interface{})) error {
	decoder := yaml.NewDecoder(strings.NewReader(manifest))
	for {
		var doc map[string]interface{}
		err := decoder.Decode(&doc)
		if stderrors.Is(err, io.EOF) {
			return nil
		}
		var typeErr *yaml.TypeError
		if stderrors.As(err, &typeErr) {
			continue
		}
		if err != nil {
			return fmt.Errorf("failed to parse manifest: %w", err)
		}
		if doc != nil {
			fn(doc)
		}
	}
}
