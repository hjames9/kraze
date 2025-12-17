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
	kindConfig, err := kind.buildKindConfig(cfg)
	if err != nil {
		return fmt.Errorf("failed to build kind config: %w", err)
	}

	// Create the cluster
	createOpts := []cluster.CreateOption{
		cluster.CreateWithV1Alpha4Config(kindConfig),
		cluster.CreateWithWaitForReady(5 * time.Minute),
		cluster.CreateWithDisplayUsage(false),
		cluster.CreateWithDisplaySalutation(false),
	}

	fmt.Printf("Creating kind cluster '%s'...\n", cfg.Name)

	// Create cluster in background so we can apply cgroup workaround during init
	createErr := make(chan error, 1)
	go func() {
		createErr <- kind.provider.Create(cfg.Name, createOpts...)
	}()

	// Wait for container to exist, then apply cgroup workaround
	// This prevents Kubernetes 1.34.0+ kubelet failures on cgroup v1 systems
	time.Sleep(10 * time.Second) // Give kind time to create the container

	if err := kind.ensureKubeletCgroupDirectories(cfg.Name); err != nil {
		// Log but don't fail - cluster might still work without this
		fmt.Printf("Note: Could not create kubelet cgroup directories (cluster may still succeed): %v\n", err)
	}

	// Wait for cluster creation to complete
	if err := <-createErr; err != nil {
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
	fmt.Printf("Waiting for cluster to fully stabilize...\n")
	time.Sleep(5 * time.Second)

	// Update CA certificates if custom CAs were mounted
	// This is done after cluster init to avoid interfering with kubeadm init
	// Note: We don't reload containerd - the CAs will be picked up on next image pull
	if len(cfg.CACertificates) > 0 {
		// Give a bit more time for all systemd services to be fully up
		// This ensures update-ca-certificates has all dependencies ready
		fmt.Printf("Preparing to update CA certificates...\n")
		time.Sleep(3 * time.Second)

		if err := kind.updateCACertificates(cfg.Name); err != nil {
			// This is a critical error - without CA certificates, application images won't pull
			return fmt.Errorf("failed to update CA certificates: %w", err)
		}
	}

	// Configure insecure registries if specified
	// This is done after cluster init to avoid interfering with kubeadm
	if len(cfg.InsecureRegistries) > 0 {
		if err := kind.configureInsecureRegistries(cfg.Name, cfg.InsecureRegistries); err != nil {
			fmt.Printf("Warning: Could not configure insecure registries: %v\n", err)
		}
	}

	// Configure proxy if specified
	httpProxy, httpsProxy, noProxy := kind.getEffectiveProxyConfig(cfg)
	if httpProxy != "" || httpsProxy != "" || noProxy != "" {
		if err := kind.configureProxy(cfg.Name, httpProxy, httpsProxy, noProxy); err != nil {
			fmt.Printf("Warning: Could not configure proxy: %v\n", err)
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

	// Only patch kubeconfig if we're in a containerized environment (dev containers, CI)
	// On native macOS/Windows, kind's port forwarding (127.0.0.1:PORT) works fine
	// and container IPs are NOT accessible from the host
	if !kind.shouldPatchKubeconfig() {
		// On native hosts (macOS, Windows, Linux), we need to ensure the kubeconfig
		// uses localhost with port forwarding instead of container names/IPs
		// This is because container IPs are not accessible from the host on macOS/Windows
		patchedConfig, err := kind.patchKubeconfigForNativeHost(clusterName, kubeconfig, quiet)
		if err != nil {
			// If patching fails, fall back to original kubeconfig
			return kubeconfig, nil
		}
		return patchedConfig, nil
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

// shouldPatchKubeconfig determines if we should patch the kubeconfig with container IP
// Returns true if running in a containerized environment (dev containers, CI)
// Returns false if running natively on macOS, Windows, or Linux host
func (kind *KindManager) shouldPatchKubeconfig() bool {
	// Check if we're running inside a Docker container
	// The /.dockerenv file exists in Docker containers
	if _, err := os.Stat("/.dockerenv"); err == nil {
		return true
	}

	// Default: don't patch (use kind's original config)
	// This works on macOS, Windows, and Linux native hosts where kind sets up port forwarding
	// On these platforms, 127.0.0.1:PORT works and container IPs are NOT accessible
	return false
}

// patchKubeconfigForNativeHost replaces container names/IPs with localhost port mappings
// This is needed on macOS/Windows where container IPs are not accessible from the host
// but Docker provides port forwarding (e.g., 127.0.0.1:53549->6443/tcp)
func (kind *KindManager) patchKubeconfigForNativeHost(clusterName, kubeconfig string, quiet bool) (string, error) {
	containerName := clusterName + "-control-plane"

	// Check if kubeconfig contains the container name
	// If kind already returned a kubeconfig with localhost, we don't need to patch it
	if !strings.Contains(kubeconfig, containerName+":6443") {
		// Already using localhost or doesn't contain container name - no patching needed
		return kubeconfig, nil
	}

	// Get the Docker port mapping for the API server (port 6443)
	// Docker shows something like: 127.0.0.1:53549->6443/tcp
	cmd := osexec.Command("docker", "port", containerName, "6443")
	output, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("failed to get port mapping for container %s: %w", containerName, err)
	}

	// Parse the output to extract the host port
	// Output format: "0.0.0.0:53549" or "127.0.0.1:53549"
	portMapping := strings.TrimSpace(string(output))
	if portMapping == "" {
		return "", fmt.Errorf("no port mapping found for port 6443")
	}

	// Extract just the port number from the mapping
	// portMapping could be "0.0.0.0:53549" or "127.0.0.1:53549" or "[::]:53549"
	parts := strings.Split(portMapping, ":")
	if len(parts) < 2 {
		return "", fmt.Errorf("invalid port mapping format: %s", portMapping)
	}
	hostPort := parts[len(parts)-1] // Get the last part (the port number)

	// Replace container name with localhost and the mapped port
	patchedConfig := strings.Replace(kubeconfig,
		"https://"+containerName+":6443",
		"https://127.0.0.1:"+hostPort,
		-1)

	if !quiet {
		fmt.Printf("%s Using localhost port forwarding: 127.0.0.1:%s -> %s:6443\n",
			color.Checkmark(), hostPort, containerName)
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
func (kind *KindManager) buildKindConfig(cfg *config.ClusterConfig) (*v1alpha4.Cluster, error) {
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

	// Build extra mounts for CA certificates and GODEBUG configuration (applied to all nodes)
	caMounts := kind.buildCAMounts(cfg)
	godebugMount, err := kind.buildGODEBUGMount(cfg.Name)
	if err != nil {
		return nil, fmt.Errorf("failed to create GODEBUG configuration: %w", err)
	}

	// Combine CA mounts and GODEBUG mount
	allMounts := append(caMounts, godebugMount)

	// If no nodes specified in config, create a default control-plane node
	if len(cfg.Config) == 0 {
		node := v1alpha4.Node{
			Role:        v1alpha4.ControlPlaneRole,
			ExtraMounts: allMounts,
		}
		if nodeImage != "" {
			node.Image = nodeImage
		}
		kindCfg.Nodes = append(kindCfg.Nodes, node)
		return kindCfg, nil
	}

	// Convert kraze nodes to kind nodes
	for _, node := range cfg.Config {
		kindNode := kind.buildKindNode(node)

		// Set the node image if specified
		if nodeImage != "" {
			kindNode.Image = nodeImage
		}

		// Add CA certificate and GODEBUG mounts to existing mounts
		kindNode.ExtraMounts = append(kindNode.ExtraMounts, allMounts...)

		// Handle replicas for worker nodes
		if node.Replicas > 0 {
			for itr := 0; itr < node.Replicas; itr++ {
				kindCfg.Nodes = append(kindCfg.Nodes, kindNode)
			}
		} else {
			kindCfg.Nodes = append(kindCfg.Nodes, kindNode)
		}
	}

	return kindCfg, nil
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

// ensureKubeletCgroupDirectories creates the cgroup directories that kubelet expects
// This is a workaround for Kubernetes 1.34.0+ race condition on cgroup v1 systems
// where kubelet fails to start because the cgroup directories don't exist yet
func (kind *KindManager) ensureKubeletCgroupDirectories(clusterName string) error {
	nodes, err := kind.provider.ListInternalNodes(clusterName)
	if err != nil {
		return fmt.Errorf("failed to list cluster nodes: %w", err)
	}

	for _, node := range nodes {
		containerName := node.String()

		// First check if we're using cgroup v1 or v2
		// Only cgroup v1 needs this workaround
		checkCmd := osexec.Command("docker", "exec", containerName, "test", "-d", "/sys/fs/cgroup/systemd")
		if err := checkCmd.Run(); err != nil {
			// Not cgroup v1 (likely v2), skip this workaround
			continue
		}

		// Check if the directory already exists
		cgroupPath := "/sys/fs/cgroup/systemd/kubelet.slice/kubelet-kubepods.slice"
		testCmd := osexec.Command("docker", "exec", containerName, "test", "-d", cgroupPath)
		if err := testCmd.Run(); err == nil {
			// Directory already exists, no need to create it
			continue
		}

		// Create the kubelet cgroup directory structure
		// This prevents: "Failed to start ContainerManager: cgroup [...] has some missing paths"
		mkdirCmd := osexec.Command("docker", "exec", containerName, "mkdir", "-p", cgroupPath)
		if output, err := mkdirCmd.CombinedOutput(); err != nil {
			// This is a workaround, so we return error but don't fail hard
			return fmt.Errorf("failed to create kubelet cgroup directory %s in node %s: %w\nOutput: %s",
				cgroupPath, containerName, err, string(output))
		}
	}

	return nil
}

// updateCACertificates runs update-ca-certificates in all nodes
// This updates the system CA trust store with custom certificates mounted via extraMounts
// Note: We don't reload containerd - CAs will be automatically used on next image pull
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

		// Use timeout to prevent hanging - update-ca-certificates should complete in seconds
		// We use 30 seconds to be safe, but it typically completes in <1 second
		cmd := osexec.Command("timeout", "30", "docker", "exec", containerName, "update-ca-certificates")
		output, err := cmd.CombinedOutput()

		if err != nil {
			// Check if it was a timeout
			if exitErr, ok := err.(*osexec.ExitError); ok && exitErr.ExitCode() == 124 {
				return fmt.Errorf("update-ca-certificates timed out after 30 seconds in node %s\nOutput: %s",
					containerName, string(output))
			}
			return fmt.Errorf("failed to update CA certificates in node %s: %w\nOutput: %s",
				containerName, err, string(output))
		}

		fmt.Printf("  Node %s: CA certificates updated\n", containerName)
	}

	fmt.Printf("%s CA certificates updated successfully\n", color.Checkmark())
	return nil
}

// configureInsecureRegistries configures containerd to skip TLS verification for specified registries
// Uses the newer containerd v2 config_path format with hosts.toml files
// This is done AFTER cluster init to avoid breaking Docker Hub access during kubeadm init
func (kind *KindManager) configureInsecureRegistries(clusterName string, registries []string) error {
	fmt.Printf("Configuring insecure registries in cluster nodes...\n")

	// Get cluster nodes
	nodes, err := kind.provider.ListInternalNodes(clusterName)
	if err != nil {
		return fmt.Errorf("failed to list cluster nodes: %w", err)
	}

	// Configure each node
	for _, node := range nodes {
		containerName := node.String()

		// First, update containerd config to use config_path for v2 registry format
		// This must be done before creating the hosts.toml files
		configPatch := `[plugins."io.containerd.grpc.v1.cri".registry]
  config_path = "/etc/containerd/certs.d"`

		patchCmd := osexec.Command("docker", "exec", containerName, "sh", "-c",
			fmt.Sprintf("cat >> /etc/containerd/config.toml << 'EOF'\n%sEOF", configPatch))
		if output, err := patchCmd.CombinedOutput(); err != nil {
			return fmt.Errorf("failed to patch containerd config in node %s: %w\nOutput: %s",
				containerName, err, string(output))
		}

		// For each registry, create a hosts.toml file
		for _, registry := range registries {
			// Determine the protocol (http or https)
			// If registry starts with localhost or contains a port, use http, otherwise https
			protocol := "https"
			if strings.HasPrefix(registry, "localhost") || strings.Contains(registry, ":") && !strings.HasPrefix(registry, "https://") {
				protocol = "http"
			}
			server := fmt.Sprintf("%s://%s", protocol, registry)

			// Create the certs.d directory for this registry
			mkdirCmd := osexec.Command("docker", "exec", containerName, "mkdir", "-p", fmt.Sprintf("/etc/containerd/certs.d/%s", registry))
			if output, err := mkdirCmd.CombinedOutput(); err != nil {
				return fmt.Errorf("failed to create certs.d directory for %s in node %s: %w\nOutput: %s",
					registry, containerName, err, string(output))
			}

			// Create hosts.toml content
			hostsToml := fmt.Sprintf(`server = "%s"

[host."%s"]
  skip_verify = true
`, server, server)

			// Write hosts.toml file
			writeCmd := osexec.Command("docker", "exec", containerName, "sh", "-c",
				fmt.Sprintf("cat > /etc/containerd/certs.d/%s/hosts.toml << 'EOF'\n%sEOF", registry, hostsToml))
			if output, err := writeCmd.CombinedOutput(); err != nil {
				return fmt.Errorf("failed to write hosts.toml for %s in node %s: %w\nOutput: %s",
					registry, containerName, err, string(output))
			}
		}

		// Reload containerd to pick up the new configuration
		killCmd := osexec.Command("docker", "exec", containerName, "pkill", "-HUP", "containerd")
		if output, err := killCmd.CombinedOutput(); err != nil {
			return fmt.Errorf("failed to reload containerd configuration in node %s: %w\nOutput: %s",
				containerName, err, string(output))
		}
	}

	fmt.Printf("%s Insecure registries configured successfully\n", color.Checkmark())
	return nil
}

// configureProxy configures containerd to use HTTP/HTTPS proxy
// This is applied AFTER cluster initialization to avoid breaking kubeadm init
func (kind *KindManager) configureProxy(clusterName, httpProxy, httpsProxy, noProxy string) error {
	fmt.Printf("Configuring proxy settings in cluster nodes...\n")

	// Inform user about proxy configuration source
	fmt.Printf("  HTTP_PROXY=%s\n", httpProxy)
	fmt.Printf("  HTTPS_PROXY=%s\n", httpsProxy)
	fmt.Printf("  NO_PROXY=%s\n", noProxy)

	// Get cluster nodes
	nodes, err := kind.provider.ListInternalNodes(clusterName)
	if err != nil {
		return fmt.Errorf("failed to list cluster nodes: %w", err)
	}

	// Configure each node
	for _, node := range nodes {
		containerName := node.String()

		// Create systemd drop-in directory for containerd
		mkdirCmd := osexec.Command("docker", "exec", containerName, "mkdir", "-p", "/etc/systemd/system/containerd.service.d")
		if output, err := mkdirCmd.CombinedOutput(); err != nil {
			return fmt.Errorf("failed to create systemd drop-in directory in node %s: %w\nOutput: %s",
				containerName, err, string(output))
		}

		// Create http-proxy.conf file with environment variables
		var proxyConf strings.Builder
		proxyConf.WriteString("[Service]\n")
		proxyConf.WriteString("Environment=\"HTTP_PROXY=" + httpProxy + "\"\n")
		proxyConf.WriteString("Environment=\"HTTPS_PROXY=" + httpsProxy + "\"\n")
		proxyConf.WriteString("Environment=\"NO_PROXY=" + noProxy + "\"\n")

		// Write the proxy configuration file
		writeCmd := osexec.Command("docker", "exec", containerName, "sh", "-c",
			fmt.Sprintf("cat > /etc/systemd/system/containerd.service.d/http-proxy.conf << 'EOF'\n%sEOF", proxyConf.String()))
		if output, err := writeCmd.CombinedOutput(); err != nil {
			return fmt.Errorf("failed to write proxy config in node %s: %w\nOutput: %s",
				containerName, err, string(output))
		}

		// Reload systemd daemon to pick up the new drop-in file
		reloadCmd := osexec.Command("docker", "exec", containerName, "systemctl", "daemon-reload")
		if output, err := reloadCmd.CombinedOutput(); err != nil {
			return fmt.Errorf("failed to reload systemd daemon in node %s: %w\nOutput: %s",
				containerName, err, string(output))
		}

		// Note: We do NOT restart containerd here because it would kill all running containers
		// (including the Kubernetes API server and other critical components).
		// The proxy environment variables will be available to containerd's child processes
		// (like image pulls) without requiring a restart.
		// If a full restart is needed, the user can destroy and recreate the cluster.
	}

	fmt.Printf("%s Proxy configured successfully\n", color.Checkmark())
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

// buildGODEBUGMount creates a systemd drop-in file for containerd with GODEBUG settings
// This file is mounted into the container BEFORE containerd starts, eliminating the need for restarts
// GODEBUG=x509negativeserial=1 allows Go to accept X.509 certificates with negative serial numbers
// This is needed for:
// - Corporate CA certificates that may have negative serial numbers
// - SSL-inspecting proxies that inject certificates with negative serial numbers
// - Registry certificates with non-standard serial numbers
func (kind *KindManager) buildGODEBUGMount(clusterName string) (v1alpha4.Mount, error) {
	// Create a cluster-specific directory in ~/.kraze/clusters/<cluster-name>/
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return v1alpha4.Mount{}, fmt.Errorf("failed to get user home directory: %w", err)
	}

	krazeDir := filepath.Join(homeDir, ".kraze", "clusters", clusterName)
	if err := os.MkdirAll(krazeDir, 0755); err != nil {
		return v1alpha4.Mount{}, fmt.Errorf("failed to create kraze directory: %w", err)
	}

	// Create the systemd drop-in file
	godebugPath := filepath.Join(krazeDir, "containerd-godebug.conf")
	godebugContent := `[Service]
Environment="GODEBUG=x509negativeserial=1"
`
	if err := os.WriteFile(godebugPath, []byte(godebugContent), 0644); err != nil {
		return v1alpha4.Mount{}, fmt.Errorf("failed to write GODEBUG config file: %w", err)
	}

	// Mount it to the systemd drop-in directory
	// When the container starts, systemd will read this file before starting containerd
	return v1alpha4.Mount{
		HostPath:      godebugPath,
		ContainerPath: "/etc/systemd/system/containerd.service.d/godebug.conf",
		Readonly:      true,
	}, nil
}

// buildContainerdConfigPatches creates containerd configuration patches
func (kind *KindManager) buildContainerdConfigPatches(cfg *config.ClusterConfig) []string {
	var patches []string

	// Note: We do NOT configure insecure registries here via config_path
	// Setting config_path would tell containerd to ONLY use /etc/containerd/certs.d/
	// which is empty during kubeadm init, breaking Docker Hub access.
	// Insecure registries are configured AFTER cluster init via configureInsecureRegistries()

	return patches
}

// getEffectiveProxyConfig returns the effective proxy configuration
// Proxy is OPT-IN: environment variables are only used if proxy.enabled: true is set
// Priority: YAML config > environment variables
// Checks both uppercase and lowercase variants of environment variables
func (kind *KindManager) getEffectiveProxyConfig(cfg *config.ClusterConfig) (httpProxy, httpsProxy, noProxy string) {
	// If no proxy config at all, return empty (opt-in behavior)
	if cfg.Proxy == nil {
		return "", "", ""
	}

	// Check if proxy is explicitly disabled
	if cfg.Proxy.Enabled != nil && !*cfg.Proxy.Enabled {
		// Proxy explicitly disabled, return empty values
		return "", "", ""
	}

	// If any proxy values are explicitly set in YAML, use them (don't need enabled: true)
	hasExplicitValues := cfg.Proxy.HTTPProxy != "" || cfg.Proxy.HTTPSProxy != "" || cfg.Proxy.NoProxy != ""

	// If proxy is explicitly enabled OR has explicit values, proceed
	if (cfg.Proxy.Enabled != nil && *cfg.Proxy.Enabled) || hasExplicitValues {
		// Start with environment variables if enabled (check both uppercase and lowercase)
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
		if cfg.Proxy.HTTPProxy != "" {
			httpProxy = cfg.Proxy.HTTPProxy
		}
		if cfg.Proxy.HTTPSProxy != "" {
			httpsProxy = cfg.Proxy.HTTPSProxy
		}
		if cfg.Proxy.NoProxy != "" {
			noProxy = cfg.Proxy.NoProxy
		}

		return httpProxy, httpsProxy, noProxy
	}

	// Proxy section exists but not enabled and no explicit values - return empty
	return "", "", ""
}

// buildKubeadmConfigPatches creates kubeadm configuration patches
// Note: Proxy configuration is applied AFTER cluster initialization via configureProxy()
// to avoid interfering with kubeadm init
//
// Note: CA certificates are also configured AFTER cluster initialization
// They are mounted via extraMounts and updated in the post-init phase
func (kind *KindManager) buildKubeadmConfigPatches(cfg *config.ClusterConfig) []string {
	var patches []string

	// Note: We intentionally do NOT configure proxy or CA certificates here
	// Both are applied after cluster initialization to avoid interfering with kubeadm init

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
