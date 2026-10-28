package cluster

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/hjames9/kraze/internal/color"
	"github.com/hjames9/kraze/internal/config"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"sigs.k8s.io/kind/pkg/apis/config/defaults"
	"sigs.k8s.io/kind/pkg/apis/config/v1alpha4"
	"sigs.k8s.io/kind/pkg/cluster"
	"sigs.k8s.io/kind/pkg/cluster/constants"
	"sigs.k8s.io/kind/pkg/cluster/nodes"
	kindexec "sigs.k8s.io/kind/pkg/exec"
)

// KindManager manages kind cluster operations
type KindManager struct {
	provider      kindProvider
	customNetwork string // Custom Docker network name (set during cluster creation)

	run        commandRunner
	host       hostEnv
	clients    clusterClients
	loadImages imageLoader
}

// NewKindManager creates a new kind cluster manager
func NewKindManager() *KindManager {
	return &KindManager{
		provider:   cluster.NewProvider(),
		run:        execRunner{},
		host:       realHost(),
		clients:    realClusterClients(),
		loadImages: realImageLoader(),
	}
}

// CreateCluster creates a new kind cluster based on the configuration
func (kind *KindManager) CreateCluster(ctx context.Context, cfg *config.ClusterConfig) error {
	// Store custom network name for kubeconfig patching
	if cfg.Network != "" {
		kind.customNetwork = cfg.Network
	}

	// Validate GPU prerequisites before doing anything else
	if cfg.GPU.IsNvidiaEnabled() {
		if err := kind.validateNvidiaGPUPrerequisites(); err != nil {
			return fmt.Errorf("NVIDIA GPU prerequisite check failed: %w", err)
		}
	}
	if cfg.GPU.IsAMDEnabled() {
		if err := kind.validateAMDGPUPrerequisites(); err != nil {
			return fmt.Errorf("AMD GPU prerequisite check failed: %w", err)
		}
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

	fmt.Printf("Creating kind cluster '%s' (Kubernetes %s)...\n", cfg.Name, kind.parseK8sVersion(cfg))

	// Create cluster in background so we can apply cgroup workaround during init
	createErr := make(chan error, 1)
	go func() {
		createErr <- kind.provider.Create(cfg.Name, createOpts...)
	}()

	// Wait for container to exist, then apply cgroup workaround
	// This prevents Kubernetes 1.34.0+ kubelet failures on cgroup v1 systems
	kind.host.sleep(10 * time.Second) // Give kind time to create the container

	if err := kind.ensureKubeletCgroupDirectories(cfg.Name); err != nil {
		// Log but don't fail - cluster might still work without this
		fmt.Printf("Note: Could not create kubelet cgroup directories (cluster may still succeed): %v\n", err)
	}

	// Wait for cluster creation to complete
	if err := <-createErr; err != nil {
		return fmt.Errorf("failed to create cluster: %w", enrichClusterCreateError(err))
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
	kind.host.sleep(5 * time.Second)

	// Install CA certificates if specified
	// This is done after cluster init to avoid interfering with kubeadm init
	if len(cfg.CACertificates) > 0 {
		// Give a bit more time for all systemd services to be fully up
		// This ensures update-ca-certificates has all dependencies ready
		fmt.Printf("Preparing to update CA certificates...\n")
		kind.host.sleep(3 * time.Second)

		if err := kind.updateCACertificates(ctx, cfg.Name, cfg.CACertificates); err != nil {
			// This is a critical error - without CA certificates, application images won't pull
			return fmt.Errorf("failed to update CA certificates: %w", err)
		}
	}

	// After the CA update: the restart also makes containerd load the new
	// trust store, which Go reads once per process
	if err := kind.configureContainerdGODEBUG(cfg.Name); err != nil {
		return fmt.Errorf("failed to configure containerd: %w", err)
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

	// Register NVIDIA RuntimeClass if NVIDIA GPU support is enabled
	if cfg.GPU.IsNvidiaEnabled() {
		fmt.Printf("Registering NVIDIA RuntimeClass...\n")
		if err := kind.registerNvidiaRuntimeClass(ctx, cfg.Name); err != nil {
			return fmt.Errorf("failed to register NVIDIA RuntimeClass: %w", err)
		}
	}
	if cfg.GPU.IsAMDEnabled() {
		// AMD does not require a RuntimeClass.
		// The ROCm device plugin DaemonSet is installed as a kraze service.
		fmt.Printf("%s AMD GPU mounts configured (no RuntimeClass needed for AMD)\n", color.Checkmark())
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
	// Get the base kubeconfig from kind. Its error is returned as is: callers
	// say "failed to get kubeconfig", and a wrap here repeated it
	kubeconfig, err := kind.provider.KubeConfig(clusterName, internal)
	if err != nil {
		return "", err
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
	if kind.host.fileExists("/.dockerenv") {
		return true
	}

	// Default: don't patch (use kind's original config)
	// This works on macOS, Windows, and Linux native hosts where kind sets up port forwarding
	// On these platforms, 127.0.0.1:PORT works and container IPs are NOT accessible
	return false
}

// loadBalancerName is the cluster's external load balancer container, which
// kind adds in front of the API servers when there are several control-planes,
// or "" if it has none
func (kind *KindManager) loadBalancerName(clusterName string) string {
	nodeList, err := kind.provider.ListNodes(clusterName)
	if err != nil {
		return ""
	}
	name := clusterName + "-" + constants.ExternalLoadBalancerNodeRoleValue
	for _, node := range nodeList {
		if node.String() == name {
			return name
		}
	}
	return ""
}

// apiServerContainers are the containers that serve the API, in the order to
// use them: the load balancer if there is one, so requests still reach a
// working control-plane when another is down, then the first control-plane
func (kind *KindManager) apiServerContainers(clusterName string) []string {
	controlPlane := clusterName + "-control-plane"
	if lb := kind.loadBalancerName(clusterName); lb != "" {
		return []string{lb, controlPlane}
	}
	return []string{controlPlane}
}

// loopbackServer matches kind's external kubeconfig address
var loopbackServer = regexp.MustCompile(`https://(127\.0\.0\.1|localhost):\d+`)

// rewriteAPIServer points every API server address kind writes for the
// cluster at server: a container name (the control-plane's, or the load
// balancer's with several control-planes) or kind's 127.0.0.1 port
func rewriteAPIServer(kubeconfig, clusterName, server string) string {
	for _, container := range []string{clusterName + "-" + constants.ExternalLoadBalancerNodeRoleValue, clusterName + "-control-plane"} {
		kubeconfig = strings.ReplaceAll(kubeconfig, "https://"+container+":6443", server)
	}
	return loopbackServer.ReplaceAllString(kubeconfig, server)
}

// patchKubeconfigForNativeHost replaces container names with localhost port mappings
// This is needed on macOS/Windows where container IPs are not accessible from the host.
// The API is published on the load balancer with several control-planes, on
// the control-plane otherwise
func (kind *KindManager) patchKubeconfigForNativeHost(clusterName, kubeconfig string, quiet bool) (string, error) {
	for _, containerName := range []string{clusterName + "-" + constants.ExternalLoadBalancerNodeRoleValue, clusterName + "-control-plane"} {
		// If kind already returned a kubeconfig with localhost, we don't need to patch it
		if !strings.Contains(kubeconfig, "https://"+containerName+":6443") {
			continue
		}

		// Get the Docker port mapping for the API server (port 6443)
		// Docker shows something like: 127.0.0.1:53549->6443/tcp
		output, err := kind.run.Output(context.Background(), "docker", "port", containerName, "6443")
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
		kubeconfig = strings.ReplaceAll(kubeconfig,
			"https://"+containerName+":6443",
			"https://127.0.0.1:"+hostPort)

		if !quiet {
			fmt.Printf("%s Using localhost port forwarding: 127.0.0.1:%s -> %s:6443\n",
				color.Checkmark(), hostPort, containerName)
		}
	}

	return kubeconfig, nil
}

// patchKubeconfigWithContainerIP replaces the server address with the API
// server container's IP: the load balancer's with several control-planes, so
// the API stays reachable while one control-plane is down, else the
// control-plane's. A load balancer that isn't on any network this machine can
// reach (a cluster created before kraze connected it) falls back to the first
// control-plane
func (kind *KindManager) patchKubeconfigWithContainerIP(clusterName, kubeconfig string, customNetwork string, quiet ...bool) (string, error) {
	shouldPrint := len(quiet) == 0 || !quiet[0]
	containers := kind.apiServerContainers(clusterName)

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
		for iter, containerName := range containers {
			output, err := kind.run.Output(context.Background(), "docker", "inspect", containerName,
				"-f", networkIPTemplate(network))
			if err != nil {
				continue
			}
			containerIP := strings.Trim(strings.TrimSpace(string(output)), "\"")
			if containerIP == "" || containerIP == "<no value>" {
				continue
			}
			// Found IP on this network
			if shouldPrint {
				switch {
				case len(containers) == 1:
					fmt.Printf("%s Using container IP %s from '%s' network\n", color.Checkmark(), containerIP, network)
				case iter == 0:
					fmt.Printf("%s Using load balancer IP %s from '%s' network\n", color.Checkmark(), containerIP, network)
				default:
					fmt.Printf("Note: the cluster's load balancer isn't on the '%s' network, so the API is reached through '%s' (%s) only; recreate the cluster to use the load balancer\n",
						network, containerName, containerIP)
				}
			}
			return rewriteAPIServer(kubeconfig, clusterName, "https://"+containerIP+":6443"), nil
		}
	}

	// Fallback: get any available IP
	for _, containerName := range containers {
		output, err := kind.run.Output(context.Background(), "docker", "inspect", containerName,
			"-f", "{{range .NetworkSettings.Networks}}{{.IPAddress}} {{end}}")
		if err != nil {
			continue
		}

		// Take the first IP from the space-separated list
		if ips := strings.Fields(string(output)); len(ips) > 0 {
			return rewriteAPIServer(kubeconfig, clusterName, "https://"+ips[0]+":6443"), nil
		}
	}
	return "", fmt.Errorf("no IP found for %s", strings.Join(containers, " or "))
}

// UpdateKubeconfigFile updates ~/.kube/config with cluster access, patched for dev container compatibility
// This ensures kubectl works inside dev containers by using container IP instead of 127.0.0.1
func (kind *KindManager) UpdateKubeconfigFile(clusterName string) error {
	config, err := kind.kubectlConfig(clusterName)
	if err != nil {
		return err
	}
	contextName := "kind-" + clusterName

	// Get path to user's kubeconfig
	kubeconfigPath := kind.host.kubeconfigPath

	// Load existing kubeconfig or create new one
	existingConfig, err := kind.host.loadKubeconfig()
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

// kubectlConfig is the kubeconfig UpdateKubeconfigFile writes for kubectl:
// kind's internal one, patched for dev containers, with TLS verification off
// (the certificate doesn't name the container IP)
func (kind *KindManager) kubectlConfig(clusterName string) (*clientcmdapi.Config, error) {
	// Get patched kubeconfig with container IP (quiet mode to avoid duplicate messages)
	kubeconfigContent, err := kind.GetKubeConfigQuiet(clusterName, true, true)
	if err != nil {
		return nil, fmt.Errorf("failed to get kubeconfig: %w", err)
	}

	// Parse the kubeconfig
	config, err := clientcmd.Load([]byte(kubeconfigContent))
	if err != nil {
		return nil, fmt.Errorf("failed to parse kubeconfig: %w", err)
	}

	// Add insecure-skip-tls-verify to the cluster (needed because cert is for 127.0.0.1, not container IP)
	if cluster, exists := config.Clusters["kind-"+clusterName]; exists {
		cluster.InsecureSkipTLSVerify = true
		cluster.CertificateAuthorityData = nil // Remove CA data when using insecure
	}
	return config, nil
}

// RefreshKubeconfigFile brings the cluster's entry in ~/.kube/config up to
// date when its API server address no longer is: the container's IP changed
// after a restart, or the load balancer joined the network (RepairClusterNetwork
// on a cluster an older kraze created). Unlike UpdateKubeconfigFile it changes
// only that address, and leaves the current context, and a kubeconfig without
// the entry, alone
func (kind *KindManager) RefreshKubeconfigFile(clusterName string) error {
	key := "kind-" + clusterName
	existing, err := kind.host.loadKubeconfig()
	if err != nil {
		return nil // no kubeconfig to refresh
	}
	entry := existing.Clusters[key]
	if entry == nil {
		return nil
	}

	config, err := kind.kubectlConfig(clusterName)
	if err != nil {
		return err
	}
	fresh := config.Clusters[key]
	if fresh == nil || fresh.Server == entry.Server {
		return nil
	}

	old := entry.Server
	entry.Server = fresh.Server
	entry.InsecureSkipTLSVerify = fresh.InsecureSkipTLSVerify
	entry.CertificateAuthorityData = fresh.CertificateAuthorityData
	if err := clientcmd.WriteToFile(*existing, kind.host.kubeconfigPath); err != nil {
		return fmt.Errorf("failed to write kubeconfig: %w", err)
	}
	fmt.Printf("%s Updated kubeconfig context '%s': API server %s -> %s\n", color.Checkmark(), key, old, fresh.Server)
	return nil
}

// parseK8sVersion returns a human-readable Kubernetes version string for display.
// Extracts the version tag from the node image, or returns "default" if none is configured.
func (kind *KindManager) parseK8sVersion(cfg *config.ClusterConfig) string {
	image := kind.getNodeImage(cfg)
	if image == "" {
		image = defaults.Image
	}
	// Strip digest first (e.g. "kindest/node:v1.35.0@sha256:abc" -> "kindest/node:v1.35.0")
	if atIdx := strings.Index(image, "@"); atIdx != -1 {
		image = image[:atIdx]
	}
	// Extract tag (e.g. "kindest/node:v1.35.0" -> "v1.35.0")
	if colonIdx := strings.LastIndex(image, ":"); colonIdx != -1 {
		return image[colonIdx+1:]
	}
	return image
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

	// CA certificates and the GODEBUG drop-in aren't bind-mounted: a mount
	// names a path on the Docker daemon's host, which isn't where kraze runs
	// in a dev container using the host's daemon. CreateCluster writes them
	// into the nodes instead.
	gpuMounts := kind.buildGPUMounts(cfg)

	// If no nodes specified in config, create a default control-plane node
	// GPU mounts are applied to this node since it handles all workloads
	if len(cfg.Config) == 0 {
		node := v1alpha4.Node{
			Role:        v1alpha4.ControlPlaneRole,
			ExtraMounts: gpuMounts,
		}
		if cfg.GPU.IsNvidiaEnabled() {
			node.GPUs = "all"
		}
		if nodeImage != "" {
			node.Image = nodeImage
		}
		kindCfg.Nodes = append(kindCfg.Nodes, node)
		return kindCfg
	}

	// Determine whether to apply GPU mounts to worker nodes or fall back to control-plane
	hasWorker := false
	if cfg.GPU.IsAnyEnabled() {
		for _, node := range cfg.Config {
			if node.Role == "worker" {
				hasWorker = true
				break
			}
		}
	}

	// Convert kraze nodes to kind nodes
	for _, node := range cfg.Config {
		kindNode := kind.buildKindNode(node)

		// Set the node image if specified
		if nodeImage != "" {
			kindNode.Image = nodeImage
		}

		// Add GPU mounts: to worker nodes when workers exist, otherwise to control-plane
		applyGPU := kindNode.Role == v1alpha4.WorkerRole || (!hasWorker && kindNode.Role == v1alpha4.ControlPlaneRole)
		if applyGPU {
			kindNode.ExtraMounts = append(kindNode.ExtraMounts, gpuMounts...)
			if cfg.GPU.IsNvidiaEnabled() {
				kindNode.GPUs = "all"
			}
		}

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
	defer func() { _ = os.RemoveAll(tmpDir) }()

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
			if _, err := kind.run.Output(ctx, "docker", "inspect", imageName); err == nil {
				// Image exists, tag it with the repo:tag format
				if _, err := kind.run.Output(ctx, "docker", "tag", imageName, repoTag); err == nil {
					saveImageRef = repoTag
				}
			}
		}
	}

	// Use docker save to export the image
	if _, err := kind.run.Output(ctx, "docker", "save", "-o", imageTar, saveImageRef); err != nil {
		return fmt.Errorf("failed to save image '%s': %w (make sure the image exists locally)", imageName, err)
	}

	if err := kind.loadArchive(imageTar, nodes); err != nil {
		return fmt.Errorf("failed to load image '%s': %w", imageName, err)
	}

	return nil
}

// loadArchive loads an image archive onto every node, as `kind load
// docker-image` does: concurrently, and layer-aware where possible, so layers
// a node already has aren't sent again. An archive layer-aware loading can't
// read, or a failed layer-aware load on a node, falls back to loading the full
// archive there. Unlike kind, it waits for every node before returning, since
// the caller removes the archive once it returns; every node that failed is
// reported.
func (kind *KindManager) loadArchive(archive string, nodeList []nodes.Node) error {
	metadata, inspectErr := kind.loadImages.inspect(archive)
	errs := make([]error, len(nodeList))
	var wg sync.WaitGroup
	for iter, node := range nodeList {
		wg.Go(func() {
			layerErr := inspectErr
			if layerErr == nil {
				if layerErr = kind.loadImages.layerAware(archive, metadata, node); layerErr == nil {
					return
				}
			}
			if err := kind.loadImages.full(archive, node); err != nil {
				errs[iter] = fmt.Errorf("failed to load image onto node %s: %w (layer-aware load: %v)", node.String(), err, layerErr)
			}
		})
	}
	wg.Wait()
	return errors.Join(errs...)
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
	output, err := kind.run.CombinedOutput(ctx, "", "docker", "exec", containerName,
		"ctr", "-n", "k8s.io", "images", "rm", clusterImageName)

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
		if _, err := kind.run.Output(context.Background(), "docker", "exec", containerName, "test", "-d", "/sys/fs/cgroup/systemd"); err != nil {
			// Not cgroup v1 (likely v2), skip this workaround
			continue
		}

		// Check if the directory already exists
		cgroupPath := "/sys/fs/cgroup/systemd/kubelet.slice/kubelet-kubepods.slice"
		if _, err := kind.run.Output(context.Background(), "docker", "exec", containerName, "test", "-d", cgroupPath); err == nil {
			// Directory already exists, no need to create it
			continue
		}

		// Create the kubelet cgroup directory structure
		// This prevents: "Failed to start ContainerManager: cgroup [...] has some missing paths"
		if output, err := kind.run.CombinedOutput(context.Background(), "", "docker", "exec", containerName, "mkdir", "-p", cgroupPath); err != nil {
			// This is a workaround, so we return error but don't fail hard
			return fmt.Errorf("failed to create kubelet cgroup directory %s in node %s: %w\nOutput: %s",
				cgroupPath, containerName, err, string(output))
		}
	}

	return nil
}

// caUpdateTimeout bounds update-ca-certificates in a node, which takes well
// under a second; a variable so tests run fast
var caUpdateTimeout = 30 * time.Second

// updateCACertificates copies the CA certificates into all nodes and runs
// update-ca-certificates there. containerd picks them up when it restarts
// (configureContainerdGODEBUG).
func (kind *KindManager) updateCACertificates(ctx context.Context, clusterName string, certPaths []string) error {
	fmt.Printf("Updating CA certificates in cluster nodes...\n")

	certs := make([]string, len(certPaths))
	for iter, certPath := range certPaths {
		data, err := kind.host.readFile(certPath)
		if err != nil {
			return fmt.Errorf("failed to read CA certificate: %w", err)
		}
		certs[iter] = string(data)
	}

	// Get cluster nodes
	nodes, err := kind.provider.ListInternalNodes(clusterName)
	if err != nil {
		return fmt.Errorf("failed to list cluster nodes: %w", err)
	}

	// Run update-ca-certificates in each node
	for _, node := range nodes {
		containerName := node.String()

		// The file name must end in .crt for update-ca-certificates
		for iter, cert := range certs {
			path := fmt.Sprintf("%s/kraze-ca-%d.crt", nodeCACertificateDir, iter)
			if err := kind.writeNodeFile(containerName, path, cert); err != nil {
				return fmt.Errorf("failed to write CA certificate in node %s: %w", containerName, err)
			}
		}

		// A timeout prevents hanging: update-ca-certificates completes in seconds.
		// (The timeout(1) command this used isn't on macOS.)
		nodeCtx, cancel := context.WithTimeout(ctx, caUpdateTimeout)
		output, err := kind.run.CombinedOutput(nodeCtx, "", "docker", "exec", containerName, "update-ca-certificates")
		timedOut := nodeCtx.Err() == context.DeadlineExceeded
		cancel()

		if err != nil {
			if timedOut {
				return fmt.Errorf("update-ca-certificates timed out after %v in node %s\nOutput: %s",
					caUpdateTimeout, containerName, string(output))
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

		// Point containerd at certs.d (v2 registry format) before writing the
		// hosts.toml files
		if err := kind.runInNode(containerName, appendRegistryConfigScript, containerdConfigPath, containerdRegistryConfigPatch); err != nil {
			return fmt.Errorf("failed to patch containerd config in node %s: %w", containerName, err)
		}

		for _, registry := range registries {
			dir, hostsTOML, err := registryHostsConfig(registry)
			if err != nil {
				return err
			}
			if err := kind.writeNodeFile(containerName, dir+"/hosts.toml", hostsTOML); err != nil {
				return fmt.Errorf("failed to write hosts.toml for %s in node %s: %w", registry, containerName, err)
			}
		}

		if err := kind.restartContainerd(containerName); err != nil {
			return err
		}
	}

	fmt.Printf("%s Insecure registries configured successfully\n", color.Checkmark())
	return nil
}

// configureProxy configures containerd to use HTTP/HTTPS proxy
// This is applied AFTER cluster initialization to avoid breaking kubeadm init
func (kind *KindManager) configureProxy(clusterName, httpProxy, httpsProxy, noProxy string) error {
	fmt.Printf("Configuring proxy settings in cluster nodes...\n")

	// Inform user about proxy configuration source (without passwords)
	fmt.Printf("  HTTP_PROXY=%s\n", redactProxyURL(httpProxy))
	fmt.Printf("  HTTPS_PROXY=%s\n", redactProxyURL(httpsProxy))
	fmt.Printf("  NO_PROXY=%s\n", noProxy)

	// Get cluster nodes
	nodes, err := kind.provider.ListInternalNodes(clusterName)
	if err != nil {
		return fmt.Errorf("failed to list cluster nodes: %w", err)
	}

	// Configure each node
	for _, node := range nodes {
		containerName := node.String()

		if err := kind.writeNodeFile(containerName, containerdProxyConf, containerdProxyDropIn(httpProxy, httpsProxy, noProxy)); err != nil {
			return fmt.Errorf("failed to write proxy config in node %s: %w", containerName, err)
		}

		// A unit's Environment= applies when the process starts, so containerd
		// (which pulls images) must restart. Running containers are kept: their
		// shims don't depend on the containerd process.
		if err := kind.runInNode(containerName, "systemctl daemon-reload", "", ""); err != nil {
			return fmt.Errorf("failed to reload systemd in node %s: %w", containerName, err)
		}
		if err := kind.restartContainerd(containerName); err != nil {
			return err
		}
	}

	fmt.Printf("%s Proxy configured successfully\n", color.Checkmark())
	return nil
}

// buildGPUMounts returns GPU-related extra mounts for a node.
// For NVIDIA: mounts nvidia-container-runtime and nvidia-ctk into the kind node.
// For AMD: mounts /dev/kfd and /dev/dri/renderD* device files.
func (kind *KindManager) buildGPUMounts(cfg *config.ClusterConfig) []v1alpha4.Mount {
	var mounts []v1alpha4.Mount
	mounts = append(mounts, kind.buildNvidiaGPUMounts(cfg)...)
	mounts = append(mounts, kind.buildAMDGPUMounts(cfg)...)
	return mounts
}

// buildNvidiaGPUMounts mounts NVIDIA toolkit binaries into the kind node.
//
// nvidia-container-runtime is registered as containerd's nvidia OCI runtime handler.
// nvidia-ctk is called by nvidia-container-runtime as an OCI hook to inject GPU
// devices and libraries into containers. Both must be present inside the kind node
// at the same paths they occupy on the host.
func (kind *KindManager) buildNvidiaGPUMounts(cfg *config.ClusterConfig) []v1alpha4.Mount {
	if !cfg.GPU.IsNvidiaEnabled() {
		return nil
	}
	var mounts []v1alpha4.Mount
	for _, bin := range []string{"nvidia-container-runtime", "nvidia-ctk"} {
		binPath, err := kind.host.lookPath(bin)
		if err == nil && binPath != "" {
			mounts = append(mounts, v1alpha4.Mount{
				HostPath:      binPath,
				ContainerPath: binPath,
				Readonly:      true,
			})
		}
	}
	return mounts
}

// buildAMDGPUMounts produces bind-mounts for AMD GPU device files.
//
// All available GPU devices are mounted so the cluster has full access to host
// hardware. Applications should use ROCR_VISIBLE_DEVICES or HIP_VISIBLE_DEVICES
// to restrict which GPUs a workload uses.
//
//   - /dev/kfd is the Kernel Fusion Driver shared by all AMD GPUs (required by ROCm).
//     It exposes all GPUs regardless, so restricting by device count provides no isolation.
//   - /dev/dri/renderD<128+i> is the DRM render node for GPU i.
//     renderD numbering always starts at 128 on Linux.
func (kind *KindManager) buildAMDGPUMounts(cfg *config.ClusterConfig) []v1alpha4.Mount {
	if !cfg.GPU.IsAMDEnabled() {
		return nil
	}
	mounts := []v1alpha4.Mount{{
		HostPath:      "/dev/kfd",
		ContainerPath: "/dev/kfd",
	}}
	for i := 0; ; i++ {
		device := fmt.Sprintf("/dev/dri/renderD%d", 128+i)
		if !kind.host.fileExists(device) {
			break
		}
		mounts = append(mounts, v1alpha4.Mount{
			HostPath:      device,
			ContainerPath: device,
		})
	}
	return mounts
}

// configureContainerdGODEBUG writes the GODEBUG drop-in
// (containerdGODEBUGDropIn) into every node and restarts containerd, since a
// unit's Environment= only applies when the process starts. Running
// containers are kept: their shims don't depend on the containerd process.
func (kind *KindManager) configureContainerdGODEBUG(clusterName string) error {
	nodes, err := kind.provider.ListInternalNodes(clusterName)
	if err != nil {
		return fmt.Errorf("failed to list cluster nodes: %w", err)
	}
	for _, node := range nodes {
		containerName := node.String()
		if err := kind.writeNodeFile(containerName, containerdGODEBUGConf, containerdGODEBUGDropIn); err != nil {
			return fmt.Errorf("failed to write GODEBUG config in node %s: %w", containerName, err)
		}
		if err := kind.runInNode(containerName, "systemctl daemon-reload", "", ""); err != nil {
			return fmt.Errorf("failed to reload systemd in node %s: %w", containerName, err)
		}
		if err := kind.restartContainerd(containerName); err != nil {
			return err
		}
	}
	return nil
}

// validateNvidiaGPUPrerequisites checks that nvidia-container-toolkit is installed.
// The default Docker runtime no longer needs to be changed — GPU passthrough uses
// Docker's --gpus flag (DeviceRequests) which works with the standard runc runtime.
// enrichClusterCreateError inspects the raw error from kind's provider.Create and
// returns a more actionable message for known failure modes. kind runs docker as a
// subprocess and only surfaces the exit code in the error string; the actual Docker
// error message is captured in RunError.Output and is otherwise silently discarded.
//
// This is especially important in Docker-outside-of-Docker environments (devcontainers)
// where pre-flight port checks cannot see the host network namespace.
func enrichClusterCreateError(err error) error {
	var runErr *kindexec.RunError
	if !errors.As(err, &runErr) || len(runErr.Output) == 0 {
		return err
	}

	output := string(runErr.Output)

	// Port already in use: "failed to bind host port for 0.0.0.0:8081:172.19.0.2:30801/tcp: address already in use"
	portRe := regexp.MustCompile(`bind host port for [^:]+:(\d+):[^/]+/(\w+): address already in use`)
	if m := portRe.FindStringSubmatch(output); m != nil {
		port, proto := m[1], strings.ToUpper(m[2])
		ssFlag := "-tlnup"
		if strings.EqualFold(proto, "UDP") {
			ssFlag = "-ulnup"
		}
		return fmt.Errorf(
			"host port %s/%s is already in use\n"+
				"  Another process (or VS Code port forwarding) is holding this port.\n"+
				"  To find it: ss %s | grep %s\n"+
				"  Docker output: %s",
			port, proto, ssFlag, port, strings.TrimSpace(output),
		)
	}

	// Device not found: "error gathering device information while adding custom device ... no such file"
	deviceRe := regexp.MustCompile(`(?i)(no such file|cannot open|device not found)[^\n]*(/dev/\S+)`)
	if m := deviceRe.FindStringSubmatch(output); m != nil {
		return fmt.Errorf(
			"required device %s not found on this host\n"+
				"  Check that the GPU device exists before starting the cluster.\n"+
				"  Docker output: %s",
			m[2], strings.TrimSpace(output),
		)
	}

	// Generic fallback: at least show what Docker said.
	return fmt.Errorf("%w\n  Docker output: %s", err, strings.TrimSpace(output))
}

func (kind *KindManager) validateNvidiaGPUPrerequisites() error {
	// Both binaries must be present: nvidia-container-runtime is registered as
	// containerd's nvidia OCI runtime handler; nvidia-ctk is called by it as an OCI
	// hook to inject GPU devices and libraries into containers.
	for _, bin := range []string{"nvidia-container-runtime", "nvidia-ctk"} {
		if _, err := kind.host.lookPath(bin); err != nil {
			return fmt.Errorf(
				"%s not found in PATH — is nvidia-container-toolkit installed?\n"+
					"Install from: https://docs.nvidia.com/datacenter/cloud-native/container-toolkit/install-guide.html",
				bin,
			)
		}
	}
	return nil
}

// validateAMDGPUPrerequisites verifies the host has the AMD GPU device files
// required for kind GPU passthrough:
//   - /dev/kfd: Kernel Fusion Driver, shared interface for all AMD GPUs (required by ROCm)
//   - /dev/dri/renderD128: at least one DRM render node must be present
func (kind *KindManager) validateAMDGPUPrerequisites() error {
	const prereqHelp = `
Ensure the ROCm stack and AMDGPU kernel driver are installed on the host:
  https://rocm.docs.amd.com/en/latest/deploy/linux/index.html

After installation, verify:
  ls -la /dev/kfd /dev/dri/renderD128`

	if _, err := kind.host.stat("/dev/kfd"); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("/dev/kfd not found — is the ROCm/AMDGPU driver loaded?%s", prereqHelp)
		}
		return fmt.Errorf("failed to stat /dev/kfd: %w", err)
	}

	if _, err := kind.host.stat("/dev/dri/renderD128"); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("/dev/dri/renderD128 not found — no AMD GPUs detected%s", prereqHelp)
		}
		return fmt.Errorf("failed to stat /dev/dri/renderD128: %w", err)
	}

	return nil
}

// registerNvidiaRuntimeClass creates the nvidia RuntimeClass in the cluster.
// This allows pods to request GPU access via runtimeClassName: nvidia.
func (kind *KindManager) registerNvidiaRuntimeClass(ctx context.Context, clusterName string) error {
	kubeconfig, err := kind.GetKubeConfigQuiet(clusterName, true, true)
	if err != nil {
		return fmt.Errorf("failed to get kubeconfig: %w", err)
	}

	restConfig, err := clientcmd.RESTConfigFromKubeConfig([]byte(kubeconfig))
	if err != nil {
		return fmt.Errorf("failed to build REST config: %w", err)
	}
	// Use insecure TLS since kubeconfig uses the container IP (same pattern as UpdateKubeconfigFile)
	restConfig.Insecure = true
	restConfig.CAData = nil

	dynClient, err := kind.clients.dynamic(restConfig)
	if err != nil {
		return fmt.Errorf("failed to create dynamic client: %w", err)
	}

	runtimeClassGVR := schema.GroupVersionResource{
		Group:    "node.k8s.io",
		Version:  "v1",
		Resource: "runtimeclasses",
	}

	obj := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "node.k8s.io/v1",
			"kind":       "RuntimeClass",
			"metadata": map[string]interface{}{
				"name": "nvidia",
			},
			"handler": "nvidia",
		},
	}

	existing, err := dynClient.Resource(runtimeClassGVR).Get(ctx, "nvidia", metav1.GetOptions{})
	if err != nil {
		if !k8serrors.IsNotFound(err) {
			return fmt.Errorf("failed to check for existing RuntimeClass: %w", err)
		}
		// Does not exist — create it
		if _, err := dynClient.Resource(runtimeClassGVR).Create(ctx, obj, metav1.CreateOptions{}); err != nil {
			return fmt.Errorf("failed to create NVIDIA RuntimeClass: %w", err)
		}
	} else {
		// Already exists — update, preserving the resourceVersion
		obj.SetResourceVersion(existing.GetResourceVersion())
		if _, err := dynClient.Resource(runtimeClassGVR).Update(ctx, obj, metav1.UpdateOptions{}); err != nil {
			return fmt.Errorf("failed to update NVIDIA RuntimeClass: %w", err)
		}
	}

	fmt.Printf("%s NVIDIA RuntimeClass registered\n", color.Checkmark())
	return nil
}

// buildContainerdConfigPatches creates containerd configuration patches
func (kind *KindManager) buildContainerdConfigPatches(cfg *config.ClusterConfig) []string {
	var patches []string

	// Note: We do NOT configure insecure registries here via config_path
	// Setting config_path would tell containerd to ONLY use /etc/containerd/certs.d/
	// which is empty during kubeadm init, breaking Docker Hub access.
	// Insecure registries are configured AFTER cluster init via configureInsecureRegistries()

	// Configure NVIDIA GPU support in containerd when NVIDIA GPUs are used.
	// Registers the "nvidia" runtime handler using nvidia-container-runtime, which
	// intercepts NVIDIA_VISIBLE_DEVICES and injects GPU devices and libraries.
	// Applied before containerd starts, so no restart is needed.
	if cfg.GPU.IsNvidiaEnabled() {
		// nvidia-container-runtime is validated and mounted into the kind node by
		// buildNvidiaGPUMounts(). Use LookPath result directly — no fallback needed.
		nvidiaRuntimePath, _ := kind.host.lookPath("nvidia-container-runtime")
		patches = append(patches, fmt.Sprintf(`[plugins."io.containerd.grpc.v1.cri".containerd.runtimes.nvidia]
  runtime_type = "io.containerd.runc.v2"
  [plugins."io.containerd.grpc.v1.cri".containerd.runtimes.nvidia.options]
    BinaryName = %q`, nvidiaRuntimePath))
	}

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

	// Set node-level kubeadm config patches (applied after any cluster-level patches)
	if len(node.KubeadmConfigPatches) > 0 {
		kindNode.KubeadmConfigPatches = node.KubeadmConfigPatches
	}

	return kindNode
}

// connectToHostNetwork connects the kind cluster to the specified or auto-detected Docker network
// This enables connectivity in Docker-in-Docker environments like dev containers
// Parameters:
// - clusterName: name of the kind cluster
// - networkName: explicit network name (optional, auto-detected if empty)
// - subnet: network subnet for creation (optional, e.g., "172.1.0.0/16")
// - ipv4Address: static IP for the control-plane container (optional)
//
// The control-plane decides which network is used: the first one it can join.
// Every other node then joins it too, so fixDefaultRoutes can route each
// node's traffic through it; a node left off the network keeps kind's route.
func (kind *KindManager) connectToHostNetwork(clusterName string, networkName string, subnet string, ipv4Address string) error {
	controlPlane := clusterName + "-control-plane"

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
		// Auto-detect networks (never empty: bridge is the fallback)
		networksToTry = kind.detectNetworks()
	}

	for _, network := range networksToTry {
		// Check if network exists
		if _, err := kind.run.Output(context.Background(), "docker", "network", "inspect", network); err != nil {
			continue
		}

		// Try to connect to this network, with the static IP if one is set
		connectArgs := []string{"network", "connect"}
		if ipv4Address != "" {
			connectArgs = append(connectArgs, "--ip", ipv4Address)
		}
		connectArgs = append(connectArgs, network, controlPlane)

		if _, err := kind.run.Output(context.Background(), "docker", connectArgs...); err != nil {
			// Might already be connected or other error, try next network
			continue
		}

		if ipv4Address != "" {
			fmt.Printf("%s Connected cluster to '%s' network with IP %s\n", color.Checkmark(), network, ipv4Address)
		} else {
			fmt.Printf("%s Connected cluster to '%s' network for better connectivity\n", color.Checkmark(), network)
		}

		kind.joinNetwork(clusterName, network, false)
		return nil
	}

	// No networks worked, return error
	return fmt.Errorf("could not connect to any common Docker network")
}

// joinNetwork brings the rest of the cluster onto the network the
// control-plane joined: the other nodes (with an address Docker assigns: a
// static IP is the control-plane's) and the load balancer, and points each
// node's default route at the network's gateway.
//
// For a new cluster (repair false) every node is connected. Repairing an
// existing one, on a later up, connects only what isn't on the network (nodes
// of a cluster an older kraze created) and changes only routes that aren't
// via the gateway (a route set in a node doesn't survive a restart of it, or
// of Docker), saying what it changed
func (kind *KindManager) joinNetwork(clusterName, network string, repair bool) {
	controlPlane := clusterName + "-control-plane"
	connect := func(what, container string) bool {
		if repair && kind.onNetwork(container, network) {
			return true
		}
		if out, err := kind.run.CombinedOutput(context.Background(), "", "docker", "network", "connect", network, container); err != nil {
			fmt.Printf("Warning: Could not connect %s '%s' to '%s' network: %v: %s\n", what, container, network, err, strings.TrimSpace(string(out)))
			return false
		}
		fmt.Printf("%s Connected %s '%s' to '%s' network\n", color.Checkmark(), what, container, network)
		return true
	}

	connected := []string{controlPlane}
	for _, node := range kind.clusterNodeNames(clusterName) {
		if node != controlPlane && connect("node", node) {
			connected = append(connected, node)
		}
	}

	// With several control-planes, kind puts a load balancer in front of
	// them: on this network too, the kubeconfig can use it, and the API
	// stays reachable while a control-plane is down. It needs no route:
	// it only proxies to the control-planes on kind's network
	if lb := kind.loadBalancerName(clusterName); lb != "" {
		connect("load balancer", lb)
	}

	// Fix the default route inside every connected node so internet traffic
	// flows through this network's NAT rules. Without this, nodes with two
	// network interfaces (kind + this network) keep routing internet traffic
	// through the kind interface, which may lack working iptables masquerade
	// rules.
	if err := kind.fixDefaultRoutes(network, connected, repair); err != nil {
		fmt.Printf("Warning: Could not update default route via '%s': %v\n", network, err)
	}
}

// onNetwork reports whether a container has an address on the network
func (kind *KindManager) onNetwork(container, network string) bool {
	out, err := kind.run.Output(context.Background(), "docker", "inspect", container, "-f", networkIPTemplate(network))
	ip := strings.Trim(strings.TrimSpace(string(out)), "\"")
	return err == nil && ip != "" && ip != "<no value>"
}

// RepairClusterNetwork restores, on an existing kind cluster, what
// connectToHostNetwork set up when it was created and what an older kraze
// left out: see joinNetwork. The network is the one the control-plane joined:
// network if it's set, else the first auto-detected one it's on. A cluster
// whose control-plane joined none is left alone. Changes are printed;
// nothing is printed for a cluster that needs none
func (kind *KindManager) RepairClusterNetwork(clusterName, network string) {
	networks := kind.detectNetworks()
	if network != "" {
		networks = []string{network}
	}
	for _, candidate := range networks {
		if kind.onNetwork(clusterName+"-control-plane", candidate) {
			kind.joinNetwork(clusterName, candidate, true)
			return
		}
	}
}

// clusterNodeNames lists the cluster's node containers, or just the
// control-plane if kind can't list them (with a warning: the other nodes
// then stay off the network)
func (kind *KindManager) clusterNodeNames(clusterName string) []string {
	nodeList, err := kind.provider.ListInternalNodes(clusterName)
	if err != nil || len(nodeList) == 0 {
		fmt.Printf("Warning: Could not list the nodes of cluster '%s' (%v); only the control-plane is connected\n", clusterName, err)
		return []string{clusterName + "-control-plane"}
	}
	names := make([]string, len(nodeList))
	for iter, node := range nodeList {
		names[iter] = node.String()
	}
	// kind lists them in no particular order
	slices.Sort(names)
	return names
}

// fixDefaultRoutes updates the default route inside the given nodes, which
// must be connected to the network, to use the network's gateway. This ensures
// outbound internet traffic is NATted through that network's iptables rules
// rather than through the kind-internal network, which may have no NAT.
func (kind *KindManager) fixDefaultRoutes(networkName string, nodeNames []string, repair bool) error {
	// Get the gateway IP for this network
	out, err := kind.run.Output(context.Background(), "docker", "network", "inspect", networkName,
		"--format", "{{range .IPAM.Config}}{{.Gateway}}{{end}}")
	if err != nil {
		return fmt.Errorf("inspect network %s: %w", networkName, err)
	}
	gateway := strings.TrimSpace(string(out))
	if gateway == "" {
		return fmt.Errorf("no gateway found for network %s", networkName)
	}

	for _, node := range nodeNames {
		// Repairing, a route that's already right is left alone, quietly
		if repair {
			current, err := kind.run.Output(context.Background(), "docker", "exec", node, "ip", "route", "show", "default")
			if err == nil && strings.HasPrefix(string(current), "default via "+gateway+" ") {
				continue
			}
		}
		routeOut, err := kind.run.CombinedOutput(context.Background(), "", "docker", "exec", node,
			"ip", "route", "replace", "default", "via", gateway)
		switch {
		case err != nil:
			fmt.Printf("Warning: Could not update default route in node '%s': %v: %s\n", node, err, routeOut)
		case repair:
			fmt.Printf("%s Restored default route in '%s' to use '%s' gateway (%s)\n",
				color.Checkmark(), node, networkName, gateway)
		default:
			fmt.Printf("%s Default route in '%s' updated to use '%s' gateway (%s)\n",
				color.Checkmark(), node, networkName, gateway)
		}
	}
	return nil
}

// ensureNetworkExists checks if a Docker network exists and creates it if needed
func (kind *KindManager) ensureNetworkExists(networkName string, subnet string) error {
	// Check if network already exists
	if _, err := kind.run.Output(context.Background(), "docker", "network", "inspect", networkName); err == nil {
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
	if output, err := kind.run.CombinedOutput(context.Background(), "", "docker", "network", "create",
		"--driver", "bridge",
		"--subnet", subnet,
		networkName); err != nil {
		return fmt.Errorf("failed to create network: %w\nOutput: %s", err, string(output))
	}

	fmt.Printf("%s Created Docker network '%s'\n", color.Checkmark(), networkName)
	return nil
}

// detectNetworks detects which Docker networks to use based on the current environment.
// On bare metal hosts, connects to the Docker bridge network which ensures kind nodes
// have outbound internet access via the host's NAT rules.
// Inside a container (dev container / CI), connects to the container's own networks
// so the kind cluster is reachable on the same Docker network as the host container.
func (kind *KindManager) detectNetworks() []string {
	if kind.isRunningInContainer() {
		// We're in a container - try to get its networks
		currentNetworks := kind.getCurrentContainerNetworks()
		if len(currentNetworks) > 0 {
			// Found networks from current container - use those plus bridge as fallback
			return append(currentNetworks, "bridge")
		}
	}

	// Bare metal host or fallback: use bridge for internet connectivity
	return []string{"bridge"}
}

// isRunningInContainer checks if we're running inside a Docker container
func (kind *KindManager) isRunningInContainer() bool {
	// Method 1: Check for /.dockerenv file
	if kind.host.fileExists("/.dockerenv") {
		return true
	}

	// Method 2: Check /proc/self/cgroup for docker/containerd
	data, err := kind.host.readFile("/proc/self/cgroup")
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
	hostname, err := kind.host.hostname()
	if err != nil {
		return nil
	}

	// Try to inspect this container by hostname
	output, err := kind.run.Output(context.Background(), "docker", "inspect", hostname,
		"-f", "{{range $net, $config := .NetworkSettings.Networks}}{{$net}} {{end}}")
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

// GetKubeconfigForExternalCluster returns the kubeconfig content for an external cluster
func (kind *KindManager) GetKubeconfigForExternalCluster(cfg *config.ClusterConfig) (string, error) {
	if cfg.External == nil || !cfg.External.Enabled {
		return "", fmt.Errorf("cluster is not configured as external")
	}

	// Use specified kubeconfig or default
	kubeconfigPath := cfg.External.Kubeconfig
	if kubeconfigPath == "" {
		// Use default kubeconfig location
		home, err := kind.host.homeDir()
		if err != nil {
			return "", fmt.Errorf("failed to get user home directory: %w", err)
		}
		kubeconfigPath = filepath.Join(home, ".kube", "config")
	} else {
		// Expand ~ if present
		if strings.HasPrefix(kubeconfigPath, "~/") {
			home, err := kind.host.homeDir()
			if err != nil {
				return "", fmt.Errorf("failed to get user home directory: %w", err)
			}
			kubeconfigPath = filepath.Join(home, kubeconfigPath[2:])
		}
	}

	// Verify kubeconfig file exists
	if _, err := kind.host.stat(kubeconfigPath); err != nil {
		return "", fmt.Errorf("kubeconfig file not found: %s", kubeconfigPath)
	}

	// Read the kubeconfig file content
	content, err := kind.host.readFile(kubeconfigPath)
	if err != nil {
		return "", fmt.Errorf("failed to read kubeconfig file: %w", err)
	}

	return string(content), nil
}

// VerifyClusterAccess verifies that the external cluster is accessible
func (kind *KindManager) VerifyClusterAccess(ctx context.Context, kubeconfigContent string) error {
	// Parse kubeconfig from content
	clientConfig, err := clientcmd.NewClientConfigFromBytes([]byte(kubeconfigContent))
	if err != nil {
		return fmt.Errorf("failed to parse kubeconfig: %w", err)
	}

	// Get REST config
	restConfig, err := clientConfig.ClientConfig()
	if err != nil {
		return fmt.Errorf("failed to create REST config: %w", err)
	}

	// Create Kubernetes client
	clientset, err := kind.clients.clientset(restConfig)
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
