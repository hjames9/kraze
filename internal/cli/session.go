package cli

import (
	"context"
	"fmt"
	"strings"

	"github.com/hjames9/kraze/internal/config"
	"github.com/hjames9/kraze/internal/pack"
	"github.com/hjames9/kraze/internal/providers"
	"github.com/hjames9/kraze/internal/state"
	"github.com/spf13/cobra"
	"k8s.io/client-go/kubernetes"
)

// loadedConfig is a parsed config and where it came from
type loadedConfig struct {
	cfg *config.Config
	// paths are the files parsed; original are the files given (a pack
	// archive rather than its extracted contents), which is what state stores
	// so later commands can find the config without -f
	paths, original []string
}

// loadConfig resolves, extracts and parses the config. verboseFormat, if
// set, is logged with the parsed file names before parsing. The caller must
// call cleanup, which removes an extracted archive.
func loadConfig(cmd *cobra.Command, verboseFormat string) (*loadedConfig, func(), error) {
	original, err := resolveConfigFiles(cmd)
	if err != nil {
		return nil, func() {}, err
	}
	paths, cleanup, err := pack.MaybeExtract(original)
	if err != nil {
		return nil, func() {}, err
	}
	if verboseFormat != "" {
		Verbose(verboseFormat, strings.Join(paths, ", "))
	}
	cfg, err := config.ParseMultiple(paths)
	if err != nil {
		cleanup()
		return nil, func() {}, fmt.Errorf("failed to parse config: %w", err)
	}
	return &loadedConfig{cfg: cfg, paths: paths, original: original}, cleanup, nil
}

// requireDocker checks that Docker is available, which a kind cluster needs
func requireDocker(ctx context.Context, cfg *config.Config) error {
	if cfg.Cluster.IsExternal() {
		return nil
	}
	Verbose("Checking Docker availability...")
	if err := deps.checkDocker(ctx); err != nil {
		return err
	}
	Verbose("Docker is available")
	return nil
}

// clusterKubeconfig returns the kubeconfig of the config's cluster: the
// external cluster's, or the kind cluster's
func clusterKubeconfig(mgr clusterManager, cfg *config.Config) (string, error) {
	if cfg.Cluster.IsExternal() {
		kubeconfig, err := mgr.GetKubeconfigForExternalCluster(&cfg.Cluster)
		if err != nil {
			return "", fmt.Errorf("failed to get kubeconfig for external cluster: %w", err)
		}
		return kubeconfig, nil
	}
	kubeconfig, err := mgr.GetKubeConfig(cfg.Cluster.Name, false)
	if err != nil {
		return "", fmt.Errorf("failed to get kubeconfig: %w", err)
	}
	return kubeconfig, nil
}

// clusterClient creates a client for the config's cluster. TLS verification
// is only skipped for kind clusters.
func clusterClient(cfg *config.Config, kubeconfig string) (kubernetes.Interface, error) {
	return deps.clientset(kubeconfig, !cfg.Cluster.IsExternal())
}

// clusterConn is a connection to the config's cluster
type clusterConn struct {
	kubeconfig string
	clientset  kubernetes.Interface
	// created is set when connectCluster created the kind cluster
	created bool
}

// missingCluster says what connectCluster does when the kind cluster doesn't
// exist
type missingCluster int

const (
	// createMissing creates it (up)
	createMissing missingCluster = iota
	// skipMissing returns a nil connection (down)
	skipMissing
)

// connectCluster connects to the config's cluster: an external cluster
// must be reachable; a kind cluster that doesn't exist is created or
// skipped, as onMissing says
func connectCluster(ctx context.Context, mgr clusterManager, cfg *config.Config, onMissing missingCluster) (*clusterConn, error) {
	conn := &clusterConn{}
	if cfg.Cluster.IsExternal() {
		Verbose("Using external cluster '%s'", cfg.Cluster.Name)
		kubeconfig, err := clusterKubeconfig(mgr, cfg)
		if err != nil {
			return nil, err
		}
		Verbose("Verifying cluster connectivity...")
		if err := mgr.VerifyClusterAccess(ctx, kubeconfig); err != nil {
			return nil, fmt.Errorf("failed to access external cluster '%s': %w", cfg.Cluster.Name, err)
		}
		Verbose("External cluster is accessible")
		conn.kubeconfig = kubeconfig
	} else {
		exists, err := mgr.ClusterExists(cfg.Cluster.Name)
		if err != nil {
			return nil, fmt.Errorf("failed to check cluster: %w", err)
		}
		switch {
		case exists:
			Verbose("Cluster '%s' already exists", cfg.Cluster.Name)
		case onMissing == skipMissing:
			return nil, nil
		default:
			fmt.Printf("Cluster '%s' does not exist, creating it...\n", cfg.Cluster.Name)
			if err := mgr.CreateCluster(ctx, &cfg.Cluster); err != nil {
				return nil, fmt.Errorf("failed to create cluster: %w", err)
			}
			conn.created = true

			// Update ~/.kube/config with cluster access (Use container IP)
			Verbose("Updating kubeconfig...")
			if err := mgr.UpdateKubeconfigFile(cfg.Cluster.Name); err != nil {
				Verbose("Warning: failed to update kubeconfig: %v", err)
			} else {
				Verbose("Kubeconfig updated (context: kind-%s)", cfg.Cluster.Name)
			}
		}

		// Patched with the container IP
		if conn.kubeconfig, err = clusterKubeconfig(mgr, cfg); err != nil {
			return nil, err
		}
	}

	clientset, err := clusterClient(cfg, conn.kubeconfig)
	if err != nil {
		return nil, fmt.Errorf("failed to create Kubernetes client: %w", err)
	}
	conn.clientset = clientset
	return conn, nil
}

// newClusterState returns an empty state for the config's cluster
func newClusterState(cfg *config.Config, recordGPU bool) *state.ClusterState {
	nvidia, amd := false, false
	if recordGPU {
		nvidia, amd = cfg.Cluster.GPU.IsNvidiaEnabled(), cfg.Cluster.GPU.IsAMDEnabled()
	}
	return state.New(cfg.Cluster.Name, cfg.Cluster.IsExternal(), nvidia, 0, amd, 0)
}

// loadClusterState loads the cluster's state, or returns an empty one that
// records the cluster's GPUs if none is recorded yet; existing reports which.
// A state that can't be read is an error: up and init write it back.
func loadClusterState(ctx context.Context, clientset kubernetes.Interface, cfg *config.Config) (st *state.ClusterState, existing bool, err error) {
	st, err = state.Load(ctx, clientset, cfg.Cluster.Name)
	if err != nil {
		return nil, false, fmt.Errorf("failed to load cluster state: %w", err)
	}
	if st == nil {
		return newClusterState(cfg, true), false, nil
	}
	return st, true, nil
}

// loadClusterStateOrEmpty loads the cluster's state for down and plan, which
// treat a state that can't be read, or none, as empty
func loadClusterStateOrEmpty(ctx context.Context, clientset kubernetes.Interface, cfg *config.Config) *state.ClusterState {
	st, err := state.Load(ctx, clientset, cfg.Cluster.Name)
	if err != nil {
		Verbose("Warning: failed to load cluster state: %v", err)
		Verbose("Treating as empty state (no services installed)")
	}
	if err != nil || st == nil {
		return newClusterState(cfg, false)
	}
	return st
}

// adoptProject makes st act as the config's project, warning about unowned
// entries it won't claim. configured is every service in the config, from
// before filtering by name or label. With claim, it adopts the unowned
// entries of cfg.Services, as up, down and plan do.
func adoptProject(st *state.ClusterState, cfg *config.Config, configured map[string]bool, claim bool) {
	st.SetProject(cfg.Project)
	checkProjectAccess(st, cfg, configured)
	if claim {
		claimServices(st, cfg.Services)
	}
}

// baseProviderOptions are the provider options every command sets
func baseProviderOptions(cfg *config.Config, kubeconfig string) *providers.ProviderOptions {
	return &providers.ProviderOptions{
		ClusterName: cfg.Cluster.Name,
		Project:     cfg.Project,
		KubeConfig:  kubeconfig,
		Verbose:     verbose,
	}
}

// quietProviderOptions are baseProviderOptions for commands with a progress
// display, which providers mustn't write into unless verbose
func quietProviderOptions(cfg *config.Config, kubeconfig string) *providers.ProviderOptions {
	opts := baseProviderOptions(cfg, kubeconfig)
	opts.Quiet = !verbose
	return opts
}
