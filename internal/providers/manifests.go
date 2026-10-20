package providers

import (
	"bytes"
	"context"
	stderrors "errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"crypto/sha256"

	"github.com/hjames9/kraze/internal/color"
	"github.com/hjames9/kraze/internal/config"
	"github.com/hjames9/kraze/internal/state"
	yamlv3 "go.yaml.in/yaml/v3"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/wait"
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
	projectLabel   = "kraze.project"

	// helmReleaseAnnotation is set by Helm on every object a release owns
	helmReleaseAnnotation = "meta.helm.sh/release-name"
)

// ManifestsProvider implements the Provider interface for raw Kubernetes manifests
type ManifestsProvider struct {
	opts          *ProviderOptions
	dynamicClient dynamic.Interface
	clientset     kubernetes.Interface
	mapper        meta.RESTMapper
	// completedJobs holds the Jobs (jobKey) seen succeeding while Install
	// waited; ResourceRefs records them as Completed
	completedJobs map[string]bool
}

// jobKey identifies a Job by namespace and name
func jobKey(namespace, name string) string {
	return namespace + "/" + name
}

// ErrOwnershipConflict is returned by Install when objects belong to another
// service; nothing has been applied
var ErrOwnershipConflict = stderrors.New("ownership conflict")

// NewManifestsProvider creates a new Manifests provider
func NewManifestsProvider(opts *ProviderOptions) (*ManifestsProvider, error) {
	// Without a project, applied objects would carry an empty kraze.project
	// label, which the ownership check reads as an older kraze's object
	if opts.Project == "" {
		return nil, fmt.Errorf("manifests provider needs a project (ProviderOptions.Project is empty)")
	}

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

	// Parse every manifest before applying any, so that an ownership
	// conflict leaves the cluster untouched
	objects, err := manifest.parseServiceObjects(manifests, service)
	if err != nil {
		return err
	}
	if err := manifest.checkOwnership(ctx, objects, service.Name); err != nil {
		return err
	}

	if !manifest.opts.Quiet {
		fmt.Printf("Applying %d manifest(s) for service '%s'...\n", len(manifests), service.Name)
	}

	// Track applied resources with their fully resolved state (including namespace)
	var appliedObjects []*unstructured.Unstructured
	apply := func(objs []*unstructured.Unstructured) error {
		for _, obj := range objs {
			manifest.addTrackingLabels(obj, service)
			if err := manifest.applyResource(ctx, obj); err != nil {
				return fmt.Errorf("failed to apply %s: %w", describeObject(obj), err)
			}
			if manifest.opts.Verbose {
				fmt.Printf("  %s Applied %s/%s\n", color.Checkmark(), obj.GetKind(), obj.GetName())
			}
			// Track the fully resolved object for waiting
			appliedObjects = append(appliedObjects, obj.DeepCopy())
		}
		return nil
	}

	// Custom resources can't be created until the API server serves their
	// type, so a service that ships CRDs applies them first and waits for
	// them to be established
	crds, others := splitCRDs(objects)
	if len(crds) > 0 {
		if err := apply(crds); err != nil {
			return err
		}
		if err := manifest.waitForCRDsEstablished(ctx, crds); err != nil {
			return err
		}
		manifest.resetMapper()

		// Parse again: the namespace of a custom resource was defaulted before
		// the mapper knew whether its kind is namespaced
		if objects, err = manifest.parseServiceObjects(manifests, service); err != nil {
			return err
		}
		_, others = splitCRDs(objects)
	}
	// Annotate workloads with a checksum of the service's ConfigMaps and
	// Secrets before applying them, so a config change rolls them out once
	// and an unchanged config leaves the pod template as it was
	if err := annotateConfigChecksum(objects); err != nil && manifest.opts.Verbose {
		fmt.Printf("Warning: failed to calculate config checksum: %v\n", err)
	}
	if err := apply(others); err != nil {
		return err
	}

	if !manifest.opts.Quiet {
		fmt.Printf("%s Manifests applied successfully for '%s'\n", color.Checkmark(), service.Name)
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
	if !manifest.opts.Quiet {
		fmt.Printf("Deleting resources for service '%s'...\n", service.Name)
	}

	// Load manifests to get resource info
	manifests, err := manifest.loadManifests(service)
	if err != nil {
		return fmt.Errorf("failed to load manifests: %w", err)
	}

	var objects []*unstructured.Unstructured
	for itr, manifestContent := range manifests {
		obj, err := manifest.parseManifest(manifestContent)
		if err != nil {
			// Log warning but continue
			if !manifest.opts.Quiet {
				fmt.Printf("  Warning: failed to parse manifest %d: %v\n", itr+1, err)
			}
			continue
		}

		if obj == nil {
			continue
		}

		// Set namespace if not specified and resource is namespaced
		if obj.GetNamespace() == "" && manifest.isNamespacedResource(obj) {
			obj.SetNamespace(service.GetNamespace())
		}
		objects = append(objects, obj)
	}

	// Delete CRDs last: deleting one removes its custom resources, which
	// would then fail to delete
	crds, others := splitCRDs(objects)

	// Delete each resource
	deletedCount := 0
	for _, obj := range append(others, crds...) {
		// Never delete an object another service has taken over
		if owner, _ := manifest.existingOwner(ctx, obj, service.Name); owner != "" {
			if !manifest.opts.Quiet {
				fmt.Printf("  Skipping %s: owned by %s\n", describeObject(obj), owner)
			}
			continue
		}

		// Delete the resource
		if err := manifest.deleteResource(ctx, obj); err != nil {
			if !manifest.opts.Quiet {
				fmt.Printf("  Warning: failed to delete %s/%s: %v\n",
					obj.GetKind(), obj.GetName(), err)
			}
			continue
		}

		if manifest.opts.Verbose {
			fmt.Printf("  %s Deleted %s/%s\n", color.Checkmark(), obj.GetKind(), obj.GetName())
		}
		deletedCount++
	}

	if !manifest.opts.Quiet {
		fmt.Printf("%s Deleted %d resource(s) for '%s'\n", color.Checkmark(), deletedCount, service.Name)
	}
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

	// Jobs the last `up` saw complete
	completed := map[string]bool{}
	for _, ref := range manifest.opts.Recorded {
		if ref.Kind == "Job" && ref.Completed {
			completed[jobKey(ref.Namespace, ref.Name)] = true
		}
	}

	// Check if resources exist. A document without an object (only comments,
	// like the license header of many upstream install files) is not a
	// resource; counting it made a service never ready ("12/13 resources
	// exist"). One that doesn't parse counts as missing.
	existCount, total, finishedJobs := 0, 0, 0
	for _, manifestContent := range manifests {
		obj, err := manifest.parseManifest(manifestContent)
		if err == nil && obj == nil {
			continue
		}
		total++
		if err != nil {
			continue
		}

		if obj.GetNamespace() == "" && manifest.isNamespacedResource(obj) {
			obj.SetNamespace(service.GetNamespace())
		}

		exists, _ := manifest.resourceExists(ctx, obj)
		if exists {
			existCount++
		} else if isTTLJob(obj) && completed[jobKey(obj.GetNamespace(), obj.GetName())] {
			// Deleted by its TTL after `up` saw it complete: expected
			existCount++
			finishedJobs++
		}
	}

	installed := existCount > 0
	ready := existCount == total

	return &ServiceStatus{
		Name:      service.Name,
		Installed: installed,
		Ready:     ready,
		Message:   statusMessage(existCount, total, finishedJobs),
	}, nil
}

// isTTLJob reports whether obj is a Job that Kubernetes deletes some time after
// it finishes (spec.ttlSecondsAfterFinished)
func isTTLJob(obj *unstructured.Unstructured) bool {
	if obj.GetKind() != "Job" {
		return false
	}
	_, found, _ := unstructured.NestedFieldNoCopy(obj.Object, "spec", "ttlSecondsAfterFinished")
	return found
}

// statusMessage summarises a manifests service's objects for `kraze status`
func statusMessage(exist, total, finishedJobs int) string {
	message := fmt.Sprintf("%d/%d resources exist", exist, total)
	switch finishedJobs {
	case 0:
	case 1:
		message += " (1 job finished)"
	default:
		message += fmt.Sprintf(" (%d jobs finished)", finishedJobs)
	}
	return message
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
			docs, err := splitYAMLDocuments(content)
			if err != nil {
				return nil, fmt.Errorf("failed to split manifest from %s: %w", service.Path, err)
			}
			return docs, nil
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
		docs, err := splitYAMLDocuments(string(content))
		if err != nil {
			return nil, fmt.Errorf("failed to split %s: %w", file, err)
		}
		manifests = append(manifests, docs...)
	}

	return manifests, nil
}

// downloadManifest downloads a manifest from a remote URL
func (manifest *ManifestsProvider) downloadManifest(url string) (string, error) {
	resp, err := http.Get(url)
	if err != nil {
		return "", fmt.Errorf("failed to fetch URL: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

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
	if len(obj.Object) == 0 {
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
	if manifest.opts != nil && manifest.opts.Project != "" {
		labels[projectLabel] = manifest.opts.Project
	}

	obj.SetLabels(labels)
}

// crdEstablishTimeout is how long Install waits for a service's CRDs to be
// served before applying the rest of it
var crdEstablishTimeout = time.Minute

// isCRD reports whether apiVersion/kind is a CustomResourceDefinition
func isCRD(apiVersion, kind string) bool {
	return kind == "CustomResourceDefinition" && strings.HasPrefix(apiVersion, "apiextensions.k8s.io/")
}

// splitCRDs separates CustomResourceDefinitions from the other objects,
// keeping the order of each
func splitCRDs(objects []*unstructured.Unstructured) (crds, others []*unstructured.Unstructured) {
	for _, obj := range objects {
		if isCRD(obj.GetAPIVersion(), obj.GetKind()) {
			crds = append(crds, obj)
		} else {
			others = append(others, obj)
		}
	}
	return crds, others
}

// waitForCRDsEstablished waits until the API server reports every CRD as
// Established, meaning it serves the CRD's custom resources
func (manifest *ManifestsProvider) waitForCRDsEstablished(ctx context.Context, crds []*unstructured.Unstructured) error {
	for _, crd := range crds {
		var lastErr error
		err := wait.PollUntilContextTimeout(ctx, 250*time.Millisecond, crdEstablishTimeout, true, func(ctx context.Context) (bool, error) {
			current, err := manifest.getExisting(ctx, crd)
			if err != nil || current == nil {
				lastErr = err
				return false, nil
			}
			conditions, _, _ := unstructured.NestedSlice(current.Object, "status", "conditions")
			for _, c := range conditions {
				cond, ok := c.(map[string]interface{})
				if ok && cond["type"] == "Established" && cond["status"] == "True" {
					return true, nil
				}
			}
			return false, nil
		})
		if err != nil {
			if lastErr != nil {
				err = lastErr
			}
			return fmt.Errorf("CRD %s was not established within %v: %w", crd.GetName(), crdEstablishTimeout, err)
		}
		if manifest.opts.Verbose {
			fmt.Printf("  %s CRD %s established\n", color.Checkmark(), crd.GetName())
		}
	}
	return nil
}

// resetMapper drops cached API discovery, so kinds from CRDs applied since
// it was filled can be mapped
func (manifest *ManifestsProvider) resetMapper() {
	if resettable, ok := manifest.mapper.(meta.ResettableRESTMapper); ok {
		resettable.Reset()
	}
}

// parseServiceObjects parses a service's manifests into objects, skipping
// empty documents and defaulting the namespace of namespaced objects to the
// service's namespace
func (manifest *ManifestsProvider) parseServiceObjects(manifests []string, service *config.ServiceConfig) ([]*unstructured.Unstructured, error) {
	objects := make([]*unstructured.Unstructured, 0, len(manifests))
	for itr, manifestContent := range manifests {
		obj, err := manifest.parseManifest(manifestContent)
		if err != nil {
			return nil, fmt.Errorf("failed to parse manifest %d: %w", itr+1, err)
		}
		if obj == nil {
			continue
		}
		if obj.GetNamespace() == "" && manifest.isNamespacedResource(obj) {
			obj.SetNamespace(service.GetNamespace())
		}
		objects = append(objects, obj)
	}
	return objects, nil
}

// ResourceRefs returns the objects a service's manifests define, as they
// would be applied (namespace defaulted), so they can be removed later
// without the service's config
func (manifest *ManifestsProvider) ResourceRefs(service *config.ServiceConfig) ([]state.ResourceRef, error) {
	manifests, err := manifest.loadManifests(service)
	if err != nil {
		return nil, fmt.Errorf("failed to load manifests: %w", err)
	}
	objects, err := manifest.parseServiceObjects(manifests, service)
	if err != nil {
		return nil, err
	}
	refs := make([]state.ResourceRef, 0, len(objects))
	for _, obj := range objects {
		refs = append(refs, state.ResourceRef{
			APIVersion: obj.GetAPIVersion(),
			Kind:       obj.GetKind(),
			Namespace:  obj.GetNamespace(),
			Name:       obj.GetName(),
			Completed:  obj.GetKind() == "Job" && manifest.completedJobs[jobKey(obj.GetNamespace(), obj.GetName())],
		})
	}
	return refs, nil
}

// DeleteResources deletes the objects a service applied, recorded by
// ResourceRefs. It is used for services no longer in the config, so it only
// deletes objects still labeled as this project's service; anything else
// (taken over by another service, or replaced by hand) is left alone.
// Returns the number of objects deleted.
func (manifest *ManifestsProvider) DeleteResources(ctx context.Context, serviceName string, refs []state.ResourceRef) (int, error) {
	// Delete CRDs last, as Uninstall does
	ordered := make([]state.ResourceRef, 0, len(refs))
	var crdRefs []state.ResourceRef
	for _, ref := range refs {
		if isCRD(ref.APIVersion, ref.Kind) {
			crdRefs = append(crdRefs, ref)
		} else {
			ordered = append(ordered, ref)
		}
	}

	deleted := 0
	var failures []string
	for _, ref := range append(ordered, crdRefs...) {
		obj := &unstructured.Unstructured{}
		obj.SetAPIVersion(ref.APIVersion)
		obj.SetKind(ref.Kind)
		obj.SetNamespace(ref.Namespace)
		obj.SetName(ref.Name)

		existing, err := manifest.getExisting(ctx, obj)
		if err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", describeObject(obj), err))
			continue
		}
		if existing == nil {
			continue
		}
		labels := existing.GetLabels()
		if labels[managedByLabel] != "kraze" || labels[serviceLabel] != serviceName ||
			(labels[projectLabel] != "" && labels[projectLabel] != manifest.opts.Project) {
			if !manifest.opts.Quiet {
				fmt.Printf("  Skipping %s: no longer labeled as service '%s'\n", describeObject(obj), serviceName)
			}
			continue
		}
		if err := manifest.deleteResource(ctx, obj); err != nil && !errors.IsNotFound(err) {
			failures = append(failures, fmt.Sprintf("%s: %v", describeObject(obj), err))
			continue
		}
		deleted++
	}
	if len(failures) > 0 {
		return deleted, fmt.Errorf("failed to delete %d object(s):\n  %s", len(failures), strings.Join(failures, "\n  "))
	}
	return deleted, nil
}

// checkOwnership fails if any of the objects already exists and belongs to
// something other than this service: another kraze service (in this or
// another project) or a Helm release. Existing objects with no owner are
// adopted, as before.
func (manifest *ManifestsProvider) checkOwnership(ctx context.Context, objects []*unstructured.Unstructured, serviceName string) error {
	var conflicts, ownProject []string
	for _, obj := range objects {
		owner, sameProject := manifest.existingOwner(ctx, obj, serviceName)
		if owner == "" {
			continue
		}
		conflicts = append(conflicts, fmt.Sprintf("%s is owned by %s", describeObject(obj), owner))
		if sameProject && !slices.Contains(ownProject, owner) {
			ownProject = append(ownProject, owner)
		}
	}
	if len(conflicts) == 0 {
		return nil
	}
	hint := "rename the objects (or, for namespaced objects, move the service to another namespace)"
	if len(ownProject) > 0 {
		hint = fmt.Sprintf("if %s was renamed or removed from this config, `kraze up --remove-orphans` removes it first; otherwise %s",
			strings.Join(ownProject, ", "), hint)
	}
	return fmt.Errorf("%w: service '%s' would overwrite objects it doesn't own:\n  %s\n%s", ErrOwnershipConflict, serviceName, strings.Join(conflicts, "\n  "), hint)
}

// existingOwner returns who owns the existing copy of obj, if that isn't this
// service. It returns "" when the object doesn't exist, can't be read, or has
// no owner. Objects labeled by older kraze carry no project label; they count
// as this service's if the service label matches. sameProject reports an owner
// that is another service of this project.
func (manifest *ManifestsProvider) existingOwner(ctx context.Context, obj *unstructured.Unstructured, serviceName string) (owner string, sameProject bool) {
	existing, err := manifest.getExisting(ctx, obj)
	if err != nil || existing == nil {
		return "", false
	}
	if release := existing.GetAnnotations()[helmReleaseAnnotation]; release != "" {
		return fmt.Sprintf("Helm release '%s'", release), false
	}
	labels := existing.GetLabels()
	if labels[managedByLabel] != "kraze" {
		return "", false
	}
	service, project := labels[serviceLabel], labels[projectLabel]
	if service == serviceName && (project == "" || project == manifest.opts.Project) {
		return "", false
	}
	if project == "" {
		return fmt.Sprintf("kraze service '%s'", service), false
	}
	if project == manifest.opts.Project {
		return fmt.Sprintf("kraze service '%s'", service), true
	}
	return fmt.Sprintf("kraze service '%s' of project '%s'", service, project), false
}

// getExisting fetches the cluster's copy of obj, or nil if it doesn't exist
func (manifest *ManifestsProvider) getExisting(ctx context.Context, obj *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	gvr, err := manifest.getGVR(obj)
	if err != nil {
		return nil, err
	}
	var client dynamic.ResourceInterface
	if obj.GetNamespace() != "" {
		client = manifest.dynamicClient.Resource(gvr).Namespace(obj.GetNamespace())
	} else {
		client = manifest.dynamicClient.Resource(gvr)
	}
	existing, err := client.Get(ctx, obj.GetName(), metav1.GetOptions{})
	if err != nil {
		if errors.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	return existing, nil
}

// describeObject names an object as Kind/name or Kind/namespace/name
func describeObject(obj *unstructured.Unstructured) string {
	if obj.GetNamespace() == "" {
		return fmt.Sprintf("%s/%s", obj.GetKind(), obj.GetName())
	}
	return fmt.Sprintf("%s/%s/%s", obj.GetKind(), obj.GetNamespace(), obj.GetName())
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

	// PVCs are immutable once bound — skip update to avoid "spec is immutable" errors.
	// Storage resize is the only allowed mutation, but kraze does not manage that.
	if obj.GetKind() == "PersistentVolumeClaim" {
		return nil
	}

	// Resource exists — update it. Kubernetes controllers continuously update resource
	// status, so the resourceVersion can change between our Get and Update. Retry on
	// 409 Conflict by re-fetching the latest version each time.
	const maxUpdateRetries = 5
	latest := existing
	for attempt := range maxUpdateRetries {
		obj.SetResourceVersion(latest.GetResourceVersion())
		_, err = client.Update(ctx, obj, metav1.UpdateOptions{})
		if err == nil {
			return nil
		}
		if !errors.IsConflict(err) {
			return err
		}
		// Conflict: re-fetch and retry
		if attempt < maxUpdateRetries-1 {
			latest, err = client.Get(ctx, name, metav1.GetOptions{})
			if err != nil {
				return fmt.Errorf("failed to re-fetch resource after conflict: %w", err)
			}
		}
	}
	return fmt.Errorf("failed to update resource after %d attempts due to conflicts: %w", maxUpdateRetries, err)
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

	if !manifest.opts.Quiet {
		fmt.Printf("Waiting for resources to be ready (timeout: %v)...\n", timeout)
	}

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

		if !manifest.opts.Quiet {
			fmt.Printf("  Waiting for %s/%s to be ready...\n", kind, name)
		}

		if err := waitForResourceReady(waitCtx, manifest.dynamicClient, manifest.clientset, manifest.mapper, obj, manifest.opts.Verbose); err != nil {
			if waitCtx.Err() == context.DeadlineExceeded {
				return fmt.Errorf("timeout waiting for %s/%s to be ready", kind, name)
			}
			return fmt.Errorf("error waiting for %s/%s: %w", kind, name, err)
		}

		if kind == "Job" {
			// A Job is ready once it has succeeded
			if manifest.completedJobs == nil {
				manifest.completedJobs = map[string]bool{}
			}
			manifest.completedJobs[jobKey(obj.GetNamespace(), name)] = true
		}

		if !manifest.opts.Quiet {
			fmt.Printf("  %s %s/%s is ready\n", color.Checkmark(), kind, name)
		}
	}

	if !manifest.opts.Quiet {
		fmt.Printf("%s All resources are ready\n", color.Checkmark())
	}
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

		for _, field := range configChecksumFields {
			value, found, err := unstructured.NestedMap(obj.Object, field)
			if err == nil && found && value != nil {
				// Convert to YAML string for consistent hashing
				if valueBytes, err := yamlv3.Marshal(value); err == nil {
					configData = append(configData, string(valueBytes))
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

// annotateConfigChecksum sets the configChecksumAnnotation on the pod
// template of every Deployment, StatefulSet and DaemonSet in objects, to a
// checksum of the ConfigMaps and Secrets among them. It is set before the
// objects are applied: patching it on afterwards meant every apply removed it
// (one rollout) and the patch restored it (another), on every `up`.
func annotateConfigChecksum(objects []*unstructured.Unstructured) error {
	checksum, err := calculateConfigChecksumFromObjects(objects)
	if err != nil || checksum == "" {
		return err
	}
	for _, obj := range objects {
		switch obj.GetKind() {
		case "Deployment", "StatefulSet", "DaemonSet":
		default:
			continue
		}
		annotations, _, _ := unstructured.NestedStringMap(obj.Object, "spec", "template", "metadata", "annotations")
		if annotations == nil {
			annotations = map[string]string{}
		}
		annotations[configChecksumAnnotation] = checksum
		if err := unstructured.SetNestedStringMap(obj.Object, annotations, "spec", "template", "metadata", "annotations"); err != nil {
			return err
		}
	}
	return nil
}
