package providers

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/hjames9/kraze/internal/config"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/portforward"
	"k8s.io/client-go/transport/spdy"
)

// GetClientsetFromKubeconfig creates a Kubernetes clientset from a kubeconfig file path
func GetClientsetFromKubeconfig(kubeconfigPath string) (kubernetes.Interface, error) {
	if kubeconfigPath == "" {
		return nil, fmt.Errorf("kubeconfig path is empty")
	}

	// Load kubeconfig from file
	config, err := clientcmd.BuildConfigFromFlags("", kubeconfigPath)
	if err != nil {
		return nil, fmt.Errorf("failed to build config from kubeconfig: %w", err)
	}

	// Create clientset
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("failed to create clientset: %w", err)
	}

	return clientset, nil
}

// GetClientsetFromKubeconfigContent creates a Kubernetes clientset from kubeconfig content (YAML string)
// skipTLSVerify should only be set to true for local kind clusters where IPs may be patched for custom networks.
// For external clusters (Docker Desktop, Minikube, etc.) this should be false to maintain proper TLS verification.
func GetClientsetFromKubeconfigContent(kubeconfigContent string, skipTLSVerify bool) (kubernetes.Interface, error) {
	if kubeconfigContent == "" {
		return nil, fmt.Errorf("kubeconfig content is empty")
	}

	// Parse kubeconfig from bytes
	clientConfig, err := clientcmd.NewClientConfigFromBytes([]byte(kubeconfigContent))
	if err != nil {
		return nil, fmt.Errorf("failed to parse kubeconfig: %w", err)
	}

	// Get REST config
	restConfig, err := clientConfig.ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("failed to create REST config: %w", err)
	}

	// Skip TLS verification only for kind clusters where IP patching may cause cert hostname mismatches
	// This is safe for local development but should NEVER be used for external/production clusters
	if skipTLSVerify {
		restConfig.TLSClientConfig.Insecure = true
		restConfig.TLSClientConfig.CAData = nil
		restConfig.TLSClientConfig.CAFile = ""
	}

	// Create clientset
	clientset, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to create clientset: %w", err)
	}

	return clientset, nil
}

// GetPodsForService returns pod names for a given service
// For Helm services: uses helm release labels
// For manifest services: uses user-specified labels or service name
func GetPodsForService(ctx context.Context, kubeconfig string, service *config.ServiceConfig) ([]string, error) {
	restConfig, err := getRESTConfigFromKubeconfig(kubeconfig)
	if err != nil {
		return nil, err
	}

	clientset, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to create clientset: %w", err)
	}

	namespace := service.GetNamespace()

	// Build label selector based on service type
	var labelSelector string
	switch service.Type {
	case "helm":
		// Helm uses app.kubernetes.io/instance=<release-name>
		labelSelector = fmt.Sprintf("app.kubernetes.io/instance=%s", service.Name)
	case "manifests":
		// For manifests, try to use user-specified labels or fallback to app label
		if len(service.Labels) > 0 {
			// Build selector from service labels
			selectors := make([]string, 0, len(service.Labels))
			for key, value := range service.Labels {
				selectors = append(selectors, fmt.Sprintf("%s=%s", key, value))
			}
			labelSelector = strings.Join(selectors, ",")
		} else {
			// Fallback to app=service-name
			labelSelector = fmt.Sprintf("app=%s", service.Name)
		}
	default:
		return nil, fmt.Errorf("unsupported service type: %s", service.Type)
	}

	// List pods with the label selector
	pods, err := clientset.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: labelSelector,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to list pods: %w", err)
	}

	// Filter out terminated/terminating pods
	var podNames []string
	for _, pod := range pods.Items {
		// Skip pods that are terminating
		if pod.DeletionTimestamp != nil {
			continue
		}

		// Skip pods that are not in a good state
		if pod.Status.Phase != corev1.PodRunning && pod.Status.Phase != corev1.PodPending {
			continue
		}

		podNames = append(podNames, pod.Name)
	}

	if len(podNames) == 0 {
		return nil, fmt.Errorf("no running pods found for service '%s' in namespace '%s'", service.Name, namespace)
	}

	return podNames, nil
}

// PortForward establishes a port-forward connection to a pod
func PortForward(ctx context.Context, kubeconfigContent, namespace, podName string, ports []string) error {
	// Parse kubeconfig
	clientConfig, err := clientcmd.NewClientConfigFromBytes([]byte(kubeconfigContent))
	if err != nil {
		return fmt.Errorf("failed to parse kubeconfig: %w", err)
	}

	restConfig, err := clientConfig.ClientConfig()
	if err != nil {
		return fmt.Errorf("failed to create REST config: %w", err)
	}

	// Skip TLS verification for kind clusters
	if restConfig.TLSClientConfig.CAData != nil || restConfig.TLSClientConfig.CAFile != "" {
		restConfig.TLSClientConfig.Insecure = true
		restConfig.TLSClientConfig.CAData = nil
		restConfig.TLSClientConfig.CAFile = ""
	}

	// Create clientset
	clientset, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return fmt.Errorf("failed to create Kubernetes client: %w", err)
	}

	// Build port-forward URL
	req := clientset.CoreV1().RESTClient().Post().
		Resource("pods").
		Namespace(namespace).
		Name(podName).
		SubResource("portforward")

	// Create SPDY transport for port forwarding
	transport, upgrader, err := spdy.RoundTripperFor(restConfig)
	if err != nil {
		return fmt.Errorf("failed to create SPDY round tripper: %w", err)
	}

	// Create dialer
	dialer := spdy.NewDialer(upgrader, &http.Client{Transport: transport}, "POST", req.URL())

	// Create port forwarder
	stopChan := make(chan struct{}, 1)
	readyChan := make(chan struct{}, 1)

	// Create a temporary directory for port-forward output (required by the API but we don't use it)
	tmpDir, err := os.MkdirTemp("", "kraze-portforward-*")
	if err != nil {
		return fmt.Errorf("failed to create temp directory: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	outFile, err := os.Create(filepath.Join(tmpDir, "out.log"))
	if err != nil {
		return fmt.Errorf("failed to create output file: %w", err)
	}
	defer outFile.Close()

	errFile, err := os.Create(filepath.Join(tmpDir, "err.log"))
	if err != nil {
		return fmt.Errorf("failed to create error file: %w", err)
	}
	defer errFile.Close()

	forwarder, err := portforward.New(dialer, ports, stopChan, readyChan, outFile, errFile)
	if err != nil {
		return fmt.Errorf("failed to create port forwarder: %w", err)
	}

	// Start port forwarding in a goroutine
	errChan := make(chan error, 1)
	go func() {
		if err := forwarder.ForwardPorts(); err != nil {
			errChan <- fmt.Errorf("port forward failed: %w", err)
		}
	}()

	// Wait for ready or context cancellation
	select {
	case <-readyChan:
		// Port forwarding is ready
	case err := <-errChan:
		return err
	case <-ctx.Done():
		close(stopChan)
		return ctx.Err()
	}

	// Wait for context cancellation
	select {
	case err := <-errChan:
		return err
	case <-ctx.Done():
		close(stopChan)
		return nil
	}
}

// getKubeconfigFromFile reads kubeconfig from a file path and returns the content
func getKubeconfigFromFile(path string) (string, error) {
	// Expand ~ if present
	if strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("failed to get user home directory: %w", err)
		}
		path = filepath.Join(home, path[2:])
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("failed to read kubeconfig: %w", err)
	}

	return string(data), nil
}

// buildKubeconfigURL builds the URL for a kubeconfig from path
func buildKubeconfigURL(kubeconfigPath string) (*url.URL, error) {
	// Parse kubeconfig to get the server URL
	config, err := clientcmd.LoadFromFile(kubeconfigPath)
	if err != nil {
		return nil, fmt.Errorf("failed to load kubeconfig: %w", err)
	}

	currentContext := config.CurrentContext
	if currentContext == "" {
		return nil, fmt.Errorf("no current context in kubeconfig")
	}

	context, ok := config.Contexts[currentContext]
	if !ok {
		return nil, fmt.Errorf("context %s not found in kubeconfig", currentContext)
	}

	cluster, ok := config.Clusters[context.Cluster]
	if !ok {
		return nil, fmt.Errorf("cluster %s not found in kubeconfig", context.Cluster)
	}

	return url.Parse(cluster.Server)
}
