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

var initCmd = &cobra.Command{
	Use:   "init",
	Short: "Create and initialize the kind cluster",
	Long: `Create a new kind (Kubernetes in Docker) cluster based on the cluster
configuration defined in kraze.yml.

This command will:
  - Check if Docker is running
  - Create a kind cluster with the specified configuration
  - Initialize the state file`,
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := context.Background()

		Verbose("Initializing cluster from config file: %s", configFile)

		// Check Docker availability
		Verbose("Checking Docker availability...")
		if err := cluster.CheckDockerAvailable(ctx); err != nil {
			return err
		}
		Verbose("Docker is available")

		// Parse config file
		Verbose("Parsing configuration...")
		cfg, err := config.Parse(configFile)
		if err != nil {
			return fmt.Errorf("failed to parse config: %w", err)
		}
		Verbose("Configuration parsed successfully")

		if dryRun {
			fmt.Printf("[DRY RUN] Would create kind cluster '%s'\n", cfg.Cluster.Name)
			if cfg.Cluster.Version != "" {
				fmt.Printf("  Kubernetes version: %s\n", cfg.Cluster.Version)
			}
			fmt.Printf("  Nodes: %d\n", len(cfg.Cluster.Config))
			return nil
		}

		// Create kind cluster
		Verbose("Creating kind cluster...")
		kindMgr := cluster.NewKindManager()
		if err := kindMgr.CreateCluster(ctx, &cfg.Cluster); err != nil {
			return fmt.Errorf("failed to create cluster: %w", err)
		}

		// Preload images if specified
		if len(cfg.Cluster.PreloadImages) > 0 {
			fmt.Printf("\nPreloading %d image(s) into cluster...\n", len(cfg.Cluster.PreloadImages))

			for itr, image := range cfg.Cluster.PreloadImages {
				fmt.Printf("[%d/%d] Loading image '%s'...\n", itr+1, len(cfg.Cluster.PreloadImages), image)

				if err := kindMgr.LoadImage(ctx, cfg.Cluster.Name, image); err != nil {
					// Don't fail cluster creation if image loading fails
					fmt.Printf("Warning: failed to load image '%s': %v\n", image, err)
					fmt.Printf("  You can load it later with: kraze load-image %s\n", image)
				} else {
					fmt.Printf("%s Image '%s' loaded successfully\n", color.Checkmark(), image)
				}
			}

			fmt.Printf("\n%s Images preloaded successfully\n", color.Checkmark())
		}

		// Initialize state file
		Verbose("Initializing state file...")
		configDir := filepath.Dir(configFile)
		stateFilePath := state.GetStateFilePath(configDir)

		st := state.New(cfg.Cluster.Name)
		if err := st.Save(stateFilePath); err != nil {
			return fmt.Errorf("failed to save state: %w", err)
		}
		Verbose("State file created: %s", stateFilePath)

		fmt.Printf("\n%s Cluster initialized successfully\n", color.Checkmark())
		fmt.Printf("\nTo start services, run: kraze up\n")
		return nil
	},
}
