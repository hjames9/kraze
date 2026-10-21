package cluster

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/hjames9/kraze/internal/config"
	"github.com/hjames9/kraze/internal/providers"
	"go.yaml.in/yaml/v3"
	chartcommon "helm.sh/helm/v4/pkg/chart/common"
	chartcommonutil "helm.sh/helm/v4/pkg/chart/common/util"
	v2loader "helm.sh/helm/v4/pkg/chart/v2/loader"
	"helm.sh/helm/v4/pkg/engine"
	k8syaml "k8s.io/apimachinery/pkg/util/yaml"
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
	Reference     ImageReference
	SHA256        string // Docker's image ID: what crictl reports for the same image in a node (see GetClusterImageHash)
	IsLocal       bool   // True if image was built locally (no RepoDigests — never pushed to a registry)
	InLocalDaemon bool   // True if image physically exists in the local Docker daemon (built or pulled)
	Size          int64  // Image size in bytes
}

// ImageManager handles image detection, loading, and management
type ImageManager struct {
	verbose bool

	run commandRunner
	// pullChart downloads a remote chart into dir, returning its path
	pullChart func(svc *config.ServiceConfig, dir string) (string, error)
}

// NewImageManager creates a new image manager
func NewImageManager(verbose bool) *ImageManager {
	return &ImageManager{
		verbose: verbose,
		run:     execRunner{},
		pullChart: func(svc *config.ServiceConfig, dir string) (string, error) {
			// The provider's pull handles OCI and HTTP(S) repositories the way
			// install does (repository registration, cached index, credentials)
			return providers.NewHelmProviderForPacking(verbose).PullChartToDir(svc, dir)
		},
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
	output, err := im.run.Output(ctx, "docker", "inspect", imageName)

	if err != nil {
		// Image doesn't exist in the local Docker daemon
		return info, nil
	}

	// Image exists in the local Docker daemon regardless of its origin.
	info.InLocalDaemon = true

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
		info.Size = inspectData[0].Size

		// The image ID (sha256:...), for pulled images too. A pulled image's
		// registry digest hashes its manifest, not the image: it never equals
		// the ID crictl reports in the node, so every `up` reloaded the image
		info.SHA256 = inspectData[0].ID

		if len(inspectData[0].RepoDigests) > 0 {
			// Image was pulled from a registry (has RepoDigests).
			// IsLocal stays false — it describes origin, not daemon presence.
			// InLocalDaemon is already true (set above).
		} else {
			// No repo digests — image was built locally and never pushed to a registry.
			info.IsLocal = true
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

	// Check if image exists in cluster using crictl inspecti (inspect image, not container)
	output, err := im.run.Output(ctx, "docker", "exec", containerName, "crictl", "inspecti", clusterImageName)

	if err != nil {
		// Image doesn't exist in cluster
		return "", nil
	}

	// Parse crictl inspecti output to get image ID
	var inspectData struct {
		Status struct {
			ID string `json:"id"`
		} `json:"status"`
	}

	if err := json.Unmarshal(output, &inspectData); err != nil {
		return "", fmt.Errorf("failed to parse crictl inspecti output: %w", err)
	}

	return inspectData.Status.ID, nil
}

// ClusterImage represents an image loaded in a kind cluster node
type ClusterImage struct {
	ID       string
	RepoTags []string
	Size     string
}

// ListClusterImages returns all images currently loaded in the kind cluster
func (im *ImageManager) ListClusterImages(ctx context.Context, clusterName string) ([]ClusterImage, error) {
	containerName := clusterName + "-control-plane"

	output, err := im.run.Output(ctx, "docker", "exec", containerName, "crictl", "images", "-o", "json")
	if err != nil {
		return nil, fmt.Errorf("failed to list cluster images: %w", err)
	}

	return parseClusterImages(output)
}

// parseClusterImages parses the JSON output of `crictl images -o json`
func parseClusterImages(output []byte) ([]ClusterImage, error) {
	var result struct {
		Images []struct {
			ID       string   `json:"id"`
			RepoTags []string `json:"repoTags"`
			Size     string   `json:"size"`
		} `json:"images"`
	}

	if err := json.Unmarshal(output, &result); err != nil {
		return nil, fmt.Errorf("failed to parse crictl images output: %w", err)
	}

	images := make([]ClusterImage, 0, len(result.Images))
	for _, img := range result.Images {
		images = append(images, ClusterImage{
			ID:       img.ID,
			RepoTags: img.RepoTags,
			Size:     img.Size,
		})
	}
	return images, nil
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
		// Flat string image reference: image: "registry/repo:tag"
		// Used for image volumes and other non-standard patterns where the full
		// image reference is a single string rather than repository+tag fields.
		if imgStr, ok := v["image"].(string); ok && imgStr != "" {
			*images = append(*images, imgStr)
		}

		// Check if this looks like an image definition
		if repo, hasRepo := v["repository"].(string); hasRepo && repo != "" {
			var tag string
			switch t := v["tag"].(type) {
			case string:
				tag = t
			case int, int64, uint64, float64:
				// An unquoted numeric tag (tag: 20240101)
				tag = fmt.Sprintf("%v", t)
			default:
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

// renderedHelmImages renders the service's chart (local, or pulled from its
// repository) with the values kraze installs it with, and returns the
// container images in the result. This is what the chart actually runs:
// values alone can't tell a tag defaulted from appVersion, a registry from
// global.imageRegistry, or a container a value turns on.
func (im *ImageManager) renderedHelmImages(ctx context.Context, svc *config.ServiceConfig) ([]string, error) {
	chartPath := svc.Path
	if svc.IsRemoteChart() {
		tmpDir, err := os.MkdirTemp("", "kraze-chart-*")
		if err != nil {
			return nil, fmt.Errorf("failed to create temp dir: %w", err)
		}
		defer func() { _ = os.RemoveAll(tmpDir) }()
		chartPath, err = im.pullChart(svc, tmpDir)
		if err != nil {
			return nil, err
		}
	}

	chart, err := v2loader.Load(chartPath)
	if err != nil {
		return nil, fmt.Errorf("failed to load chart: %w", err)
	}
	values, err := providers.LoadHelmValues(svc)
	if err != nil {
		return nil, err
	}
	releaseOptions := chartcommon.ReleaseOptions{Name: svc.Name, Namespace: svc.GetNamespace(), IsInstall: true}
	renderValues, err := chartcommonutil.ToRenderValues(chart, values, releaseOptions, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to prepare values: %w", err)
	}
	rendered, err := (&engine.Engine{}).RenderWithContext(ctx, chart, renderValues)
	if err != nil {
		return nil, fmt.Errorf("failed to render chart templates: %w", err)
	}

	var images []string
	for name, manifest := range rendered {
		if strings.HasSuffix(name, "NOTES.txt") {
			continue
		}
		images = append(images, im.extractImagesFromManifest(manifest)...)
	}
	if im.verbose {
		fmt.Printf("  Extracted %d image(s) from rendered chart templates\n", len(images))
	}
	return DeduplicateImages(images), nil
}

// deepMergeValues merges override into base, recursing into nested maps.
// Override values take precedence; non-map values in override replace those in base.
// This mirrors how Helm merges values files so that image tags defined only in an
// override file are correctly combined with the repository defined in chart defaults.
func deepMergeValues(base, override map[string]interface{}) map[string]interface{} {
	result := make(map[string]interface{}, len(base))
	for k, v := range base {
		result[k] = v
	}
	for k, overrideVal := range override {
		if baseVal, exists := result[k]; exists {
			baseMap, baseIsMap := baseVal.(map[string]interface{})
			overrideMap, overrideIsMap := overrideVal.(map[string]interface{})
			if baseIsMap && overrideIsMap {
				result[k] = deepMergeValues(baseMap, overrideMap)
				continue
			}
		}
		result[k] = overrideVal
	}
	return result
}

func (im *ImageManager) extractImagesFromLocalChart(svc *config.ServiceConfig) []string {
	// Start with chart's default values.yaml as the base, then deep-merge each
	// override file on top — exactly as Helm does. This ensures that a tag defined
	// only in an override file is paired with the repository from the chart defaults.
	merged := make(map[string]interface{})

	isArchive := strings.HasSuffix(svc.Path, ".tgz") || strings.HasSuffix(svc.Path, ".tar.gz")

	if isArchive {
		// Packed chart archive (.tgz/.tar.gz) — use Helm's v2 loader to get default values
		archChart, err := v2loader.Load(svc.Path)
		if err != nil {
			if im.verbose {
				fmt.Printf("Warning: Failed to load chart archive %s: %v\n", svc.Path, err)
			}
		} else if archChart.Values != nil {
			merged = archChart.Values
		}
	} else {
		// Directory-based chart — read values.yaml directly
		defaultValuesPath := filepath.Join(svc.Path, "values.yaml")
		if _, err := os.Stat(defaultValuesPath); err == nil {
			data, err := os.ReadFile(defaultValuesPath)
			if err != nil {
				if im.verbose {
					fmt.Printf("Warning: Failed to read default values: %v\n", err)
				}
			} else {
				var base map[string]interface{}
				if err := yaml.Unmarshal(data, &base); err != nil {
					if im.verbose {
						fmt.Printf("Warning: Failed to parse default values: %v\n", err)
					}
				} else if base != nil {
					merged = base
				}
			}
		}
	}

	if !svc.Values.IsEmpty() {
		for _, valuesFile := range svc.Values.Files() {
			data, err := os.ReadFile(valuesFile)
			if err != nil {
				if im.verbose {
					fmt.Printf("Warning: Failed to read values file %s: %v\n", valuesFile, err)
				}
				continue
			}
			var override map[string]interface{}
			if err := yaml.Unmarshal(data, &override); err != nil {
				if im.verbose {
					fmt.Printf("Warning: Failed to parse values file %s: %v\n", valuesFile, err)
				}
				continue
			}
			if override != nil {
				merged = deepMergeValues(merged, override)
			}
		}
	}

	images := make([]string, 0)
	im.extractImagesRecursive(merged, &images)
	return images
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

// extractImagesFromManifest extracts image references from a Kubernetes YAML
// (or JSON) manifest, multi-document or not: every string value of an
// "image" key (containers, and custom resources that run an image) and every
// image volume reference. The documents are parsed rather than matched line by
// line, which missed `image: x  # comment`, flow style and JSON, and read a
// folded scalar's ">-" as an image. A document that doesn't parse is skipped.
func (im *ImageManager) extractImagesFromManifest(manifest string) []string {
	images := make([]string, 0)
	reader := k8syaml.NewYAMLReader(bufio.NewReader(strings.NewReader(manifest)))
	for {
		doc, err := reader.Read()
		if err != nil {
			break // EOF, or a document too malformed to delimit
		}
		var obj interface{}
		if err := yaml.Unmarshal(doc, &obj); err != nil {
			continue
		}
		extractImageFieldsRecursive(obj, &images)
	}
	return DeduplicateImages(images)
}

// extractImageFieldsRecursive collects the string values of "image" keys and
// the references of image volumes (image: {reference: ...})
func extractImageFieldsRecursive(node interface{}, images *[]string) {
	switch v := node.(type) {
	case map[string]interface{}:
		for key, val := range v {
			if key == "image" {
				switch image := val.(type) {
				case string:
					if image = strings.TrimSpace(image); image != "" {
						*images = append(*images, image)
					}
					continue
				case map[string]interface{}:
					if ref, ok := image["reference"].(string); ok && ref != "" {
						*images = append(*images, ref)
						continue
					}
				}
			}
			extractImageFieldsRecursive(val, images)
		}
	case []interface{}:
		for _, item := range v {
			extractImageFieldsRecursive(item, images)
		}
	}
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
		// The rendered chart is authoritative
		rendered, err := im.renderedHelmImages(ctx, svc)
		if err != nil {
			if im.verbose {
				fmt.Printf("Warning: Failed to render chart for '%s' to find its images: %v\n", svc.Name, err)
			}
		} else {
			images = append(images, rendered...)
		}

		// Images named in values are added too: a chart may hand one to
		// something other than a pod spec (an operator's custom resource),
		// and they are all there is when rendering fails. A guess that names
		// no local image is skipped when images are loaded.
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
		if svc.IsLocalChart() {
			images = append(images, im.extractImagesFromLocalChart(svc)...)
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

	// Merge any images explicitly listed in the service config. These supplement
	// the auto-detected set and handle cases where images are referenced in
	// non-standard locations (e.g., extraInitContainers YAML strings) that the
	// automatic extraction cannot reach.
	images = append(images, svc.Images...)

	// Deduplicate
	images = DeduplicateImages(images)

	return images, nil
}
