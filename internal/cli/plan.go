package cli

import (
	"context"
	"fmt"
	"strings"

	"github.com/hjames9/kraze/internal/cluster"
	"github.com/hjames9/kraze/internal/color"
	"github.com/hjames9/kraze/internal/config"
	"github.com/hjames9/kraze/internal/graph"
	"github.com/hjames9/kraze/internal/state"
	"github.com/spf13/cobra"
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
	NamespaceAction string // "create", "exists"
	DependsOn       []string
	Details         string
}

func runPlan(cmd *cobra.Command, args []string) error {
	ctx := context.Background()

	Verbose("Planning deployment from config file: %s", configFile)

	// Parse configuration
	cfg, err := config.Parse(configFile)
	if err != nil {
		return fmt.Errorf("failed to parse config: %w", err)
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

	// Load state to compare
	stateFilePath := state.GetStateFilePath(".")
	st, err := state.Load(stateFilePath)
	if err != nil {
		return fmt.Errorf("failed to load state: %w", err)
	}
	if st == nil {
		st = state.New(cfg.Cluster.Name, cfg.Cluster.IsExternal())
	}

	// Check cluster existence
	kindMgr := cluster.NewKindManager()
	clusterExists, err := kindMgr.ClusterExists(cfg.Cluster.Name)
	if err != nil {
		Verbose("Warning: failed to check cluster existence: %v", err)
		clusterExists = false
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

	for name, svc := range cfg.Services {
		svcCopy := svc
		info := analyzeService(ctx, &svcCopy, st, cfg)
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
	printPlanSummary(toAdd, toChange, noChange)

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

func analyzeService(ctx context.Context, svc *config.ServiceConfig, st *state.State, cfg *config.Config) *ServicePlanInfo {
	info := &ServicePlanInfo{
		Name:       svc.Name,
		Type:       svc.Type,
		Namespace:  svc.GetNamespace(),
		DependsOn:  svc.DependsOn,
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

	// Determine namespace action
	if svcState, exists := st.Services[svc.Name]; exists && svcState.CreatedNamespace {
		info.NamespaceAction = "managed by kraze"
	} else {
		// Check if namespace would be created
		if svc.ShouldCreateNamespace() {
			info.NamespaceAction = "will be created"
		} else {
			info.NamespaceAction = "existing"
		}
	}

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
	symbol := " "
	var formattedSymbol string
	switch info.Action {
	case "add":
		symbol = "+"
		formattedSymbol = color.Green(symbol)
	case "change":
		symbol = "~"
		formattedSymbol = color.Yellow(symbol)
	case "no-change":
		symbol = " "
		formattedSymbol = symbol
	}

	// Print service line
	fmt.Printf("  %s %s", formattedSymbol, color.Bold(info.Name))
	if info.Action != "no-change" {
		fmt.Printf(" (%s)", info.Action)
	}
	fmt.Printf(" - %s\n", info.Details)

	// Print namespace
	nsSymbol := " "
	switch info.NamespaceAction {
	case "will be created":
		nsSymbol = color.Green("+")
	case "managed by kraze":
		nsSymbol = color.Green("✓")
	}
	fmt.Printf("    %s Namespace: %s (%s)\n", nsSymbol, info.Namespace, info.NamespaceAction)

	// Print dependencies
	if len(info.DependsOn) > 0 {
		fmt.Printf("      Depends on: %s\n", strings.Join(info.DependsOn, ", "))
	}
}

func printPlanSummary(toAdd, toChange, noChange int) {
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
