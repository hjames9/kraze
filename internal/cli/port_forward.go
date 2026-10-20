package cli

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
)

var (
	portForwardLabels []string
	portForwardPod    string
)

var portForwardCmd = &cobra.Command{
	Use:   "port-forward SERVICE [LOCAL_PORT:]REMOTE_PORT [...[LOCAL_PORT:]REMOTE_PORT]",
	Short: "Forward one or more local ports to a service",
	Long: `Forward one or more local ports to a service running in the cluster.

The command will find a pod associated with the service and forward the specified ports.

Examples:
  kraze port-forward redis 6379           # Forward local 6379 to remote 6379
  kraze port-forward redis 6380:6379      # Forward local 6380 to remote 6379
  kraze port-forward web 8080:80 8443:443 # Forward multiple ports
  kraze port-forward redis 6379 --pod redis-master-0  # Forward to specific pod`,
	Args: cobra.MinimumNArgs(2),
	RunE: runPortForward,
}

// PortMapping represents a port forwarding mapping
type PortMapping struct {
	LocalPort  int
	RemotePort int
}

func parsePortMapping(portStr string) (*PortMapping, error) {
	parts := strings.Split(portStr, ":")

	var localPort, remotePort int
	var err error

	if len(parts) == 1 {
		// Just remote port, use same for local
		remotePort, err = strconv.Atoi(parts[0])
		if err != nil {
			return nil, fmt.Errorf("invalid port number '%s': %w", parts[0], err)
		}
		localPort = remotePort
	} else if len(parts) == 2 {
		// Local:Remote
		localPort, err = strconv.Atoi(parts[0])
		if err != nil {
			return nil, fmt.Errorf("invalid local port '%s': %w", parts[0], err)
		}
		remotePort, err = strconv.Atoi(parts[1])
		if err != nil {
			return nil, fmt.Errorf("invalid remote port '%s': %w", parts[1], err)
		}
	} else {
		return nil, fmt.Errorf("invalid port mapping '%s': expected format [LOCAL_PORT:]REMOTE_PORT", portStr)
	}

	if localPort < 1 || localPort > 65535 {
		return nil, fmt.Errorf("local port %d out of range (1-65535)", localPort)
	}
	if remotePort < 1 || remotePort > 65535 {
		return nil, fmt.Errorf("remote port %d out of range (1-65535)", remotePort)
	}

	return &PortMapping{
		LocalPort:  localPort,
		RemotePort: remotePort,
	}, nil
}

func runPortForward(cmd *cobra.Command, args []string) error {
	ctx := cmd.Context()

	serviceName := args[0]
	portSpecs := args[1:]

	Verbose("Port-forwarding to service: %s", serviceName)

	// Parse port mappings
	var portMappings []*PortMapping
	for _, portSpec := range portSpecs {
		mapping, err := parsePortMapping(portSpec)
		if err != nil {
			return err
		}
		portMappings = append(portMappings, mapping)
	}

	// Parse configuration
	loaded, cleanupPack, err := loadConfig(cmd, "")
	if err != nil {
		return err
	}
	defer cleanupPack()
	cfg := loaded.cfg

	if err := requireDocker(ctx, cfg); err != nil {
		return err
	}

	// Find the service
	svc, ok := cfg.Services[serviceName]
	if !ok {
		return fmt.Errorf("service '%s' not found in configuration", serviceName)
	}

	// Check if cluster exists
	kindMgr := deps.newClusterManager()

	if !cfg.Cluster.IsExternal() {
		clusterExists, err := kindMgr.ClusterExists(cfg.Cluster.Name)
		if err != nil {
			return fmt.Errorf("failed to check cluster: %w", err)
		}
		if !clusterExists {
			return fmt.Errorf("cluster '%s' is not running", cfg.Cluster.Name)
		}
	}

	kubeconfig, err := clusterKubeconfig(kindMgr, cfg)
	if err != nil {
		return err
	}

	provider, err := deps.newProvider(&svc, baseProviderOptions(cfg, kubeconfig))
	if err != nil {
		return fmt.Errorf("failed to create provider for '%s': %w", svc.Name, err)
	}

	// Check if service is installed
	installed, err := provider.IsInstalled(ctx, &svc)
	if err != nil {
		return fmt.Errorf("failed to check if service is installed: %w", err)
	}

	if !installed {
		return fmt.Errorf("service '%s' is not installed", serviceName)
	}

	// Get pod name
	podName := portForwardPod
	if podName == "" {
		// Auto-select a pod from the service
		pods, err := deps.podsForService(ctx, kubeconfig, &svc)
		if err != nil {
			return fmt.Errorf("failed to get pods for service: %w", err)
		}

		if len(pods) == 0 {
			return fmt.Errorf("no pods found for service '%s'", serviceName)
		}

		// Use the first pod
		podName = pods[0]
		if len(pods) > 1 {
			fmt.Printf("Multiple pods found, using '%s' (use --pod to specify)\n", podName)
			Verbose("Available pods: %v", pods)
		}
	}

	// Build port-forward arguments
	ports := make([]string, len(portMappings))
	for i, mapping := range portMappings {
		ports[i] = fmt.Sprintf("%d:%d", mapping.LocalPort, mapping.RemotePort)
	}

	fmt.Printf("Forwarding from %s/%s:\n", svc.GetNamespace(), podName)
	for _, mapping := range portMappings {
		fmt.Printf("  localhost:%d -> :%d\n", mapping.LocalPort, mapping.RemotePort)
	}
	fmt.Println("\nPress Ctrl+C to stop forwarding")

	// The command context is cancelled on Ctrl+C (see interruptContext)
	pfCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Start port forwarding in a goroutine
	errChan := make(chan error, 1)
	go func() {
		err := deps.portForward(pfCtx, kubeconfig, svc.GetNamespace(), podName, ports)
		if err != nil {
			errChan <- err
		}
	}()

	// Wait for interrupt or error
	select {
	case <-ctx.Done():
		fmt.Println("Stopping port-forward...")
		cancel()
		return nil
	case err := <-errChan:
		return fmt.Errorf("port-forward failed: %w", err)
	}
}

func init() {
	portForwardCmd.Flags().StringSliceVarP(&portForwardLabels, "label", "l", []string{}, "Filter services by label (format: key=value)")
	portForwardCmd.Flags().StringVarP(&portForwardPod, "pod", "p", "", "Specific pod name to forward to (optional, auto-selects if not specified)")
}
