package cli

import (
	"context"
	"fmt"

	"github.com/hjames9/kraze/internal/cluster"
	"github.com/hjames9/kraze/internal/config"
	"github.com/hjames9/kraze/internal/providers"
	"github.com/hjames9/kraze/internal/state"
	"github.com/spf13/cobra"
)

var (
	statusLabels []string
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
	ctx := context.Background()

	Verbose("Checking status from config file: %s", configFile)

	// Parse configuration
	cfg, err := config.Parse(configFile)
	if err != nil {
		return fmt.Errorf("failed to parse config: %w", err)
	}

	// Check Docker availability (only for kind clusters, not external)
	if !cfg.Cluster.IsExternal() {
		Verbose("Checking Docker availability...")
		if err := cluster.CheckDockerAvailable(ctx); err != nil {
			return err
		}
		Verbose("Docker is available")
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

	// Check if cluster exists
	kindMgr := cluster.NewKindManager()

	clusterExists, err := kindMgr.ClusterExists(cfg.Cluster.Name)
	if err != nil {
		return fmt.Errorf("failed to check cluster: %w", err)
	}

	if !clusterExists {
		fmt.Printf("Cluster '%s' is not running\n", cfg.Cluster.Name)
		fmt.Println("\nNo services are currently deployed.")
		return nil
	}

	// Get kubeconfig
	kubeconfig, err := kindMgr.GetKubeConfig(cfg.Cluster.Name, false)
	if err != nil {
		return fmt.Errorf("failed to get kubeconfig: %w", err)
	}

	// Load state
	stateFilePath := state.GetStateFilePath(".")
	st, err := state.Load(stateFilePath)
	if err != nil {
		Verbose("Warning: failed to load state: %v", err)
		st = state.New(cfg.Cluster.Name, cfg.Cluster.IsExternal())
	}

	fmt.Printf("Cluster: %s\n\n", cfg.Cluster.Name)

	// Print header
	fmt.Printf("%-20s %-12s %-10s %-10s %s\n", "SERVICE", "TYPE", "INSTALLED", "READY", "MESSAGE")
	fmt.Println("--------------------------------------------------------------------------------")

	// Check status of each service
	for name, svc := range cfg.Services {
		// Create provider options
		providerOpts := &providers.ProviderOptions{
			ClusterName: cfg.Cluster.Name,
			KubeConfig:  kubeconfig,
			Verbose:     verbose,
		}

		// Create provider
		provider, err := providers.NewProvider(&svc, providerOpts)
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

	// Summary
	installedServices := st.GetInstalledServices()
	fmt.Printf("Summary: %d/%d services installed\n", len(installedServices), len(cfg.Services))

	return nil
}

func init() {
	statusCmd.Flags().StringSliceVarP(&statusLabels, "label", "l", []string{}, "Filter services by label (format: key=value, can be specified multiple times)")
}
