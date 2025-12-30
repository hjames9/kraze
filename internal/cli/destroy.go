package cli

import (
	"context"
	"fmt"

	"github.com/hjames9/kraze/internal/cluster"
	"github.com/hjames9/kraze/internal/color"
	"github.com/hjames9/kraze/internal/config"
	"github.com/hjames9/kraze/internal/providers"
	"github.com/hjames9/kraze/internal/state"
	"github.com/spf13/cobra"
)

var destroyCmd = &cobra.Command{
	Use:   "destroy",
	Short: "Delete the cluster and clean up state",
	Long: `Completely remove the cluster and delete all associated state.

For kind clusters (default):
  - Permanently delete the cluster and all data in it
  - Delete the state file

For external clusters (cluster.external.enabled: true):
  - Only delete the state file (preserves the external cluster)

WARNING: For kind clusters, this will permanently delete the cluster and all data.
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

		isExternal := cfg.Cluster.IsExternal()

		if dryRun {
			if isExternal {
				fmt.Printf("[DRY RUN] Would delete state for external cluster '%s' (cluster preserved)\n", cfg.Cluster.Name)
			} else {
				fmt.Printf("[DRY RUN] Would destroy kind cluster '%s' and state\n", cfg.Cluster.Name)
			}
			return nil
		}

		// Delete cluster state ConfigMap (must be done before cluster deletion for external clusters)
		if isExternal {
			// External cluster - delete state ConfigMap before cluster is removed
			fmt.Printf("External cluster '%s' - preserving cluster, deleting state only\n", cfg.Cluster.Name)

			Verbose("Deleting cluster state ConfigMap...")
			kindMgr := cluster.NewKindManager()

			// Get kubeconfig content for external cluster
			kubeconfig, err := kindMgr.GetKubeconfigForExternalCluster(&cfg.Cluster)
			if err != nil {
				fmt.Printf("Warning: failed to get kubeconfig for external cluster: %v\n", err)
			} else {
				// Create clientset from kubeconfig content (external cluster uses TLS verification)
				clientset, err := providers.GetClientsetFromKubeconfigContent(kubeconfig, false)
				if err != nil {
					fmt.Printf("Warning: failed to create Kubernetes client: %v\n", err)
				} else {
					if err := state.Delete(ctx, clientset); err != nil {
						fmt.Printf("Warning: failed to delete cluster state: %v\n", err)
					} else {
						Verbose("Cluster state ConfigMap deleted from kube-system namespace")
					}
				}
			}
		} else {
			// Kind cluster - delete the cluster (ConfigMap will be deleted with cluster)
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
			Verbose("Kind cluster deleted (cluster state ConfigMap deleted with cluster)")
		}

		// TODO: Clean up cache (Helm chart cache, etc.)

		if isExternal {
			fmt.Printf("\n%s State deleted successfully (external cluster preserved)\n", color.Checkmark())
		} else {
			fmt.Printf("\n%s Cluster destroyed successfully\n", color.Checkmark())
		}
		return nil
	},
}
