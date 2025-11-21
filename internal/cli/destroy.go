package cli

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/hjames9/kraze/internal/cluster"
	"github.com/hjames9/kraze/internal/color"
	"github.com/hjames9/kraze/internal/config"
	"github.com/hjames9/kraze/internal/state"
	"github.com/spf13/cobra"
)

var destroyCmd = &cobra.Command{
	Use:   "destroy",
	Short: "Delete the kind cluster and clean up state",
	Long: `Completely remove the kind cluster and delete all associated state.

WARNING: This will permanently delete the cluster and all data in it.
Services do not need to be uninstalled first - the entire cluster is removed.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := context.Background()

		Verbose("Destroying cluster from config file: %s", configFile)

		// Parse config file to get cluster name
		Verbose("Parsing configuration...")
		cfg, err := config.Parse(configFile)
		if err != nil {
			return fmt.Errorf("failed to parse config: %w", err)
		}

		if dryRun {
			fmt.Printf("[DRY RUN] Would destroy kind cluster '%s' and state\n", cfg.Cluster.Name)
			return nil
		}

		// Check if Docker is available
		Verbose("Checking Docker availability...")
		if err := cluster.CheckDockerAvailable(ctx); err != nil {
			return err
		}

		// Delete kind cluster
		Verbose("Deleting kind cluster...")
		kindMgr := cluster.NewKindManager()
		if err := kindMgr.DeleteCluster(cfg.Cluster.Name); err != nil {
			return fmt.Errorf("failed to delete cluster: %w", err)
		}

		// Delete state file
		Verbose("Deleting state file...")
		configDir := filepath.Dir(configFile)
		stateFilePath := state.GetStateFilePath(configDir)
		if err := state.Delete(stateFilePath); err != nil {
			// Log warning but don't fail
			fmt.Printf("Warning: failed to delete state file: %v\n", err)
		} else {
			Verbose("State file deleted: %s", stateFilePath)
		}

		// TODO: Clean up cache (Helm chart cache, etc.)

		fmt.Printf("\n%s Cluster destroyed successfully\n", color.Checkmark())
		return nil
	},
}
