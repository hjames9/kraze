package cli

import (
	"context"
	"fmt"

	"github.com/hjames9/kraze/internal/config"
	"github.com/hjames9/kraze/internal/state"
	"github.com/spf13/cobra"
)

var (
	statusLabels      []string
	statusAllProjects bool
)

var statusCmd = &cobra.Command{
	Use:     "status [services...]",
	Aliases: []string{"ps"},
	Short:   "Show status of services",
	Long: `Display the current status of all services defined in kraze.yml.

You can filter services by name or by labels:
  kraze status service1 service2    # Show status of specific services
  kraze status --label env=dev      # Show status of services with label env=dev
  kraze status --label tier=backend # Show status of services with label tier=backend`,
	RunE: runStatus,
}

func runStatus(cmd *cobra.Command, args []string) error {
	ctx := cmd.Context()

	loaded, cleanupPack, err := loadConfig(cmd, "Checking status from config file(s): %s")
	if err != nil {
		return err
	}
	defer cleanupPack()
	cfg := loaded.cfg

	if err := requireDocker(ctx, cfg); err != nil {
		return err
	}

	// Filter services if specified
	requestedServices := args

	// Check if both service names and labels are specified
	if len(requestedServices) > 0 && len(statusLabels) > 0 {
		return fmt.Errorf("cannot specify both service names and labels, use one or the other")
	}

	if len(statusLabels) > 0 {
		// Filter by labels
		Verbose("Filtering services by labels: %v", statusLabels)
		filteredServices, err := cfg.FilterServicesByLabels(statusLabels)
		if err != nil {
			return fmt.Errorf("failed to filter services by labels: %w", err)
		}
		cfg.Services = filteredServices
		Verbose("Found %d service(s) matching labels", len(filteredServices))
	} else if len(requestedServices) > 0 {
		// Filter by service names
		Verbose("Filtering services: %v", requestedServices)
		filteredServices, err := cfg.FilterServices(requestedServices)
		if err != nil {
			return fmt.Errorf("failed to filter services: %w", err)
		}
		cfg.Services = filteredServices
	}

	kindMgr := deps.newClusterManager()
	if !cfg.Cluster.IsExternal() {
		clusterExists, err := kindMgr.ClusterExists(cfg.Cluster.Name)
		if err != nil {
			return fmt.Errorf("failed to check cluster: %w", err)
		}
		if !clusterExists {
			fmt.Printf("Cluster '%s' is not running\n", cfg.Cluster.Name)
			fmt.Println("\nNo services are currently deployed.")
			return nil
		}
	}

	kubeconfig, err := clusterKubeconfig(kindMgr, cfg)
	if err != nil {
		return err
	}

	if statusAllProjects {
		return printAllProjectsStatus(ctx, cfg, kubeconfig)
	}

	fmt.Printf("Cluster: %s\n", cfg.Cluster.Name)
	if cfg.Project != "" {
		fmt.Printf("Project: %s\n", cfg.Project)
	}
	fmt.Println()

	// Print header
	fmt.Printf("%-20s %-12s %-10s %-10s %s\n", "SERVICE", "TYPE", "INSTALLED", "READY", "MESSAGE")
	fmt.Println("--------------------------------------------------------------------------------")

	// What `up` recorded (completed Jobs, see ManifestsProvider.Status).
	// Without it, status still reports what exists.
	var st *state.ClusterState
	if clientset, err := clusterClient(cfg, kubeconfig); err != nil {
		Verbose("Warning: failed to create Kubernetes client: %v", err)
	} else if st, err = state.Load(ctx, clientset, cfg.Cluster.Name); err != nil {
		Verbose("Warning: failed to load cluster state: %v", err)
		st = nil
	} else if st != nil { // nil: nothing recorded on this cluster yet
		st.SetProject(cfg.Project)
	}

	// Track counts for summary
	installedCount := 0
	readyCount := 0

	// Check status of each service
	for name, svc := range cfg.Services {
		// Skip disabled services but show them in the status
		if !svc.IsEnabled() {
			Verbose("Service '%s' is disabled (skipping status check)", name)
			fmt.Printf("%-20s %-12s %-10s %-10s %s\n",
				name, svc.Type, "N/A", "N/A", "DISABLED")
			continue
		}

		// Create provider options
		providerOpts := baseProviderOptions(cfg, kubeconfig)
		if st != nil {
			if recorded, ok := st.GetService(name); ok {
				providerOpts.Recorded = recorded.Resources
			}
		}

		// Create provider
		provider, err := deps.newProvider(&svc, providerOpts)
		if err != nil {
			fmt.Printf("%-20s %-12s %-10s %-10s %s\n",
				name, svc.Type, "ERROR", "ERROR", fmt.Sprintf("Failed to create provider: %v", err))
			continue
		}

		// Get status from provider
		status, err := provider.Status(ctx, &svc)
		if err != nil {
			fmt.Printf("%-20s %-12s %-10s %-10s %s\n",
				name, svc.Type, "ERROR", "ERROR", fmt.Sprintf("Failed to get status: %v", err))
			continue
		}

		// Count installed and ready services
		if status.Installed {
			installedCount++
		}
		if status.Ready {
			readyCount++
		}

		// Format output
		installedStr := "No"
		if status.Installed {
			installedStr = "Yes"
		}

		readyStr := "No"
		if status.Ready {
			readyStr = "Yes"
		}

		// Truncate message if too long
		message := status.Message
		if len(message) > 40 {
			message = message[:37] + "..."
		}

		fmt.Printf("%-20s %-12s %-10s %-10s %s\n",
			name, svc.Type, installedStr, readyStr, message)
	}

	fmt.Println()

	// Summary based on actual status checks (not state file)
	fmt.Printf("Summary: %d/%d services installed, %d ready\n", installedCount, len(cfg.Services), readyCount)

	return nil
}

func init() {
	statusCmd.Flags().BoolVar(&statusAllProjects, "all-projects", false, "List every project's services recorded on the cluster (from cluster state)")
	statusCmd.Flags().StringSliceVarP(&statusLabels, "label", "l", []string{}, "Filter services by label (format: key=value, can be specified multiple times)")
}

// printAllProjectsStatus lists every service recorded in the cluster state,
// across all projects sharing the cluster. It reads state only and doesn't
// query each service's live status.
func printAllProjectsStatus(ctx context.Context, cfg *config.Config, kubeconfig string) error {
	clientset, err := clusterClient(cfg, kubeconfig)
	if err != nil {
		return fmt.Errorf("failed to create Kubernetes client: %w", err)
	}
	st, err := state.Load(ctx, clientset, cfg.Cluster.Name)
	if err != nil {
		return fmt.Errorf("failed to load cluster state: %w", err)
	}
	printProjectsTable(cfg.Cluster.Name, st)
	return nil
}

// printProjectsTable prints every service in st, grouped by project
func printProjectsTable(clusterName string, st *state.ClusterState) {
	fmt.Printf("Cluster: %s\n\n", clusterName)
	if st == nil || len(st.Services) == 0 {
		fmt.Println("No services are recorded in the cluster state.")
		return
	}

	fmt.Printf("%-20s %-20s %-20s %s\n", "PROJECT", "SERVICE", "NAMESPACE", "UPDATED")
	fmt.Println("--------------------------------------------------------------------------------")
	for _, svc := range st.AllServices() {
		fmt.Printf("%-20s %-20s %-20s %s\n",
			projectLabel(svc.Project), svc.Name, svc.Namespace, svc.UpdatedAt.Local().Format("2006-01-02 15:04:05"))
	}
}
