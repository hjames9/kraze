package cli

import (
	"context"
	"fmt"

	"github.com/hjames9/kraze/internal/cluster"
	"github.com/hjames9/kraze/internal/color"
	"github.com/hjames9/kraze/internal/config"
	"github.com/hjames9/kraze/internal/graph"
	"github.com/hjames9/kraze/internal/providers"
	"github.com/hjames9/kraze/internal/state"
	"github.com/hjames9/kraze/internal/ui"
	"github.com/spf13/cobra"
)

var (
	downKeepCRDs bool
	downLabels   []string
)

var downCmd = &cobra.Command{
	Use:   "down [services...]",
	Short: "Uninstall services",
	Long: `Uninstall one or more services.

If no services are specified, all services will be uninstalled.
Services will be uninstalled in reverse dependency order.

You can filter services by name or by labels:
  kraze down service1 service2    # Uninstall specific services
  kraze down --label env=dev      # Uninstall services with label env=dev
  kraze down --label tier=backend # Uninstall services with label tier=backend`,
	ValidArgsFunction: getServiceNames,
	RunE:              runDown,
}

func runDown(cmd *cobra.Command, args []string) error {
	ctx := context.Background()

	Verbose("Stopping services from config file: %s", configFile)

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
	specificServicesRequested := len(requestedServices) > 0 || len(downLabels) > 0

	// Check if both service names and labels are specified
	if len(requestedServices) > 0 && len(downLabels) > 0 {
		return fmt.Errorf("cannot specify both service names and labels, use one or the other")
	}

	if len(downLabels) > 0 {
		// Filter by labels (note: down doesn't include dependencies, just the services themselves)
		Verbose("Filtering services by labels: %v", downLabels)
		filteredServices, err := cfg.FilterServicesByLabels(downLabels)
		if err != nil {
			return fmt.Errorf("failed to filter services by labels: %w", err)
		}
		cfg.Services = filteredServices
		Verbose("Found %d service(s) matching labels", len(filteredServices))
	} else if len(requestedServices) > 0 {
		Verbose("Services to uninstall: %v", requestedServices)
		filteredServices, err := cfg.FilterServices(requestedServices)
		if err != nil {
			return fmt.Errorf("failed to filter services: %w", err)
		}
		cfg.Services = filteredServices
	} else {
		Verbose("No services specified, will uninstall all services")
	}

	// Filter out disabled services
	disabledCount := 0
	enabledServices := make(map[string]config.ServiceConfig)
	for name, svc := range cfg.Services {
		if svc.IsEnabled() {
			enabledServices[name] = svc
		} else {
			disabledCount++
			Verbose("Skipping disabled service '%s' (not attempting uninstall)", name)
		}
	}
	if disabledCount > 0 {
		Verbose("Filtered out %d disabled service(s)", disabledCount)
	}
	cfg.Services = enabledServices

	if dryRun {
		fmt.Printf("[DRY RUN] Would uninstall %d service(s)\n", len(cfg.Services))
		for name := range cfg.Services {
			fmt.Printf("  - %s\n", name)
		}
		return nil
	}

	var orderedServices []*config.ServiceConfig

	if specificServicesRequested {
		// When specific services are requested, uninstall them in the order specified
		// (no dependency resolution needed - just uninstall what was asked)
		if len(downLabels) > 0 {
			// Labels: iterate over filtered services
			for name, svc := range cfg.Services {
				_ = name // use name to avoid unused variable error
				svcCopy := svc
				orderedServices = append(orderedServices, &svcCopy)
			}
		} else {
			// Service names: iterate in the order specified
			for _, name := range requestedServices {
				if svc, ok := cfg.Services[name]; ok {
					orderedServices = append(orderedServices, &svc)
				}
			}
		}
	} else {
		// When uninstalling all services, respect dependencies (reverse order)

		// Create dependency graph
		depGraph := graph.NewDependencyGraph(cfg.Services)

		// Get uninstall order (reverse topological sort)
		var err error
		orderedServices, err = depGraph.ReverseTopologicalSort()
		if err != nil {
			return fmt.Errorf("failed to resolve dependencies: %w", err)
		}
	}

	// Get state file path
	stateFilePath := state.GetStateFilePath(".")

	// Load state
	st, err := state.Load(stateFilePath)
	if err != nil {
		Verbose("Warning: failed to load state: %v", err)
		st = state.New(cfg.Cluster.Name, cfg.Cluster.IsExternal())
	}
	if st == nil {
		// State file doesn't exist yet (Load returns nil, nil in this case)
		st = state.New(cfg.Cluster.Name, cfg.Cluster.IsExternal())
	}

	// Collect namespaces to clean up BEFORE uninstalling (since uninstall removes from state)
	// For local dev environments, aggressively clean up namespaces when uninstalling services
	// - When uninstalling specific services: clean up namespaces used by those services (if no other services need them)
	// - When uninstalling all services: clean up ALL namespaces (nothing will be using them)
	var namespacesToCleanup map[string]int
	if specificServicesRequested {
		// Get list of service names being uninstalled
		var serviceNames []string
		for _, svc := range orderedServices {
			serviceNames = append(serviceNames, svc.Name)
		}
		namespacesToCleanup = st.GetNamespacesForServices(serviceNames)
	} else {
		// Uninstalling all services - clean up all namespaces (count will be 0 for all)
		// Get namespaces from state file
		namespacesToCleanup = st.GetAllNamespacesUsedForCleanup()

		// Also collect namespaces from the services we're actually uninstalling
		// This handles cases where state file is missing namespace data (e.g., services
		// installed before namespace tracking was implemented, or corrupted state)
		for _, svc := range orderedServices {
			ns := svc.GetNamespace()
			if ns != "" {
				// Set count to 0 since we're uninstalling everything
				namespacesToCleanup[ns] = 0
			}
		}
	}

	// Verify cluster exists
	kindMgr := cluster.NewKindManager()

	exists, err := kindMgr.ClusterExists(cfg.Cluster.Name)
	if err != nil {
		return fmt.Errorf("failed to check cluster: %w", err)
	}

	if !exists {
		fmt.Printf("Cluster '%s' does not exist, nothing to uninstall\n", cfg.Cluster.Name)
		return nil
	}

	// Get kubeconfig for the cluster
	kubeconfig, err := kindMgr.GetKubeConfig(cfg.Cluster.Name, false)
	if err != nil {
		return fmt.Errorf("failed to get kubeconfig: %w", err)
	}

	// Create progress manager
	progress := ui.NewProgressManager(verbose, plain)

	// Start progress display
	progress.Start(len(orderedServices), "Uninstalling")

	// Initialize all services as pending
	for i, svc := range orderedServices {
		progress.UpdateService(i, svc.Name, ui.StatusPending, "")
	}

	uninstalledCount := 0

	// Uninstall each service in reverse dependency order
	for itr, svc := range orderedServices {
		// Update progress to show we're uninstalling this service
		progress.UpdateService(itr, svc.Name, ui.StatusUninstalling, fmt.Sprintf("(%s)", svc.Type))
		progress.Verbose("Uninstalling '%s' (%s)...", svc.Name, svc.Type)

		// Create provider options
		providerOpts := &providers.ProviderOptions{
			ClusterName: cfg.Cluster.Name,
			KubeConfig:  kubeconfig,
			Verbose:     verbose,
			KeepCRDs:    downKeepCRDs,
			Quiet:       !verbose, // Suppress intermediate output unless verbose
		}

		// Create provider for this service
		provider, err := providers.NewProvider(svc, providerOpts)
		if err != nil {
			progress.Verbose("Warning: failed to create provider for '%s': %v", svc.Name, err)
			progress.UpdateService(itr, svc.Name, ui.StatusSkipped, "Failed to create provider")
			continue
		}

		// Check if installed
		installed, err := provider.IsInstalled(ctx, svc)
		if err != nil {
			progress.Verbose("Warning: failed to check if '%s' is installed: %v", svc.Name, err)
			installed = true // Try to uninstall anyway
		}

		if !installed {
			progress.UpdateService(itr, svc.Name, ui.StatusSkipped, "Not installed")
			continue
		}

		// Update status to show we're removing resources
		progress.UpdateService(itr, svc.Name, ui.StatusUninstalling, "Removing resources")

		// Uninstall the service
		if err := provider.Uninstall(ctx, svc); err != nil {
			progress.Verbose("Warning: failed to uninstall '%s': %v", svc.Name, err)
			progress.UpdateService(itr, svc.Name, ui.StatusFailed, err.Error())
			continue
		}

		// Update state
		st.MarkServiceUninstalled(svc.Name)
		if err := st.Save(stateFilePath); err != nil {
			progress.Verbose("Warning: failed to save state: %v", err)
		}

		// Mark service as uninstalled
		progress.UpdateService(itr, svc.Name, ui.StatusReady, "Removed")
		uninstalledCount++
	}

	// Finish progress display
	progress.Finish(uninstalledCount)

	// Clean up namespaces
	// For local dev environments, aggressively delete namespaces used by uninstalled services
	// Only delete if no other services are using the namespace
	if len(namespacesToCleanup) > 0 {
		fmt.Printf("\nCleaning up namespaces...\n")
		deletedNamespaces := 0
		skippedNamespaces := 0

		for ns, otherServicesCount := range namespacesToCleanup {
			// Skip namespace if other services are still using it
			if otherServicesCount > 0 {
				progress.Verbose("Skipping namespace '%s' (still used by %d other service(s))", ns, otherServicesCount)
				skippedNamespaces++
				continue
			}

			progress.Verbose("Cleaning up namespace '%s'...", ns)

			// Check if namespace still exists
			exists, err := providers.CheckNamespaceExists(ctx, kubeconfig, ns)
			if err != nil {
				fmt.Printf("%s Warning: failed to check if namespace '%s' exists: %v\n", color.Warning(), ns, err)
				continue
			}

			if !exists {
				progress.Verbose("Namespace '%s' already deleted", ns)
				continue
			}

			// Delete the namespace (cascades to all resources including secrets, configmaps, etc.)
			// This is aggressive but appropriate for local dev environments
			progress.Verbose("Deleting namespace '%s' (including all remaining resources)...", ns)
			if err := providers.DeleteNamespace(ctx, kubeconfig, ns); err != nil {
				fmt.Printf("%s Warning: failed to delete namespace '%s': %v\n", color.Warning(), ns, err)
				continue
			}

			fmt.Printf("%s Deleted namespace '%s'\n", color.Checkmark(), ns)
			deletedNamespaces++
		}

		if deletedNamespaces > 0 {
			fmt.Printf("%s Deleted %d namespace(s)", color.Checkmark(), deletedNamespaces)
			if skippedNamespaces > 0 {
				fmt.Printf(" (skipped %d still in use)", skippedNamespaces)
			}
			fmt.Printf("\n")
		} else if skippedNamespaces > 0 {
			fmt.Printf("No namespaces deleted (%d still in use by other services)\n", skippedNamespaces)
		} else {
			fmt.Printf("No namespaces to clean up\n")
		}
	}

	return nil
}

func init() {
	downCmd.Flags().BoolVar(&downKeepCRDs, "keep-crds", false, "Keep CRDs when uninstalling Helm charts")
	downCmd.Flags().StringSliceVarP(&downLabels, "label", "l", []string{}, "Filter services by label (format: key=value, can be specified multiple times)")
}
