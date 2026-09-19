package cli

import (
	"fmt"
	"strings"

	"github.com/hjames9/kraze/internal/cluster"
	"github.com/hjames9/kraze/internal/color"
	"github.com/hjames9/kraze/internal/config"
	"github.com/hjames9/kraze/internal/graph"
	"github.com/hjames9/kraze/internal/providers"
	"github.com/hjames9/kraze/internal/state"
	"github.com/spf13/cobra"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

var (
	planLabels []string
)

var planCmd = &cobra.Command{
	Use:   "plan [services...]",
	Short: "Show what would be installed or changed",
	Long: `Show a detailed plan of what would happen when running 'kraze up'.

Displays:
- Services that would be installed (new, existing, or changed)
- Dependency levels and parallel execution groups
- Namespaces that would be created
- Cluster status and network configuration

You can filter services by name or by labels:
  kraze plan service1 service2      # Plan specific services
  kraze plan --label env=dev        # Plan services with label env=dev
  kraze plan --label tier=backend   # Plan services with label tier=backend`,
	ValidArgsFunction: getServiceNames,
	RunE:              runPlan,
}

// ServicePlanInfo contains planning information for a service
type ServicePlanInfo struct {
	Name            string
	Type            string
	Action          string // "add", "change", "no-change"
	Version         string
	Namespace       string
	NamespaceAction string // one of the namespace* constants
	DependsOn       []string
	Details         string
}

// Namespace actions shown by plan
const (
	namespaceManaged      = "managed by kraze"
	namespaceExisting     = "existing"
	namespaceWillCreate   = "will be created"
	namespaceMissing      = "missing, and create_namespace is false"
	namespaceCreateIfNeed = "will be created if missing"
	namespaceMustExist    = "must already exist"
)

// builtinNamespaces exist in every new cluster, so a plan for a kind cluster that
// has not been created yet can treat them as existing
var builtinNamespaces = []string{"default", "kube-system", "kube-public", "kube-node-lease"}

// clusterNamespaces is what plan knows about the namespaces in the cluster.
// known is false when the cluster could not be queried.
type clusterNamespaces struct {
	known    bool
	existing map[string]bool
}

func newClusterNamespaces(names ...string) clusterNamespaces {
	cn := clusterNamespaces{known: true, existing: make(map[string]bool, len(names))}
	for _, name := range names {
		cn.existing[name] = true
	}
	return cn
}

// namespaceAction decides what kraze up would do with a service's namespace,
// mirroring installService: a namespace is created only if it does not exist and
// create_namespace allows it. created counts the namespaces kraze has created,
// from cluster state.
func namespaceAction(namespace string, shouldCreate bool, created map[string]int, cn clusterNamespaces) string {
	if !cn.known {
		if created[namespace] > 0 {
			return namespaceManaged
		}
		if shouldCreate {
			return namespaceCreateIfNeed
		}
		return namespaceMustExist
	}

	if cn.existing[namespace] {
		if created[namespace] > 0 {
			return namespaceManaged
		}
		return namespaceExisting
	}
	// Missing, even if state says kraze created it once (it was deleted since)
	if shouldCreate {
		return namespaceWillCreate
	}
	return namespaceMissing
}

func runPlan(cmd *cobra.Command, args []string) error {
	ctx := cmd.Context()

	cfgPaths, cleanupPack, err := resolveAndExtractConfigFiles(cmd)
	if err != nil {
		return err
	}
	defer cleanupPack()

	Verbose("Planning deployment from config file(s): %s", strings.Join(cfgPaths, ", "))

	// Parse configuration
	cfg, err := config.ParseMultiple(cfgPaths)
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

	// Filter services if specified (including dependencies)
	requestedServices := args

	// Check if both service names and labels are specified
	if len(requestedServices) > 0 && len(planLabels) > 0 {
		return fmt.Errorf("cannot specify both service names and labels, use one or the other")
	}

	if len(planLabels) > 0 {
		// Filter by labels
		Verbose("Filtering services by labels: %v", planLabels)
		filteredServices, err := cfg.FilterServicesByLabelsWithDependencies(planLabels)
		if err != nil {
			return fmt.Errorf("failed to filter services by labels: %w", err)
		}
		cfg.Services = filteredServices
		Verbose("Found %d service(s) matching labels (including dependencies)", len(filteredServices))
	} else if len(requestedServices) > 0 {
		// Filter by service names
		Verbose("Services to plan: %v", requestedServices)
		filteredServices, err := cfg.FilterServicesWithDependencies(requestedServices)
		if err != nil {
			return fmt.Errorf("failed to filter services: %w", err)
		}
		cfg.Services = filteredServices
	} else {
		Verbose("No services specified, will plan all services")
	}

	// Create dependency graph
	depGraph := graph.NewDependencyGraph(cfg.Services)

	// Validate dependencies
	if err := depGraph.Validate(); err != nil {
		return fmt.Errorf("dependency validation failed: %w", err)
	}

	// Get installation order grouped by dependency level
	serviceLevels, err := depGraph.TopologicalSortByLevel()
	if err != nil {
		return fmt.Errorf("failed to resolve dependencies: %w", err)
	}

	// Load cluster state to compare
	var st *state.ClusterState
	var namespaces clusterNamespaces

	// Check cluster existence
	kindMgr := cluster.NewKindManager()
	isExternal := cfg.Cluster.IsExternal()

	// Try to get kubeconfig and load state from cluster
	var kubeconfig string
	if isExternal {
		// External cluster
		kubeconfig, err = kindMgr.GetKubeconfigForExternalCluster(&cfg.Cluster)
		if err != nil {
			Verbose("Warning: failed to get kubeconfig for external cluster: %v", err)
			Verbose("Treating as empty state (no services installed)")
			st = state.New(cfg.Cluster.Name, isExternal, false, 0, false, 0)
			kubeconfig = "" // Clear to skip state loading
		}
	} else {
		// Kind cluster
		exists, err := kindMgr.ClusterExists(cfg.Cluster.Name)
		if err != nil || !exists {
			if err != nil {
				Verbose("Warning: failed to check cluster: %v", err)
			} else {
				Verbose("Cluster doesn't exist yet")
				namespaces = newClusterNamespaces(builtinNamespaces...)
			}
			Verbose("Treating as empty state (no services installed)")
			st = state.New(cfg.Cluster.Name, isExternal, false, 0, false, 0)
			kubeconfig = "" // Clear to skip state loading
		} else {
			kubeconfig, err = kindMgr.GetKubeConfig(cfg.Cluster.Name, false)
			if err != nil {
				Verbose("Warning: failed to get kubeconfig: %v", err)
				Verbose("Treating as empty state (no services installed)")
				st = state.New(cfg.Cluster.Name, isExternal, false, 0, false, 0)
				kubeconfig = "" // Clear to skip state loading
			}
		}
	}

	// If we have kubeconfig, try to load state from cluster
	if kubeconfig != "" {
		clientset, err := providers.GetClientsetFromKubeconfigContent(kubeconfig, !isExternal)
		if err != nil {
			Verbose("Warning: failed to create Kubernetes client: %v", err)
			Verbose("Treating as empty state (no services installed)")
			st = state.New(cfg.Cluster.Name, isExternal, false, 0, false, 0)
		} else {
			if nsList, err := clientset.CoreV1().Namespaces().List(ctx, metav1.ListOptions{}); err != nil {
				Verbose("Warning: failed to list namespaces: %v", err)
			} else {
				names := make([]string, 0, len(nsList.Items))
				for _, ns := range nsList.Items {
					names = append(names, ns.Name)
				}
				namespaces = newClusterNamespaces(names...)
			}

			st, err = state.Load(ctx, clientset, cfg.Cluster.Name)
			if err != nil {
				Verbose("Warning: failed to load cluster state: %v", err)
				Verbose("Treating as empty state (no services installed)")
				st = state.New(cfg.Cluster.Name, isExternal, false, 0, false, 0)
			}
			if st == nil {
				st = state.New(cfg.Cluster.Name, isExternal, false, 0, false, 0)
			}
		}
	}

	// Determine if cluster exists for display purposes
	clusterExists := false
	if isExternal {
		// For external clusters, check if we successfully got kubeconfig
		clusterExists = (kubeconfig != "")
	} else {
		// For kind clusters, check if cluster exists
		exists, err := kindMgr.ClusterExists(cfg.Cluster.Name)
		if err != nil {
			Verbose("Warning: failed to check cluster existence: %v", err)
		} else {
			clusterExists = exists
		}
	}

	// Print plan header
	fmt.Println()
	printClusterPlan(cfg, clusterExists)
	fmt.Println()

	// Analyze services
	serviceInfos := make(map[string]*ServicePlanInfo)
	toAdd := 0
	toChange := 0
	noChange := 0
	skipped := 0

	for name, svc := range cfg.Services {
		svcCopy := svc

		// Check if service is disabled
		if !svc.IsEnabled() {
			Verbose("Service '%s' is disabled (will not be installed)", name)
			serviceInfos[name] = &ServicePlanInfo{
				Name:    name,
				Type:    svc.Type,
				Action:  "skipped",
				Details: "Disabled in configuration",
			}
			skipped++
			continue
		}

		info := analyzeService(&svcCopy, st, namespaces)
		serviceInfos[name] = info

		switch info.Action {
		case "add":
			toAdd++
		case "change":
			toChange++
		case "no-change":
			noChange++
		}
	}

	// Print service plan by level
	fmt.Printf("Services to install: %d\n\n", len(cfg.Services))

	for levelNum, level := range serviceLevels {
		if len(level) > 1 {
			fmt.Printf("%s (parallel installation):\n", color.Bold(fmt.Sprintf("Level %d", levelNum)))
		} else {
			fmt.Printf("%s:\n", color.Bold(fmt.Sprintf("Level %d", levelNum)))
		}

		for _, svc := range level {
			info := serviceInfos[svc.Name]
			printServicePlan(info)
		}
		fmt.Println()
	}

	// Print summary
	printPlanSummary(toAdd, toChange, noChange, skipped)

	return nil
}

func printClusterPlan(cfg *config.Config, exists bool) {
	fmt.Printf("%s %s\n", color.Bold("Cluster:"), cfg.Cluster.Name)

	existsStr := "no, will be created"
	if exists {
		existsStr = "yes"
	}
	fmt.Printf("  Status: %s\n", existsStr)

	if cfg.Cluster.Version != "" {
		fmt.Printf("  Version: %s\n", cfg.Cluster.Version)
	}

	if cfg.Cluster.Network != "" {
		fmt.Printf("  Network: %s", cfg.Cluster.Network)
		if cfg.Cluster.IPv4Address != "" {
			fmt.Printf(" (IP: %s)", cfg.Cluster.IPv4Address)
		}
		fmt.Println()
	}

	if cfg.Cluster.IsExternal() {
		fmt.Printf("  %s", color.Bold("External cluster"))
		if cfg.Cluster.External.Context != "" {
			fmt.Printf(" (context: %s)", cfg.Cluster.External.Context)
		}
		fmt.Println()
	}
}

func analyzeService(svc *config.ServiceConfig, st *state.ClusterState, namespaces clusterNamespaces) *ServicePlanInfo {
	info := &ServicePlanInfo{
		Name:      svc.Name,
		Type:      svc.Type,
		Namespace: svc.GetNamespace(),
		DependsOn: svc.DependsOn,
	}

	// Determine if service is installed
	installed := st.IsServiceInstalled(svc.Name)

	if !installed {
		info.Action = "add"
	} else {
		// Service exists - check for changes
		// For now, we'll mark as "no-change" but could detect version/config changes
		info.Action = "no-change"
	}

	info.NamespaceAction = namespaceAction(info.Namespace, svc.ShouldCreateNamespace(), st.GetCreatedNamespaces(), namespaces)

	// Build details string based on type
	if svc.IsHelm() {
		if svc.IsLocalChart() {
			info.Details = fmt.Sprintf("local chart %s", svc.Path)
		} else {
			chartRef := svc.Chart
			if svc.Version != "" {
				chartRef += "@" + svc.Version
				info.Version = svc.Version
			}
			info.Details = fmt.Sprintf("helm chart %s", chartRef)
		}
	} else if svc.IsManifests() {
		if strings.HasPrefix(svc.Path, "http://") || strings.HasPrefix(svc.Path, "https://") {
			info.Details = "manifests from remote URL"
		} else {
			info.Details = "manifests from local path"
		}
	}

	return info
}

func printServicePlan(info *ServicePlanInfo) {
	// Action symbol and formatting
	formattedSymbol := " " // no-change, and a safe default for unknown actions
	switch info.Action {
	case "add":
		formattedSymbol = color.Green("+")
	case "change":
		formattedSymbol = color.Yellow("~")
	case "skipped":
		formattedSymbol = color.Gray("-")
	}

	// Print service line
	fmt.Printf("  %s %s", formattedSymbol, color.Bold(info.Name))
	if info.Action != "no-change" {
		fmt.Printf(" (%s)", info.Action)
	}
	fmt.Printf(" - %s\n", info.Details)

	// Skip namespace and dependency info for skipped services
	if info.Action == "skipped" {
		return
	}

	// Print namespace
	nsSymbol := " "
	nsAction := info.NamespaceAction
	switch info.NamespaceAction {
	case namespaceWillCreate:
		nsSymbol = color.Green("+")
	case namespaceManaged:
		nsSymbol = color.Green("✓")
	case namespaceMissing:
		// kraze up will fail for this service unless the namespace is created first
		nsSymbol = color.Yellow("!")
		nsAction = color.Yellow(nsAction)
	}
	fmt.Printf("    %s Namespace: %s (%s)\n", nsSymbol, info.Namespace, nsAction)

	// Print dependencies
	if len(info.DependsOn) > 0 {
		fmt.Printf("      Depends on: %s\n", strings.Join(info.DependsOn, ", "))
	}
}

func printPlanSummary(toAdd, toChange, noChange, skipped int) {
	fmt.Printf("%s", color.Bold("Plan:"))

	parts := []string{}
	if toAdd > 0 {
		parts = append(parts, color.Green(fmt.Sprintf("%d to add", toAdd)))
	}
	if toChange > 0 {
		parts = append(parts, color.Yellow(fmt.Sprintf("%d to change", toChange)))
	}
	if noChange > 0 {
		parts = append(parts, fmt.Sprintf("%d no change", noChange))
	}
	if skipped > 0 {
		parts = append(parts, color.Gray(fmt.Sprintf("%d skipped", skipped)))
	}

	if len(parts) > 0 {
		fmt.Printf(" %s\n", strings.Join(parts, ", "))
	} else {
		fmt.Println(" No changes")
	}

	fmt.Println()
	fmt.Printf("Run %s to execute this plan.\n", color.Bold("kraze up"))
}

func init() {
	planCmd.Flags().StringSliceVarP(&planLabels, "label", "l", []string{}, "Filter services by label (format: key=value, can be specified multiple times)")
}
