package providers

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/hjames9/kraze/internal/color"
	"github.com/hjames9/kraze/internal/config"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"
	"k8s.io/client-go/tools/clientcmd"
)

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

// CheckNamespaceExists checks if a namespace exists in the cluster
func CheckNamespaceExists(ctx context.Context, kubeconfig, namespace string) (bool, error) {
	restConfig, err := getRESTConfigFromKubeconfig(kubeconfig)
	if err != nil {
		return false, err
	}
	return namespaceExists(ctx, restConfig, namespace)
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
	if restConfig.TLSClientConfig.CAData != nil || restConfig.TLSClientConfig.CAFile != "" {
		restConfig.TLSClientConfig.Insecure = true
		// Clear CA data when using insecure mode
		restConfig.TLSClientConfig.CAData = nil
		restConfig.TLSClientConfig.CAFile = ""
	}

	return restConfig, nil
}

// namespaceExists checks if a namespace exists in the cluster
func namespaceExists(ctx context.Context, restConfig *rest.Config, namespace string) (bool, error) {
	clientset, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return false, fmt.Errorf("failed to create clientset: %w", err)
	}

	_, err = clientset.CoreV1().Namespaces().Get(ctx, namespace, metav1.GetOptions{})
	if err != nil {
		if errors.IsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("failed to check namespace: %w", err)
	}

	return true, nil
}

// IsNamespaceEmpty checks if a namespace is empty (has no user-created resources)
// Ignores auto-created Kubernetes resources like default ServiceAccount and kube-root-ca.crt
func IsNamespaceEmpty(ctx context.Context, kubeconfig, namespace string) (bool, error) {
	restConfig, err := getRESTConfigFromKubeconfig(kubeconfig)
	if err != nil {
		return false, err
	}

	clientset, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return false, fmt.Errorf("failed to create clientset: %w", err)
	}

	// Check for pods (most common resource)
	// Ignore pods that are terminating (have DeletionTimestamp set)
	pods, err := clientset.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return false, fmt.Errorf("failed to list pods: %w", err)
	}

	// Count non-terminating pods
	nonTerminatingPods := 0
	for _, pod := range pods.Items {
		if pod.DeletionTimestamp == nil {
			nonTerminatingPods++
		}
	}

	if nonTerminatingPods > 0 {
		return false, nil
	}

	// Check for services
	services, err := clientset.CoreV1().Services(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return false, fmt.Errorf("failed to list services: %w", err)
	}
	if len(services.Items) > 0 {
		return false, nil
	}

	// Check for PVCs (important for data safety)
	// Ignore PVCs that are being deleted (have DeletionTimestamp set)
	pvcs, err := clientset.CoreV1().PersistentVolumeClaims(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return false, fmt.Errorf("failed to list PVCs: %w", err)
	}

	// Count non-deleting PVCs
	nonDeletingPVCs := 0
	for _, pvc := range pvcs.Items {
		if pvc.DeletionTimestamp == nil {
			nonDeletingPVCs++
		}
	}

	if nonDeletingPVCs > 0 {
		return false, nil
	}

	// Check for ConfigMaps (excluding auto-created ones)
	configMaps, err := clientset.CoreV1().ConfigMaps(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return false, fmt.Errorf("failed to list ConfigMaps: %w", err)
	}
	userConfigMapCount := 0
	for _, cm := range configMaps.Items {
		// Ignore the auto-created kube-root-ca.crt ConfigMap
		if cm.Name != "kube-root-ca.crt" {
			userConfigMapCount++
		}
	}
	if userConfigMapCount > 0 {
		return false, nil
	}

	// Check for Secrets (excluding auto-created ones)
	secrets, err := clientset.CoreV1().Secrets(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return false, fmt.Errorf("failed to list Secrets: %w", err)
	}
	userSecretCount := 0
	for _, secret := range secrets.Items {
		// Ignore auto-created service account tokens and Helm-managed secrets
		// Helm often leaves behind webhook CA certificates when uninstalling
		if secret.Type == "kubernetes.io/service-account-token" {
			continue // Ignore service account tokens
		}
		// Ignore secrets managed by Helm that contain webhook CAs or TLS certs
		if strings.Contains(strings.ToLower(secret.Name), "webhook") ||
			strings.Contains(strings.ToLower(secret.Name), "-ca") ||
			strings.Contains(strings.ToLower(secret.Name), "-tls") {
			continue
		}
		userSecretCount++
	}
	if userSecretCount > 0 {
		return false, nil
	}

	// Check for ServiceAccounts (excluding the default one)
	serviceAccounts, err := clientset.CoreV1().ServiceAccounts(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return false, fmt.Errorf("failed to list ServiceAccounts: %w", err)
	}
	userServiceAccountCount := 0
	for _, sa := range serviceAccounts.Items {
		// Ignore the auto-created default ServiceAccount
		if sa.Name != "default" {
			userServiceAccountCount++
		}
	}
	if userServiceAccountCount > 0 {
		return false, nil
	}

	// Check for Deployments
	deployments, err := clientset.AppsV1().Deployments(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return false, fmt.Errorf("failed to list Deployments: %w", err)
	}
	if len(deployments.Items) > 0 {
		return false, nil
	}

	// Check for StatefulSets
	statefulSets, err := clientset.AppsV1().StatefulSets(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return false, fmt.Errorf("failed to list StatefulSets: %w", err)
	}
	if len(statefulSets.Items) > 0 {
		return false, nil
	}

	// Namespace is empty (only has auto-created Kubernetes resources)
	return true, nil
}

// DeletePVCsInNamespace deletes all PVCs in a namespace
// This is needed for clean namespace deletion since Helm doesn't delete PVCs by default
func DeletePVCsInNamespace(ctx context.Context, kubeconfig, namespace string) (int, error) {
	restConfig, err := getRESTConfigFromKubeconfig(kubeconfig)
	if err != nil {
		return 0, err
	}

	clientset, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return 0, fmt.Errorf("failed to create clientset: %w", err)
	}

	// List all PVCs in the namespace
	pvcs, err := clientset.CoreV1().PersistentVolumeClaims(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return 0, fmt.Errorf("failed to list PVCs: %w", err)
	}

	if len(pvcs.Items) == 0 {
		return 0, nil
	}

	// Delete each PVC
	deletedCount := 0
	for _, pvc := range pvcs.Items {
		err := clientset.CoreV1().PersistentVolumeClaims(namespace).Delete(ctx, pvc.Name, metav1.DeleteOptions{})
		if err != nil {
			if errors.IsNotFound(err) {
				continue // Already deleted
			}
			return deletedCount, fmt.Errorf("failed to delete PVC %s: %w", pvc.Name, err)
		}
		deletedCount++
	}

	return deletedCount, nil
}

// DeleteNamespace deletes a namespace, with safety checks
func DeleteNamespace(ctx context.Context, kubeconfig, namespace string) error {
	// Safety check: never delete system namespaces
	systemNamespaces := map[string]bool{
		"default":         true,
		"kube-system":     true,
		"kube-public":     true,
		"kube-node-lease": true,
	}

	if systemNamespaces[namespace] {
		return fmt.Errorf("refusing to delete system namespace: %s", namespace)
	}

	restConfig, err := getRESTConfigFromKubeconfig(kubeconfig)
	if err != nil {
		return err
	}

	clientset, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return fmt.Errorf("failed to create clientset: %w", err)
	}

	err = clientset.CoreV1().Namespaces().Delete(ctx, namespace, metav1.DeleteOptions{})
	if err != nil {
		if errors.IsNotFound(err) {
			// Namespace already deleted
			return nil
		}
		return fmt.Errorf("failed to delete namespace: %w", err)
	}

	return nil
}

// WaitForNamespaceDeletion waits for a namespace to be fully deleted from the cluster
func WaitForNamespaceDeletion(ctx context.Context, kubeconfig, namespace string, timeout time.Duration) error {
	restConfig, err := getRESTConfigFromKubeconfig(kubeconfig)
	if err != nil {
		return err
	}

	clientset, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return fmt.Errorf("failed to create clientset: %w", err)
	}

	// Create a context with timeout
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// Poll for namespace deletion
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("timeout waiting for namespace '%s' to be deleted", namespace)
		case <-ticker.C:
			_, err := clientset.CoreV1().Namespaces().Get(ctx, namespace, metav1.GetOptions{})
			if err != nil {
				if errors.IsNotFound(err) {
					// Namespace is gone!
					return nil
				}
				// Other error - continue waiting
			}
		}
	}
}

// WaitForManifests waits for resources defined in YAML manifests to become ready
// This is a convenience wrapper for WaitForManifestsInNamespace with no default namespace
func WaitForManifests(ctx context.Context, kubeconfigContent, manifestYAML string, opts *ProviderOptions) error {
	return WaitForManifestsInNamespace(ctx, kubeconfigContent, manifestYAML, "", opts)
}

// WaitForManifestsInNamespace waits for resources defined in YAML manifests to become ready
// The defaultNamespace is applied to resources that don't have a namespace set
func WaitForManifestsInNamespace(ctx context.Context, kubeconfigContent, manifestYAML, defaultNamespace string, opts *ProviderOptions) error {
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

	// Create REST config and dynamic client
	restConfig, err := getRESTConfigFromKubeconfig(kubeconfigContent)
	if err != nil {
		return fmt.Errorf("failed to get REST config: %w", err)
	}

	dynamicClient, err := dynamic.NewForConfig(restConfig)
	if err != nil {
		return fmt.Errorf("failed to create dynamic client: %w", err)
	}

	// Create clientset for fetching events and logs
	clientset, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return fmt.Errorf("failed to create clientset: %w", err)
	}

	// Create discovery client for REST mapper
	discoveryClient, err := discovery.NewDiscoveryClientForConfig(restConfig)
	if err != nil {
		return fmt.Errorf("failed to create discovery client: %w", err)
	}

	cachedDiscoveryClient := memory.NewMemCacheClient(discoveryClient)
	mapper := restmapper.NewDeferredDiscoveryRESTMapper(cachedDiscoveryClient)

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
	docs := splitYAMLDocuments(manifestYAML)

	for _, doc := range docs {
		doc = strings.TrimSpace(doc)
		if doc == "" {
			continue
		}

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

// splitYAMLDocuments splits multi-document YAML by --- separator
func splitYAMLDocuments(content string) []string {
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
	if obj.Object == nil || len(obj.Object) == 0 {
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

// waitForResourceReady waits for a specific resource to become ready
func waitForResourceReady(ctx context.Context, dynamicClient dynamic.Interface, clientset *kubernetes.Clientset, mapper *restmapper.DeferredDiscoveryRESTMapper, obj *unstructured.Unstructured, verbose bool) error {
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

	// Poll until ready
	ticker := time.NewTicker(2 * time.Second)
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
				if err := checkControlledPodsForFailures(ctx, clientset, current, kind); err != nil {
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

// isDeploymentReady checks if a Deployment is ready
func isDeploymentReady(obj *unstructured.Unstructured, status map[string]interface{}) (bool, error) {
	spec, _, err := unstructured.NestedMap(obj.Object, "spec")
	if err != nil {
		return false, err
	}

	desiredReplicas := int64(1) // default
	if replicas, found, _ := unstructured.NestedInt64(spec, "replicas"); found {
		desiredReplicas = replicas
	}

	availableReplicas, found, err := unstructured.NestedInt64(status, "availableReplicas")
	if err != nil {
		return false, err
	}
	if !found {
		return false, nil
	}

	updatedReplicas, found, err := unstructured.NestedInt64(status, "updatedReplicas")
	if err != nil {
		return false, err
	}
	if !found {
		return false, nil
	}

	return availableReplicas >= desiredReplicas && updatedReplicas >= desiredReplicas, nil
}

// isStatefulSetReady checks if a StatefulSet is ready
func isStatefulSetReady(obj *unstructured.Unstructured, status map[string]interface{}) (bool, error) {
	spec, _, err := unstructured.NestedMap(obj.Object, "spec")
	if err != nil {
		return false, err
	}

	desiredReplicas := int64(1) // default
	if replicas, found, _ := unstructured.NestedInt64(spec, "replicas"); found {
		desiredReplicas = replicas
	}

	readyReplicas, found, err := unstructured.NestedInt64(status, "readyReplicas")
	if err != nil {
		return false, err
	}
	if !found {
		// No readyReplicas field yet - StatefulSet controller hasn't updated status
		return false, nil
	}

	// Check if we have enough ready replicas
	isReady := readyReplicas >= desiredReplicas

	// Debug output for troubleshooting
	if !isReady {
		fmt.Printf("      StatefulSet not ready: readyReplicas=%d, desiredReplicas=%d\n", readyReplicas, desiredReplicas)
	}

	return isReady, nil
}

// isDaemonSetReady checks if a DaemonSet is ready
func isDaemonSetReady(obj *unstructured.Unstructured, status map[string]interface{}) (bool, error) {
	desiredScheduled, found, err := unstructured.NestedInt64(status, "desiredNumberScheduled")
	if err != nil || !found {
		return false, err
	}

	numberReady, found, err := unstructured.NestedInt64(status, "numberReady")
	if err != nil || !found {
		return false, err
	}

	return numberReady >= desiredScheduled && desiredScheduled > 0, nil
}

// isJobReady checks if a Job has completed
func isJobReady(obj *unstructured.Unstructured, status map[string]interface{}) (bool, error) {
	succeeded, found, err := unstructured.NestedInt64(status, "succeeded")
	if err != nil {
		return false, err
	}
	if found && succeeded > 0 {
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
			return false, fmt.Errorf("job failed")
		}
	}

	return false, nil
}

// isPodReady checks if a Pod is ready
func isPodReady(obj *unstructured.Unstructured, status map[string]interface{}) (bool, error) {
	phase, found, err := unstructured.NestedString(status, "phase")
	if err != nil || !found {
		return false, err
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

// checkControlledPodsForFailures checks Pods controlled by a Deployment/StatefulSet/etc for failures
func checkControlledPodsForFailures(ctx context.Context, clientset *kubernetes.Clientset, obj *unstructured.Unstructured, kind string) error {
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

		// Check for failure using typed Pod object
		if failed, failureMsg := checkTypedPodFailureState(&pod); failed {
			// Create minimal unstructured for displayPodDiagnostics
			podUnstructured := &unstructured.Unstructured{}
			podUnstructured.SetNamespace(pod.Namespace)
			podUnstructured.SetName(pod.Name)

			displayPodDiagnostics(ctx, clientset, podUnstructured, failureMsg)
			return fmt.Errorf("%s has failing pod %s: %s", kind, pod.Name, failureMsg)
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
func getPodEvents(ctx context.Context, clientset *kubernetes.Clientset, namespace, podName string, limit int) ([]string, error) {
	events, err := clientset.CoreV1().Events(namespace).List(ctx, metav1.ListOptions{
		FieldSelector: fmt.Sprintf("involvedObject.name=%s,involvedObject.kind=Pod", podName),
	})
	if err != nil {
		return nil, err
	}

	// Sort events by timestamp (most recent first) and limit
	var eventMsgs []string
	count := 0
	for i := len(events.Items) - 1; i >= 0 && count < limit; i-- {
		event := events.Items[i]
		eventMsgs = append(eventMsgs, fmt.Sprintf("  - %s: %s", event.Reason, event.Message))
		count++
	}

	return eventMsgs, nil
}

// getContainerLogs fetches recent logs from a container
func getContainerLogs(ctx context.Context, clientset *kubernetes.Clientset, namespace, podName, containerName string, tailLines int) ([]string, error) {
	tailLinesInt64 := int64(tailLines)
	req := clientset.CoreV1().Pods(namespace).GetLogs(podName, &corev1.PodLogOptions{
		Container: containerName,
		TailLines: &tailLinesInt64,
	})

	logs, err := req.Stream(ctx)
	if err != nil {
		return nil, err
	}
	defer logs.Close()

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
func displayPodDiagnostics(ctx context.Context, clientset *kubernetes.Clientset, obj *unstructured.Unstructured, failureMsg string) {
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
			logs, err := getContainerLogs(ctx, clientset, namespace, podName, containerName, 20)
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

// patchWorkloadWithConfigChecksum patches a Deployment, StatefulSet, or DaemonSet
// with a config checksum annotation to force a rollout when the checksum changes
func patchWorkloadWithConfigChecksum(
	ctx context.Context,
	dynamicClient dynamic.Interface,
	mapper *restmapper.DeferredDiscoveryRESTMapper,
	kind, name, namespace, checksum string,
	verbose bool,
) error {
	// Get the GVR for this resource
	gvk := schema.GroupVersionKind{
		Group:   "apps",
		Version: "v1",
		Kind:    kind,
	}

	gk := schema.GroupKind{
		Group: gvk.Group,
		Kind:  gvk.Kind,
	}

	mapping, err := mapper.RESTMapping(gk, gvk.Version)
	if err != nil {
		if verbose {
			fmt.Printf("Warning: failed to get REST mapping for %s: %v\n", kind, err)
		}
		return err
	}

	// Get the resource interface
	resourceClient := dynamicClient.Resource(mapping.Resource).Namespace(namespace)

	// Patch the resource with checksum annotation
	// We use merge patch to add the annotation to spec.template.metadata.annotations
	patch := fmt.Sprintf(`{"spec":{"template":{"metadata":{"annotations":{"kraze.dev/config-hash":"%s"}}}}}`, checksum)

	_, err = resourceClient.Patch(ctx, name, types.MergePatchType, []byte(patch), metav1.PatchOptions{})
	if err != nil {
		// Log warning but don't fail - the resource might not exist yet or might not support patching
		if verbose {
			fmt.Printf("Warning: failed to patch %s/%s with config checksum: %v\n", kind, name, err)
		}
		return err
	}

	if verbose {
		fmt.Printf("Added config checksum annotation to %s/%s (hash: %s)\n", kind, name, checksum[:8])
	}

	return nil
}
