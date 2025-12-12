package cli

import (
	"fmt"

	"github.com/hjames9/kraze/internal/color"
	"github.com/hjames9/kraze/internal/config"
	"github.com/spf13/cobra"
)

var validateCmd = &cobra.Command{
	Use:   "validate",
	Short: "Validate kraze.yml configuration",
	Long:  `Validate the syntax and structure of your kraze.yml configuration file.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		Verbose("Validating configuration file: %s", configFile)

		// Parse configuration file
		cfg, err := config.Parse(configFile)
		if err != nil {
			return fmt.Errorf("validation failed: %w", err)
		}

		// Print summary
		fmt.Printf("%s Configuration is valid\n\n", color.Checkmark())
		fmt.Printf("Cluster: %s\n", cfg.Cluster.Name)
		if cfg.Cluster.Version != "" {
			fmt.Printf("Kubernetes version: %s\n", cfg.Cluster.Version)
		}
		if cfg.Cluster.NodeImage != "" {
			fmt.Printf("Node image: %s\n", cfg.Cluster.NodeImage)
		}
		fmt.Printf("Services: %d\n", len(cfg.Services))

		if verbose {
			fmt.Println("\nServices:")
			enabledCount := 0
			disabledCount := 0
			for name, svc := range cfg.Services {
				if svc.IsEnabled() {
					enabledCount++
					fmt.Printf("  - %s (%s)\n", name, svc.Type)
					if len(svc.DependsOn) > 0 {
						fmt.Printf("    depends_on: %v\n", svc.DependsOn)
					}
				} else {
					disabledCount++
				}
			}
			if disabledCount > 0 {
				fmt.Printf("\n  (%d disabled service(s) not shown)\n", disabledCount)
			}
		}

		return nil
	},
}
