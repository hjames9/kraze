package cli

import (
	"context"
	"fmt"
	"strings"

	"github.com/hjames9/kraze/internal/cluster"
	"github.com/hjames9/kraze/internal/config"
	"github.com/spf13/cobra"
)

var listImagesCmd = &cobra.Command{
	Use:     "list-images",
	Aliases: []string{"images"},
	Short:   "List images loaded in the kind cluster",
	Long: `Display all Docker images currently loaded in the kind cluster nodes.

Examples:
  kraze list-images
  kraze images`,
	RunE: runListImages,
}

func runListImages(cmd *cobra.Command, args []string) error {
	ctx := context.Background()

	cfgPaths, err := resolveConfigFiles(cmd)
	if err != nil {
		return err
	}

	cfg, err := config.ParseMultiple(cfgPaths)
	if err != nil {
		return fmt.Errorf("failed to parse config: %w", err)
	}

	if cfg.Cluster.IsExternal() {
		return fmt.Errorf("list-images is only supported for kind clusters, not external clusters")
	}

	if err := cluster.CheckDockerAvailable(ctx); err != nil {
		return err
	}

	kindMgr := cluster.NewKindManager()
	exists, err := kindMgr.ClusterExists(cfg.Cluster.Name)
	if err != nil {
		return fmt.Errorf("failed to check cluster: %w", err)
	}
	if !exists {
		return fmt.Errorf("cluster '%s' does not exist. Run 'kraze up' first", cfg.Cluster.Name)
	}

	imgMgr := cluster.NewImageManager(verbose)
	images, err := imgMgr.ListClusterImages(ctx, cfg.Cluster.Name)
	if err != nil {
		return fmt.Errorf("failed to list images: %w", err)
	}

	fmt.Printf("Cluster: %s\n\n", cfg.Cluster.Name)
	fmt.Printf("%-72s %-20s %s\n", "IMAGE", "IMAGE ID", "SIZE")
	fmt.Println(strings.Repeat("-", 100))

	for _, img := range images {
		// Truncate image ID for display (keep sha256: prefix + first 12 chars of hash)
		id := img.ID
		if strings.HasPrefix(id, "sha256:") && len(id) > 19 {
			id = id[7:19]
		}

		size := formatImageSize(img.Size)

		if len(img.RepoTags) == 0 {
			fmt.Printf("%-72s %-20s %s\n", "<none>", id, size)
			continue
		}
		for i, tag := range img.RepoTags {
			if i == 0 {
				fmt.Printf("%-72s %-20s %s\n", tag, id, size)
			} else {
				fmt.Printf("%-72s\n", tag)
			}
		}
	}

	fmt.Printf("\n%d image(s) loaded in cluster '%s'\n", len(images), cfg.Cluster.Name)
	return nil
}

// formatImageSize converts a byte count string to a human-readable size
func formatImageSize(sizeStr string) string {
	if sizeStr == "" {
		return "unknown"
	}
	var bytes int64
	if _, err := fmt.Sscanf(sizeStr, "%d", &bytes); err != nil {
		return sizeStr
	}
	switch {
	case bytes >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(bytes)/float64(1<<30))
	case bytes >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(bytes)/float64(1<<20))
	case bytes >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(bytes)/float64(1<<10))
	default:
		return fmt.Sprintf("%d B", bytes)
	}
}
