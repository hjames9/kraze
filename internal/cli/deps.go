package cli

import (
	"context"
	"time"

	"github.com/hjames9/kraze/internal/cluster"
	"github.com/hjames9/kraze/internal/config"
	"github.com/hjames9/kraze/internal/providers"
	"k8s.io/client-go/kubernetes"
)

// clusterManager is what the commands use of *cluster.KindManager
type clusterManager interface {
	ClusterExists(name string) (bool, error)
	ListClusters() ([]string, error)
	CreateCluster(ctx context.Context, cfg *config.ClusterConfig) error
	DeleteCluster(name string) error
	GetKubeConfig(name string, internal bool) (string, error)
	GetKubeconfigForExternalCluster(cfg *config.ClusterConfig) (string, error)
	VerifyClusterAccess(ctx context.Context, kubeconfig string) error
	UpdateKubeconfigFile(name string) error
	LoadImage(ctx context.Context, clusterName, image string) error
	UntagImage(ctx context.Context, clusterName, image string) error
}

// imageManager is what the commands use of *cluster.ImageManager
type imageManager interface {
	GetImagesForService(ctx context.Context, svc *config.ServiceConfig, kubeconfig string) ([]string, error)
	GetImageInfo(ctx context.Context, image string) (*cluster.ImageInfo, error)
	GetClusterImageHash(ctx context.Context, clusterName, image string) (string, error)
	ListClusterImages(ctx context.Context, clusterName string) ([]cluster.ClusterImage, error)
}

var (
	_ clusterManager = (*cluster.KindManager)(nil)
	_ imageManager   = (*cluster.ImageManager)(nil)
)

// cliDeps is everything the commands reach outside the process: Docker, kind,
// the cluster and the clock. Commands go through deps rather than calling the
// cluster and providers packages directly, so tests can replace it (see
// withFakeDeps).
type cliDeps struct {
	// newClusterManager returns a new manager per command: a KindManager
	// remembers the network CreateCluster used, for GetKubeConfig
	newClusterManager func() clusterManager
	newImageManager   func(verbose bool) imageManager
	clientset         func(kubeconfig string, skipTLSVerify bool) (kubernetes.Interface, error)
	newProvider       func(svc *config.ServiceConfig, opts *providers.ProviderOptions) (providers.Provider, error)
	orphanBackend     func(opts *providers.ProviderOptions) orphanBackend
	checkDocker       func(ctx context.Context) error
	sleep             func(time.Duration)

	deleteNamespace      func(ctx context.Context, kubeconfig, namespace string) error
	waitNamespaceDeleted func(ctx context.Context, kubeconfig, namespace string, timeout time.Duration) error
	podsForService       func(ctx context.Context, kubeconfig string, svc *config.ServiceConfig) ([]string, error)
	portForward          func(ctx context.Context, kubeconfig, namespace, pod string, ports []string) error
}

var deps = realDeps()

func realDeps() cliDeps {
	return cliDeps{
		newClusterManager: func() clusterManager { return cluster.NewKindManager() },
		newImageManager:   func(verbose bool) imageManager { return cluster.NewImageManager(verbose) },
		clientset:         providers.GetClientsetFromKubeconfigContent,
		newProvider:       providers.NewProvider,
		orphanBackend: func(opts *providers.ProviderOptions) orphanBackend {
			return providerOrphanBackend{opts: opts}
		},
		checkDocker: cluster.CheckDockerAvailable,
		sleep:       time.Sleep,

		deleteNamespace:      providers.DeleteNamespace,
		waitNamespaceDeleted: providers.WaitForNamespaceDeletion,
		podsForService:       providers.GetPodsForService,
		portForward:          providers.PortForward,
	}
}
