package cluster

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	osexec "os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/hjames9/kraze/internal/config"
	"gopkg.in/yaml.v3"
	"helm.sh/helm/v3/pkg/action"
	"helm.sh/helm/v3/pkg/chart/loader"
	"helm.sh/helm/v3/pkg/chartutil"
	"helm.sh/helm/v3/pkg/cli"
	"helm.sh/helm/v3/pkg/engine"
	"helm.sh/helm/v3/pkg/registry"
)

// ImageReference represents a parsed Docker image reference
type ImageReference struct {
	Registry   string // e.g., "docker.io", "gcr.io", "" (for Docker Hub)
	Repository string // e.g., "library/nginx", "bitnami/redis"
	Tag        string // e.g., "latest", "7.0.0"
	Digest     string // e.g., "sha256:abc123..." (optional)
	Original   string // The original image string
}

// ImageInfo contains metadata about a Docker image
type ImageInfo struct {
	Reference ImageReference
	SHA256    string // Image digest/hash from Docker
	IsLocal   bool   // Whether image exists in local Docker daemon
	Size      int64  // Image size in bytes
}

// ImageManager handles image detection, loading, and management
type ImageManager struct {
	verbose bool
}

// NewImageManager creates a new image manager
func NewImageManager(verbose bool) *ImageManager {
	return &ImageManager{
		verbose: verbose,
	}
}

// ParseImageReference parses a Docker image reference into components
// Supports formats:
//   - nginx:latest
//   - docker.io/library/nginx:latest
//   - gcr.io/project/image:tag
//   - registry.io/repo/image@sha256:abc123
func ParseImageReference(image string) ImageReference {
	ref := ImageReference{Original: image}

	// Handle digest format (image@sha256:...)
	if strings.Contains(image, "@") {
		parts := strings.SplitN(image, "@", 2)
		image = parts[0]
		ref.Digest = parts[1]
	}

	// Extract tag
	tagIndex := strings.LastIndex(image, ":")
	slashAfterTag := strings.LastIndex(image, "/")

	// Only consider it a tag if the colon comes after the last slash
	// This handles registry.io:5000/image correctly
	if tagIndex > slashAfterTag && tagIndex != -1 {
		ref.Tag = image[tagIndex+1:]
		image = image[:tagIndex]
	} else {
		ref.Tag = "latest" // Default tag
	}

	// Extract registry and repository
	parts := strings.SplitN(image, "/", 2)

	// Check if first part is a registry (contains . or : or is localhost)
	if len(parts) == 2 && (strings.Contains(parts[0], ".") ||
		strings.Contains(parts[0], ":") ||
		parts[0] == "localhost") {
		ref.Registry = parts[0]
		ref.Repository = parts[1]
	} else {
		// No explicit registry, assume Docker Hub
		ref.Registry = "docker.io"
		if len(parts) == 2 {
			ref.Repository = parts[0] + "/" + parts[1]
		} else {
			// Single name like "nginx" -> "library/nginx"
			ref.Repository = "library/" + image
		}
	}

	return ref
}

// String returns the full image reference as a string
func (ref *ImageReference) String() string {
	if ref.Original != "" {
		return ref.Original
	}

	image := ref.Repository
	if ref.Registry != "" && ref.Registry != "docker.io" {
		image = ref.Registry + "/" + image
	}
	if ref.Tag != "" {
		image = image + ":" + ref.Tag
	}
	if ref.Digest != "" {
		image = image + "@" + ref.Digest
	}
	return image
}

// IsDockerHub returns true if this image is from Docker Hub
func (ref *ImageReference) IsDockerHub() bool {
	return ref.Registry == "docker.io" || ref.Registry == ""
}

// GetImageInfo retrieves metadata about a Docker image
func (im *ImageManager) GetImageInfo(ctx context.Context, imageName string) (*ImageInfo, error) {
	ref := ParseImageReference(imageName)

	info := &ImageInfo{
		Reference: ref,
	}

	// Check if image exists locally using docker inspect
	cmd := osexec.CommandContext(ctx, "docker", "inspect", imageName)
	output, err := cmd.Output()

	if err != nil {
		// Image doesn't exist locally
		info.IsLocal = false
		return info, nil
	}

	// Parse docker inspect output to get image details
	var inspectData []struct {
		RepoDigests []string `json:"RepoDigests"`
		Size        int64    `json:"Size"`
		ID          string   `json:"Id"`
	}

	if err := json.Unmarshal(output, &inspectData); err != nil {
		return nil, fmt.Errorf("failed to parse docker inspect output: %w", err)
	}

	if len(inspectData) > 0 {
		info.IsLocal = true
		info.Size = inspectData[0].Size

		// Extract SHA256 from ID (format: sha256:abc123...)
		if inspectData[0].ID != "" {
			info.SHA256 = inspectData[0].ID
		}

		// If we have repo digests, use the first one
		if len(inspectData[0].RepoDigests) > 0 {
			digestParts := strings.SplitN(inspectData[0].RepoDigests[0], "@", 2)
			if len(digestParts) == 2 {
				info.SHA256 = digestParts[1]
			}
		}
	}

	return info, nil
}

// GetClusterImageHash retrieves the SHA256 hash of an image loaded in the cluster
// Returns empty string if image is not found in the cluster
func (im *ImageManager) GetClusterImageHash(ctx context.Context, clusterName, imageName string) (string, error) {
	// Normalize image name for cluster lookup
	// crictl uses docker.io/ prefix for Docker Hub images
	ref := ParseImageReference(imageName)
	clusterImageName := imageName

	// Add docker.io prefix if it's a Docker Hub image without explicit registry
	if ref.IsDockerHub() && !strings.HasPrefix(imageName, "docker.io/") {
		// If it's library/* (official images), use docker.io/library/
		if !strings.Contains(imageName, "/") {
			clusterImageName = "docker.io/library/" + imageName
		} else {
			clusterImageName = "docker.io/" + imageName
		}
	}

	// Get the control plane container name
	containerName := clusterName + "-control-plane"

	// Check if image exists in cluster using crictl
	cmd := osexec.CommandContext(ctx, "docker", "exec", containerName, "crictl", "inspecti", clusterImageName)
	output, err := cmd.Output()

	if err != nil {
		// Image doesn't exist in cluster
		return "", nil
	}

	// Parse crictl inspect output to get image ID
	var inspectData struct {
		Status struct {
			ID string `json:"id"`
		} `json:"status"`
	}

	if err := json.Unmarshal(output, &inspectData); err != nil {
		return "", fmt.Errorf("failed to parse crictl inspect output: %w", err)
	}

	return inspectData.Status.ID, nil
}

// ExtractImagesFromValues extracts image references from a Helm values file
// Looks for common patterns like:
//
//	image:
//	  repository: nginx
//	  tag: latest
//	images:
//	  myapp:
//	    repository: myapp
//	    tag: v1.0
//
// ExtractImagesFromYAMLString extracts image references from a YAML string
func (im *ImageManager) ExtractImagesFromYAMLString(yamlContent string) ([]string, error) {
	var values map[string]interface{}
	if err := yaml.Unmarshal([]byte(yamlContent), &values); err != nil {
		return nil, fmt.Errorf("failed to parse YAML: %w", err)
	}

	images := make([]string, 0)

	// Recursively extract images from the values structure
	im.extractImagesRecursive(values, &images)

	return images, nil
}

func (im *ImageManager) ExtractImagesFromValues(valuesPath string) ([]string, error) {
	data, err := os.ReadFile(valuesPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read values file: %w", err)
	}

	var values map[string]interface{}
	if err := yaml.Unmarshal(data, &values); err != nil {
		return nil, fmt.Errorf("failed to parse values file: %w", err)
	}

	images := make([]string, 0)

	// Recursively extract images from the values structure
	im.extractImagesRecursive(values, &images)

	return images, nil
}

// extractImagesRecursive recursively searches for image definitions in values
func (im *ImageManager) extractImagesRecursive(data interface{}, images *[]string) {
	switch v := data.(type) {
	case map[string]interface{}:
		// Check if this looks like an image definition
		if repo, hasRepo := v["repository"].(string); hasRepo && repo != "" {
			var tag string
			if t, hasTag := v["tag"].(string); hasTag {
				tag = t
			} else if t, hasTag := v["tag"].(float64); hasTag {
				// YAML numbers are parsed as float64
				tag = fmt.Sprintf("%v", t)
			} else {
				tag = "latest"
			}

			// Build image reference
			image := repo
			if tag != "" && tag != "latest" {
				image = repo + ":" + tag
			} else if tag == "latest" {
				image = repo + ":latest"
			}

			// Check for registry prefix
			if registry, hasRegistry := v["registry"].(string); hasRegistry && registry != "" {
				image = registry + "/" + image
			}

			*images = append(*images, image)
		}

		// Recursively check all nested maps
		for _, val := range v {
			im.extractImagesRecursive(val, images)
		}

	case []interface{}:
		// Recursively check all items in arrays
		for _, item := range v {
			im.extractImagesRecursive(item, images)
		}
	}
}

// ExtractImagesFromHelmChart extracts images from a Helm chart using 'helm template'
// This is more accurate than parsing values files as it renders the actual templates
func (im *ImageManager) ExtractImagesFromHelmChart(ctx context.Context, svc *config.ServiceConfig, kubeconfig string) ([]string, error) {
	settings := cli.New()

	// Create action configuration
	actionConfig := new(action.Configuration)

	// Initialize with namespace
	namespace := svc.GetNamespace()
	if err := actionConfig.Init(settings.RESTClientGetter(), namespace, os.Getenv("HELM_DRIVER"), func(format string, v ...interface{}) {
		if im.verbose {
			fmt.Printf(format+"\n", v...)
		}
	}); err != nil {
		return nil, fmt.Errorf("failed to initialize helm config: %w", err)
	}

	// Initialize registry client for OCI support
	registryClient, err := registry.NewClient(
		registry.ClientOptDebug(im.verbose),
		registry.ClientOptCredentialsFile(settings.RegistryConfig),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create registry client: %w", err)
	}
	actionConfig.RegistryClient = registryClient

	// For local charts
	if svc.IsLocalChart() {
		return im.extractImagesFromLocalChart(svc)
	}

	// For remote charts, we need to render the template
	client := action.NewInstall(actionConfig)
	client.DryRun = true
	client.ReleaseName = svc.Name
	client.Namespace = namespace
	client.Replace = true
	client.ClientOnly = true

	// Locate chart
	chartPath := svc.Path
	if chartPath == "" && svc.Chart != "" {
		// For remote charts, we'd need to download first
		// For now, return empty - we'll enhance this later
		if im.verbose {
			fmt.Printf("Warning: Cannot extract images from remote chart %s without downloading\n", svc.Chart)
		}
		return []string{}, nil
	}

	// Load the chart
	chart, err := loader.Load(chartPath)
	if err != nil {
		return nil, fmt.Errorf("failed to load chart: %w", err)
	}

	// Load values if specified
	vals := make(map[string]interface{})
	if !svc.Values.IsEmpty() {
		for _, valuesFile := range svc.Values.Files() {
			valuesData, err := os.ReadFile(valuesFile)
			if err != nil {
				return nil, fmt.Errorf("failed to read values file %s: %w", valuesFile, err)
			}
			var fileVals map[string]interface{}
			if err := yaml.Unmarshal(valuesData, &fileVals); err != nil {
				return nil, fmt.Errorf("failed to parse values file %s: %w", valuesFile, err)
			}
			// Merge values (simple merge for now - could use helm's merge logic)
			for k, v := range fileVals {
				vals[k] = v
			}
		}
	}

	// Render the chart
	rel, err := client.Run(chart, vals)
	if err != nil {
		return nil, fmt.Errorf("failed to render chart: %w", err)
	}

	// Extract images from the rendered manifest
	return im.extractImagesFromManifest(rel.Manifest), nil
}

// extractImagesFromLocalChart extracts images from a local Helm chart
// extractImagesFromRemoteChart downloads and templates a remote Helm chart to extract images
func (im *ImageManager) extractImagesFromRemoteChart(ctx context.Context, svc *config.ServiceConfig) ([]string, error) {
	images := make([]string, 0)

	// Create temp directory for chart download
	tmpDir, err := os.MkdirTemp("", "kraze-chart-*")
	if err != nil {
		return nil, fmt.Errorf("failed to create temp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	// Create Helm settings
	settings := cli.New()

	// Create action configuration (minimal, no K8s connection needed for pull/template)
	actionConfig := new(action.Configuration)

	// Initialize registry client for OCI support
	registryClient, err := registry.NewClient(
		registry.ClientOptDebug(im.verbose),
		registry.ClientOptCredentialsFile(settings.RegistryConfig),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create registry client: %w", err)
	}
	actionConfig.RegistryClient = registryClient

	// Create pull action
	pull := action.NewPullWithOpts(action.WithConfig(actionConfig))
	pull.Settings = settings
	pull.DestDir = tmpDir
	pull.Untar = true

	if svc.Version != "" {
		pull.Version = svc.Version
	}

	// Build chart reference
	chartRef := fmt.Sprintf("%s/%s", svc.Repo, svc.Chart)

	// Pull the chart using SDK
	if im.verbose {
		fmt.Printf("  Pulling chart %s...\n", chartRef)
	}

	_, err = pull.Run(chartRef)
	if err != nil {
		return nil, fmt.Errorf("failed to pull chart: %w", err)
	}

	// Find the downloaded chart directory
	entries, err := os.ReadDir(tmpDir)
	if err != nil {
		return nil, fmt.Errorf("failed to read temp dir: %w", err)
	}

	var chartPath string
	for _, entry := range entries {
		if entry.IsDir() {
			chartPath = filepath.Join(tmpDir, entry.Name())
			break
		}
	}

	if chartPath == "" {
		return nil, fmt.Errorf("no chart found after pull")
	}

	// Load the chart
	chart, err := loader.Load(chartPath)
	if err != nil {
		return nil, fmt.Errorf("failed to load chart: %w", err)
	}

	// Prepare values (use chart's default values)
	values := make(map[string]interface{})
	if chart.Values != nil {
		values = chart.Values
	}

	// Create release options for rendering
	releaseOptions := chartutil.ReleaseOptions{
		Name:      "kraze-temp",
		Namespace: svc.Namespace,
		IsInstall: true,
	}

	// Compute values with built-in objects
	valuesToRender, err := chartutil.ToRenderValues(chart, values, releaseOptions, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to prepare values: %w", err)
	}

	// Render templates using Helm engine
	eng := engine.Engine{
		LintMode: false,
	}

	rendered, err := eng.Render(chart, valuesToRender)
	if err != nil {
		return nil, fmt.Errorf("failed to render chart templates: %w", err)
	}

	// Combine all rendered manifests
	var allManifests strings.Builder
	for _, manifest := range rendered {
		allManifests.WriteString(manifest)
		allManifests.WriteString("\n---\n")
	}

	// Extract images from rendered manifests
	images = im.extractImagesFromManifest(allManifests.String())

	if im.verbose {
		fmt.Printf("  Extracted %d image(s) from chart templates\n", len(images))
	}

	return images, nil
}

func (im *ImageManager) extractImagesFromLocalChart(svc *config.ServiceConfig) ([]string, error) {
	images := make([]string, 0)

	// Try to extract from values files if specified
	if !svc.Values.IsEmpty() {
		for _, valuesFile := range svc.Values.Files() {
			valuesImages, err := im.ExtractImagesFromValues(valuesFile)
			if err != nil {
				if im.verbose {
					fmt.Printf("Warning: Failed to extract images from values file %s: %v\n", valuesFile, err)
				}
			} else {
				images = append(images, valuesImages...)
			}
		}
	}

	// Try to extract from chart's default values.yaml
	defaultValuesPath := filepath.Join(svc.Path, "values.yaml")
	if _, err := os.Stat(defaultValuesPath); err == nil {
		valuesImages, err := im.ExtractImagesFromValues(defaultValuesPath)
		if err != nil {
			if im.verbose {
				fmt.Printf("Warning: Failed to extract images from default values: %v\n", err)
			}
		} else {
			images = append(images, valuesImages...)
		}
	}

	return images, nil
}

// ExtractImagesFromManifests extracts images from Kubernetes manifest files
func (im *ImageManager) ExtractImagesFromManifests(manifestPaths []string) ([]string, error) {
	allImages := make([]string, 0)

	for _, path := range manifestPaths {
		// Check if path is a file or directory
		info, err := os.Stat(path)
		if err != nil {
			return nil, fmt.Errorf("failed to stat %s: %w", path, err)
		}

		var files []string
		if info.IsDir() {
			// Read all YAML files in directory
			entries, err := os.ReadDir(path)
			if err != nil {
				return nil, fmt.Errorf("failed to read directory %s: %w", path, err)
			}

			for _, entry := range entries {
				if !entry.IsDir() && (strings.HasSuffix(entry.Name(), ".yaml") || strings.HasSuffix(entry.Name(), ".yml")) {
					files = append(files, filepath.Join(path, entry.Name()))
				}
			}
		} else {
			files = append(files, path)
		}

		// Extract images from each file
		for _, file := range files {
			data, err := os.ReadFile(file)
			if err != nil {
				return nil, fmt.Errorf("failed to read %s: %w", file, err)
			}

			images := im.extractImagesFromManifest(string(data))
			allImages = append(allImages, images...)
		}
	}

	return allImages, nil
}

// extractImagesFromManifest extracts image references from Kubernetes YAML manifest
func (im *ImageManager) extractImagesFromManifest(manifest string) []string {
	images := make([]string, 0)

	// Use regex to find image fields in YAML
	// Matches patterns like:
	//   image: nginx:latest
	//   image: "nginx:latest"
	//   - image: nginx:latest
	imageRegex := regexp.MustCompile(`(?m)^\s*-?\s*image:\s*["']?([^\s"']+)["']?\s*$`)

	matches := imageRegex.FindAllStringSubmatch(manifest, -1)
	for _, match := range matches {
		if len(match) > 1 {
			image := strings.TrimSpace(match[1])
			if image != "" {
				images = append(images, image)
			}
		}
	}

	return images
}

// FilterLocalImages returns only images that exist in the local Docker daemon
func (im *ImageManager) FilterLocalImages(ctx context.Context, images []string) ([]string, error) {
	localImages := make([]string, 0)

	for _, image := range images {
		info, err := im.GetImageInfo(ctx, image)
		if err != nil {
			return nil, fmt.Errorf("failed to get info for image %s: %w", image, err)
		}

		if info.IsLocal {
			localImages = append(localImages, image)
		}
	}

	return localImages, nil
}

// DeduplicateImages removes duplicate images from a list
func DeduplicateImages(images []string) []string {
	seen := make(map[string]bool)
	result := make([]string, 0)

	for _, image := range images {
		if !seen[image] {
			seen[image] = true
			result = append(result, image)
		}
	}

	return result
}

// GetImagesForService extracts all images needed by a service
func (im *ImageManager) GetImagesForService(ctx context.Context, svc *config.ServiceConfig, kubeconfig string) ([]string, error) {
	images := make([]string, 0)

	if svc.IsHelm() {
		// For Helm charts, try multiple extraction methods

		// Method 1: Extract from inline values
		if svc.ValuesInline != "" {
			inlineImages, err := im.ExtractImagesFromYAMLString(svc.ValuesInline)
			if err != nil {
				if im.verbose {
					fmt.Printf("Warning: Failed to extract images from inline values: %v\n", err)
				}
			} else {
				images = append(images, inlineImages...)
			}
		}

		// Method 2: Extract from values files
		if !svc.Values.IsEmpty() {
			for _, valuesFile := range svc.Values.Files() {
				valuesImages, err := im.ExtractImagesFromValues(valuesFile)
				if err != nil {
					if im.verbose {
						fmt.Printf("Warning: Failed to extract images from values file %s: %v\n", valuesFile, err)
					}
				} else {
					images = append(images, valuesImages...)
				}
			}
		}

		// Method 3: For local charts, check default values
		if svc.IsLocalChart() {
			chartImages, err := im.extractImagesFromLocalChart(svc)
			if err != nil {
				if im.verbose {
					fmt.Printf("Warning: Failed to extract images from chart: %v\n", err)
				}
			} else {
				images = append(images, chartImages...)
			}
		}

		// Method 4: For remote charts, use helm template to render and extract images
		// This is the most accurate method but requires downloading the chart
		if svc.IsRemoteChart() && len(images) == 0 {
			if im.verbose {
				fmt.Printf("No images detected from values, attempting helm template rendering...\n")
			}
			templateImages, err := im.extractImagesFromRemoteChart(ctx, svc)
			if err != nil {
				if im.verbose {
					fmt.Printf("Warning: Failed to extract images from remote chart: %v\n", err)
				}
			} else {
				images = append(images, templateImages...)
			}
		}

	} else if svc.IsManifests() {
		// For manifests, extract from files
		manifestPaths := svc.Paths
		if svc.Path != "" {
			manifestPaths = append(manifestPaths, svc.Path)
		}

		manifestImages, err := im.ExtractImagesFromManifests(manifestPaths)
		if err != nil {
			return nil, fmt.Errorf("failed to extract images from manifests: %w", err)
		}
		images = append(images, manifestImages...)
	}

	// Deduplicate
	images = DeduplicateImages(images)

	return images, nil
}
