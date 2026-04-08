package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/hjames9/kraze/internal/cluster"
	"github.com/hjames9/kraze/internal/pack"
	"github.com/hjames9/kraze/internal/providers"
	"github.com/hjames9/kraze/internal/state"
	"github.com/spf13/cobra"
)

var (
	// Global flags
	configFiles []string
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
	return rootCmd.Execute()
}

func init() {
	// Global flags
	rootCmd.PersistentFlags().StringArrayVarP(&configFiles, "file", "f", []string{}, "Path to kraze configuration file (can be specified multiple times)")
	rootCmd.PersistentFlags().BoolVarP(&verbose, "verbose", "v", false, "Verbose output")
	rootCmd.PersistentFlags().BoolVar(&dryRun, "dry-run", false, "Show what would happen without executing")
	rootCmd.PersistentFlags().BoolVar(&plain, "plain", false, "Use plain scrolling output instead of interactive mode")

	// Add subcommands
	rootCmd.AddCommand(initCmd)
	rootCmd.AddCommand(upCmd)
	rootCmd.AddCommand(downCmd)
	rootCmd.AddCommand(statusCmd)
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

// resolveConfigFiles returns the absolute paths to the config files to use.
// Resolution order:
//  1. If one or more -f flags were explicitly provided, use those paths.
//  2. Enumerate kind clusters; if exactly one has stored ConfigPaths that exist
//     on disk, use them and print an informational message.
//  3. If kraze.yml exists in the current directory, use it.
//  4. Fall back to []string{"kraze.yml"} (preserves the original error from ParseMultiple).
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

	// Attempt cluster discovery — cluster state is authoritative for which config
	// files belong to this deployment, even if a kraze.yml exists in the cwd.
	kindMgr := cluster.NewKindManager()
	clusters, err := kindMgr.ListClusters()
	if err == nil && len(clusters) > 0 {
		type viableCluster struct {
			clusterName string
			configPaths []string
		}
		var viableClusters []viableCluster
		krazeClusterFound := false

		ctx := context.Background()
		for _, clusterName := range clusters {
			kubeconfig, err := kindMgr.GetKubeConfig(clusterName, false)
			if err != nil {
				continue
			}
			clientset, err := providers.GetClientsetFromKubeconfigContent(kubeconfig, true)
			if err != nil {
				continue
			}
			st, err := state.Load(ctx, clientset, clusterName)
			if err != nil || st == nil {
				continue
			}
			krazeClusterFound = true
			if !st.HasConfigPaths() {
				continue
			}
			var existingPaths []string
			for _, p := range st.GetConfigPaths() {
				if _, err := os.Stat(p); err == nil {
					existingPaths = append(existingPaths, p)
				} else {
					fmt.Printf("Warning: stored config path '%s' (cluster: %s) does not exist on this machine\n", p, clusterName)
				}
			}
			if len(existingPaths) > 0 {
				viableClusters = append(viableClusters, viableCluster{clusterName: clusterName, configPaths: existingPaths})
			}
		}

		switch len(viableClusters) {
		case 0:
			if krazeClusterFound {
				return nil, fmt.Errorf("no valid config path found for the kraze cluster — use -f to specify the config file (e.g. kraze status -f kraze.yml or kraze status -f package.tgz)")
			}
		case 1:
			v := viableClusters[0]
			fmt.Printf("Using config from cluster state: %s\n", strings.Join(v.configPaths, ", "))
			return v.configPaths, nil
		default:
			msg := "Multiple kraze clusters found with stored config paths. Use -f to specify which config to use:\n"
			for _, v := range viableClusters {
				msg += fmt.Sprintf("  %s (cluster: %s)\n", strings.Join(v.configPaths, ", "), v.clusterName)
			}
			return nil, fmt.Errorf("%s", msg)
		}
	}

	// kraze.yml exists in cwd
	if _, err := os.Stat("kraze.yml"); err == nil {
		abs, err := filepath.Abs("kraze.yml")
		if err != nil {
			return nil, fmt.Errorf("failed to resolve config path: %w", err)
		}
		return []string{abs}, nil
	}

	return []string{"kraze.yml"}, nil
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
