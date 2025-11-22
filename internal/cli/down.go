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
	RunE: runDown,
}

func runDown(cmd *cobra.Command, args []string) error {
	ctx := context.Background()

	Verbose("Stopping services from config file: %s", configFile)

	// Parse configuration
	cfg, err := config.Parse(configFile)
	if err != nil {
		return fmt.Errorf("failed to parse config: %w", err)
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
		fmt.Printf("Uninstalling %d service(s)...\n", len(cfg.Services))
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
		fmt.Printf("Uninstalling %d service(s) in reverse dependency order...\n", len(cfg.Services))

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

	// Collect namespaces to clean up BEFORE uninstalling (since uninstall removes from state)
	namespacesToCleanup := st.GetCreatedNamespaces()

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

	// Uninstall each service in reverse dependency order
	uninstalledCount := 0
	for itr, svc := range orderedServices {
		fmt.Printf("\n[%d/%d] Uninstalling '%s' (%s)...\n", itr+1, len(orderedServices), svc.Name, svc.Type)

		// Create provider options
		providerOpts := &providers.ProviderOptions{
			ClusterName: cfg.Cluster.Name,
			KubeConfig:  kubeconfig,
			Verbose:     verbose,
			KeepCRDs:    downKeepCRDs,
		}

		// Create provider for this service
		provider, err := providers.NewProvider(svc, providerOpts)
		if err != nil {
			Verbose("Warning: failed to create provider for '%s': %v", svc.Name, err)
			continue
		}

		// Check if installed
		installed, err := provider.IsInstalled(ctx, svc)
		if err != nil {
			Verbose("Warning: failed to check if '%s' is installed: %v", svc.Name, err)
			installed = true // Try to uninstall anyway
		}

		if !installed {
			fmt.Printf("Service '%s' is not installed, skipping...\n", svc.Name)
			continue
		}

		// Uninstall the service
		if err := provider.Uninstall(ctx, svc); err != nil {
			Verbose("Warning: failed to uninstall '%s': %v", svc.Name, err)
			continue
		}

		// Update state
		st.MarkServiceUninstalled(svc.Name)
		if err := st.Save(stateFilePath); err != nil {
			Verbose("Warning: failed to save state: %v", err)
		}

		fmt.Printf("%s Service '%s' uninstalled successfully\n", color.Checkmark(), svc.Name)
		uninstalledCount++
	}

	fmt.Printf("\n%s Uninstalled %d service(s)\n", color.Checkmark(), uninstalledCount)

	// Clean up namespaces we created (if they're empty)
	if len(namespacesToCleanup) > 0 {
		fmt.Printf("\nCleaning up empty namespaces we created...\n")
		deletedNamespaces := 0

		for ns := range namespacesToCleanup {
			Verbose("Checking namespace '%s' for cleanup...", ns)

			// Check if namespace still exists
			exists, err := providers.CheckNamespaceExists(ctx, kubeconfig, ns)
			if err != nil {
				fmt.Printf("%s Warning: failed to check if namespace '%s' exists: %v\n", color.Warning(), ns, err)
				continue
			}

			if !exists {
				Verbose("Namespace '%s' already deleted", ns)
				continue
			}

			// Delete PVCs in the namespace (Helm doesn't delete them by default)
			pvcCount, err := providers.DeletePVCsInNamespace(ctx, kubeconfig, ns)
			if err != nil {
				fmt.Printf("%s Warning: failed to delete PVCs in namespace '%s': %v\n", color.Warning(), ns, err)
				// Continue anyway - maybe we can still delete the namespace
			} else if pvcCount > 0 {
				Verbose("Deleted %d PVC(s) from namespace '%s'", pvcCount, ns)
			}

			// Check if namespace is empty (after PVC deletion)
			isEmpty, err := providers.IsNamespaceEmpty(ctx, kubeconfig, ns)
			if err != nil {
				fmt.Printf("%s Warning: failed to check if namespace '%s' is empty: %v\n", color.Warning(), ns, err)
				continue
			}

			if !isEmpty {
				fmt.Printf("%s Namespace '%s' is not empty, skipping deletion\n", color.Warning(), ns)
				continue
			}

			Verbose("Namespace '%s' is empty, deleting...", ns)

			// Delete the namespace
			if err := providers.DeleteNamespace(ctx, kubeconfig, ns); err != nil {
				fmt.Printf("%s Warning: failed to delete namespace '%s': %v\n", color.Warning(), ns, err)
				continue
			}

			fmt.Printf("%s Deleted empty namespace '%s'\n", color.Checkmark(), ns)
			deletedNamespaces++
		}

		if deletedNamespaces > 0 {
			fmt.Printf("%s Cleaned up %d empty namespace(s)\n", color.Checkmark(), deletedNamespaces)
		} else {
			fmt.Printf("No empty namespaces to clean up\n")
		}
	}

	return nil
}

func init() {
	downCmd.Flags().BoolVar(&downKeepCRDs, "keep-crds", false, "Keep CRDs when uninstalling Helm charts")
	downCmd.Flags().StringSliceVarP(&downLabels, "label", "l", []string{}, "Filter services by label (format: key=value, can be specified multiple times)")
}
