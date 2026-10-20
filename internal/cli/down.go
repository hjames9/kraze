package cli

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/hjames9/kraze/internal/color"
	"github.com/hjames9/kraze/internal/config"
	"github.com/hjames9/kraze/internal/graph"
	"github.com/hjames9/kraze/internal/history"
	"github.com/hjames9/kraze/internal/providers"
	"github.com/hjames9/kraze/internal/state"
	"github.com/hjames9/kraze/internal/ui"
	"github.com/spf13/cobra"
	"k8s.io/client-go/kubernetes"
)

var (
	downKeepCRDs                 bool
	downLabels                   []string
	downNamespaceDeletionTimeout time.Duration
	downDisabledOnly             bool
	downDeleteNamespaces         bool
	downRemoveOrphans            bool
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
  kraze down --label tier=backend # Uninstall services with label tier=backend

Services with enabled: false are normally left alone. Use --disabled to
instead target only services that are disabled in config but still
installed (e.g. you flipped enabled: false after deploying it):
  kraze down --disabled           # Remove disabled-but-installed services only

Namespaces are deleted only if kraze created them and no other service uses
them. Namespaces created by kraze before it labeled them are kept; to remove
them once, use --delete-namespaces. It also deletes unlabeled namespaces that
no other service uses, unless workloads remain in them:
  kraze down --delete-namespaces

Services this project installed that are no longer in its config (orphans)
are left alone; --remove-orphans removes them too:
  kraze down --remove-orphans`,
	ValidArgsFunction: getServiceNames,
	RunE:              runDown,
}

func runDown(cmd *cobra.Command, args []string) (retErr error) {
	ctx := cmd.Context()

	loaded, cleanupPack, err := loadConfig(cmd, "Stopping services from config file(s): %s")
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
	specificServicesRequested := len(requestedServices) > 0 || len(downLabels) > 0

	if downDisabledOnly && specificServicesRequested {
		return fmt.Errorf("--disabled cannot be combined with service names or --label")
	}
	if downRemoveOrphans && (specificServicesRequested || downDisabledOnly) {
		return fmt.Errorf("--remove-orphans can't be combined with service names, --label or --disabled")
	}
	configured := configuredServices(cfg)

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
	} else if downDisabledOnly {
		Verbose("--disabled flag set, will only target services disabled in config that are still installed")
	} else {
		Verbose("No services specified, will uninstall all services")
	}

	// A disabled service that was explicitly named or matched by label is
	// still uninstalled (an explicit request takes precedence over enabled:
	// false); no extra filtering needed for that path below.
	if specificServicesRequested {
		for name, svc := range cfg.Services {
			if !svc.IsEnabled() {
				Verbose("Service '%s' is disabled but was explicitly requested, uninstalling anyway", name)
			}
		}
	}

	var orderedServices []*config.ServiceConfig

	switch {
	case specificServicesRequested:
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

	case downDisabledOnly:
		// Build the graph from the full config (not just disabled services)
		// so DependsOn edges referencing enabled services still resolve
		// correctly, then keep only the disabled ones from the resulting
		// order. The IsInstalled() check in the uninstall loop below skips
		// any disabled service that was never actually deployed.
		depGraph := graph.NewDependencyGraph(cfg.Services)
		fullOrder, err := depGraph.ReverseTopologicalSort()
		if err != nil {
			return fmt.Errorf("failed to resolve dependencies: %w", err)
		}
		for _, svc := range fullOrder {
			if !svc.IsEnabled() {
				orderedServices = append(orderedServices, svc)
			}
		}
		if len(orderedServices) == 0 {
			fmt.Println("No disabled services found in config")
			return nil
		}

	default:
		// When uninstalling all services, respect dependencies (reverse
		// order). Disabled services are skipped entirely here; use
		// 'kraze down --disabled' to remove disabled-but-installed services.
		disabledCount := 0
		enabledServices := make(map[string]config.ServiceConfig)
		for name, svc := range cfg.Services {
			if svc.IsEnabled() {
				enabledServices[name] = svc
			} else {
				disabledCount++
				Verbose("Skipping disabled service '%s' (use 'kraze down --disabled' to remove disabled-but-installed services)", name)
			}
		}
		if disabledCount > 0 {
			Verbose("Filtered out %d disabled service(s)", disabledCount)
		}
		cfg.Services = enabledServices

		// Create dependency graph
		depGraph := graph.NewDependencyGraph(cfg.Services)

		// Get uninstall order (reverse topological sort)
		var err error
		orderedServices, err = depGraph.ReverseTopologicalSort()
		if err != nil {
			return fmt.Errorf("failed to resolve dependencies: %w", err)
		}
	}

	if dryRun {
		fmt.Printf("[DRY RUN] Would uninstall %d service(s)\n", len(orderedServices))
		for _, svc := range orderedServices {
			if downDisabledOnly {
				fmt.Printf("  - %s (disabled in config, removed only if still installed)\n", svc.Name)
			} else {
				fmt.Printf("  - %s\n", svc.Name)
			}
		}
		if downRemoveOrphans {
			fmt.Printf("[DRY RUN] --remove-orphans: services this project installed that are no longer in its config are also removed (`kraze plan` lists them)\n")
		}
		if downDeleteNamespaces {
			fmt.Printf("[DRY RUN] --delete-namespaces: unlabeled namespaces of these services are also deleted if no other service uses them and no workloads remain\n")
		}
		return nil
	}

	conn, err := connectCluster(ctx, deps.newClusterManager(), cfg, skipMissing)
	if err != nil {
		return err
	}
	if conn == nil {
		fmt.Printf("Cluster '%s' does not exist, nothing to uninstall\n", cfg.Cluster.Name)
		return nil
	}
	kubeconfig, clientset := conn.kubeconfig, conn.clientset

	rec := beginHistory(ctx, clientset, cfg.Project, "down", false)
	defer func() { finishHistory(rec, retErr) }()

	// Load cluster state
	st, _, err := loadClusterState(ctx, clientset, cfg, stateBestEffort)
	if err != nil {
		return err
	}
	adoptProject(st, cfg, configured, true)

	// Collect namespace candidates BEFORE uninstalling, since uninstalling
	// removes the services' entries from state
	namespaceCandidates := collectNamespaceCandidates(st, orderedServices)

	// Create progress manager
	progress := ui.NewProgressManager(verbose, plain, len(orderedServices))

	// Start progress display
	progress.Start(len(orderedServices), "Uninstalling")

	// Initialize all services as pending
	for i, svc := range orderedServices {
		progress.UpdateService(i, svc.Name, ui.StatusPending, "")
	}

	uninstalledCount := 0

	// Removes orphans, and services that moved namespace or changed type since
	// they were installed (the config no longer says where they are)
	backend := deps.orphanBackend(quietProviderOptions(cfg, kubeconfig))

	// Uninstall each service in reverse dependency order
	for itr, svc := range orderedServices {
		// Helm uninstalls ignore the context, so stop between services on Ctrl-C
		if ctx.Err() != nil {
			progress.UpdateService(itr, svc.Name, ui.StatusSkipped, "Cancelled")
			continue
		}

		// Update progress to show we're uninstalling this service
		progress.UpdateService(itr, svc.Name, ui.StatusUninstalling, fmt.Sprintf("(%s)", svc.Type))
		progress.Verbose("Uninstalling '%s' (%s)...", svc.Name, svc.Type)

		if meta, moved := movedInstall(st, svc); moved {
			progress.UpdateService(itr, svc.Name, ui.StatusUninstalling, fmt.Sprintf("Removing from namespace '%s'", meta.Namespace))
			start := time.Now()
			removed, err := removeOrphan(ctx, meta, backend, progress.Verbose)
			rec.RecordChange(svc.Name, history.ActionUninstall, time.Since(start), err)
			if err != nil {
				progress.UpdateService(itr, svc.Name, ui.StatusFailed, err.Error())
				delete(namespaceCandidates, meta.Namespace) // its resources may still be there
				continue
			}
			st.MarkServiceUninstalled(svc.Name)
			if err := st.Save(ctx, clientset); err != nil {
				progress.Verbose("Warning: failed to save cluster state: %v", err)
			}
			if removed {
				progress.UpdateService(itr, svc.Name, ui.StatusReady, "Removed")
				uninstalledCount++
			} else {
				progress.UpdateService(itr, svc.Name, ui.StatusSkipped,
					fmt.Sprintf("Forgotten; its objects are unknown (kubectl delete -n %s -l kraze.service=%s)", meta.Namespace, svc.Name))
			}
			continue
		}

		// Create provider options
		providerOpts := quietProviderOptions(cfg, kubeconfig)
		providerOpts.KeepCRDs = downKeepCRDs

		// Create provider for this service
		provider, err := deps.newProvider(svc, providerOpts)
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
		start := time.Now()
		err = provider.Uninstall(ctx, svc)
		if providers.IsReleaseNotFound(err) {
			// Removed since IsInstalled saw it, e.g. by deleting its namespace
			progress.Verbose("Helm release '%s' is already gone", svc.Name)
			err = nil
		}
		rec.RecordChange(svc.Name, history.ActionUninstall, time.Since(start), err)
		if err != nil {
			progress.Verbose("Warning: failed to uninstall '%s': %v", svc.Name, err)
			progress.UpdateService(itr, svc.Name, ui.StatusFailed, err.Error())
			// Deleting its namespace would remove what's left of it behind
			// kraze's back, leaving its state entry pointing at nothing
			delete(namespaceCandidates, svc.GetNamespace())
			continue
		}

		// Update cluster state
		st.MarkServiceUninstalled(svc.Name)
		if err := st.Save(ctx, clientset); err != nil {
			progress.Verbose("Warning: failed to save cluster state: %v", err)
		}

		// Mark service as uninstalled
		progress.UpdateService(itr, svc.Name, ui.StatusReady, "Removed")
		uninstalledCount++
	}

	// Finish progress display
	progress.Finish(uninstalledCount)

	if ctx.Err() != nil {
		return ctx.Err()
	}

	// Clean up namespaces kraze created that nothing else uses any more
	cleanupNamespaces(ctx, clientset, kubeconfig, namespaceCandidates, downDeleteNamespaces, progress.Verbose)

	orphans := st.Orphans(configured)
	if downRemoveOrphans {
		if err := removeOrphans(ctx, st, clientset, kubeconfig, orphans, nil, backend, downDeleteNamespaces, rec, progress.Verbose); err != nil {
			return err
		}
	} else if !specificServicesRequested && !downDisabledOnly {
		printOrphanHint(orphans)
	}

	return nil
}

func init() {
	downCmd.Flags().BoolVar(&downKeepCRDs, "keep-crds", false, "Keep CRDs when uninstalling Helm charts")
	downCmd.Flags().StringSliceVarP(&downLabels, "label", "l", []string{}, "Filter services by label (format: key=value, can be specified multiple times)")
	downCmd.Flags().DurationVar(&downNamespaceDeletionTimeout, "namespace-deletion-timeout", 30*time.Second, "How long to wait for each namespace to be deleted (0 = don't wait, e.g., 30s, 1m)")
	downCmd.Flags().BoolVar(&downRemoveOrphans, "remove-orphans", false, "Also remove services this project installed that are no longer in its config")
	downCmd.Flags().BoolVar(&downDeleteNamespaces, "delete-namespaces", false, "Also delete namespaces kraze didn't label as its own (e.g. created by older kraze versions), if no other service uses them and no workloads remain")
	downCmd.Flags().BoolVar(&downDisabledOnly, "disabled", false, "Only uninstall services that are disabled in config (enabled: false) but still installed")
}

// namespaceCandidate is a namespace a `down` run may delete
type namespaceCandidate struct {
	otherServices  int  // installed services, in any project, that still use it
	createdInState bool // some state entry records that kraze created it
}

// collectNamespaceCandidates gathers the namespaces of the services being
// uninstalled, from state and from config (state may lack namespace data).
// It must run before uninstalling, which removes the services from state.
func collectNamespaceCandidates(st *state.ClusterState, services []*config.ServiceConfig) map[string]namespaceCandidate {
	names := make([]string, 0, len(services))
	for _, svc := range services {
		names = append(names, svc.Name)
	}

	candidates := make(map[string]namespaceCandidate)
	for ns, count := range st.GetNamespacesForServices(names) {
		candidates[ns] = namespaceCandidate{otherServices: count, createdInState: st.NamespaceCreatedByKraze(ns)}
	}
	for _, svc := range services {
		ns := svc.GetNamespace()
		if _, seen := candidates[ns]; ns == "" || seen {
			continue
		}
		candidates[ns] = namespaceCandidate{
			otherServices:  st.CountOtherServicesInNamespace(ns, names),
			createdInState: st.NamespaceCreatedByKraze(ns),
		}
	}
	return candidates
}

// unmanagedNamespaceSettle is how long selectNamespacesToDelete waits for the
// workloads of just-uninstalled services to go away before it decides that an
// unmanaged namespace still holds someone else's workloads
var unmanagedNamespaceSettle = 20 * time.Second

// keptNamespace is a namespace `down` leaves in place, and why
type keptNamespace struct {
	name   string
	reason string
}

// selectNamespacesToDelete returns the candidates to delete: those still
// present, used by no other installed service, and created by kraze (labeled
// as kraze-managed, or recorded as created in state). With deleteUnmanaged
// (--delete-namespaces), a namespace kraze didn't create also qualifies if it
// isn't a system namespace and no workloads remain in it. Existing namespaces
// that don't qualify are returned as kept. skipped counts namespaces still in
// use.
func selectNamespacesToDelete(ctx context.Context, clientset kubernetes.Interface, candidates map[string]namespaceCandidate, deleteUnmanaged bool, verbosef func(string, ...interface{})) (toDelete []string, kept []keptNamespace, skipped int) {
	names := make([]string, 0, len(candidates))
	for ns := range candidates {
		names = append(names, ns)
	}
	sort.Strings(names)

	for _, ns := range names {
		candidate := candidates[ns]
		if candidate.otherServices > 0 {
			verbosef("Skipping namespace '%s' (still used by %d other service(s))", ns, candidate.otherServices)
			skipped++
			continue
		}

		exists, managed, err := providers.NamespaceInfo(ctx, clientset, ns)
		if err != nil {
			fmt.Printf("%s Warning: %v\n", color.Warning(), err)
			continue
		}
		if !exists {
			verbosef("Namespace '%s' already deleted", ns)
			continue
		}
		if managed || candidate.createdInState {
			toDelete = append(toDelete, ns)
			continue
		}
		if !deleteUnmanaged {
			kept = append(kept, keptNamespace{ns, "not created by kraze; --delete-namespaces deletes it"})
			continue
		}
		if providers.IsSystemNamespace(ns) {
			kept = append(kept, keptNamespace{ns, "system namespace"})
			continue
		}
		workloads, err := settledWorkloads(ctx, clientset, ns, verbosef)
		if err != nil {
			fmt.Printf("%s Warning: %v\n", color.Warning(), err)
			continue
		}
		if len(workloads) > 0 {
			kept = append(kept, keptNamespace{ns, "not created by kraze and still has " + strings.Join(workloads, ", ")})
			continue
		}
		toDelete = append(toDelete, ns)
	}
	return toDelete, kept, skipped
}

// settledWorkloads returns the workloads left in a namespace, waiting up to
// unmanagedNamespaceSettle for those of just-uninstalled services to go away
func settledWorkloads(ctx context.Context, clientset kubernetes.Interface, ns string, verbosef func(string, ...interface{})) ([]string, error) {
	deadline := time.Now().Add(unmanagedNamespaceSettle)
	for waited := false; ; waited = true {
		workloads, err := providers.NamespaceWorkloads(ctx, clientset, ns)
		if err != nil || len(workloads) == 0 || time.Now().After(deadline) {
			return workloads, err
		}
		if !waited {
			verbosef("Waiting up to %v for workloads in namespace '%s' to go away (%s)...",
				unmanagedNamespaceSettle, ns, strings.Join(workloads, ", "))
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

// cleanupNamespaces deletes the candidate namespaces that
// selectNamespacesToDelete selects, in parallel, and reports the rest
func cleanupNamespaces(ctx context.Context, clientset kubernetes.Interface, kubeconfig string, candidates map[string]namespaceCandidate, deleteUnmanaged bool, verbosef func(string, ...interface{})) {
	if len(candidates) == 0 {
		return
	}
	fmt.Printf("\nCleaning up namespaces...\n")

	namespacesToDelete, keptNamespaces, skippedNamespaces := selectNamespacesToDelete(ctx, clientset, candidates, deleteUnmanaged, verbosef)
	for _, kept := range keptNamespaces {
		fmt.Printf("Kept namespace '%s' (%s)\n", kept.name, kept.reason)
	}

	if len(namespacesToDelete) == 0 {
		if skippedNamespaces > 0 {
			fmt.Printf("No namespaces deleted (%d still in use by other services)\n", skippedNamespaces)
		} else if len(keptNamespaces) == 0 {
			fmt.Printf("No namespaces to clean up\n")
		}
		return
	}

	var wg sync.WaitGroup
	var mu sync.Mutex
	deletedCount := 0
	timeoutCount := 0

	for _, ns := range namespacesToDelete {
		wg.Add(1)
		go func(namespace string) {
			defer wg.Done()

			verbosef("Deleting namespace '%s' (including all remaining resources)...", namespace)

			// Delete the namespace (cascades to all resources including secrets, configmaps, etc.)
			if err := deps.deleteNamespace(ctx, kubeconfig, namespace); err != nil {
				fmt.Printf("%s Warning: failed to delete namespace '%s': %v\n", color.Warning(), namespace, err)
				return
			}

			// Wait for deletion (unless timeout is 0)
			if downNamespaceDeletionTimeout > 0 {
				if err := deps.waitNamespaceDeleted(ctx, kubeconfig, namespace, downNamespaceDeletionTimeout); err != nil {
					fmt.Printf("%s Warning: namespace '%s' still terminating after %v\n", color.Warning(), namespace, downNamespaceDeletionTimeout)
					mu.Lock()
					timeoutCount++
					mu.Unlock()
				} else {
					fmt.Printf("%s Deleted namespace '%s'\n", color.Checkmark(), namespace)
					mu.Lock()
					deletedCount++
					mu.Unlock()
				}
			} else {
				fmt.Printf("%s Namespace '%s' deletion initiated\n", color.Checkmark(), namespace)
				mu.Lock()
				deletedCount++
				mu.Unlock()
			}
		}(ns)
	}

	// Wait for all deletions to complete
	wg.Wait()

	// Print summary
	if deletedCount > 0 || timeoutCount > 0 {
		if downNamespaceDeletionTimeout > 0 {
			fmt.Printf("%s Deleted %d namespace(s)", color.Checkmark(), deletedCount)
			if timeoutCount > 0 {
				fmt.Printf(" (%d still terminating)", timeoutCount)
			}
		} else {
			fmt.Printf("%s Initiated deletion of %d namespace(s)", color.Checkmark(), deletedCount)
		}
		if skippedNamespaces > 0 {
			fmt.Printf(" (skipped %d still in use)", skippedNamespaces)
		}
		fmt.Printf("\n")
	}
}
