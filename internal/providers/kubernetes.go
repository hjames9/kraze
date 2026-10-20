package providers

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/hjames9/kraze/internal/config"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/portforward"
	"k8s.io/client-go/transport/spdy"
)

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
		restConfig.Insecure = true
		restConfig.CAData = nil
		restConfig.CAFile = ""
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
	clientset, err := clientsetFromKubeconfig(kubeconfig)
	if err != nil {
		return nil, err
	}
	return podsForService(ctx, clientset, service)
}

func podsForService(ctx context.Context, clientset kubernetes.Interface, service *config.ServiceConfig) ([]string, error) {
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
	if restConfig.CAData != nil || restConfig.CAFile != "" {
		restConfig.Insecure = true
		restConfig.CAData = nil
		restConfig.CAFile = ""
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

	forwarder, err := portforward.New(dialer, ports, stopChan, readyChan, io.Discard, io.Discard) // output is unused
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
