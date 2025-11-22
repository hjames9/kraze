package providers

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"crypto/sha256"

	"github.com/hjames9/kraze/internal/color"
	"github.com/hjames9/kraze/internal/config"
	yamlv3 "gopkg.in/yaml.v3"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/restmapper"
)

const (
	managedByLabel = "app.kubernetes.io/managed-by"
	serviceLabel   = "kraze.service"
)

// ManifestsProvider implements the Provider interface for raw Kubernetes manifests
type ManifestsProvider struct {
	opts          *ProviderOptions
	dynamicClient dynamic.Interface
	clientset     *kubernetes.Clientset
	mapper        *restmapper.DeferredDiscoveryRESTMapper
}

// NewManifestsProvider creates a new Manifests provider
func NewManifestsProvider(opts *ProviderOptions) (*ManifestsProvider, error) {
	// Create REST config
	restConfig, err := getRESTConfigFromKubeconfig(opts.KubeConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to get REST config: %w", err)
	}

	// Create dynamic client
	dynamicClient, err := dynamic.NewForConfig(restConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to create dynamic client: %w", err)
	}

	// Create clientset for fetching events and logs
	clientset, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to create clientset: %w", err)
	}

	// Create discovery client for REST mapper
	discoveryClient, err := discovery.NewDiscoveryClientForConfig(restConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to create discovery client: %w", err)
	}

	// Wrap in a cached discovery client
	cachedDiscoveryClient := memory.NewMemCacheClient(discoveryClient)

	// Create REST mapper for GVK to GVR mapping
	mapper := restmapper.NewDeferredDiscoveryRESTMapper(cachedDiscoveryClient)

	return &ManifestsProvider{
		opts:          opts,
		dynamicClient: dynamicClient,
		clientset:     clientset,
		mapper:        mapper,
	}, nil
}

// Install applies Kubernetes manifests
func (manifest *ManifestsProvider) Install(ctx context.Context, service *config.ServiceConfig) error {
	// Create namespace if it doesn't exist and should be created
	if service.ShouldCreateNamespace() {
		if err := manifest.ensureNamespace(ctx, service.GetNamespace()); err != nil {
			return fmt.Errorf("failed to ensure namespace: %w", err)
		}
	}

	// Load manifest files
	manifests, err := manifest.loadManifests(service)
	if err != nil {
		return fmt.Errorf("failed to load manifests: %w", err)
	}

	if len(manifests) == 0 {
		return fmt.Errorf("no manifests found")
	}

	fmt.Printf("Applying %d manifest(s) for service '%s'...\n", len(manifests), service.Name)

	// Track applied resources with their fully resolved state (including namespace)
	var appliedObjects []*unstructured.Unstructured

	// Apply each manifest
	for itr, manifestContent := range manifests {
		// Parse manifest
		obj, err := manifest.parseManifest(manifestContent)
		if err != nil {
			return fmt.Errorf("failed to parse manifest %d: %w", itr+1, err)
		}

		// Skip empty objects
		if obj == nil {
			continue
		}

		// Add tracking labels
		manifest.addTrackingLabels(obj, service)

		// Set namespace if not specified and resource is namespaced
		if obj.GetNamespace() == "" && manifest.isNamespacedResource(obj) {
			obj.SetNamespace(service.GetNamespace())
		}

		// Apply the resource
		if err := manifest.applyResource(ctx, obj); err != nil {
			return fmt.Errorf("failed to apply manifest %d (%s/%s): %w",
				itr+1, obj.GetKind(), obj.GetName(), err)
		}

		if manifest.opts.Verbose {
			fmt.Printf("  %s Applied %s/%s\n", color.Checkmark(), obj.GetKind(), obj.GetName())
		}

		// Track the fully resolved object for waiting
		appliedObjects = append(appliedObjects, obj.DeepCopy())
	}

	fmt.Printf("%s Manifests applied successfully for '%s'\n", color.Checkmark(), service.Name)

	// Inject config checksums to force rollouts when ConfigMaps/Secrets change
	checksum, err := calculateConfigChecksumFromObjects(appliedObjects)
	if err != nil {
		if manifest.opts.Verbose {
			fmt.Printf("Warning: failed to calculate config checksum: %v\n", err)
		}
	} else if checksum != "" {
		if err := manifest.injectConfigChecksumsToObjects(ctx, appliedObjects, checksum); err != nil {
			if manifest.opts.Verbose {
				fmt.Printf("Warning: failed to inject config checksums: %v\n", err)
			}
		}
	}

	// Wait for resources to be ready using shared wait logic
	if manifest.opts.Wait {
		if err := manifest.waitForAppliedResources(ctx, appliedObjects); err != nil {
			return fmt.Errorf("failed waiting for resources: %w", err)
		}
	}

	return nil
}

// Uninstall removes Kubernetes resources
func (manifest *ManifestsProvider) Uninstall(ctx context.Context, service *config.ServiceConfig) error {
	fmt.Printf("Deleting resources for service '%s'...\n", service.Name)

	// Load manifests to get resource info
	manifests, err := manifest.loadManifests(service)
	if err != nil {
		return fmt.Errorf("failed to load manifests: %w", err)
	}

	// Delete each resource
	deletedCount := 0
	for itr, manifestContent := range manifests {
		obj, err := manifest.parseManifest(manifestContent)
		if err != nil {
			// Log warning but continue
			fmt.Printf("  Warning: failed to parse manifest %d: %v\n", itr+1, err)
			continue
		}

		if obj == nil {
			continue
		}

		// Set namespace if not specified and resource is namespaced
		if obj.GetNamespace() == "" && manifest.isNamespacedResource(obj) {
			obj.SetNamespace(service.GetNamespace())
		}

		// Delete the resource
		if err := manifest.deleteResource(ctx, obj); err != nil {
			fmt.Printf("  Warning: failed to delete %s/%s: %v\n",
				obj.GetKind(), obj.GetName(), err)
			continue
		}

		if manifest.opts.Verbose {
			fmt.Printf("  %s Deleted %s/%s\n", color.Checkmark(), obj.GetKind(), obj.GetName())
		}
		deletedCount++
	}

	fmt.Printf("%s Deleted %d resource(s) for '%s'\n", color.Checkmark(), deletedCount, service.Name)
	return nil
}

// Status returns the status of manifests
func (manifest *ManifestsProvider) Status(ctx context.Context, service *config.ServiceConfig) (*ServiceStatus, error) {
	manifests, err := manifest.loadManifests(service)
	if err != nil {
		return &ServiceStatus{
			Name:      service.Name,
			Installed: false,
			Ready:     false,
			Message:   fmt.Sprintf("Failed to load manifests: %v", err),
		}, nil
	}

	if len(manifests) == 0 {
		return &ServiceStatus{
			Name:      service.Name,
			Installed: false,
			Ready:     false,
			Message:   "No manifests found",
		}, nil
	}

	// Check if resources exist
	existCount := 0
	for _, manifestContent := range manifests {
		obj, err := manifest.parseManifest(manifestContent)
		if err != nil || obj == nil {
			continue
		}

		if obj.GetNamespace() == "" && manifest.isNamespacedResource(obj) {
			obj.SetNamespace(service.GetNamespace())
		}

		exists, _ := manifest.resourceExists(ctx, obj)
		if exists {
			existCount++
		}
	}

	installed := existCount > 0
	ready := existCount == len(manifests)

	return &ServiceStatus{
		Name:      service.Name,
		Installed: installed,
		Ready:     ready,
		Message:   fmt.Sprintf("%d/%d resources exist", existCount, len(manifests)),
	}, nil
}

// IsInstalled checks if manifests are installed
func (manifest *ManifestsProvider) IsInstalled(ctx context.Context, service *config.ServiceConfig) (bool, error) {
	status, err := manifest.Status(ctx, service)
	if err != nil {
		return false, err
	}
	return status.Installed, nil
}

// loadManifests loads manifest files and returns their contents
func (manifest *ManifestsProvider) loadManifests(service *config.ServiceConfig) ([]string, error) {
	var files []string

	// Collect file paths
	if len(service.Paths) > 0 {
		// Multiple files specified
		files = service.Paths
	} else if service.Path != "" {
		// Check if path is a URL
		if config.IsHTTPURL(service.Path) {
			// Remote manifest - download and process directly
			if manifest.opts.Verbose {
				fmt.Printf("Downloading manifest from %s...\n", service.Path)
			}
			content, err := manifest.downloadManifest(service.Path)
			if err != nil {
				return nil, fmt.Errorf("failed to download manifest from %s: %w", service.Path, err)
			}
			// Split multi-document YAML and return
			return manifest.splitYAML(content), nil
		}

		// Single path specified (file or directory)
		info, err := os.Stat(service.Path)
		if err != nil {
			return nil, fmt.Errorf("failed to stat path %s: %w", service.Path, err)
		}

		if info.IsDir() {
			// Load all YAML files in directory
			entries, err := os.ReadDir(service.Path)
			if err != nil {
				return nil, fmt.Errorf("failed to read directory %s: %w", service.Path, err)
			}

			for _, entry := range entries {
				if entry.IsDir() {
					continue
				}
				name := entry.Name()
				if strings.HasSuffix(name, ".yaml") || strings.HasSuffix(name, ".yml") {
					files = append(files, filepath.Join(service.Path, name))
				}
			}
		} else {
			// Single file
			files = []string{service.Path}
		}
	} else {
		return nil, fmt.Errorf("no manifest paths specified")
	}

	// Read and split manifests
	var manifests []string
	for _, file := range files {
		content, err := os.ReadFile(file)
		if err != nil {
			return nil, fmt.Errorf("failed to read file %s: %w", file, err)
		}

		// Split multi-document YAML
		docs := manifest.splitYAML(string(content))
		manifests = append(manifests, docs...)
	}

	return manifests, nil
}

// splitYAML splits multi-document YAML by --- separator
func (manifest *ManifestsProvider) splitYAML(content string) []string {
	var docs []string
	scanner := bufio.NewScanner(strings.NewReader(content))
	var currentDoc strings.Builder

	for scanner.Scan() {
		line := scanner.Text()

		// Check for document separator
		if strings.TrimSpace(line) == "---" {
			if currentDoc.Len() > 0 {
				docs = append(docs, currentDoc.String())
				currentDoc.Reset()
			}
			continue
		}

		currentDoc.WriteString(line)
		currentDoc.WriteString("\n")
	}

	// Add last document
	if currentDoc.Len() > 0 {
		docs = append(docs, currentDoc.String())
	}

	return docs
}

// downloadManifest downloads a manifest from a remote URL
func (manifest *ManifestsProvider) downloadManifest(url string) (string, error) {
	resp, err := http.Get(url)
	if err != nil {
		return "", fmt.Errorf("failed to fetch URL: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %d: %s", resp.StatusCode, resp.Status)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("failed to read response body: %w", err)
	}

	return string(body), nil
}

// parseManifest parses a YAML manifest into an unstructured object
func (manifest *ManifestsProvider) parseManifest(content string) (*unstructured.Unstructured, error) {
	// Trim whitespace
	content = strings.TrimSpace(content)
	if content == "" {
		return nil, nil
	}

	decoder := yaml.NewYAMLOrJSONDecoder(bytes.NewReader([]byte(content)), 4096)
	obj := &unstructured.Unstructured{}

	if err := decoder.Decode(obj); err != nil {
		if err == io.EOF {
			return nil, nil
		}
		return nil, fmt.Errorf("failed to decode YAML: %w", err)
	}

	// Skip empty objects
	if obj.Object == nil || len(obj.Object) == 0 {
		return nil, nil
	}

	return obj, nil
}

// addTrackingLabels adds kraze labels to a resource for tracking
func (manifest *ManifestsProvider) addTrackingLabels(obj *unstructured.Unstructured, service *config.ServiceConfig) {
	labels := obj.GetLabels()
	if labels == nil {
		labels = make(map[string]string)
	}

	labels[managedByLabel] = "kraze"
	labels[serviceLabel] = service.Name

	obj.SetLabels(labels)
}

// applyResource applies a resource using the dynamic client
func (manifest *ManifestsProvider) applyResource(ctx context.Context, obj *unstructured.Unstructured) error {
	gvr, err := manifest.getGVR(obj)
	if err != nil {
		return err
	}

	namespace := obj.GetNamespace()
	name := obj.GetName()

	if manifest.opts.Verbose {
		gvk := obj.GroupVersionKind()
		fmt.Printf("  Applying %s/%s (GVK: %s/%s/%s -> GVR: %s/%s/%s)\n",
			obj.GetKind(), obj.GetName(),
			gvk.Group, gvk.Version, gvk.Kind,
			gvr.Group, gvr.Version, gvr.Resource)
	}

	var client dynamic.ResourceInterface
	if namespace != "" {
		client = manifest.dynamicClient.Resource(gvr).Namespace(namespace)
	} else {
		client = manifest.dynamicClient.Resource(gvr)
	}

	// Try to get existing resource
	existing, err := client.Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		// Only create if resource doesn't exist; return other errors
		if errors.IsNotFound(err) {
			_, createErr := client.Create(ctx, obj, metav1.CreateOptions{})
			if createErr != nil {
				// Return error with context
				return fmt.Errorf("failed to create resource: %w", createErr)
			}
			return nil
		}
		// Return other errors (e.g., permission denied, API not available)
		return fmt.Errorf("failed to check if resource exists: %w", err)
	}

	// Resource exists, update it
	// Preserve the resourceVersion for optimistic concurrency control
	obj.SetResourceVersion(existing.GetResourceVersion())
	_, err = client.Update(ctx, obj, metav1.UpdateOptions{})
	return err
}

// deleteResource deletes a resource using the dynamic client
func (manifest *ManifestsProvider) deleteResource(ctx context.Context, obj *unstructured.Unstructured) error {
	gvr, err := manifest.getGVR(obj)
	if err != nil {
		return err
	}

	namespace := obj.GetNamespace()
	name := obj.GetName()

	var client dynamic.ResourceInterface
	if namespace != "" {
		client = manifest.dynamicClient.Resource(gvr).Namespace(namespace)
	} else {
		client = manifest.dynamicClient.Resource(gvr)
	}

	return client.Delete(ctx, name, metav1.DeleteOptions{})
}

// resourceExists checks if a resource exists
func (manifest *ManifestsProvider) resourceExists(ctx context.Context, obj *unstructured.Unstructured) (bool, error) {
	gvr, err := manifest.getGVR(obj)
	if err != nil {
		return false, err
	}

	namespace := obj.GetNamespace()
	name := obj.GetName()

	var client dynamic.ResourceInterface
	if namespace != "" {
		client = manifest.dynamicClient.Resource(gvr).Namespace(namespace)
	} else {
		client = manifest.dynamicClient.Resource(gvr)
	}

	_, err = client.Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return false, nil
	}

	return true, nil
}

// isNamespacedResource checks if a resource is namespaced (not cluster-scoped)
func (manifest *ManifestsProvider) isNamespacedResource(obj *unstructured.Unstructured) bool {
	gvk := obj.GroupVersionKind()

	// Use REST mapper to determine if resource is namespaced
	mapping, err := manifest.mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
	if err != nil {
		// If REST mapper fails, use a list of known cluster-scoped resources
		clusterScopedKinds := map[string]bool{
			"Namespace":                      true,
			"Node":                           true,
			"PersistentVolume":               true,
			"ClusterRole":                    true,
			"ClusterRoleBinding":             true,
			"CustomResourceDefinition":       true,
			"StorageClass":                   true,
			"VolumeAttachment":               true,
			"APIService":                     true,
			"MutatingWebhookConfiguration":   true,
			"ValidatingWebhookConfiguration": true,
			"PriorityClass":                  true,
			"RuntimeClass":                   true,
			"CSIDriver":                      true,
			"CSINode":                        true,
			"IngressClass":                   true,
		}
		return !clusterScopedKinds[gvk.Kind]
	}

	return mapping.Scope.Name() == "namespace"
}

// getGVR returns the GroupVersionResource for an object
func (manifest *ManifestsProvider) getGVR(obj *unstructured.Unstructured) (schema.GroupVersionResource, error) {
	gvk := obj.GroupVersionKind()

	// Use REST mapper to properly map GVK to GVR
	mapping, err := manifest.mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
	if err != nil {
		// Fallback to simple pluralization if discovery fails
		resource := strings.ToLower(gvk.Kind)
		if !strings.HasSuffix(resource, "s") {
			resource = resource + "s"
		}
		return schema.GroupVersionResource{
			Group:    gvk.Group,
			Version:  gvk.Version,
			Resource: resource,
		}, nil
	}

	return mapping.Resource, nil
}

// waitForAppliedResources waits for already-parsed and applied resources to be ready
func (manifest *ManifestsProvider) waitForAppliedResources(ctx context.Context, resources []*unstructured.Unstructured) error {
	// Parse timeout
	timeout := 10 * time.Minute // default
	if manifest.opts.Timeout != "" {
		parsedTimeout, err := time.ParseDuration(manifest.opts.Timeout)
		if err == nil {
			timeout = parsedTimeout
		}
	}

	fmt.Printf("Waiting for resources to be ready (timeout: %v)...\n", timeout)

	// Create context with timeout
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// Wait for each resource
	for _, obj := range resources {
		kind := obj.GetKind()
		name := obj.GetName()

		// Only wait for resources that have a meaningful ready state
		if !shouldWaitForResource(kind) {
			if manifest.opts.Verbose {
				fmt.Printf("  Skipping wait for %s/%s (not a waitable resource)\n", kind, name)
			}
			continue
		}

		fmt.Printf("  Waiting for %s/%s to be ready...\n", kind, name)

		if err := waitForResourceReady(waitCtx, manifest.dynamicClient, manifest.clientset, manifest.mapper, obj, manifest.opts.Verbose); err != nil {
			if waitCtx.Err() == context.DeadlineExceeded {
				return fmt.Errorf("timeout waiting for %s/%s to be ready", kind, name)
			}
			return fmt.Errorf("error waiting for %s/%s: %w", kind, name, err)
		}

		fmt.Printf("  %s %s/%s is ready\n", color.Checkmark(), kind, name)
	}

	fmt.Printf("%s All resources are ready\n", color.Checkmark())
	return nil
}

// ensureNamespace creates a namespace if it doesn't exist
func (manifest *ManifestsProvider) ensureNamespace(ctx context.Context, namespace string) error {
	// Don't create default namespace or system namespaces
	if namespace == "default" || namespace == "kube-system" || namespace == "kube-public" || namespace == "kube-node-lease" {
		return nil
	}

	// Create namespace object
	ns := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "v1",
			"kind":       "Namespace",
			"metadata": map[string]interface{}{
				"name": namespace,
			},
		},
	}

	// Get namespace GVR
	gvr := schema.GroupVersionResource{
		Group:    "",
		Version:  "v1",
		Resource: "namespaces",
	}

	// Check if namespace exists
	client := manifest.dynamicClient.Resource(gvr)
	_, err := client.Get(ctx, namespace, metav1.GetOptions{})
	if err == nil {
		// Namespace already exists
		return nil
	}

	if !errors.IsNotFound(err) {
		// Some other error occurred
		return fmt.Errorf("failed to check namespace: %w", err)
	}

	// Namespace doesn't exist, create it
	if manifest.opts.Verbose {
		fmt.Printf("Creating namespace '%s'...\n", namespace)
	}
	_, err = client.Create(ctx, ns, metav1.CreateOptions{})
	if err != nil && !errors.IsAlreadyExists(err) {
		return fmt.Errorf("failed to create namespace: %w", err)
	}

	return nil
}

// calculateConfigChecksumFromObjects calculates a checksum of all ConfigMaps and Secrets
// in a list of unstructured objects
func calculateConfigChecksumFromObjects(objects []*unstructured.Unstructured) (string, error) {
	var configData []string

	for _, obj := range objects {
		kind := obj.GetKind()

		// Only process ConfigMaps and Secrets
		if kind != "ConfigMap" && kind != "Secret" {
			continue
		}

		// Extract data field
		data, found, err := unstructured.NestedMap(obj.Object, "data")
		if err == nil && found && data != nil {
			// Convert to YAML string for consistent hashing
			dataBytes, err := yamlv3.Marshal(data)
			if err == nil {
				configData = append(configData, string(dataBytes))
			}
		}

		// For Secrets, also check stringData
		if kind == "Secret" {
			stringData, found, err := unstructured.NestedMap(obj.Object, "stringData")
			if err == nil && found && stringData != nil {
				stringDataBytes, err := yamlv3.Marshal(stringData)
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

// injectConfigChecksumsToObjects patches Deployments, StatefulSets, and DaemonSets
// with config checksum annotations to force rollouts when ConfigMaps or Secrets change
func (manifest *ManifestsProvider) injectConfigChecksumsToObjects(ctx context.Context, objects []*unstructured.Unstructured, checksum string) error {
	if checksum == "" {
		return nil
	}

	for _, obj := range objects {
		kind := obj.GetKind()

		// Only process workload resources that have Pod templates
		if kind != "Deployment" && kind != "StatefulSet" && kind != "DaemonSet" {
			continue
		}

		name := obj.GetName()
		namespace := obj.GetNamespace()

		if name == "" || namespace == "" {
			continue
		}

		// Use shared patching function
		patchWorkloadWithConfigChecksum(ctx, manifest.dynamicClient, manifest.mapper, kind, name, namespace, checksum, manifest.opts.Verbose)
	}

	return nil
}
