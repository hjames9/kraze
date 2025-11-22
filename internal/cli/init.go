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
	Short: "Initialize the cluster configuration",
	Long: `Initialize the cluster configuration defined in kraze.yml.

For kind clusters (default):
  - Check if Docker is running
  - Create a kind cluster with the specified configuration
  - Initialize the state file

For external clusters (cluster.external.enabled: true):
  - Verify the cluster is accessible
  - Initialize the state file`,
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := context.Background()

		Verbose("Initializing cluster from config file: %s", configFile)

		// Parse config file
		Verbose("Parsing configuration...")
		cfg, err := config.Parse(configFile)
		if err != nil {
			return fmt.Errorf("failed to parse config: %w", err)
		}
		Verbose("Configuration parsed successfully")

		isExternal := cfg.Cluster.IsExternal()
		kindMgr := cluster.NewKindManager()

		if isExternal {
			// External cluster mode
			fmt.Printf("Using external cluster '%s'\n", cfg.Cluster.Name)
			Verbose("External cluster mode - skipping cluster creation")

			if dryRun {
				fmt.Printf("[DRY RUN] Would verify external cluster '%s' is accessible\n", cfg.Cluster.Name)
				return nil
			}

			// Verify cluster is accessible
			kubeconfig, err := kindMgr.GetKubeconfigForExternalCluster(&cfg.Cluster)
			if err != nil {
				return fmt.Errorf("failed to get kubeconfig for external cluster: %w", err)
			}

			// Test cluster connectivity
			Verbose("Verifying cluster connectivity...")
			if err := kindMgr.VerifyClusterAccess(ctx, kubeconfig); err != nil {
				return fmt.Errorf("failed to access external cluster '%s': %w", cfg.Cluster.Name, err)
			}

			fmt.Printf("%s External cluster is accessible\n", color.Checkmark())
		} else {
			// Kind cluster mode (default)
			Verbose("Creating kind cluster...")

			// Check Docker availability
			Verbose("Checking Docker availability...")
			if err := cluster.CheckDockerAvailable(ctx); err != nil {
				return err
			}
			Verbose("Docker is available")

			if dryRun {
				fmt.Printf("[DRY RUN] Would create kind cluster '%s'\n", cfg.Cluster.Name)
				if cfg.Cluster.Version != "" {
					fmt.Printf("  Kubernetes version: %s\n", cfg.Cluster.Version)
				}
				if cfg.Cluster.NodeImage != "" {
					fmt.Printf("  Node image: %s\n", cfg.Cluster.NodeImage)
				} else if cfg.Cluster.Version != "" {
					fmt.Printf("  Node image: kindest/node:v%s\n", cfg.Cluster.Version)
				}
				fmt.Printf("  Nodes: %d\n", len(cfg.Cluster.Config))
				return nil
			}

			// Create kind cluster
			if err := kindMgr.CreateCluster(ctx, &cfg.Cluster); err != nil {
				return fmt.Errorf("failed to create cluster: %w", err)
			}

			// Update ~/.kube/config with cluster access (Use container IP in case you're accessing control plane from another container)
			fmt.Printf("\nUpdating kubeconfig...\n")
			if err := kindMgr.UpdateKubeconfigFile(cfg.Cluster.Name); err != nil {
				fmt.Printf("Warning: failed to update kubeconfig: %v\n", err)
				fmt.Printf("You may need to manually run: kind export kubeconfig --name %s\n", cfg.Cluster.Name)
			} else {
				fmt.Printf("%s Kubeconfig updated (context: kind-%s)\n", color.Checkmark(), cfg.Cluster.Name)
			}
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

		st := state.New(cfg.Cluster.Name, isExternal)
		if err := st.Save(stateFilePath); err != nil {
			return fmt.Errorf("failed to save state: %w", err)
		}
		Verbose("State file created: %s", stateFilePath)

		if isExternal {
			fmt.Printf("\n%s External cluster initialized successfully\n", color.Checkmark())
		} else {
			fmt.Printf("\n%s Cluster initialized successfully\n", color.Checkmark())
		}
		fmt.Printf("\nTo start services, run: kraze up\n")
		return nil
	},
}
