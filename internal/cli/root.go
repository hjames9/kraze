package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/hjames9/kraze/internal/cluster"
	"github.com/hjames9/kraze/internal/providers"
	"github.com/hjames9/kraze/internal/state"
	"github.com/spf13/cobra"
)

var (
	// Global flags
	configFile string
	verbose    bool
	dryRun     bool
	plain      bool

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
	rootCmd.PersistentFlags().StringVarP(&configFile, "file", "f", "", "Path to kraze configuration file (default: kraze.yml in current directory)")
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
	rootCmd.AddCommand(portForwardCmd)
	rootCmd.AddCommand(completionCmd)
}

// resolveConfigFile returns the absolute path to the config file to use.
// Resolution order:
//  1. If -f was explicitly provided, use that path.
//  2. If kraze.yml exists in the current directory, use it.
//  3. Enumerate kind clusters; if exactly one has a stored ConfigPath that exists
//     on disk, use it and print an informational message to stderr.
//  4. Fall back to "kraze.yml" (preserves the original error message from config.Parse).
func resolveConfigFile(cmd *cobra.Command) (string, error) {
	// -f was explicitly provided
	if configFile != "" {
		abs, err := filepath.Abs(configFile)
		if err != nil {
			return "", fmt.Errorf("failed to resolve config path: %w", err)
		}
		return abs, nil
	}

	// kraze.yml exists in cwd
	if _, err := os.Stat("kraze.yml"); err == nil {
		abs, err := filepath.Abs("kraze.yml")
		if err != nil {
			return "", fmt.Errorf("failed to resolve config path: %w", err)
		}
		return abs, nil
	}

	// Attempt cluster discovery
	kindMgr := cluster.NewKindManager()
	clusters, err := kindMgr.ListClusters()
	if err != nil || len(clusters) == 0 {
		return "kraze.yml", nil
	}

	type viable struct {
		clusterName string
		configPath  string
	}
	var viableClusters []viable
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
		for _, p := range st.GetConfigPaths() {
			if _, err := os.Stat(p); err == nil {
				viableClusters = append(viableClusters, viable{clusterName: clusterName, configPath: p})
				break
			} else {
				fmt.Printf("Warning: stored config path '%s' (cluster: %s) does not exist on this machine\n", p, clusterName)
			}
		}
	}

	switch len(viableClusters) {
	case 0:
		if krazeClusterFound {
			return "", fmt.Errorf("kraze cluster found but no config path stored — run 'kraze up -f <config>' to register it, or use -f to specify the config file")
		}
		return "kraze.yml", nil
	case 1:
		fmt.Printf("No kraze.yml in current directory. Using config from cluster state: %s\n", viableClusters[0].configPath)
		return viableClusters[0].configPath, nil
	default:
		msg := "Multiple kraze clusters found with stored config paths. Use -f to specify which config to use:\n"
		for _, v := range viableClusters {
			msg += fmt.Sprintf("  %s (cluster: %s)\n", v.configPath, v.clusterName)
		}
		return "", fmt.Errorf("%s", msg)
	}
}

// SetVersionInfo sets the version information for the CLI
func SetVersionInfo(ver, commit, date string) {
	version = ver
	gitCommit = commit
	buildDate = date
}

// GetConfigFile returns the path to the configuration file
func GetConfigFile() string {
	return configFile
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
