package cli

import (
	"fmt"

	"github.com/hjames9/kraze/internal/cluster"
	"github.com/hjames9/kraze/internal/color"
	"github.com/hjames9/kraze/internal/config"
	"github.com/spf13/cobra"
)

var loadImageCmd = &cobra.Command{
	Use:   "load-image IMAGE [IMAGE...]",
	Short: "Load Docker images into the kind cluster",
	Long: `Manually load one or more Docker images from your local Docker daemon
into the kind cluster.

This is useful for loading locally built images that aren't available in a registry.
Images must exist in your local Docker daemon before loading.

Examples:
  # Load a single image
  kraze load-image myapp:latest

  # Load multiple images
  kraze load-image myapp:latest myworker:v1.0

  # Load image for specific cluster
  kraze load-image --file dev.yml myapp:latest`,
	Args: cobra.MinimumNArgs(1),
	RunE: runLoadImage,
}

func runLoadImage(cmd *cobra.Command, args []string) error {
	ctx := cmd.Context()
	images := args

	Verbose("Loading images: %v", images)

	if dryRun {
		fmt.Printf("[DRY RUN] Would load %d image(s): %v\n", len(images), images)
		return nil
	}

	// Parse configuration to get cluster name
	cfg, err := config.Parse(configFile)
	if err != nil {
		return fmt.Errorf("failed to parse config: %w", err)
	}

	clusterName := cfg.Cluster.Name

	if verbose {
		fmt.Printf("Loading %d image(s) into cluster '%s'...\n", len(images), clusterName)
	}

	// Create kind manager
	kindMgr := cluster.NewKindManager()

	// Check if cluster exists
	exists, err := kindMgr.ClusterExists(clusterName)
	if err != nil {
		return fmt.Errorf("failed to check cluster: %w", err)
	}
	if !exists {
		return fmt.Errorf("cluster '%s' does not exist. Run 'kraze up' first", clusterName)
	}

	// Load each image
	for _, image := range images {
		fmt.Printf("Loading image '%s'...\n", image)

		if err := kindMgr.LoadImage(ctx, clusterName, image); err != nil {
			return fmt.Errorf("failed to load image '%s': %w", image, err)
		}

		fmt.Printf("%s Image '%s' loaded successfully\n", color.Checkmark(), image)
	}

	fmt.Printf("\n%s Successfully loaded %d image(s) into cluster '%s'\n", color.Checkmark(), len(images), clusterName)
	return nil
}
