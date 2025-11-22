package cli

import (
	"fmt"

	"github.com/spf13/cobra"
)

var (
	// Global flags
	configFile string
	verbose    bool
	dryRun     bool

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
	rootCmd.PersistentFlags().StringVarP(&configFile, "file", "f", "kraze.yml", "Path to kraze configuration file")
	rootCmd.PersistentFlags().BoolVarP(&verbose, "verbose", "v", false, "Verbose output")
	rootCmd.PersistentFlags().BoolVar(&dryRun, "dry-run", false, "Show what would happen without executing")

	// Add subcommands
	rootCmd.AddCommand(initCmd)
	rootCmd.AddCommand(upCmd)
	rootCmd.AddCommand(downCmd)
	rootCmd.AddCommand(statusCmd)
	rootCmd.AddCommand(destroyCmd)
	rootCmd.AddCommand(versionCmd)
	rootCmd.AddCommand(validateCmd)
	rootCmd.AddCommand(loadImageCmd)
	rootCmd.AddCommand(portForwardCmd)
}

// SetVersionInfo sets the version information for the CLI
func SetVersionInfo(v, commit, date string) {
	version = v
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
