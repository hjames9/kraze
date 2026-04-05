package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/hjames9/kraze/internal/config"
	"github.com/hjames9/kraze/internal/pack"
	"github.com/spf13/cobra"
)

var packOutput string

var packCmd = &cobra.Command{
	Use:   "pack",
	Short: "Package a kraze deployment into a portable archive",
	Long: `Bundle a kraze deployment into a portable .tar.gz archive for sharing.

The package includes:
  - Configuration file(s)
  - Local Helm chart directories
  - Local manifest files and directories
  - Values files
  - CA certificate files
  - Remote Helm charts (pulled from OCI/HTTPS registries)
  - Remote HTTP manifests (downloaded at pack time)

Container images are NOT bundled — they are fetched from registries at deploy time.

The recipient can run the package directly:
  kraze up -f myapp.tar.gz
  kraze validate -f myapp.tar.gz`,
	RunE: runPack,
}

func init() {
	packCmd.Flags().StringVarP(&packOutput, "output", "o", "", "Output file path (default: <cluster-name>.tar.gz)")
}

func runPack(cmd *cobra.Command, args []string) error {
	cfgPaths, err := resolveConfigFiles(cmd)
	if err != nil {
		return err
	}

	cfg, err := config.ParseMultiple(cfgPaths)
	if err != nil {
		return fmt.Errorf("failed to parse config: %w", err)
	}

	// Determine output path.
	outputPath := packOutput
	if outputPath == "" {
		outputPath = cfg.Cluster.Name + ".tar.gz"
	}
	absOutput, err := filepath.Abs(outputPath)
	if err != nil {
		return fmt.Errorf("resolving output path: %w", err)
	}

	// Warn about extraMounts that won't be bundled.
	if hasExtraMounts(cfg) {
		fmt.Printf("Warning: cluster.config[].extraMounts are not bundled (they are runtime host paths).\n")
		fmt.Printf("         The target machine must have these paths available.\n\n")
	}

	// Summarise what will be pulled/downloaded.
	summariseRemoteAssets(cfg)

	fmt.Printf("Packaging to %s...\n", outputPath)

	if err := pack.CreatePackage(cfgPaths, cfg, version, absOutput, verbose); err != nil {
		return fmt.Errorf("packaging failed: %w", err)
	}

	// Print final size.
	info, err := os.Stat(absOutput)
	if err == nil {
		fmt.Printf("Created %s (%s)\n", outputPath, humanBytes(info.Size()))
	} else {
		fmt.Printf("Created %s\n", outputPath)
	}

	return nil
}

func hasExtraMounts(cfg *config.Config) bool {
	for _, node := range cfg.Cluster.Config {
		if len(node.ExtraMounts) > 0 {
			return true
		}
	}
	return false
}

func summariseRemoteAssets(cfg *config.Config) {
	var charts, manifests []string
	for name, svc := range cfg.Services {
		if !svc.IsEnabled() {
			continue
		}
		if svc.IsRemoteChart() {
			charts = append(charts, name)
		}
		if svc.IsManifests() && config.IsHTTPURL(svc.Path) {
			manifests = append(manifests, name)
		}
	}
	if len(charts) > 0 {
		fmt.Printf("  Pulling remote Helm chart(s): %s\n", strings.Join(charts, ", "))
	}
	if len(manifests) > 0 {
		fmt.Printf("  Downloading HTTP manifest(s): %s\n", strings.Join(manifests, ", "))
	}

	// Warn about external cluster kubeconfig not being bundled.
	if cfg.Cluster.External != nil && cfg.Cluster.External.Kubeconfig != "" {
		fmt.Printf("Warning: cluster.external.kubeconfig is not bundled (environment-specific).\n")
	}
}

// humanBytes returns a human-readable byte size string.
func humanBytes(n int64) string {
	switch {
	case n >= 1024*1024:
		return fmt.Sprintf("%.1f MB", float64(n)/(1024*1024))
	case n >= 1024:
		return fmt.Sprintf("%.1f KB", float64(n)/1024)
	default:
		return fmt.Sprintf("%d B", n)
	}
}
