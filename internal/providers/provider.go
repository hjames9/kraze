package providers

import (
	"bufio"
	"bytes"
	"context"
	stderrors "errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/hjames9/kraze/internal/color"
	"github.com/hjames9/kraze/internal/config"
	"github.com/hjames9/kraze/internal/state"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"
	"k8s.io/client-go/tools/clientcmd"
)

// imagePullGracePeriod is how long to tolerate ImagePullBackOff / ErrImagePull on a pod
// before treating it as a hard failure. This covers the window between kraze loading an
// image into kind and containerd finishing its indexing — during which a newly-created pod
// can transiently hit a pull error even though the image is already present locally.
const imagePullGracePeriod = 30 * time.Second

// Provider is the interface that all service providers must implement
type Provider interface {
	// Install installs a service
	Install(ctx context.Context, service *config.ServiceConfig) error

	// Uninstall removes a service
	Uninstall(ctx context.Context, service *config.ServiceConfig) error

	// Status returns the current status of a service
	Status(ctx context.Context, service *config.ServiceConfig) (*ServiceStatus, error)

	// IsInstalled checks if a service is currently installed
	IsInstalled(ctx context.Context, service *config.ServiceConfig) (bool, error)
}

// ResourceRecorder is implemented by providers that know the objects a
// service applies, so they can be recorded in state and removed once the
// service leaves the config
type ResourceRecorder interface {
	ResourceRefs(service *config.ServiceConfig) ([]state.ResourceRef, error)
}

var _ ResourceRecorder = (*ManifestsProvider)(nil)

// ServiceStatus represents the status of a deployed service
type ServiceStatus struct {
	Name      string
	Installed bool
	Ready     bool
	Message   string
}

// ProviderOptions contains options for creating providers
type ProviderOptions struct {
	// ClusterName is the name of the kind cluster
	ClusterName string

	// KubeConfig is the kubeconfig content for the cluster
	KubeConfig string

	// Wait determines if we should wait for resources to be ready
	Wait bool

	// Timeout is the timeout for wait operations
	Timeout string

	// Verbose enables verbose output
	Verbose bool

	// KeepCRDs determines if CRDs should be kept when uninstalling Helm charts
	KeepCRDs bool

	// Quiet suppresses intermediate status messages (for clean progress UI)
	Quiet bool

	// Project is the kraze project acting on the cluster. Manifests services
	// label the objects they apply with it, so other projects' services can
	// tell the objects aren't theirs.
	Project string

	// Recorded is what the last `up` recorded for the service (status uses it
	// to count a Job removed by its TTL after `up` saw it complete)
	Recorded []state.ResourceRef
}

// NewProvider creates a provider based on the service type
func NewProvider(service *config.ServiceConfig, opts *ProviderOptions) (Provider, error) {
	switch service.Type {
	case "helm":
		return NewHelmProvider(opts)
	case "manifests":
		return NewManifestsProvider(opts)
	default:
		return nil, fmt.Errorf("unsupported service type: %s", service.Type)
	}
}

// getRESTConfigFromKubeconfig creates a REST config from kubeconfig content
func getRESTConfigFromKubeconfig(kubeconfigContent string) (*rest.Config, error) {
	if kubeconfigContent == "" {
		return nil, fmt.Errorf("kubeconfig content is empty")
	}

	clientConfig, err := clientcmd.NewClientConfigFromBytes([]byte(kubeconfigContent))
	if err != nil {
		return nil, fmt.Errorf("failed to create client config: %w", err)
	}

	restConfig, err := clientConfig.ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("failed to create REST config: %w", err)
	}

	// Skip TLS verification when connecting via container IP
	// This is necessary because the API server cert may not include all network IPs
	if restConfig.CAData != nil || restConfig.CAFile != "" {
		restConfig.Insecure = true
		// Clear CA data when using insecure mode
		restConfig.CAData = nil
		restConfig.CAFile = ""
	}

	return restConfig, nil
}

// clientsetFromKubeconfig creates a clientset from kubeconfig content
func clientsetFromKubeconfig(kubeconfig string) (kubernetes.Interface, error) {
	restConfig, err := getRESTConfigFromKubeconfig(kubeconfig)
	if err != nil {
		return nil, err
	}
	clientset, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to create clientset: %w", err)
	}
	return clientset, nil
}

// DeleteNamespace deletes a namespace, with safety checks
func DeleteNamespace(ctx context.Context, kubeconfig, namespace string) error {
	if IsSystemNamespace(namespace) {
		return fmt.Errorf("refusing to delete system namespace: %s", namespace)
	}
	clientset, err := clientsetFromKubeconfig(kubeconfig)
	if err != nil {
		return err
	}
	return deleteNamespace(ctx, clientset, namespace)
}

func deleteNamespace(ctx context.Context, clientset kubernetes.Interface, namespace string) error {
	if IsSystemNamespace(namespace) {
		return fmt.Errorf("refusing to delete system namespace: %s", namespace)
	}
	err := clientset.CoreV1().Namespaces().Delete(ctx, namespace, metav1.DeleteOptions{})
	if err != nil && !errors.IsNotFound(err) { // not found: already deleted
		return fmt.Errorf("failed to delete namespace: %w", err)
	}
	return nil
}

// namespaceDeletionPollInterval is how often WaitForNamespaceDeletion checks;
// a variable so tests run fast
var namespaceDeletionPollInterval = time.Second

// WaitForNamespaceDeletion waits for a namespace to be fully deleted from the cluster
func WaitForNamespaceDeletion(ctx context.Context, kubeconfig, namespace string, timeout time.Duration) error {
	clientset, err := clientsetFromKubeconfig(kubeconfig)
	if err != nil {
		return err
	}
	return waitForNamespaceDeletion(ctx, clientset, namespace, timeout)
}

func waitForNamespaceDeletion(ctx context.Context, clientset kubernetes.Interface, namespace string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	ticker := time.NewTicker(namespaceDeletionPollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("timeout waiting for namespace '%s' to be deleted", namespace)
		case <-ticker.C:
			// Other errors: keep waiting
			if _, err := clientset.CoreV1().Namespaces().Get(ctx, namespace, metav1.GetOptions{}); errors.IsNotFound(err) {
				return nil
			}
		}
	}
}

// kubeClients are the clients waiting for resources needs
type kubeClients struct {
	dynamic   dynamic.Interface
	clientset kubernetes.Interface // for events and logs of failing pods
	mapper    meta.RESTMapper
}

// newKubeClients creates kubeClients from kubeconfig content
func newKubeClients(kubeconfigContent string) (*kubeClients, error) {
	restConfig, err := getRESTConfigFromKubeconfig(kubeconfigContent)
	if err != nil {
		return nil, fmt.Errorf("failed to get REST config: %w", err)
	}
	dynamicClient, err := dynamic.NewForConfig(restConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to create dynamic client: %w", err)
	}
	clientset, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to create clientset: %w", err)
	}
	discoveryClient, err := discovery.NewDiscoveryClientForConfig(restConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to create discovery client: %w", err)
	}
	mapper := restmapper.NewDeferredDiscoveryRESTMapper(memory.NewMemCacheClient(discoveryClient))
	return &kubeClients{dynamic: dynamicClient, clientset: clientset, mapper: mapper}, nil
}

// WaitForManifestsInNamespace waits for resources defined in YAML manifests to become ready
// The defaultNamespace is applied to resources that don't have a namespace set
func WaitForManifestsInNamespace(ctx context.Context, kubeconfigContent, manifestYAML, defaultNamespace string, opts *ProviderOptions) error {
	if !opts.Wait {
		return nil
	}
	clients, err := newKubeClients(kubeconfigContent)
	if err != nil {
		return err
	}
	return waitForManifests(ctx, clients, manifestYAML, defaultNamespace, opts)
}

// waitForManifests is WaitForManifestsInNamespace with the clients given
func waitForManifests(ctx context.Context, clients *kubeClients, manifestYAML, defaultNamespace string, opts *ProviderOptions) error {
	if !opts.Wait {
		return nil
	}

	// Parse timeout
	timeout := 10 * time.Minute // default
	if opts.Timeout != "" {
		parsedTimeout, err := time.ParseDuration(opts.Timeout)
		if err == nil {
			timeout = parsedTimeout
		}
	}
	dynamicClient, clientset, mapper := clients.dynamic, clients.clientset, clients.mapper

	// Parse YAML manifests into resources
	resources, err := parseManifestsYAML(manifestYAML)
	if err != nil {
		return fmt.Errorf("failed to parse manifests: %w", err)
	}

	if len(resources) == 0 {
		return nil // Nothing to wait for
	}

	if !opts.Quiet {
		fmt.Printf("Waiting for resources to be ready (timeout: %v)...\n", timeout)
	}

	// Create context with timeout
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// Wait for each resource
	for _, obj := range resources {
		kind := obj.GetKind()
		name := obj.GetName()

		// Apply default namespace if the resource doesn't have one and is namespaced
		if obj.GetNamespace() == "" && defaultNamespace != "" {
			// Check if this resource type is namespaced
			gvk := obj.GroupVersionKind()
			mapping, err := mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
			if err == nil && mapping.Scope.Name() == "namespace" {
				obj.SetNamespace(defaultNamespace)
			}
		}

		// Only wait for resources that have a meaningful ready state
		if !shouldWaitForResource(kind) {
			if opts.Verbose {
				fmt.Printf("  Skipping wait for %s/%s (not a waitable resource)\n", kind, name)
			}
			continue
		}

		if !opts.Quiet {
			fmt.Printf("  Waiting for %s/%s to be ready...\n", kind, name)
		}

		if err := waitForResourceReady(waitCtx, dynamicClient, clientset, mapper, obj, opts.Verbose); err != nil {
			if waitCtx.Err() == context.DeadlineExceeded {
				return fmt.Errorf("timeout waiting for %s/%s to be ready", kind, name)
			}
			return fmt.Errorf("error waiting for %s/%s: %w", kind, name, err)
		}

		if !opts.Quiet {
			fmt.Printf("  %s %s/%s is ready\n", color.Checkmark(), kind, name)
		}
	}

	if !opts.Quiet {
		fmt.Printf("%s All resources are ready\n", color.Checkmark())
	}
	return nil
}

// parseManifestsYAML parses a multi-document YAML string into unstructured objects
func parseManifestsYAML(manifestYAML string) ([]*unstructured.Unstructured, error) {
	var resources []*unstructured.Unstructured

	// Split by document separator
	docs, err := splitYAMLDocuments(manifestYAML)
	if err != nil {
		return nil, err
	}

	// Blank documents were dropped by the split
	for _, doc := range docs {
		obj, err := parseYAMLToUnstructured(doc)
		if err != nil {
			// Skip parse errors (might be comments or invalid YAML)
			continue
		}

		if obj != nil {
			resources = append(resources, obj)
		}
	}

	return resources, nil
}

// splitYAMLDocuments splits multi-document YAML into its documents, with the
// reader kubectl uses: a separator is a "---" line at column 0 (so an indented
// "---" inside a block scalar stays in its document), optionally followed by
// a comment, and lines may be any length. Blank documents are dropped.
func splitYAMLDocuments(content string) ([]string, error) {
	reader := yaml.NewYAMLReader(bufio.NewReader(strings.NewReader(content)))
	var docs []string
	for {
		doc, err := reader.Read()
		if stderrors.Is(err, io.EOF) {
			return docs, nil
		}
		if err != nil {
			return nil, err
		}
		if len(bytes.TrimSpace(doc)) > 0 {
			docs = append(docs, string(doc))
		}
	}
}

// parseYAMLToUnstructured parses a single YAML document into an unstructured object
func parseYAMLToUnstructured(content string) (*unstructured.Unstructured, error) {
	decoder := yaml.NewYAMLOrJSONDecoder(bytes.NewReader([]byte(content)), 4096)
	obj := &unstructured.Unstructured{}

	if err := decoder.Decode(obj); err != nil {
		if err == io.EOF {
			return nil, nil
		}
		return nil, err
	}

	// Skip empty objects
	if len(obj.Object) == 0 {
		return nil, nil
	}

	return obj, nil
}

// shouldWaitForResource determines if we should wait for a resource type
func shouldWaitForResource(kind string) bool {
	waitableKinds := map[string]bool{
		"Deployment":  true,
		"StatefulSet": true,
		"DaemonSet":   true,
		"Job":         true,
		"Pod":         true,
	}
	return waitableKinds[kind]
}

// unreadyWorkloads checks once, with the rules `up` waits by, the workloads
// among objs (shouldWaitForResource) and names those that aren't ready. Objects
// without a namespace get defaultNamespace if their kind is namespaced. A
// missing workload is named "(missing)" if reportMissing; a gone Job with
// ttlSecondsAfterFinished never is, since Kubernetes deletes it once finished.
func unreadyWorkloads(ctx context.Context, dynamicClient dynamic.Interface, mapper meta.RESTMapper, objs []*unstructured.Unstructured, defaultNamespace string, reportMissing bool) ([]string, error) {
	var unready []string
	for _, obj := range objs {
		kind := obj.GetKind()
		if !shouldWaitForResource(kind) {
			continue
		}
		gvk := obj.GroupVersionKind()
		mapping, err := mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
		if err != nil {
			return nil, err
		}
		namespace := obj.GetNamespace()
		if namespace == "" && mapping.Scope.Name() == meta.RESTScopeNameNamespace {
			namespace = defaultNamespace
		}
		name := kind + "/" + obj.GetName()

		// Namespace("") addresses a cluster-scoped object
		current, err := dynamicClient.Resource(mapping.Resource).Namespace(namespace).Get(ctx, obj.GetName(), metav1.GetOptions{})
		if errors.IsNotFound(err) {
			if reportMissing && !isTTLJob(obj) {
				unready = append(unready, name+" (missing)")
			}
			continue
		}
		if err != nil {
			return nil, err
		}
		if ready, err := isResourceReady(current, kind); err != nil || !ready {
			unready = append(unready, name)
		}
	}
	return unready, nil
}

// markUnready makes the status not ready, with a message naming the workloads
// that aren't, or the error that kept them from being checked
func (status *ServiceStatus) markUnready(unready []string, err error) {
	switch {
	case err != nil:
		status.Ready = false
		status.Message = fmt.Sprintf("readiness unknown: %v", err)
	case len(unready) > 0:
		status.Ready = false
		status.Message = "not ready: " + strings.Join(unready, ", ")
	}
}

// readinessPollInterval is how often a resource's readiness is polled; a
// variable so tests run fast
var readinessPollInterval = 2 * time.Second

// errResourceFailed marks a readiness failure that waiting can't fix (a
// failed Job, a Deployment past its progress deadline)
var errResourceFailed = stderrors.New("resource failed")

// waitForResourceReady waits for a specific resource to become ready
func waitForResourceReady(ctx context.Context, dynamicClient dynamic.Interface, clientset kubernetes.Interface, mapper meta.RESTMapper, obj *unstructured.Unstructured, verbose bool) error {
	gvk := obj.GroupVersionKind()
	mapping, err := mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
	if err != nil {
		return err
	}

	gvr := mapping.Resource
	namespace := obj.GetNamespace()
	name := obj.GetName()
	kind := obj.GetKind()

	var client dynamic.ResourceInterface
	if namespace != "" {
		client = dynamicClient.Resource(gvr).Namespace(namespace)
	} else {
		client = dynamicClient.Resource(gvr)
	}

	// Track whether we've seen the resource at least once
	resourceSeen := false

	// Per-pod grace period tracking for image-pull failures (see checkControlledPodsForFailures)
	imagePullFailFirstSeen := make(map[string]time.Time)

	// Poll until ready
	ticker := time.NewTicker(readinessPollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			// Get current state
			current, err := client.Get(ctx, name, metav1.GetOptions{})
			if err != nil {
				if errors.IsNotFound(err) {
					// Only treat as "deleted" if we've seen it before
					if resourceSeen {
						return fmt.Errorf("resource was deleted")
					}
					// Resource not created yet, keep waiting
					if verbose {
						fmt.Printf("    Resource not found yet, waiting for creation...\n")
					}
					continue
				}
				// Transient error, continue polling
				if verbose {
					fmt.Printf("    Warning: failed to get resource status: %v\n", err)
				}
				continue
			}

			// We've successfully retrieved the resource
			resourceSeen = true

			// Check if ready based on kind
			ready, err := isResourceReady(current, kind)
			if stderrors.Is(err, errResourceFailed) {
				return err
			}
			if err != nil {
				if verbose {
					fmt.Printf("    Warning: failed to check readiness: %v\n", err)
				}
				continue
			}

			if ready {
				return nil
			}

			// Check for failure states in Pods (direct or owned by this resource)
			if kind == "Pod" {
				// Skip Pods that are being terminated - they're expected to go away
				deletionTimestamp, _, _ := unstructured.NestedString(current.Object, "metadata", "deletionTimestamp")
				if deletionTimestamp != "" {
					continue
				}

				// Direct Pod resource
				if failed, failureMsg := checkPodFailureState(current); failed {
					displayPodDiagnostics(ctx, clientset, current, failureMsg)
					return fmt.Errorf("pod failed: %s", failureMsg)
				}
			} else if kind == "Deployment" || kind == "StatefulSet" || kind == "DaemonSet" || kind == "Job" {
				// Check Pods controlled by this resource
				if err := checkControlledPodsForFailures(ctx, clientset, current, kind, imagePullFailFirstSeen); err != nil {
					return err
				}
			}

			// If not ready and verbose, show we're still waiting
			if verbose {
				fmt.Printf("    Still waiting (not ready yet)...\n")
			}
		}
	}
}

// isResourceReady checks if a resource is ready based on its kind and status
func isResourceReady(obj *unstructured.Unstructured, kind string) (bool, error) {
	status, found, err := unstructured.NestedMap(obj.Object, "status")
	if err != nil {
		return false, err
	}
	if !found {
		return false, nil
	}

	switch kind {
	case "Deployment":
		return isDeploymentReady(obj, status)
	case "StatefulSet":
		return isStatefulSetReady(obj, status)
	case "DaemonSet":
		return isDaemonSetReady(obj, status)
	case "Job":
		return isJobReady(obj, status)
	case "Pod":
		return isPodReady(obj, status)
	default:
		// For other resources (like CRDs), try checking status.conditions
		return hasReadyCondition(status)
	}
}

// statusInt reads an integer status field; the controllers omit zero counts,
// so a missing field is 0
func statusInt(status map[string]interface{}, field string) int64 {
	value, _, _ := unstructured.NestedInt64(status, field)
	return value
}

// specReplicas returns spec.replicas, which defaults to 1
func specReplicas(obj *unstructured.Unstructured) int64 {
	if replicas, found, _ := unstructured.NestedInt64(obj.Object, "spec", "replicas"); found {
		return replicas
	}
	return 1
}

// statusIsCurrent reports whether the controller has acted on the latest
// spec. Until it has, status describes the previous version, which may look
// fully rolled out.
func statusIsCurrent(obj *unstructured.Unstructured, status map[string]interface{}) bool {
	observed, found, _ := unstructured.NestedInt64(status, "observedGeneration")
	return found && observed >= obj.GetGeneration()
}

// isDeploymentReady reports whether a Deployment's rollout is complete, as
// `kubectl rollout status` decides it: every replica updated and available,
// and no old replicas left
func isDeploymentReady(obj *unstructured.Unstructured, status map[string]interface{}) (bool, error) {
	if !statusIsCurrent(obj, status) {
		return false, nil
	}
	conditions, _, _ := unstructured.NestedSlice(status, "conditions")
	for _, cond := range conditions {
		condMap, ok := cond.(map[string]interface{})
		if !ok {
			continue
		}
		if condMap["type"] == "Progressing" && condMap["reason"] == "ProgressDeadlineExceeded" {
			return false, fmt.Errorf("%w: deployment exceeded its progress deadline", errResourceFailed)
		}
	}

	updated := statusInt(status, "updatedReplicas")
	return updated >= specReplicas(obj) &&
		statusInt(status, "replicas") <= updated &&
		statusInt(status, "availableReplicas") >= updated, nil
}

// isStatefulSetReady reports whether a StatefulSet's rollout is complete, as
// `kubectl rollout status` decides it: enough ready replicas and, for a
// rolling update, every pod at or above the partition on the new revision
func isStatefulSetReady(obj *unstructured.Unstructured, status map[string]interface{}) (bool, error) {
	if !statusIsCurrent(obj, status) {
		return false, nil
	}
	replicas := specReplicas(obj)
	if statusInt(status, "readyReplicas") < replicas {
		return false, nil
	}

	strategy, _, _ := unstructured.NestedString(obj.Object, "spec", "updateStrategy", "type")
	if strategy == "OnDelete" {
		// Pods only change revision when deleted by hand; nothing to wait for
		return true, nil
	}
	if partition, found, _ := unstructured.NestedInt64(obj.Object, "spec", "updateStrategy", "rollingUpdate", "partition"); found && partition > 0 {
		return statusInt(status, "updatedReplicas") >= replicas-partition, nil
	}
	current, _, _ := unstructured.NestedString(status, "currentRevision")
	update, _, _ := unstructured.NestedString(status, "updateRevision")
	return current == update, nil
}

// isDaemonSetReady reports whether a DaemonSet's rollout is complete, as
// `kubectl rollout status` decides it: every scheduled pod updated and
// available
func isDaemonSetReady(obj *unstructured.Unstructured, status map[string]interface{}) (bool, error) {
	if !statusIsCurrent(obj, status) {
		return false, nil
	}
	desired := statusInt(status, "desiredNumberScheduled")
	return statusInt(status, "updatedNumberScheduled") >= desired &&
		statusInt(status, "numberAvailable") >= desired, nil
}

// isJobReady checks if a Job has completed. A failed Job is a terminal
// failure.
func isJobReady(obj *unstructured.Unstructured, status map[string]interface{}) (bool, error) {
	if statusInt(status, "succeeded") > 0 {
		return true, nil
	}

	conditions, found, err := unstructured.NestedSlice(status, "conditions")
	if err != nil || !found {
		return false, err
	}

	for _, cond := range conditions {
		condMap, ok := cond.(map[string]interface{})
		if !ok {
			continue
		}

		condType, _, _ := unstructured.NestedString(condMap, "type")
		condStatus, _, _ := unstructured.NestedString(condMap, "status")

		if condType == "Complete" && condStatus == "True" {
			return true, nil
		}
		if condType == "Failed" && condStatus == "True" {
			return false, fmt.Errorf("%w: job failed", errResourceFailed)
		}
	}

	return false, nil
}

// isPodReady checks if a Pod is ready.
// Completed pods (Succeeded) are considered ready — they ran to completion successfully.
func isPodReady(obj *unstructured.Unstructured, status map[string]interface{}) (bool, error) {
	phase, found, err := unstructured.NestedString(status, "phase")
	if err != nil || !found {
		return false, err
	}

	if phase == "Succeeded" {
		return true, nil
	}
	if phase != "Running" {
		return false, nil
	}

	conditions, found, err := unstructured.NestedSlice(status, "conditions")
	if err != nil || !found {
		return false, err
	}

	for _, cond := range conditions {
		condMap, ok := cond.(map[string]interface{})
		if !ok {
			continue
		}

		condType, _, _ := unstructured.NestedString(condMap, "type")
		condStatus, _, _ := unstructured.NestedString(condMap, "status")

		if condType == "Ready" && condStatus == "True" {
			return true, nil
		}
	}

	return false, nil
}

// isImagePullFailure returns true when the failure message comes from an image-pull error.
// These can be transient immediately after kraze loads a local image into kind.
func isImagePullFailure(msg string) bool {
	return strings.Contains(msg, "ImagePullBackOff") || strings.Contains(msg, "ErrImagePull")
}

// withinImagePullGracePeriod records the first time a pod is seen in an image-pull failure
// and returns true while the pod is still within imagePullGracePeriod. It returns false once
// the grace period has elapsed, at which point the caller should surface the failure.
// podKey should be "namespace/name". firstSeen is mutated in place.
func withinImagePullGracePeriod(podKey string, firstSeen map[string]time.Time) bool {
	if _, seen := firstSeen[podKey]; !seen {
		firstSeen[podKey] = time.Now()
	}
	return time.Since(firstSeen[podKey]) < imagePullGracePeriod
}

// checkControlledPodsForFailures checks Pods controlled by a Deployment/StatefulSet/etc for failures.
// imagePullFailFirstSeen tracks the first time each pod (namespace/name) was seen in an image-pull
// failure state so that transient pull errors are tolerated for imagePullGracePeriod before failing.
func checkControlledPodsForFailures(ctx context.Context, clientset kubernetes.Interface, obj *unstructured.Unstructured, kind string, imagePullFailFirstSeen map[string]time.Time) error {
	namespace := obj.GetNamespace()

	// Get the selector for finding Pods
	var labelSelector string
	spec, found, err := unstructured.NestedMap(obj.Object, "spec")
	if !found || err != nil {
		return nil // Can't check without selector
	}

	selector, found, err := unstructured.NestedMap(spec, "selector")
	if !found || err != nil {
		return nil // Can't check without selector
	}

	matchLabels, found, err := unstructured.NestedStringMap(selector, "matchLabels")
	if !found || err != nil {
		return nil // Can't check without matchLabels
	}

	// Build label selector string
	var selectors []string
	for key, value := range matchLabels {
		selectors = append(selectors, fmt.Sprintf("%s=%s", key, value))
	}
	labelSelector = strings.Join(selectors, ",")

	// List Pods matching the selector
	pods, err := clientset.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: labelSelector,
	})
	if err != nil {
		return nil // Ignore error, just continue waiting
	}

	// Check each Pod for failures
	for _, pod := range pods.Items {
		// Skip Pods that are being terminated - they're expected to go away
		if pod.DeletionTimestamp != nil {
			continue
		}

		podKey := pod.Namespace + "/" + pod.Name

		// Check for failure using typed Pod object
		if failed, failureMsg := checkTypedPodFailureState(&pod); failed {
			if isImagePullFailure(failureMsg) {
				// On the first time we see this pod in an image-pull failure, delete it
				// immediately. This resets Kubernetes' exponential backoff timer so the
				// replacement pod gets a fresh pull attempt right away, rather than
				// waiting minutes for the backoff to expire. It also ensures the pod we
				// eventually diagnose (if the failure persists) was created during this
				// run and has fresh, relevant events.
				if _, seen := imagePullFailFirstSeen[podKey]; !seen {
					_ = clientset.CoreV1().Pods(pod.Namespace).Delete(ctx, pod.Name, metav1.DeleteOptions{})
				}
				// Wait out the grace period before treating this as a hard failure.
				if withinImagePullGracePeriod(podKey, imagePullFailFirstSeen) {
					continue
				}
			}

			// Create minimal unstructured for displayPodDiagnostics
			podUnstructured := &unstructured.Unstructured{}
			podUnstructured.SetNamespace(pod.Namespace)
			podUnstructured.SetName(pod.Name)

			displayPodDiagnostics(ctx, clientset, podUnstructured, failureMsg)
			return fmt.Errorf("%s has failing pod %s: %s", kind, pod.Name, failureMsg)
		} else {
			// Pod recovered — clear any grace period state so a future failure on
			// this pod name starts a fresh timer.
			delete(imagePullFailFirstSeen, podKey)
		}
	}

	return nil
}

// checkTypedPodFailureState checks if a typed Pod is in a failure state
func checkTypedPodFailureState(pod *corev1.Pod) (bool, string) {
	// Check for outright failure phases
	if pod.Status.Phase == corev1.PodFailed {
		return true, fmt.Sprintf("Pod failed: %s - %s", pod.Status.Reason, pod.Status.Message)
	}

	// Check container statuses for common failure states
	for _, cs := range pod.Status.ContainerStatuses {
		// Check waiting state
		if cs.State.Waiting != nil {
			reason := cs.State.Waiting.Reason
			message := cs.State.Waiting.Message

			// Common failure reasons
			switch reason {
			case "ImagePullBackOff", "ErrImagePull":
				return true, fmt.Sprintf("Container '%s': %s - %s", cs.Name, reason, message)
			case "CrashLoopBackOff":
				return true, fmt.Sprintf("Container '%s': %s - %s", cs.Name, reason, message)
			case "CreateContainerConfigError", "CreateContainerError":
				return true, fmt.Sprintf("Container '%s': %s - %s", cs.Name, reason, message)
			case "InvalidImageName":
				return true, fmt.Sprintf("Container '%s': %s - %s", cs.Name, reason, message)
			case "ErrImageNeverPull":
				return true, fmt.Sprintf("Container '%s': %s - %s", cs.Name, reason, message)
			}
		}

		// Check terminated state with non-zero exit code
		if cs.State.Terminated != nil && cs.State.Terminated.ExitCode != 0 {
			return true, fmt.Sprintf("Container '%s': Exited with code %d - %s", cs.Name, cs.State.Terminated.ExitCode, cs.State.Terminated.Reason)
		}

		// Check for running containers that are not ready (probe failures)
		// This catches containers that are stuck failing startup/readiness probes
		// Note: We're tolerant of restarts during startup since some services (like webhook
		// controllers) need to restart once during initialization. Kubernetes will mark
		// containers as CrashLoopBackOff if they're truly stuck in a restart loop.
		if cs.State.Running != nil && !cs.Ready {
			runningDuration := time.Since(cs.State.Running.StartedAt.Time)

			// Only fail if we have excessive restarts AND still not ready
			// A few restarts during startup is normal, especially for webhook-based controllers
			if cs.RestartCount >= 5 {
				return true, fmt.Sprintf("Container '%s': Running but not ready after %d restart(s) - likely stuck in restart loop", cs.Name, cs.RestartCount)
			}

			// If container has been running in its current iteration for more than 90 seconds
			// and still not ready (and has restarted at least twice), it's likely stuck
			// We use 90s instead of 45s to give more time for slow startup probes
			if cs.RestartCount >= 2 && runningDuration > 90*time.Second {
				return true, fmt.Sprintf("Container '%s': Running for %v but still not ready after %d restart(s) - likely failing probes", cs.Name, runningDuration.Round(time.Second), cs.RestartCount)
			}

			// For containers with 0-1 restarts, only fail if they've been running a very long time
			// This gives services time to complete their startup sequence
			if cs.RestartCount <= 1 && runningDuration > 180*time.Second {
				return true, fmt.Sprintf("Container '%s': Running for %v but still not ready - likely failing startup probes", cs.Name, runningDuration.Round(time.Second))
			}
		}
	}

	// Check init container statuses
	for _, cs := range pod.Status.InitContainerStatuses {
		// Check waiting state
		if cs.State.Waiting != nil {
			reason := cs.State.Waiting.Reason
			message := cs.State.Waiting.Message

			// Common failure reasons
			switch reason {
			case "ImagePullBackOff", "ErrImagePull":
				return true, fmt.Sprintf("Init container '%s': %s - %s", cs.Name, reason, message)
			case "CrashLoopBackOff":
				return true, fmt.Sprintf("Init container '%s': %s - %s", cs.Name, reason, message)
			case "CreateContainerConfigError", "CreateContainerError":
				return true, fmt.Sprintf("Init container '%s': %s - %s", cs.Name, reason, message)
			}
		}

		// Check terminated state with non-zero exit code
		if cs.State.Terminated != nil && cs.State.Terminated.ExitCode != 0 {
			return true, fmt.Sprintf("Init container '%s': Exited with code %d - %s", cs.Name, cs.State.Terminated.ExitCode, cs.State.Terminated.Reason)
		}

		// Check for running init containers that are not ready (probe failures)
		if cs.State.Running != nil && !cs.Ready {
			// If init container has been restarted, it's definitely stuck
			if cs.RestartCount > 0 {
				return true, fmt.Sprintf("Init container '%s': Running but not ready after %d restart(s) - likely failing probes", cs.Name, cs.RestartCount)
			}
			// If init container has been running for more than 45 seconds and still not ready, it's stuck
			runningDuration := time.Since(cs.State.Running.StartedAt.Time)
			if runningDuration > 45*time.Second {
				return true, fmt.Sprintf("Init container '%s': Running for %v but still not ready - likely failing startup probes", cs.Name, runningDuration.Round(time.Second))
			}
		}
	}

	return false, ""
}

// checkPodFailureState checks if a Pod is in a failure state and returns diagnostic information
func checkPodFailureState(obj *unstructured.Unstructured) (bool, string) {
	status, found, err := unstructured.NestedMap(obj.Object, "status")
	if err != nil || !found {
		return false, ""
	}

	phase, _, _ := unstructured.NestedString(status, "phase")

	// Check for outright failure phases
	if phase == "Failed" {
		reason, _, _ := unstructured.NestedString(status, "reason")
		message, _, _ := unstructured.NestedString(status, "message")
		return true, fmt.Sprintf("Pod failed: %s - %s", reason, message)
	}

	// Check container statuses for common failure states
	containerStatuses, found, _ := unstructured.NestedSlice(status, "containerStatuses")
	if !found {
		// Check init container statuses
		containerStatuses, found, _ = unstructured.NestedSlice(status, "initContainerStatuses")
	}

	if found {
		for _, cs := range containerStatuses {
			csMap, ok := cs.(map[string]interface{})
			if !ok {
				continue
			}

			containerName, _, _ := unstructured.NestedString(csMap, "name")
			state, found, _ := unstructured.NestedMap(csMap, "state")
			if !found {
				continue
			}

			// Check waiting state
			if waiting, found, _ := unstructured.NestedMap(state, "waiting"); found {
				reason, _, _ := unstructured.NestedString(waiting, "reason")
				message, _, _ := unstructured.NestedString(waiting, "message")

				// Common failure reasons
				switch reason {
				case "ImagePullBackOff", "ErrImagePull":
					return true, fmt.Sprintf("Container '%s': %s - %s", containerName, reason, message)
				case "CrashLoopBackOff":
					return true, fmt.Sprintf("Container '%s': %s - %s", containerName, reason, message)
				case "CreateContainerConfigError", "CreateContainerError":
					return true, fmt.Sprintf("Container '%s': %s - %s", containerName, reason, message)
				case "InvalidImageName":
					return true, fmt.Sprintf("Container '%s': %s - %s", containerName, reason, message)
				case "ErrImageNeverPull":
					return true, fmt.Sprintf("Container '%s': %s - %s", containerName, reason, message)
				}
			}

			// Check terminated state with non-zero exit code
			if terminated, found, _ := unstructured.NestedMap(state, "terminated"); found {
				exitCode, _, _ := unstructured.NestedInt64(terminated, "exitCode")
				reason, _, _ := unstructured.NestedString(terminated, "reason")

				if exitCode != 0 {
					return true, fmt.Sprintf("Container '%s': Exited with code %d - %s", containerName, exitCode, reason)
				}
			}
		}
	}

	return false, ""
}

// getPodEvents fetches recent events for a Pod
// isEventRecent returns true if the event falls within the given cutoff window.
// It prefers LastTimestamp (set for most events) and falls back to EventTime
// (the newer microsecond-precision field). Events with no timestamp pass through
// so they are never silently dropped.
func isEventRecent(event corev1.Event, cutoff time.Time) bool {
	ts := event.LastTimestamp.Time
	if ts.IsZero() {
		ts = event.EventTime.Time
	}
	return ts.IsZero() || !ts.Before(cutoff)
}

func getPodEvents(ctx context.Context, clientset kubernetes.Interface, namespace, podName string, limit int) ([]string, error) {
	events, err := clientset.CoreV1().Events(namespace).List(ctx, metav1.ListOptions{
		FieldSelector: fmt.Sprintf("involvedObject.name=%s,involvedObject.kind=Pod", podName),
	})
	if err != nil {
		return nil, err
	}

	// Collect the most recent events, skipping anything older than 5 minutes.
	// This avoids surfacing accumulated backoff events from previous runs or
	// earlier retry cycles that are no longer relevant to the current failure.
	cutoff := time.Now().Add(-5 * time.Minute)
	var eventMsgs []string
	count := 0
	for i := len(events.Items) - 1; i >= 0 && count < limit; i-- {
		event := events.Items[i]
		if !isEventRecent(event, cutoff) {
			continue
		}
		eventMsgs = append(eventMsgs, fmt.Sprintf("  - %s: %s", event.Reason, event.Message))
		count++
	}

	return eventMsgs, nil
}

// getContainerLogs fetches recent logs from a container
func getContainerLogs(ctx context.Context, clientset kubernetes.Interface, namespace, podName, containerName string, tailLines int) ([]string, error) {
	tailLinesInt64 := int64(tailLines)
	req := clientset.CoreV1().Pods(namespace).GetLogs(podName, &corev1.PodLogOptions{
		Container: containerName,
		TailLines: &tailLinesInt64,
	})

	logs, err := req.Stream(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = logs.Close() }()

	var logLines []string
	scanner := bufio.NewScanner(logs)
	for scanner.Scan() {
		logLines = append(logLines, "    "+scanner.Text())
	}

	if err := scanner.Err(); err != nil {
		return nil, err
	}

	return logLines, nil
}

// displayPodDiagnostics shows detailed diagnostics for a failed Pod
func displayPodDiagnostics(ctx context.Context, clientset kubernetes.Interface, obj *unstructured.Unstructured, failureMsg string) {
	namespace := obj.GetNamespace()
	podName := obj.GetName()

	fmt.Printf("\n  %s Pod %s/%s is experiencing issues:\n", color.Warning(), namespace, podName)
	fmt.Printf("  %s\n\n", failureMsg)

	// Fetch and display events
	events, err := getPodEvents(ctx, clientset, namespace, podName, 5)
	if err == nil && len(events) > 0 {
		fmt.Printf("  Recent events:\n")
		for _, event := range events {
			fmt.Println(event)
		}
		fmt.Println()
	}

	// Try to get container logs if it's a CrashLoopBackOff
	if strings.Contains(failureMsg, "CrashLoopBackOff") || strings.Contains(failureMsg, "Exited with code") {
		// Extract container name from failure message
		containerName := extractContainerName(failureMsg)
		if containerName != "" {
			logs, err := getContainerLogs(ctx, clientset, namespace, podName, containerName, 50)
			if err == nil && len(logs) > 0 {
				fmt.Printf("  Last %d log lines from container '%s':\n", len(logs), containerName)
				for _, log := range logs {
					fmt.Println(log)
				}
				fmt.Println()
			}
		}
	}

	// Provide helpful suggestions based on failure type
	if strings.Contains(failureMsg, "ImagePullBackOff") || strings.Contains(failureMsg, "ErrImagePull") {
		fmt.Printf("  %s\n", color.Info("Suggestion: Check that the image exists and is accessible"))
		fmt.Printf("  - For local images, use: kraze load-image <image>\n")
		fmt.Printf("  - For private registries, ensure imagePullSecrets are configured\n\n")
	} else if strings.Contains(failureMsg, "CrashLoopBackOff") {
		fmt.Printf("  %s\n", color.Info("Suggestion: The container is crashing on startup"))
		fmt.Printf("  - Check environment variables and configuration\n")
		fmt.Printf("  - Review the logs above for error messages\n\n")
	} else if strings.Contains(failureMsg, "CreateContainerConfigError") {
		fmt.Printf("  %s\n", color.Info("Suggestion: There's an issue with the container configuration"))
		fmt.Printf("  - Check ConfigMaps and Secrets are created\n")
		fmt.Printf("  - Verify volume mounts and environment variables\n\n")
	}
}

// extractContainerName extracts the container name from a failure message
func extractContainerName(msg string) string {
	// Message format: "Container 'name': ..."
	start := strings.Index(msg, "Container '")
	if start == -1 {
		return ""
	}
	start += len("Container '")
	end := strings.Index(msg[start:], "'")
	if end == -1 {
		return ""
	}
	return msg[start : start+end]
}

// hasReadyCondition checks if a resource has a Ready=True condition (for CRDs)
func hasReadyCondition(status map[string]interface{}) (bool, error) {
	conditions, found, err := unstructured.NestedSlice(status, "conditions")
	if err != nil || !found {
		return false, nil
	}

	for _, cond := range conditions {
		condMap, ok := cond.(map[string]interface{})
		if !ok {
			continue
		}

		condType, _, _ := unstructured.NestedString(condMap, "type")
		condStatus, _, _ := unstructured.NestedString(condMap, "status")

		if condType == "Ready" && condStatus == "True" {
			return true, nil
		}
	}

	return false, nil
}

// configChecksumAnnotation holds a checksum of a service's ConfigMaps and
// Secrets on its workloads' pod templates, so a config change rolls them out
const configChecksumAnnotation = "kraze.dev/config-hash"

// configChecksumFields are the ConfigMap and Secret fields whose content the
// config checksum covers, hashed in this order
var configChecksumFields = []string{"data", "stringData", "binaryData"}
