package cluster

import (
	"context"
	"io/fs"
	"os"
	osexec "os/exec"
	"strings"
	"time"

	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"sigs.k8s.io/kind/pkg/cluster"
	"sigs.k8s.io/kind/pkg/cluster/images"
	"sigs.k8s.io/kind/pkg/cluster/images/layeraware"
	"sigs.k8s.io/kind/pkg/cluster/nodes"
	"sigs.k8s.io/kind/pkg/cluster/nodeutils"
)

// What the cluster package reaches outside the process: commands (docker,
// kind), kind's provider and the host's files. KindManager and ImageManager
// hold them as fields so tests can replace them.

// commandRunner runs host commands
type commandRunner interface {
	// Output runs a command and returns its stdout. A failure is returned as
	// os/exec reports it (*exec.ExitError carries stderr).
	Output(ctx context.Context, name string, args ...string) ([]byte, error)
	// CombinedOutput runs a command with stdin (none if empty) and returns
	// its stdout and stderr together
	CombinedOutput(ctx context.Context, stdin, name string, args ...string) ([]byte, error)
}

// execRunner runs commands with os/exec
type execRunner struct{}

func (execRunner) Output(ctx context.Context, name string, args ...string) ([]byte, error) {
	return osexec.CommandContext(ctx, name, args...).Output()
}

func (execRunner) CombinedOutput(ctx context.Context, stdin, name string, args ...string) ([]byte, error) {
	cmd := osexec.CommandContext(ctx, name, args...)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	return cmd.CombinedOutput()
}

// kindProvider is what KindManager uses of kind's *cluster.Provider
type kindProvider interface {
	Create(name string, options ...cluster.CreateOption) error
	Delete(name, explicitKubeconfigPath string) error
	List() ([]string, error)
	KubeConfig(name string, internal bool) (string, error)
	ListInternalNodes(name string) ([]nodes.Node, error)
	// ListNodes also includes the external load balancer
	ListNodes(name string) ([]nodes.Node, error)
}

var _ kindProvider = (*cluster.Provider)(nil)

// hostEnv is the host's filesystem, environment and clock
type hostEnv struct {
	stat     func(path string) (fs.FileInfo, error)
	readFile func(path string) ([]byte, error)
	hostname func() (string, error)
	lookPath func(file string) (string, error)
	homeDir  func() (string, error)
	sleep    func(time.Duration)
	// kubeconfigPath is the user's kubeconfig (~/.kube/config), which
	// UpdateKubeconfigFile writes; loadKubeconfig reads what it starts from
	kubeconfigPath string
	loadKubeconfig func() (*clientcmdapi.Config, error)
}

func realHost() hostEnv {
	return hostEnv{
		stat:           os.Stat,
		readFile:       os.ReadFile,
		hostname:       os.Hostname,
		lookPath:       osexec.LookPath,
		homeDir:        os.UserHomeDir,
		sleep:          time.Sleep,
		kubeconfigPath: clientcmd.RecommendedHomeFile,
		loadKubeconfig: func() (*clientcmdapi.Config, error) {
			// The whole $KUBECONFIG chain, as kubectl sees it
			return clientcmd.NewDefaultPathOptions().GetStartingConfig()
		},
	}
}

// fileExists reports whether path exists on the host
func (h hostEnv) fileExists(path string) bool {
	_, err := h.stat(path)
	return err == nil
}

// clusterClients creates the Kubernetes clients KindManager uses
type clusterClients struct {
	clientset func(*rest.Config) (kubernetes.Interface, error)
	dynamic   func(*rest.Config) (dynamic.Interface, error)
}

func realClusterClients() clusterClients {
	return clusterClients{
		clientset: func(c *rest.Config) (kubernetes.Interface, error) { return kubernetes.NewForConfig(c) },
		dynamic:   func(c *rest.Config) (dynamic.Interface, error) { return dynamic.NewForConfig(c) },
	}
}

// imageLoader loads an image archive onto a node: layer-aware (skipping
// layers already in the node's content store) where the archive allows it,
// otherwise in full. KindManager.loadArchive decides which
type imageLoader struct {
	inspect    func(archive string) (*layeraware.ImageMetadata, error)
	layerAware func(archive string, metadata *layeraware.ImageMetadata, node nodes.Node) error
	full       func(archive string, node nodes.Node) error
}

func realImageLoader() imageLoader {
	return imageLoader{
		inspect: layeraware.InspectArchive,
		layerAware: func(archive string, metadata *layeraware.ImageMetadata, node nodes.Node) error {
			return images.LoadImageLayerAware(archive, metadata, node, nil)
		},
		full: func(archive string, node nodes.Node) error {
			f, err := os.Open(archive)
			if err != nil {
				return err
			}
			defer func() { _ = f.Close() }()
			return nodeutils.LoadImageArchive(node, f)
		},
	}
}
