package cli

import (
	"context"
	"fmt"
	"strings"

	"github.com/hjames9/kraze/internal/cluster"
	"github.com/hjames9/kraze/internal/color"
	"github.com/hjames9/kraze/internal/config"
	"github.com/hjames9/kraze/internal/history"
	"github.com/hjames9/kraze/internal/providers"
	"github.com/hjames9/kraze/internal/state"
	"github.com/spf13/cobra"
	"k8s.io/client-go/kubernetes"
)

var destroyForce bool

var destroyCmd = &cobra.Command{
	Use:   "destroy",
	Short: "Delete the cluster and clean up state",
	Long: `Completely remove the cluster and delete all associated state.

For kind clusters (default):
  - Permanently delete the cluster and all data in it
  - Delete the state file

For external clusters (cluster.external.enabled: true):
  - Only delete the state file (preserves the external cluster)

On a cluster shared with other projects (see the top-level project key):
  - kind clusters are not deleted unless --force is given
  - external clusters keep the other projects' state; only this project's is removed

WARNING: For kind clusters, this will permanently delete the cluster and all data.
Services do not need to be uninstalled first - the entire cluster is removed.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := cmd.Context()

		cfgPaths, cleanupPack, err := resolveAndExtractConfigFiles(cmd)
		if err != nil {
			return err
		}
		defer cleanupPack()
		Verbose("Destroying cluster from config file(s): %s", strings.Join(cfgPaths, ", "))

		// Parse config file to get cluster name
		Verbose("Parsing configuration...")
		cfg, err := config.ParseMultiple(cfgPaths)
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
					removeExternalClusterState(ctx, clientset, cfg)
				}
			}
		} else {
			// Kind cluster - delete the cluster (ConfigMap will be deleted with cluster)
			// Check if Docker is available
			Verbose("Checking Docker availability...")
			if err := cluster.CheckDockerAvailable(ctx); err != nil {
				return err
			}

			kindMgr := cluster.NewKindManager()
			if err := checkDestroySharedCluster(ctx, kindMgr, cfg); err != nil {
				return err
			}

			// Delete kind cluster
			Verbose("Deleting kind cluster...")
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

func init() {
	destroyCmd.Flags().BoolVar(&destroyForce, "force", false, "Delete the cluster even if other projects have services in it")
}

// checkDestroySharedCluster refuses to delete a kind cluster that other
// projects still use, unless --force is given. A state that can't be read
// (cluster already gone or unreachable) doesn't block the destroy.
func checkDestroySharedCluster(ctx context.Context, kindMgr *cluster.KindManager, cfg *config.Config) error {
	if destroyForce {
		return nil
	}
	exists, err := kindMgr.ClusterExists(cfg.Cluster.Name)
	if err != nil || !exists {
		return nil
	}
	kubeconfig, err := kindMgr.GetKubeConfig(cfg.Cluster.Name, false)
	if err != nil {
		return nil
	}
	clientset, err := providers.GetClientsetFromKubeconfigContent(kubeconfig, true)
	if err != nil {
		return nil
	}
	st, err := state.Load(ctx, clientset, cfg.Cluster.Name)
	if err != nil {
		return nil
	}
	return sharedClusterError(st, cfg, destroyForce)
}

// sharedClusterError refuses to delete a cluster whose state shows services
// of other projects, unless force is set. A nil state (none recorded) passes.
func sharedClusterError(st *state.ClusterState, cfg *config.Config, force bool) error {
	if force || st == nil {
		return nil
	}
	st.SetProject(cfg.Project)
	if others := st.OtherProjects(); len(others) > 0 {
		return fmt.Errorf("cluster '%s' is also used by project(s) %s; run `kraze down` in those projects first, or use --force to delete it anyway",
			cfg.Cluster.Name, strings.Join(others, ", "))
	}
	return nil
}

// removeExternalClusterState deletes kraze's state and history from an
// external cluster. If other projects still use the cluster, only this
// project's entries are removed and both ConfigMaps are kept.
func removeExternalClusterState(ctx context.Context, clientset kubernetes.Interface, cfg *config.Config) {
	st, err := state.Load(ctx, clientset, cfg.Cluster.Name)
	if err != nil {
		fmt.Printf("Warning: failed to load cluster state: %v\n", err)
		return
	}
	if st != nil {
		st.SetProject(cfg.Project)
		if others := st.OtherProjects(); len(others) > 0 {
			st.RemoveProject()
			if err := st.Save(ctx, clientset); err != nil {
				fmt.Printf("Warning: failed to remove project state: %v\n", err)
				return
			}
			fmt.Printf("Removed this project's state; kept state and history for project(s) %s\n", strings.Join(others, ", "))
			return
		}
	}

	if err := state.Delete(ctx, clientset); err != nil {
		fmt.Printf("Warning: failed to delete cluster state: %v\n", err)
	} else {
		Verbose("Cluster state ConfigMap deleted from kube-system namespace")
	}
	if err := history.Delete(ctx, clientset); err != nil {
		fmt.Printf("Warning: failed to delete cluster history: %v\n", err)
	} else {
		Verbose("Cluster history ConfigMap deleted from kube-system namespace")
	}
}
