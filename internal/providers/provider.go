package providers

import (
	"context"
	"fmt"

	"github.com/hjames9/kraze/internal/config"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
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
	for _, cm := range configMaps.Items {
		// Ignore the auto-created kube-root-ca.crt ConfigMap
		if cm.Name != "kube-root-ca.crt" {
			return false, nil
		}
	}

	// Check for Secrets (excluding auto-created ones)
	secrets, err := clientset.CoreV1().Secrets(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return false, fmt.Errorf("failed to list Secrets: %w", err)
	}
	for _, secret := range secrets.Items {
		// Ignore auto-created service account tokens
		if secret.Type != "kubernetes.io/service-account-token" {
			return false, nil
		}
	}

	// Check for ServiceAccounts (excluding the default one)
	serviceAccounts, err := clientset.CoreV1().ServiceAccounts(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return false, fmt.Errorf("failed to list ServiceAccounts: %w", err)
	}
	for _, sa := range serviceAccounts.Items {
		// Ignore the auto-created default ServiceAccount
		if sa.Name != "default" {
			return false, nil
		}
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
