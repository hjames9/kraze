package providers

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/hjames9/kraze/internal/color"
	"github.com/hjames9/kraze/internal/config"
	"gopkg.in/yaml.v3"
	"helm.sh/helm/v3/pkg/action"
	"helm.sh/helm/v3/pkg/chart/loader"
	"helm.sh/helm/v3/pkg/cli"
	"helm.sh/helm/v3/pkg/getter"
	"helm.sh/helm/v3/pkg/registry"
	"helm.sh/helm/v3/pkg/release"
	"helm.sh/helm/v3/pkg/repo"
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

// getActionConfig creates an action.Configuration for a specific namespace
// This ensures Helm stores release metadata in the correct namespace
func (helm *HelmProvider) getActionConfig(namespace string) (*action.Configuration, error) {
	actionConfig := new(action.Configuration)

	// Create a REST client getter with our config
	restGetter := &restClientGetter{
		config:    helm.restConfig,
		namespace: namespace,
	}

	// Initialize action config with the target namespace
	// This ensures Helm stores release secrets in the same namespace as the chart
	if err := actionConfig.Init(restGetter, namespace, os.Getenv("HELM_DRIVER"), func(format string, v ...interface{}) {
		if helm.opts.Verbose {
			fmt.Printf("[HELM] "+format+"\n", v...)
		}
	}); err != nil {
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
	actionConfig, err := helm.getActionConfig(service.GetNamespace())
	if err != nil {
		return err
	}

	// Check if release already exists
	histClient := action.NewHistory(actionConfig)
	histClient.Max = 1
	_, err = histClient.Run(service.Name)
	releaseExists := err == nil

	// Get chart path - download if remote
	chartPath, err := helm.getChartPath(ctx, service)
	if err != nil {
		return fmt.Errorf("failed to get chart: %w", err)
	}

	chart, err := loader.Load(chartPath)
	if err != nil {
		return fmt.Errorf("failed to load chart: %w", err)
	}

	// Load values
	values, err := helm.loadValues(service)
	if err != nil {
		return fmt.Errorf("failed to load values: %w", err)
	}

	var rel *release.Release

	if releaseExists {
		// Upgrade existing release
		upgradeClient := action.NewUpgrade(actionConfig)
		upgradeClient.Namespace = service.GetNamespace()
		upgradeClient.Wait = false
		upgradeClient.WaitForJobs = false

		if helm.opts.Timeout != "" {
			timeout, err := time.ParseDuration(helm.opts.Timeout)
			if err == nil {
				upgradeClient.Timeout = timeout
			}
		}

		if service.Version != "" {
			upgradeClient.Version = service.Version
		}

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
		installClient := action.NewInstall(actionConfig)
		installClient.ReleaseName = service.Name
		installClient.Namespace = service.GetNamespace()
		installClient.CreateNamespace = service.ShouldCreateNamespace()
		installClient.Wait = false
		installClient.WaitForJobs = false

		if helm.opts.Timeout != "" {
			timeout, err := time.ParseDuration(helm.opts.Timeout)
			if err == nil {
				installClient.Timeout = timeout
			}
		}

		if service.Version != "" {
			installClient.Version = service.Version
		}

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

	// Inject config checksums to force rollouts when ConfigMaps/Secrets change
	if rel != nil && rel.Manifest != "" {
		checksum, err := calculateConfigChecksum(rel.Manifest)
		if err != nil {
			if helm.opts.Verbose {
				fmt.Printf("Warning: failed to calculate config checksum: %v\n", err)
			}
		} else if checksum != "" {
			if err := helm.injectConfigChecksums(ctx, service.GetNamespace(), rel.Manifest, checksum); err != nil {
				if helm.opts.Verbose {
					fmt.Printf("Warning: failed to inject config checksums: %v\n", err)
				}
			}
		}
	}

	// Wait for resources to be ready using our shared wait logic
	if helm.opts.Wait && rel != nil && rel.Manifest != "" {
		// Use WaitForManifestsInNamespace to apply the release namespace to resources
		if err := WaitForManifestsInNamespace(ctx, helm.opts.KubeConfig, rel.Manifest, service.GetNamespace(), helm.opts); err != nil {
			return fmt.Errorf("failed waiting for resources: %w", err)
		}
	}

	return nil
}

// Uninstall removes a Helm release
func (helm *HelmProvider) Uninstall(ctx context.Context, service *config.ServiceConfig) error {
	// Get action config for this service's namespace
	actionConfig, err := helm.getActionConfig(service.GetNamespace())
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
		rel, err := statusClient.Run(service.Name)
		if err == nil && rel != nil && rel.Manifest != "" {
			// Parse manifest to find CRDs
			releaseCRDs = helm.extractCRDsFromManifest(rel.Manifest)
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

// Status returns the status of a Helm release
func (helm *HelmProvider) Status(ctx context.Context, service *config.ServiceConfig) (*ServiceStatus, error) {
	// Get action config for this service's namespace
	actionConfig, err := helm.getActionConfig(service.GetNamespace())
	if err != nil {
		return nil, err
	}

	client := action.NewStatus(actionConfig)

	rel, err := client.Run(service.Name)
	if err != nil {
		return &ServiceStatus{
			Name:      service.Name,
			Installed: false,
			Ready:     false,
			Message:   fmt.Sprintf("Not installed: %v", err),
		}, nil
	}

	status := &ServiceStatus{
		Name:      service.Name,
		Installed: true,
		Ready:     rel.Info.Status == release.StatusDeployed,
		Message:   string(rel.Info.Status),
	}

	return status, nil
}

// IsInstalled checks if a Helm release is installed
func (helm *HelmProvider) IsInstalled(ctx context.Context, service *config.ServiceConfig) (bool, error) {
	// Get action config for this service's namespace
	actionConfig, err := helm.getActionConfig(service.GetNamespace())
	if err != nil {
		return false, err
	}

	client := action.NewList(actionConfig)
	client.Filter = service.Name

	releases, err := client.Run()
	if err != nil {
		return false, fmt.Errorf("failed to list releases: %w", err)
	}

	for _, rel := range releases {
		if rel.Name == service.Name {
			return true, nil
		}
	}

	return false, nil
}

// getChartPath returns the local path to a chart, downloading it if necessary
func (helm *HelmProvider) getChartPath(ctx context.Context, service *config.ServiceConfig) (string, error) {
	// Local chart - return the local path directly
	if service.IsLocalChart() {
		return service.Path, nil
	}

	// Remote chart - need to download it
	if service.IsRemoteChart() {
		// For OCI registries, use LocateChart (it handles OCI natively)
		if config.IsOCIURL(service.Repo) {
			// Get properly initialized action config with registry client
			actionConfig, err := helm.getActionConfig(service.GetNamespace())
			if err != nil {
				return "", fmt.Errorf("failed to get action config for OCI chart: %w", err)
			}

			client := action.NewInstall(actionConfig)
			if service.Version != "" {
				client.Version = service.Version
			}
			chartRef := fmt.Sprintf("%s/%s", service.Repo, service.Chart)
			return client.ChartPathOptions.LocateChart(chartRef, helm.settings)
		}

		// For HTTP/HTTPS repositories, we need to pull the chart using the SDK
		if config.IsHTTPURL(service.Repo) {
			return helm.pullHTTPChart(service)
		}

		// Fallback: try direct path reference
		return service.Path, nil
	}

	return "", fmt.Errorf("unable to determine chart path: service must have either 'path' (for local) or 'repo' and 'chart' (for remote)")
}

// pullHTTPChart downloads a chart from an HTTP/HTTPS repository using Helm SDK
func (helm *HelmProvider) pullHTTPChart(service *config.ServiceConfig) (string, error) {
	// Create a temporary directory for chart download
	tmpDir, err := os.MkdirTemp("", "kraze-helm-")
	if err != nil {
		return "", fmt.Errorf("failed to create temp directory: %w", err)
	}

	// Add the repository first so Pull can find it
	repoName, err := helm.addHTTPRepository(service.Repo)
	if err != nil {
		return "", fmt.Errorf("failed to add repository: %w", err)
	}

	// Build the chart reference using the repo name
	chartRef := fmt.Sprintf("%s/%s", repoName, service.Chart)

	// Create Pull action
	actionConfig := new(action.Configuration)

	// Initialize registry client for OCI support (for consistency)
	registryClient, err := registry.NewClient(
		registry.ClientOptDebug(helm.opts.Verbose),
		registry.ClientOptCredentialsFile(helm.settings.RegistryConfig),
	)
	if err != nil {
		return "", fmt.Errorf("failed to create registry client: %w", err)
	}
	actionConfig.RegistryClient = registryClient

	pull := action.NewPullWithOpts(action.WithConfig(actionConfig))
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

// addHTTPRepository adds an HTTP(S) Helm repository and returns its name
func (helm *HelmProvider) addHTTPRepository(repoURL string) (string, error) {
	// Generate a unique repository name from the URL
	repoName := generateRepoName(repoURL)

	// Create repository entry
	chartRepo := &repo.Entry{
		Name: repoName,
		URL:  repoURL,
	}

	// Get the repository file path from settings
	repoFile := helm.settings.RepositoryConfig

	// Ensure the directory exists
	repoDir := filepath.Dir(repoFile)
	if err := os.MkdirAll(repoDir, 0755); err != nil {
		return "", fmt.Errorf("failed to create repository directory: %w", err)
	}

	// Load existing repositories or create new file
	var file *repo.File
	if _, err := os.Stat(repoFile); os.IsNotExist(err) {
		file = repo.NewFile()
	} else {
		var err error
		file, err = repo.LoadFile(repoFile)
		if err != nil {
			return "", fmt.Errorf("failed to load repository file: %w", err)
		}
	}

	// Check if repository already exists
	if file.Has(repoName) {
		if helm.opts.Verbose {
			fmt.Printf("Repository '%s' already exists\n", repoName)
		}
		return repoName, nil
	}

	// Add repository
	getters := getter.All(helm.settings)
	chartRepoClient, err := repo.NewChartRepository(chartRepo, getters)
	if err != nil {
		return "", fmt.Errorf("failed to create chart repository: %w", err)
	}

	// Download index file
	if helm.opts.Verbose {
		fmt.Printf("Adding Helm repository '%s' (%s)\n", repoName, repoURL)
	}

	_, err = chartRepoClient.DownloadIndexFile()
	if err != nil {
		return "", fmt.Errorf("failed to download repository index: %w", err)
	}

	// Add to repository file
	file.Update(chartRepo)

	// Write repository file
	if err := file.WriteFile(repoFile, 0644); err != nil {
		return "", fmt.Errorf("failed to write repository file: %w", err)
	}

	if helm.opts.Verbose {
		fmt.Printf("Repository '%s' added successfully\n", repoName)
	}

	return repoName, nil
}

// generateRepoName generates a unique repository name from URL
func generateRepoName(repoURL string) string {
	// Remove protocol
	name := strings.TrimPrefix(repoURL, "https://")
	name = strings.TrimPrefix(name, "http://")
	// Replace non-alphanumeric characters with hyphens
	name = strings.ReplaceAll(name, "/", "-")
	name = strings.ReplaceAll(name, ".", "-")
	// Trim trailing hyphens
	name = strings.Trim(name, "-")

	// If name is too long, hash it
	if len(name) > 50 {
		hash := sha256.Sum256([]byte(repoURL))
		name = fmt.Sprintf("repo-%x", hash)[:16]
	}

	return name
}

// mergeMaps performs a deep merge of two maps, with override values taking precedence
func mergeMaps(base, override map[string]interface{}) map[string]interface{} {
	result := make(map[string]interface{})

	// Copy all values from base
	for key, value := range base {
		result[key] = value
	}

	// Merge override values
	for key, overrideValue := range override {
		if baseValue, exists := result[key]; exists {
			// If both values are maps, merge them recursively
			baseMap, baseIsMap := baseValue.(map[string]interface{})
			overrideMap, overrideIsMap := overrideValue.(map[string]interface{})

			if baseIsMap && overrideIsMap {
				result[key] = mergeMaps(baseMap, overrideMap)
				continue
			}
		}

		// Otherwise, override the value
		result[key] = overrideValue
	}

	return result
}

// loadValues loads values from the values file(s) or inline values
func (helm *HelmProvider) loadValues(service *config.ServiceConfig) (map[string]interface{}, error) {
	values := make(map[string]interface{})

	// Priority 1: Inline values
	if service.ValuesInline != "" {
		if helm.opts.Verbose {
			fmt.Printf("Loading inline values...\n")
		}

		// Parse inline YAML
		if err := yaml.Unmarshal([]byte(service.ValuesInline), &values); err != nil {
			return nil, fmt.Errorf("failed to parse inline values: %w", err)
		}

		if helm.opts.Verbose {
			fmt.Printf("Loaded %d value(s) from inline values\n", len(values))
		}

		return values, nil
	}

	// Priority 2: Values file(s) - merge in order
	if !service.Values.IsEmpty() {
		files := service.Values.Files()

		if helm.opts.Verbose {
			if len(files) == 1 {
				fmt.Printf("Loading values from: %s\n", files[0])
			} else {
				fmt.Printf("Loading and merging values from %d file(s)...\n", len(files))
			}
		}

		for i, valuesFile := range files {
			if helm.opts.Verbose && len(files) > 1 {
				fmt.Printf("  [%d/%d] %s\n", i+1, len(files), valuesFile)
			}

			// Read the values file
			data, err := os.ReadFile(valuesFile)
			if err != nil {
				return nil, fmt.Errorf("failed to read values file %s: %w", valuesFile, err)
			}

			// Parse YAML
			var fileValues map[string]interface{}
			if err := yaml.Unmarshal(data, &fileValues); err != nil {
				return nil, fmt.Errorf("failed to parse values file %s: %w", valuesFile, err)
			}

			// Merge into existing values (later files override earlier ones)
			values = mergeMaps(values, fileValues)
		}

		if helm.opts.Verbose {
			if len(files) == 1 {
				fmt.Printf("Loaded %d value(s) from %s\n", len(values), files[0])
			} else {
				fmt.Printf("Loaded and merged %d total value(s) from %d file(s)\n", len(values), len(files))
			}
		}

		return values, nil
	}

	// No values specified
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

// extractCRDsFromManifest parses a Helm manifest and extracts CRD names
func (helm *HelmProvider) extractCRDsFromManifest(manifest string) []string {
	var crds []string

	// Split manifest by document separator
	docs := strings.Split(manifest, "\n---\n")

	for _, doc := range docs {
		doc = strings.TrimSpace(doc)
		if doc == "" {
			continue
		}

		// Simple check for CRD kind (more efficient than full YAML parsing)
		if strings.Contains(doc, "kind: CustomResourceDefinition") ||
			strings.Contains(doc, "kind:CustomResourceDefinition") {

			// Extract the CRD name from metadata.name
			// Look for "  name: something" after "metadata:"
			lines := strings.Split(doc, "\n")
			inMetadata := false
			for _, line := range lines {
				trimmed := strings.TrimSpace(line)
				if trimmed == "metadata:" {
					inMetadata = true
					continue
				}
				if inMetadata && strings.HasPrefix(trimmed, "name:") {
					// Extract name
					parts := strings.SplitN(trimmed, ":", 2)
					if len(parts) == 2 {
						name := strings.TrimSpace(parts[1])
						// Remove quotes if present
						name = strings.Trim(name, "\"'")
						if name != "" {
							crds = append(crds, name)
						}
					}
					break
				}
				// Stop if we hit another top-level key
				if inMetadata && strings.HasPrefix(line, "apiVersion") {
					break
				}
			}
		}
	}

	return crds
}

// deleteCRDs deletes the specified CRDs from the cluster
func (helm *HelmProvider) deleteCRDs(ctx context.Context, crdNames []string) error {
	if len(crdNames) == 0 {
		return nil
	}

	// Create a dynamic client to delete CRDs
	dynamicClient, err := dynamic.NewForConfig(helm.restConfig)
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

	// Parse YAML documents
	decoder := yaml.NewDecoder(strings.NewReader(manifest))

	var configData []string

	for {
		var doc map[string]interface{}
		if err := decoder.Decode(&doc); err != nil {
			if err.Error() == "EOF" {
				break
			}
			// Skip invalid documents
			continue
		}

		if doc == nil {
			continue
		}

		kind, _ := doc["kind"].(string)

		// Only process ConfigMaps and Secrets
		if kind != "ConfigMap" && kind != "Secret" {
			continue
		}

		// Extract data field
		data, hasData := doc["data"]
		if hasData {
			// Convert to YAML string for consistent hashing
			dataBytes, err := yaml.Marshal(data)
			if err == nil {
				configData = append(configData, string(dataBytes))
			}
		}

		// For Secrets, also check stringData
		if kind == "Secret" {
			stringData, hasStringData := doc["stringData"]
			if hasStringData {
				stringDataBytes, err := yaml.Marshal(stringData)
				if err == nil {
					configData = append(configData, string(stringDataBytes))
				}
			}
		}
	}

	// If no config found, return empty string
	if len(configData) == 0 {
		return "", nil
	}

	// Calculate SHA-256 hash of all config data combined
	combined := strings.Join(configData, "\n")
	hash := sha256.Sum256([]byte(combined))
	return fmt.Sprintf("%x", hash), nil
}

// injectConfigChecksums patches Deployments, StatefulSets, and DaemonSets with config checksum annotations
// This forces a rollout when ConfigMaps or Secrets change
func (helm *HelmProvider) injectConfigChecksums(ctx context.Context, namespace string, manifest string, checksum string) error {
	if manifest == "" || checksum == "" {
		return nil
	}

	// Create dynamic client
	dynamicClient, err := dynamic.NewForConfig(helm.restConfig)
	if err != nil {
		return fmt.Errorf("failed to create dynamic client: %w", err)
	}

	// Create discovery client for REST mapper
	discoveryClient, err := discovery.NewDiscoveryClientForConfig(helm.restConfig)
	if err != nil {
		return fmt.Errorf("failed to create discovery client: %w", err)
	}

	mapper := restmapper.NewDeferredDiscoveryRESTMapper(memory.NewMemCacheClient(discoveryClient))

	// Parse YAML documents
	decoder := yaml.NewDecoder(strings.NewReader(manifest))

	for {
		var doc map[string]interface{}
		if err := decoder.Decode(&doc); err != nil {
			if err.Error() == "EOF" {
				break
			}
			continue
		}

		if doc == nil {
			continue
		}

		kind, _ := doc["kind"].(string)

		// Only process workload resources that have Pod templates
		if kind != "Deployment" && kind != "StatefulSet" && kind != "DaemonSet" {
			continue
		}

		metadata, ok := doc["metadata"].(map[string]interface{})
		if !ok {
			continue
		}

		name, _ := metadata["name"].(string)
		if name == "" {
			continue
		}

		// Use the namespace from the document, or fall back to the release namespace
		docNamespace, _ := metadata["namespace"].(string)
		if docNamespace == "" {
			docNamespace = namespace
		}

		// Use shared patching function
		patchWorkloadWithConfigChecksum(ctx, dynamicClient, mapper, kind, name, docNamespace, checksum, helm.opts.Verbose)
	}

	return nil
}
