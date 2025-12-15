package cluster

import (
	"context"
	"fmt"
	"os"
	osexec "os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/hjames9/kraze/internal/color"
	"github.com/hjames9/kraze/internal/config"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/kind/pkg/apis/config/v1alpha4"
	"sigs.k8s.io/kind/pkg/cluster"
	"sigs.k8s.io/kind/pkg/cluster/nodeutils"
)

// KindManager manages kind cluster operations
type KindManager struct {
	provider      *cluster.Provider
	customNetwork string // Custom Docker network name (set during cluster creation)
}

// NewKindManager creates a new kind cluster manager
func NewKindManager() *KindManager {
	return &KindManager{
		provider: cluster.NewProvider(),
	}
}

// CreateCluster creates a new kind cluster based on the configuration
func (kind *KindManager) CreateCluster(ctx context.Context, cfg *config.ClusterConfig) error {
	// Store custom network name for kubeconfig patching
	if cfg.Network != "" {
		kind.customNetwork = cfg.Network
	}

	// Check if cluster already exists
	exists, err := kind.ClusterExists(cfg.Name)
	if err != nil {
		return fmt.Errorf("failed to check if cluster exists: %w", err)
	}
	if exists {
		return fmt.Errorf("cluster '%s' already exists", cfg.Name)
	}

	// Convert kraze config to kind config
	kindConfig := kind.buildKindConfig(cfg)

	// Create the cluster
	createOpts := []cluster.CreateOption{
		cluster.CreateWithV1Alpha4Config(kindConfig),
		cluster.CreateWithWaitForReady(5 * time.Minute),
		cluster.CreateWithDisplayUsage(false),
		cluster.CreateWithDisplaySalutation(false),
	}

	fmt.Printf("Creating kind cluster '%s'...\n", cfg.Name)
	if err := kind.provider.Create(cfg.Name, createOpts...); err != nil {
		return fmt.Errorf("failed to create cluster: %w", err)
	}

	fmt.Printf("%s Cluster '%s' created successfully\n", color.Checkmark(), cfg.Name)

	// Connect cluster to host's Docker network for better connectivity
	if err := kind.connectToHostNetwork(cfg.Name, cfg.Network, cfg.Subnet, cfg.IPv4Address); err != nil {
		// Log warning but continue - cluster might still be accessible
		fmt.Printf("Warning: Could not connect to host network: %v\n", err)
	}

	// Give the API server a few seconds to be fully ready after network changes
	// kind's CreateWithWaitForReady already waits, but connecting to a new network
	// might need a moment for routing to stabilize
	fmt.Printf("Waiting for API server to stabilize...\n")
	time.Sleep(5 * time.Second)

	// Update CA certificates if custom CAs were mounted
	if len(cfg.CACertificates) > 0 {
		if err := kind.updateCACertificates(cfg.Name); err != nil {
			fmt.Printf("Warning: Could not update CA certificates: %v\n", err)
		}
	}

	return nil
}

// DeleteCluster deletes a kind cluster
func (kind *KindManager) DeleteCluster(clusterName string) error {
	// Check if cluster exists
	exists, err := kind.ClusterExists(clusterName)
	if err != nil {
		return fmt.Errorf("failed to check if cluster exists: %w", err)
	}
	if !exists {
		return fmt.Errorf("cluster '%s' does not exist", clusterName)
	}

	fmt.Printf("Deleting kind cluster '%s'...\n", clusterName)
	if err := kind.provider.Delete(clusterName, ""); err != nil {
		return fmt.Errorf("failed to delete cluster: %w", err)
	}

	fmt.Printf("%s Cluster '%s' deleted successfully\n", color.Checkmark(), clusterName)
	return nil
}

// ListClusters returns a list of all kind clusters
func (kind *KindManager) ListClusters() ([]string, error) {
	clusters, err := kind.provider.List()
	if err != nil {
		return nil, fmt.Errorf("failed to list clusters: %w", err)
	}
	return clusters, nil
}

// ClusterExists checks if a cluster with the given name exists
func (kind *KindManager) ClusterExists(clusterName string) (bool, error) {
	clusters, err := kind.ListClusters()
	if err != nil {
		return false, err
	}

	for _, cluster := range clusters {
		if cluster == clusterName {
			return true, nil
		}
	}
	return false, nil
}

// GetKubeConfig returns the kubeconfig for the cluster
// Always patches the kubeconfig to use the container's IP address for better compatibility
func (kind *KindManager) GetKubeConfig(clusterName string, internal bool) (string, error) {
	return kind.GetKubeConfigQuiet(clusterName, internal, false)
}

// GetKubeConfigQuiet returns the kubeconfig with optional message suppression
func (kind *KindManager) GetKubeConfigQuiet(clusterName string, internal bool, quiet bool) (string, error) {
	// Get the base kubeconfig from kind
	kubeconfig, err := kind.provider.KubeConfig(clusterName, internal)
	if err != nil {
		return "", fmt.Errorf("failed to get kubeconfig: %w", err)
	}

	// Patch the kubeconfig to use the container's IP address
	// This works in dev containers, CI, and other Docker-in-Docker environments
	patchedConfig, err := kind.patchKubeconfigWithContainerIP(clusterName, kubeconfig, kind.customNetwork, quiet)
	if err != nil {
		// If patching fails, fall back to original kubeconfig
		// This ensures we don't break in normal environments
		return kubeconfig, nil
	}

	return patchedConfig, nil
}

// patchKubeconfigWithContainerIP replaces the server address with the container's IP
// This provides better compatibility across different Docker network configurations
func (kind *KindManager) patchKubeconfigWithContainerIP(clusterName, kubeconfig string, customNetwork string, quiet ...bool) (string, error) {
	shouldPrint := true
	if len(quiet) > 0 && quiet[0] {
		shouldPrint = false
	}
	containerName := clusterName + "-control-plane"

	// Determine networks to try - prioritize custom network if specified
	var networksToTry []string
	if customNetwork != "" {
		// Custom network specified - try it first, then fall back to auto-detect
		networksToTry = append([]string{customNetwork}, kind.detectNetworks()...)
	} else {
		// Auto-detect networks to try based on current environment
		networksToTry = kind.detectNetworks()
	}

	for _, network := range networksToTry {
		cmd := osexec.Command("docker", "inspect", containerName,
			"-f", fmt.Sprintf("{{.NetworkSettings.Networks.%s.IPAddress}}", network))

		output, err := cmd.Output()
		if err == nil {
			containerIP := strings.Trim(strings.TrimSpace(string(output)), "\"")
			if containerIP != "" && containerIP != "<no value>" {
				// Found IP on this network
				if shouldPrint {
					fmt.Printf("%s Using container IP %s from '%s' network\n", color.Checkmark(), containerIP, network)
				}
				patchedConfig := kubeconfig

				// Replace hostname with container IP:6443
				patchedConfig = strings.Replace(patchedConfig, containerName+":6443", containerIP+":6443", -1)

				// Replace any https://127.0.0.1:PORT or https://localhost:PORT with container IP:6443
				re := regexp.MustCompile(`https://(127\.0\.0\.1|localhost):\d+`)
				patchedConfig = re.ReplaceAllString(patchedConfig, "https://"+containerIP+":6443")

				return patchedConfig, nil
			}
		}
	}

	// Fallback: get any available IP
	cmd := osexec.Command("docker", "inspect", containerName,
		"-f", "{{range .NetworkSettings.Networks}}{{.IPAddress}} {{end}}")

	output, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("failed to get container IP: %w", err)
	}

	// Take the first IP from the space-separated list
	ips := strings.Fields(string(output))
	if len(ips) == 0 {
		return "", fmt.Errorf("no container IP found")
	}
	containerIP := ips[0]

	// Replace hostname and URL addresses with container IP
	patchedConfig := kubeconfig
	patchedConfig = strings.Replace(patchedConfig, containerName+":6443", containerIP+":6443", -1)

	// Replace any https://127.0.0.1:PORT or https://localhost:PORT with container IP:6443
	re := regexp.MustCompile(`https://(127\.0\.0\.1|localhost):\d+`)
	patchedConfig = re.ReplaceAllString(patchedConfig, "https://"+containerIP+":6443")

	return patchedConfig, nil
}

// UpdateKubeconfigFile updates ~/.kube/config with cluster access, patched for dev container compatibility
// This ensures kubectl works inside dev containers by using container IP instead of 127.0.0.1
func (kind *KindManager) UpdateKubeconfigFile(clusterName string) error {
	// Get patched kubeconfig with container IP (quiet mode to avoid duplicate messages)
	kubeconfigContent, err := kind.GetKubeConfigQuiet(clusterName, true, true)
	if err != nil {
		return fmt.Errorf("failed to get kubeconfig: %w", err)
	}

	// Parse the kubeconfig
	config, err := clientcmd.Load([]byte(kubeconfigContent))
	if err != nil {
		return fmt.Errorf("failed to parse kubeconfig: %w", err)
	}

	// Add insecure-skip-tls-verify to the cluster (needed because cert is for 127.0.0.1, not container IP)
	contextName := "kind-" + clusterName
	if cluster, exists := config.Clusters[contextName]; exists {
		cluster.InsecureSkipTLSVerify = true
		cluster.CertificateAuthorityData = nil // Remove CA data when using insecure
	}

	// Get path to user's kubeconfig
	kubeconfigPath := clientcmd.RecommendedHomeFile

	// Load existing kubeconfig or create new one
	pathOptions := clientcmd.NewDefaultPathOptions()
	existingConfig, err := pathOptions.GetStartingConfig()
	if err != nil {
		// If no existing config, use the new one
		existingConfig = config
	} else {
		// Merge the new config into existing
		// This preserves other clusters/contexts/users
		for key, value := range config.Clusters {
			existingConfig.Clusters[key] = value
		}
		for key, value := range config.AuthInfos {
			existingConfig.AuthInfos[key] = value
		}
		for key, value := range config.Contexts {
			existingConfig.Contexts[key] = value
		}
	}

	// Set current context to the new cluster
	existingConfig.CurrentContext = contextName

	// Write the merged config back
	if err := clientcmd.WriteToFile(*existingConfig, kubeconfigPath); err != nil {
		return fmt.Errorf("failed to write kubeconfig: %w", err)
	}

	return nil
}

// WaitForClusterReady waits for the cluster API server to be ready
func (kind *KindManager) WaitForClusterReady(ctx context.Context, clusterName string, timeout time.Duration) error {
	fmt.Printf("Waiting for cluster API server to be ready...\n")

	// Get internal kubeconfig (connects directly to container IP)
	kubeconfigStr, err := kind.GetKubeConfig(clusterName, true)
	if err != nil {
		return fmt.Errorf("failed to get kubeconfig: %w", err)
	}

	// Create clientset
	config, err := clientcmd.RESTConfigFromKubeConfig([]byte(kubeconfigStr))
	if err != nil {
		return fmt.Errorf("failed to create REST config: %w", err)
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return fmt.Errorf("failed to create clientset: %w", err)
	}

	// Try to connect with retries
	deadline := time.Now().Add(timeout)
	retryInterval := 2 * time.Second

	for time.Now().Before(deadline) {
		// Try to get server version as a health check
		_, err := clientset.Discovery().ServerVersion()
		if err == nil {
			fmt.Printf("%s Cluster API server is ready\n", color.Checkmark())
			return nil
		}

		// Wait before retrying
		time.Sleep(retryInterval)
	}

	return fmt.Errorf("cluster API server not ready after %v", timeout)
}

// getNodeImage determines which node image to use based on configuration
// Priority: node_image > version > default (empty string, let kind decide)
func (kind *KindManager) getNodeImage(cfg *config.ClusterConfig) string {
	// Priority 1: If node_image is explicitly specified, use it
	if cfg.NodeImage != "" {
		return cfg.NodeImage
	}

	// Priority 2: If version is specified, construct the image name
	if cfg.Version != "" {
		return fmt.Sprintf("kindest/node:v%s", cfg.Version)
	}

	// Priority 3: Return empty string to let kind use its default
	return ""
}

// buildKindConfig converts kraze cluster config to kind v1alpha4 config
func (kind *KindManager) buildKindConfig(cfg *config.ClusterConfig) *v1alpha4.Cluster {
	kindCfg := &v1alpha4.Cluster{
		TypeMeta: v1alpha4.TypeMeta{
			APIVersion: "kind.x-k8s.io/v1alpha4",
			Kind:       "Cluster",
		},
		Name:  cfg.Name,
		Nodes: []v1alpha4.Node{},
	}

	// Add networking configuration if specified
	if cfg.Networking != nil {
		kindCfg.Networking = v1alpha4.Networking{
			DisableDefaultCNI: cfg.Networking.DisableDefaultCNI,
			PodSubnet:         cfg.Networking.PodSubnet,
			ServiceSubnet:     cfg.Networking.ServiceSubnet,
		}
	}

	// Add containerd config patches for CA certificates and insecure registries
	kindCfg.ContainerdConfigPatches = kind.buildContainerdConfigPatches(cfg)

	// Add kubeadm config patches for proxy configuration
	kindCfg.KubeadmConfigPatches = kind.buildKubeadmConfigPatches(cfg)

	// Determine which node image to use
	nodeImage := kind.getNodeImage(cfg)

	// Build extra mounts for CA certificates (applied to all nodes)
	caMounts := kind.buildCAMounts(cfg)

	// If no nodes specified in config, create a default control-plane node
	if len(cfg.Config) == 0 {
		node := v1alpha4.Node{
			Role:        v1alpha4.ControlPlaneRole,
			ExtraMounts: caMounts,
		}
		if nodeImage != "" {
			node.Image = nodeImage
		}
		kindCfg.Nodes = append(kindCfg.Nodes, node)
		return kindCfg
	}

	// Convert kraze nodes to kind nodes
	for _, node := range cfg.Config {
		kindNode := kind.buildKindNode(node)

		// Set the node image if specified
		if nodeImage != "" {
			kindNode.Image = nodeImage
		}

		// Add CA certificate mounts to existing mounts
		kindNode.ExtraMounts = append(kindNode.ExtraMounts, caMounts...)

		// Handle replicas for worker nodes
		if node.Replicas > 0 {
			for itr := 0; itr < node.Replicas; itr++ {
				kindCfg.Nodes = append(kindCfg.Nodes, kindNode)
			}
		} else {
			kindCfg.Nodes = append(kindCfg.Nodes, kindNode)
		}
	}

	return kindCfg
}

// PullImage pulls a Docker image from a remote registry
func (kind *KindManager) PullImage(ctx context.Context, imageName string) error {
	// Use docker pull command
	cmd := osexec.CommandContext(ctx, "docker", "pull", imageName)

	// Suppress output unless there's an error
	var stderr strings.Builder
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to pull image: %w\n%s", err, stderr.String())
	}

	return nil
}

// LoadImage loads a Docker image into the kind cluster
func (kind *KindManager) LoadImage(ctx context.Context, clusterName, imageName string) error {
	// Get cluster nodes
	nodes, err := kind.provider.ListInternalNodes(clusterName)
	if err != nil {
		return fmt.Errorf("failed to list cluster nodes: %w", err)
	}

	if len(nodes) == 0 {
		return fmt.Errorf("no nodes found in cluster '%s'", clusterName)
	}

	// Create a temporary directory for the image tar
	tmpDir, err := os.MkdirTemp("", "kind-image-")
	if err != nil {
		return fmt.Errorf("failed to create temp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	// Save the image as a tar archive
	imageTar := filepath.Join(tmpDir, "image.tar")

	// Normalize image name - strip digest if present, keeping only repo:tag
	saveImageRef := imageName
	if strings.Contains(imageName, "@sha256:") {
		// Extract the repo:tag part before the digest
		parts := strings.Split(imageName, "@")
		if len(parts) > 0 {
			repoTag := parts[0]

			// Check if this reference exists locally
			inspectCmd := osexec.Command("docker", "inspect", imageName)
			if err := inspectCmd.Run(); err == nil {
				// Image exists, tag it with the repo:tag format
				tagCmd := osexec.Command("docker", "tag", imageName, repoTag)
				if err := tagCmd.Run(); err == nil {
					saveImageRef = repoTag
				}
			}
		}
	}

	// Use docker save to export the image
	saveCmd := osexec.Command("docker", "save", "-o", imageTar, saveImageRef)
	if err := saveCmd.Run(); err != nil {
		return fmt.Errorf("failed to save image '%s': %w (make sure the image exists locally)", imageName, err)
	}

	// Load the image onto all nodes
	for _, node := range nodes {
		// Open the tar file for reading
		imageFile, err := os.Open(imageTar)
		if err != nil {
			return fmt.Errorf("failed to open image tar: %w", err)
		}
		defer imageFile.Close()

		if err := nodeutils.LoadImageArchive(node, imageFile); err != nil {
			return fmt.Errorf("failed to load image onto node %s: %w", node.String(), err)
		}

		// Close and reopen for next node
		imageFile.Close()
	}

	return nil
}

// UntagImage removes the tag reference from an image without removing the image itself
// This allows running containers to continue using the old image while new containers get updated tags
func (kind *KindManager) UntagImage(ctx context.Context, clusterName, imageName string) error {
	// Normalize image name - add docker.io prefix if needed (same logic as GetClusterImageHash)
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

	// Use ctr (containerd CLI) to remove the tag reference
	// This removes the tag but leaves the actual image data if it's in use
	// Using ctr instead of crictl because ctr has more granular control
	cmd := osexec.CommandContext(ctx, "docker", "exec", containerName,
		"ctr", "-n", "k8s.io", "images", "rm", clusterImageName)
	output, err := cmd.CombinedOutput()

	if err != nil {
		outputStr := string(output)
		// If image doesn't exist, that's fine - nothing to untag
		if strings.Contains(outputStr, "not found") || strings.Contains(outputStr, "No such image") {
			return nil
		}
		// Other errors are problems
		return fmt.Errorf("failed to untag image: %w (output: %s)", err, outputStr)
	}

	return nil
}

// updateCACertificates runs update-ca-certificates in all nodes
func (kind *KindManager) updateCACertificates(clusterName string) error {
	fmt.Printf("Updating CA certificates in cluster nodes...\n")

	// Get cluster nodes
	nodes, err := kind.provider.ListInternalNodes(clusterName)
	if err != nil {
		return fmt.Errorf("failed to list cluster nodes: %w", err)
	}

	// Run update-ca-certificates in each node
	for _, node := range nodes {
		containerName := node.String()

		// Run update-ca-certificates command
		cmd := osexec.Command("docker", "exec", containerName, "update-ca-certificates")
		if output, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("failed to update CA certificates in node %s: %w\nOutput: %s", containerName, err, string(output))
		}

		// Restart containerd to pick up the new certificates
		restartCmd := osexec.Command("docker", "exec", containerName, "systemctl", "restart", "containerd")
		if output, err := restartCmd.CombinedOutput(); err != nil {
			// Try pkill as fallback (some containers don't have systemctl)
			killCmd := osexec.Command("docker", "exec", containerName, "pkill", "-HUP", "containerd")
			if killOutput, killErr := killCmd.CombinedOutput(); killErr != nil {
				return fmt.Errorf("failed to restart containerd in node %s: %w\nSystemctl output: %s\nPkill output: %s",
					containerName, err, string(output), string(killOutput))
			}
		}
	}

	fmt.Printf("%s CA certificates updated successfully\n", color.Checkmark())
	return nil
}

// buildCAMounts creates extra mounts for CA certificates
func (kind *KindManager) buildCAMounts(cfg *config.ClusterConfig) []v1alpha4.Mount {
	var mounts []v1alpha4.Mount

	for iter, certPath := range cfg.CACertificates {
		// Mount each CA cert to /usr/local/share/ca-certificates/ in the container
		// The filename is important - it should end in .crt
		containerPath := fmt.Sprintf("/usr/local/share/ca-certificates/kraze-ca-%d.crt", iter)
		mounts = append(mounts, v1alpha4.Mount{
			HostPath:      certPath,
			ContainerPath: containerPath,
			Readonly:      true,
		})
	}

	return mounts
}

// buildContainerdConfigPatches creates containerd configuration patches
func (kind *KindManager) buildContainerdConfigPatches(cfg *config.ClusterConfig) []string {
	var patches []string

	// Add insecure registries configuration
	if len(cfg.InsecureRegistries) > 0 {
		for _, registry := range cfg.InsecureRegistries {
			patch := fmt.Sprintf(`[plugins."io.containerd.grpc.v1.cri".registry.configs."%s".tls]
  insecure_skip_verify = true`, registry)
			patches = append(patches, patch)
		}
	}

	// Add CA certificate configuration
	if len(cfg.CACertificates) > 0 {
		// First, add a patch to update CA certificates
		// This runs update-ca-certificates to add our custom CAs to the system trust store
		patch := `[plugins."io.containerd.grpc.v1.cri".containerd.runtimes.runc.options]
  SystemdCgroup = true`
		patches = append(patches, patch)

		// Note: The actual CA certificate trust is handled by mounting the certs
		// and running update-ca-certificates in the container
		// We'll need to add a kubeadm patch to run this command
	}

	return patches
}

// getEffectiveProxyConfig returns the effective proxy configuration
// Priority: YAML config > environment variables
// Checks both uppercase and lowercase variants of environment variables
// If proxy.enabled is explicitly set to false, returns empty strings (disables proxy)
func (kind *KindManager) getEffectiveProxyConfig(cfg *config.ClusterConfig) (httpProxy, httpsProxy, noProxy string) {
	// Check if proxy is explicitly disabled
	if cfg.Proxy != nil && cfg.Proxy.Enabled != nil && !*cfg.Proxy.Enabled {
		// Proxy explicitly disabled, return empty values
		return "", "", ""
	}

	// Start with environment variables (check both uppercase and lowercase)
	httpProxy = os.Getenv("HTTP_PROXY")
	if httpProxy == "" {
		httpProxy = os.Getenv("http_proxy")
	}

	httpsProxy = os.Getenv("HTTPS_PROXY")
	if httpsProxy == "" {
		httpsProxy = os.Getenv("https_proxy")
	}

	noProxy = os.Getenv("NO_PROXY")
	if noProxy == "" {
		noProxy = os.Getenv("no_proxy")
	}

	// Override with YAML config if specified
	if cfg.Proxy != nil {
		if cfg.Proxy.HTTPProxy != "" {
			httpProxy = cfg.Proxy.HTTPProxy
		}
		if cfg.Proxy.HTTPSProxy != "" {
			httpsProxy = cfg.Proxy.HTTPSProxy
		}
		if cfg.Proxy.NoProxy != "" {
			noProxy = cfg.Proxy.NoProxy
		}
	}

	return httpProxy, httpsProxy, noProxy
}

// buildKubeadmConfigPatches creates kubeadm configuration patches
func (kind *KindManager) buildKubeadmConfigPatches(cfg *config.ClusterConfig) []string {
	var patches []string

	// Get effective proxy configuration (YAML config overrides environment variables)
	httpProxy, httpsProxy, noProxy := kind.getEffectiveProxyConfig(cfg)

	// Check if proxy was explicitly disabled
	if cfg.Proxy != nil && cfg.Proxy.Enabled != nil && !*cfg.Proxy.Enabled {
		fmt.Printf("Proxy explicitly disabled (ignoring environment variables)\n")
		return patches
	}

	// Add proxy configuration if any proxy settings are present
	if httpProxy != "" || httpsProxy != "" || noProxy != "" {
		// Inform user about proxy configuration source
		if cfg.Proxy != nil && (cfg.Proxy.HTTPProxy != "" || cfg.Proxy.HTTPSProxy != "" || cfg.Proxy.NoProxy != "") {
			fmt.Printf("Using proxy configuration from kraze.yml\n")
		} else {
			fmt.Printf("Using proxy configuration from environment variables\n")
		}
		var proxyPatch strings.Builder
		proxyPatch.WriteString("kind: InitConfiguration\n")
		proxyPatch.WriteString("nodeRegistration:\n")
		proxyPatch.WriteString("  kubeletExtraArgs:\n")

		if httpProxy != "" {
			proxyPatch.WriteString(fmt.Sprintf("    http-proxy: %s\n", httpProxy))
		}
		if httpsProxy != "" {
			proxyPatch.WriteString(fmt.Sprintf("    https-proxy: %s\n", httpsProxy))
		}
		if noProxy != "" {
			proxyPatch.WriteString(fmt.Sprintf("    no-proxy: %s\n", noProxy))
		}

		patches = append(patches, proxyPatch.String())

		// Also add a ClusterConfiguration patch to set proxy for control plane components
		var clusterPatch strings.Builder
		clusterPatch.WriteString("kind: ClusterConfiguration\n")
		clusterPatch.WriteString("apiServer:\n")
		clusterPatch.WriteString("  extraEnv:\n")

		if httpProxy != "" {
			clusterPatch.WriteString(fmt.Sprintf("  - name: HTTP_PROXY\n    value: %s\n", httpProxy))
		}
		if httpsProxy != "" {
			clusterPatch.WriteString(fmt.Sprintf("  - name: HTTPS_PROXY\n    value: %s\n", httpsProxy))
		}
		if noProxy != "" {
			clusterPatch.WriteString(fmt.Sprintf("  - name: NO_PROXY\n    value: %s\n", noProxy))
		}

		patches = append(patches, clusterPatch.String())
	}

	// Add patch to update CA certificates if custom CAs are provided
	if len(cfg.CACertificates) > 0 {
		// Add a preKubeadmCommands-style patch
		// Note: kind doesn't directly support preKubeadmCommands in v1alpha4
		// but we can use a postKubeadmCommands via JoinConfiguration for workers
		// For control-plane, we'll document that users should use node_image with pre-installed certs
		// or rely on the containerd registry configuration

		// Actually, we can use InitConfiguration to run commands before kubeadm
		caPatch := `kind: InitConfiguration
nodeRegistration:
  kubeletExtraArgs:
    node-labels: "kraze-ca-configured=true"`
		patches = append(patches, caPatch)
	}

	return patches
}

// buildKindNode converts a kraze node to a kind node
func (kind *KindManager) buildKindNode(node config.KindNode) v1alpha4.Node {
	kindNode := v1alpha4.Node{}

	// Set role
	switch node.Role {
	case "control-plane":
		kindNode.Role = v1alpha4.ControlPlaneRole
	case "worker":
		kindNode.Role = v1alpha4.WorkerRole
	default:
		kindNode.Role = v1alpha4.ControlPlaneRole
	}

	// Convert port mappings
	if len(node.ExtraPortMappings) > 0 {
		kindNode.ExtraPortMappings = make([]v1alpha4.PortMapping, len(node.ExtraPortMappings))
		for itr, pm := range node.ExtraPortMappings {
			kindNode.ExtraPortMappings[itr] = v1alpha4.PortMapping{
				ContainerPort: pm.ContainerPort,
				HostPort:      pm.HostPort,
				ListenAddress: pm.ListenAddress,
				Protocol:      v1alpha4.PortMappingProtocol(pm.Protocol),
			}
		}
	}

	// Convert mounts
	if len(node.ExtraMounts) > 0 {
		kindNode.ExtraMounts = make([]v1alpha4.Mount, len(node.ExtraMounts))
		for itr, m := range node.ExtraMounts {
			kindNode.ExtraMounts[itr] = v1alpha4.Mount{
				HostPath:      m.HostPath,
				ContainerPath: m.ContainerPath,
				Readonly:      m.ReadOnly,
			}
		}
	}

	// Set labels
	if len(node.Labels) > 0 {
		kindNode.Labels = node.Labels
	}

	return kindNode
}

// connectToHostNetwork connects the kind cluster to the specified or auto-detected Docker network
// This enables connectivity in Docker-in-Docker environments like dev containers
// Parameters:
// - clusterName: name of the kind cluster
// - networkName: explicit network name (optional, auto-detected if empty)
// - subnet: network subnet for creation (optional, e.g., "172.1.0.0/16")
// - ipv4Address: static IP for the cluster container (optional)
func (kind *KindManager) connectToHostNetwork(clusterName string, networkName string, subnet string, ipv4Address string) error {
	containerName := clusterName + "-control-plane"

	// Determine which networks to try
	var networksToTry []string
	if networkName != "" {
		// Use explicit network name
		networksToTry = []string{networkName}

		// Ensure the network exists (create if needed and subnet is provided)
		if err := kind.ensureNetworkExists(networkName, subnet); err != nil {
			return fmt.Errorf("failed to ensure network '%s' exists: %w", networkName, err)
		}
	} else {
		// Auto-detect networks
		networksToTry = kind.detectNetworks()
	}

	for _, network := range networksToTry {
		// Check if network exists
		checkCmd := osexec.Command("docker", "network", "inspect", network)
		if err := checkCmd.Run(); err != nil {
			continue
		}

		// Try to connect to this network
		var connectCmd *osexec.Cmd
		if ipv4Address != "" {
			// Connect with static IP
			connectCmd = osexec.Command("docker", "network", "connect", "--ip", ipv4Address, network, containerName)
		} else {
			// Connect with dynamic IP
			connectCmd = osexec.Command("docker", "network", "connect", network, containerName)
		}

		if err := connectCmd.Run(); err != nil {
			// Might already be connected or other error, try next network
			continue
		}

		if ipv4Address != "" {
			fmt.Printf("%s Connected cluster to '%s' network with IP %s\n", color.Checkmark(), network, ipv4Address)
		} else {
			fmt.Printf("%s Connected cluster to '%s' network for better connectivity\n", color.Checkmark(), network)
		}
		return nil
	}

	// No networks worked, return error
	return fmt.Errorf("could not connect to any common Docker network")
}

// ensureNetworkExists checks if a Docker network exists and creates it if needed
func (kind *KindManager) ensureNetworkExists(networkName string, subnet string) error {
	// Check if network already exists
	checkCmd := osexec.Command("docker", "network", "inspect", networkName)
	if err := checkCmd.Run(); err == nil {
		// Network exists
		return nil
	}

	// Network doesn't exist - create it if subnet is provided
	if subnet == "" {
		// Can't create without subnet
		return fmt.Errorf("network '%s' does not exist and no subnet specified", networkName)
	}

	fmt.Printf("Creating Docker network '%s' with subnet %s...\n", networkName, subnet)

	// Create the network with subnet
	createCmd := osexec.Command("docker", "network", "create",
		"--driver", "bridge",
		"--subnet", subnet,
		networkName)

	if output, err := createCmd.CombinedOutput(); err != nil {
		return fmt.Errorf("failed to create network: %w\nOutput: %s", err, string(output))
	}

	fmt.Printf("%s Created Docker network '%s'\n", color.Checkmark(), networkName)
	return nil
}

// detectNetworks detects which Docker networks to use based on the current environment
func (kind *KindManager) detectNetworks() []string {
	// Try to detect if we're running inside a Docker container
	if !kind.isRunningInContainer() {
		// Not in a container, use default bridge network
		return []string{"bridge"}
	}

	// We're in a container - try to get its networks
	currentNetworks := kind.getCurrentContainerNetworks()
	if len(currentNetworks) > 0 {
		// Found networks from current container - use those plus bridge as fallback
		return append(currentNetworks, "bridge")
	}

	// Couldn't detect networks, fall back to bridge
	return []string{"bridge"}
}

// isRunningInContainer checks if we're running inside a Docker container
func (kind *KindManager) isRunningInContainer() bool {
	// Method 1: Check for /.dockerenv file
	if _, err := os.Stat("/.dockerenv"); err == nil {
		return true
	}

	// Method 2: Check /proc/self/cgroup for docker/containerd
	data, err := os.ReadFile("/proc/self/cgroup")
	if err == nil {
		content := string(data)
		if strings.Contains(content, "/docker/") ||
			strings.Contains(content, "/containerd/") ||
			strings.Contains(content, "/kubepods/") {
			return true
		}
	}

	return false
}

// getCurrentContainerNetworks returns the networks of the current container
func (kind *KindManager) getCurrentContainerNetworks() []string {
	// Get the hostname (often the container ID or name)
	hostname, err := os.Hostname()
	if err != nil {
		return nil
	}

	// Try to inspect this container by hostname
	cmd := osexec.Command("docker", "inspect", hostname,
		"-f", "{{range $net, $config := .NetworkSettings.Networks}}{{$net}} {{end}}")

	output, err := cmd.Output()
	if err != nil {
		return nil
	}

	// Parse the space-separated network names
	networksStr := strings.TrimSpace(string(output))
	if networksStr == "" {
		return nil
	}

	networks := strings.Fields(networksStr)

	// Filter out common low-priority networks, preferring user-defined ones
	filtered := make([]string, 0)
	var bridgeFound bool

	for _, net := range networks {
		// Skip "none" and "host" networks
		if net == "none" || net == "host" {
			continue
		}
		// Prioritize non-bridge networks
		if net == "bridge" {
			bridgeFound = true
			continue
		}
		filtered = append(filtered, net)
	}

	// If we only found bridge, include it
	if len(filtered) == 0 && bridgeFound {
		filtered = append(filtered, "bridge")
	}

	return filtered
}

// GetKubeconfigForExternalCluster returns the kubeconfig path for an external cluster
func (kind *KindManager) GetKubeconfigForExternalCluster(cfg *config.ClusterConfig) (string, error) {
	if cfg.External == nil || !cfg.External.Enabled {
		return "", fmt.Errorf("cluster is not configured as external")
	}

	// Use specified kubeconfig or default
	kubeconfigPath := cfg.External.Kubeconfig
	if kubeconfigPath == "" {
		// Use default kubeconfig location
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("failed to get user home directory: %w", err)
		}
		kubeconfigPath = filepath.Join(home, ".kube", "config")
	} else {
		// Expand ~ if present
		if strings.HasPrefix(kubeconfigPath, "~/") {
			home, err := os.UserHomeDir()
			if err != nil {
				return "", fmt.Errorf("failed to get user home directory: %w", err)
			}
			kubeconfigPath = filepath.Join(home, kubeconfigPath[2:])
		}
	}

	// Verify kubeconfig file exists
	if _, err := os.Stat(kubeconfigPath); err != nil {
		return "", fmt.Errorf("kubeconfig file not found: %s", kubeconfigPath)
	}

	return kubeconfigPath, nil
}

// VerifyClusterAccess verifies that the external cluster is accessible
func (kind *KindManager) VerifyClusterAccess(ctx context.Context, kubeconfigPath string) error {
	// Load kubeconfig
	config, err := clientcmd.BuildConfigFromFlags("", kubeconfigPath)
	if err != nil {
		return fmt.Errorf("failed to load kubeconfig: %w", err)
	}

	// Create Kubernetes client
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return fmt.Errorf("failed to create Kubernetes client: %w", err)
	}

	// Try to get server version to verify connectivity
	_, err = clientset.Discovery().ServerVersion()
	if err != nil {
		return fmt.Errorf("failed to connect to cluster: %w", err)
	}

	return nil
}
