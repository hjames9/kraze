package cli

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"syscall"

	"github.com/hjames9/kraze/internal/config"
	"github.com/hjames9/kraze/internal/pack"
	"github.com/hjames9/kraze/internal/state"
	"github.com/spf13/cobra"
)

var (
	// Global flags
	configFiles []string
	projectFlag string
	verbose     bool
	dryRun      bool
	plain       bool

	// Version information
	version   string
	gitCommit string
	buildDate string
)

// rootCmd represents the base command when called without any subcommands
var rootCmd = &cobra.Command{
	Use:   "kraze",
	Short: "Kubernetes development environment manager",
	Long: `kraze brings the simplicity and developer experience of docker-compose
to Kubernetes local development.

Manage kind clusters and orchestrate the installation, upgrade, and removal of
services defined in a declarative YAML configuration file.`,
	SilenceUsage:  true,
	SilenceErrors: true,
}

// Execute adds all child commands to the root command and sets flags appropriately.
// This is called by main.main(). It only needs to happen once to the rootCmd.
func Execute() error {
	ctx, stop := interruptContext()
	defer stop()
	return rootCmd.ExecuteContext(ctx)
}

// interruptContext returns a context that is cancelled on the first SIGINT or
// SIGTERM, so commands can stop cleanly and record their outcome in the cluster
// history. A second signal exits immediately.
func interruptContext() (context.Context, func()) {
	ctx, cancel := context.WithCancel(context.Background())
	sigs := make(chan os.Signal, 2)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)

	go func() {
		select {
		case <-sigs:
		case <-ctx.Done():
			return
		}
		fmt.Fprintln(os.Stderr, "\nInterrupted, stopping (press Ctrl-C again to force quit)")
		cancel()
		<-sigs
		os.Exit(130)
	}()

	return ctx, func() {
		signal.Stop(sigs)
		cancel()
	}
}

func init() {
	// Global flags
	rootCmd.PersistentFlags().StringArrayVarP(&configFiles, "file", "f", []string{}, "Path to kraze configuration file (can be specified multiple times)")
	rootCmd.PersistentFlags().StringVar(&projectFlag, "project", "", "Use the config stored in cluster state for this project (when no -f is given)")
	rootCmd.PersistentFlags().BoolVarP(&verbose, "verbose", "v", false, "Verbose output")
	rootCmd.PersistentFlags().BoolVar(&dryRun, "dry-run", false, "Show what would happen without executing")
	rootCmd.PersistentFlags().BoolVar(&plain, "plain", false, "Use plain scrolling output instead of interactive mode")

	// Add subcommands
	rootCmd.AddCommand(initCmd)
	rootCmd.AddCommand(upCmd)
	rootCmd.AddCommand(downCmd)
	rootCmd.AddCommand(statusCmd)
	rootCmd.AddCommand(historyCmd)
	rootCmd.AddCommand(destroyCmd)
	rootCmd.AddCommand(versionCmd)
	rootCmd.AddCommand(validateCmd)
	rootCmd.AddCommand(planCmd)
	rootCmd.AddCommand(loadImageCmd)
	rootCmd.AddCommand(listImagesCmd)
	rootCmd.AddCommand(portForwardCmd)
	rootCmd.AddCommand(completionCmd)
	rootCmd.AddCommand(packCmd)
}

// discoveredCluster is a kind cluster and its kraze state (nil if the cluster
// has none or it couldn't be read)
type discoveredCluster struct {
	name  string
	state *state.ClusterState
}

// discoverClusterStates lists kind clusters and loads each one's kraze state,
// or only the cluster named only when it is non-empty. It is a variable so
// tests can substitute clusters without Docker.
var discoverClusterStates = func(ctx context.Context, only string) []discoveredCluster {
	kindMgr := deps.newClusterManager()
	clusters, err := kindMgr.ListClusters()
	if err != nil {
		return nil
	}

	discovered := make([]discoveredCluster, 0, len(clusters))
	for _, clusterName := range clusters {
		if only != "" && clusterName != only {
			continue
		}
		dc := discoveredCluster{name: clusterName}
		if kubeconfig, err := kindMgr.GetKubeConfig(clusterName, false); err == nil {
			if clientset, err := deps.clientset(kubeconfig, true); err == nil {
				if st, err := state.Load(ctx, clientset, clusterName); err == nil {
					dc.state = st
				}
			}
		}
		discovered = append(discovered, dc)
	}
	return discovered
}

// resolveConfigFiles returns the absolute paths to the config files to use.
// Resolution order:
//  1. If one or more -f flags were explicitly provided, use those paths.
//  2. If kraze.yml exists in the current directory, use it. If its cluster's
//     state records a multi-file config for its project that includes this
//     file, use that whole set instead (`kraze up -f kraze.yml -f extra.yml`
//     followed by a plain `kraze status`). Skipped when --project is given.
//  3. Enumerate kind clusters and the projects stored in their state; if
//     exactly one (cluster, project) has config paths that exist on disk, use
//     them. --project limits this to that project.
//  4. Fall back to []string{"kraze.yml"} (preserves the original error from ParseMultiple).
//
// The current directory's kraze.yml wins over cluster state because several
// projects can share a cluster: the state may name another project's config.
func resolveConfigFiles(cmd *cobra.Command) ([]string, error) {
	// -f was explicitly provided
	if len(configFiles) > 0 {
		resolved := make([]string, 0, len(configFiles))
		for _, f := range configFiles {
			abs, err := filepath.Abs(f)
			if err != nil {
				return nil, fmt.Errorf("failed to resolve config path '%s': %w", f, err)
			}
			resolved = append(resolved, abs)
		}
		return resolved, nil
	}

	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background() // not run via Execute (tests)
	}

	// kraze.yml exists in cwd
	if projectFlag == "" {
		if _, err := os.Stat("kraze.yml"); err == nil {
			abs, err := filepath.Abs("kraze.yml")
			if err != nil {
				return nil, fmt.Errorf("failed to resolve config path: %w", err)
			}
			if stored := storedConfigSetFor(ctx, abs); stored != nil {
				fmt.Printf("Using config from cluster state: %s\n", strings.Join(stored, ", "))
				return stored, nil
			}
			return []string{abs}, nil
		}
	}

	// Attempt cluster discovery
	type viableProject struct {
		clusterName string
		project     string
		configPaths []string
	}
	var viable []viableProject
	krazeClusterFound := false

	for _, dc := range discoverClusterStates(ctx, "") {
		if dc.state == nil {
			continue
		}
		krazeClusterFound = true

		projectPaths := dc.state.AllProjectConfigPaths()
		projects := make([]string, 0, len(projectPaths))
		for project := range projectPaths {
			projects = append(projects, project)
		}
		sort.Strings(projects)

		for _, project := range projects {
			if projectFlag != "" && project != projectFlag {
				continue
			}
			var existingPaths []string
			for _, p := range projectPaths[project] {
				if _, err := os.Stat(p); err == nil {
					existingPaths = append(existingPaths, p)
				} else {
					fmt.Printf("Warning: stored config path '%s' (cluster: %s) does not exist on this machine\n", p, dc.name)
				}
			}
			if len(existingPaths) > 0 {
				viable = append(viable, viableProject{clusterName: dc.name, project: project, configPaths: existingPaths})
			}
		}
	}

	switch len(viable) {
	case 0:
		if projectFlag != "" {
			return nil, fmt.Errorf("no stored config found for project '%s' — use -f to specify the config file", projectFlag)
		}
		if krazeClusterFound {
			return nil, fmt.Errorf("no valid config path found for the kraze cluster — use -f to specify the config file (e.g. kraze status -f kraze.yml or kraze status -f package.tgz)")
		}
	case 1:
		v := viable[0]
		fmt.Printf("Using config from cluster state: %s\n", strings.Join(v.configPaths, ", "))
		return v.configPaths, nil
	default:
		msg := "Multiple kraze projects found with stored config paths. Use -f or --project to specify which config to use:\n"
		for _, v := range viable {
			msg += fmt.Sprintf("  %s (cluster: %s, project: %s)\n", strings.Join(v.configPaths, ", "), v.clusterName, projectLabel(v.project))
		}
		return nil, fmt.Errorf("%s", msg)
	}

	return []string{"kraze.yml"}, nil
}

// storedConfigSetFor returns the config paths stored for configPath's project
// on its kind cluster, if they include configPath and all still exist.
// Otherwise it returns nil.
func storedConfigSetFor(ctx context.Context, configPath string) []string {
	identity, err := config.ParseIdentity(configPath)
	if err != nil || identity.External || identity.Cluster == "" {
		return nil
	}

	for _, dc := range discoverClusterStates(ctx, identity.Cluster) {
		if dc.name != identity.Cluster || dc.state == nil {
			continue
		}
		stored := dc.state.AllProjectConfigPaths()[identity.Project]
		if len(stored) < 2 || !slices.Contains(stored, configPath) {
			return nil
		}
		for _, p := range stored {
			if _, err := os.Stat(p); err != nil {
				return nil
			}
		}
		return stored
	}
	return nil
}

// projectLabel names a project for display; the anonymous project has no name
func projectLabel(project string) string {
	if project == "" {
		return "(no project)"
	}
	return project
}

// resolveAndExtractConfigFiles resolves config files and transparently extracts
// any pack archive (.tar.gz/.tgz) to a temp directory. The caller must defer
// the returned cleanup function to remove the temp directory.
// For non-archive configs the cleanup is a no-op and paths are unchanged.
func resolveAndExtractConfigFiles(cmd *cobra.Command) ([]string, func(), error) {
	paths, err := resolveConfigFiles(cmd)
	if err != nil {
		return nil, func() {}, err
	}
	extracted, cleanup, err := pack.MaybeExtract(paths)
	if err != nil {
		return nil, func() {}, err
	}
	return extracted, cleanup, nil
}

// SetVersionInfo sets the version information for the CLI
func SetVersionInfo(ver, commit, date string) {
	version = ver
	gitCommit = commit
	buildDate = date
}

// GetConfigFiles returns the paths to the configuration files
func GetConfigFiles() []string {
	return configFiles
}

// IsVerbose returns whether verbose mode is enabled
func IsVerbose() bool {
	return verbose
}

// IsDryRun returns whether dry-run mode is enabled
func IsDryRun() bool {
	return dryRun
}

// Verbose prints a message only if verbose mode is enabled
func Verbose(format string, args ...interface{}) {
	if verbose {
		fmt.Printf("[VERBOSE] "+format+"\n", args...)
	}
}
