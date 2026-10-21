package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hjames9/kraze/internal/cluster"
	"github.com/hjames9/kraze/internal/config"
	"github.com/hjames9/kraze/internal/providers"
	"github.com/hjames9/kraze/internal/state"
	"github.com/spf13/cobra"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
)

// The cli tests replace the package-level deps and flag variables, so they
// must not use t.Parallel.

// fakeEnv stands in for everything deps reaches: kind, Docker, the cluster
// (one fake clientset, so state and history persist across calls) and the
// providers. Every fake logs to events, so tests can check ordering across
// them.
type fakeEnv struct {
	mu     sync.Mutex
	events []string

	cluster   *fakeCluster
	images    *fakeImages
	clientset *fake.Clientset
	orphans   *fakeOrphanBackend

	clientsetErr error
	skipTLS      []bool // per clientset call

	providers    map[string]*fakeProvider
	providerOpts map[string]*providers.ProviderOptions
	providerErr  map[string]error

	dockerErr    error
	dockerChecks int
	sleeps       []time.Duration

	deletedNamespaces  []string
	waitedNamespaces   []string
	deleteNamespaceErr error
	waitNamespaceErr   error

	pods           map[string][]string // per service
	podsErr        error
	portForwardErr error
	forwarded      []string // "namespace/pod ports"
}

// withFakeDeps replaces deps with fakes for the rest of the test, and resets
// the command flags to their defaults
func withFakeDeps(t *testing.T) *fakeEnv {
	t.Helper()
	resetFlags(t)

	env := &fakeEnv{
		clientset:    fake.NewSimpleClientset(),
		orphans:      newOrphanBackend(),
		providers:    map[string]*fakeProvider{},
		providerOpts: map[string]*providers.ProviderOptions{},
		providerErr:  map[string]error{},
		pods:         map[string][]string{},
	}
	env.cluster = &fakeCluster{env: env, kubeconfig: "kind-kubeconfig", externalKubeconfig: "external-kubeconfig", errs: map[string]error{}}
	env.images = &fakeImages{images: map[string][]string{}, info: map[string]*cluster.ImageInfo{}, clusterHash: map[string]string{},
		infoErr: map[string]error{}, hashErr: map[string]error{}}

	orig := deps
	t.Cleanup(func() { deps = orig })
	deps = cliDeps{
		newClusterManager: func() clusterManager { return env.cluster },
		newImageManager:   func(bool) imageManager { return env.images },
		clientset: func(_ string, skipTLSVerify bool) (kubernetes.Interface, error) {
			env.mu.Lock()
			defer env.mu.Unlock()
			env.skipTLS = append(env.skipTLS, skipTLSVerify)
			if env.clientsetErr != nil {
				return nil, env.clientsetErr
			}
			return env.clientset, nil
		},
		newProvider: func(svc *config.ServiceConfig, opts *providers.ProviderOptions) (providers.Provider, error) {
			env.mu.Lock()
			defer env.mu.Unlock()
			env.providerOpts[svc.Name] = opts
			if err := env.providerErr[svc.Name]; err != nil {
				return nil, err
			}
			return env.providerLocked(svc.Name), nil
		},
		orphanBackend: func(*providers.ProviderOptions) orphanBackend {
			return eventOrphanBackend{env: env, inner: env.orphans}
		},
		checkDocker: func(context.Context) error {
			env.mu.Lock()
			defer env.mu.Unlock()
			env.dockerChecks++
			return env.dockerErr
		},
		sleep: func(d time.Duration) {
			env.mu.Lock()
			defer env.mu.Unlock()
			env.sleeps = append(env.sleeps, d)
		},
		deleteNamespace: func(_ context.Context, _, ns string) error {
			env.event("namespace.delete %s", ns)
			env.mu.Lock()
			defer env.mu.Unlock()
			env.deletedNamespaces = append(env.deletedNamespaces, ns)
			return env.deleteNamespaceErr
		},
		waitNamespaceDeleted: func(_ context.Context, _, ns string, _ time.Duration) error {
			env.mu.Lock()
			defer env.mu.Unlock()
			env.waitedNamespaces = append(env.waitedNamespaces, ns)
			return env.waitNamespaceErr
		},
		podsForService: func(_ context.Context, _ string, svc *config.ServiceConfig) ([]string, error) {
			env.mu.Lock()
			defer env.mu.Unlock()
			return env.pods[svc.Name], env.podsErr
		},
		portForward: func(ctx context.Context, _, ns, pod string, ports []string) error {
			env.mu.Lock()
			env.forwarded = append(env.forwarded, fmt.Sprintf("%s/%s %s", ns, pod, strings.Join(ports, ",")))
			err := env.portForwardErr
			env.mu.Unlock()
			if err != nil {
				return err
			}
			<-ctx.Done()
			return nil
		},
	}
	return env
}

func (e *fakeEnv) event(format string, args ...interface{}) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.events = append(e.events, fmt.Sprintf(format, args...))
}

// eventsWith returns the logged events that start with prefix, in order
func (e *fakeEnv) eventsWith(prefix string) []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []string
	for _, ev := range e.events {
		if strings.HasPrefix(ev, prefix) {
			out = append(out, ev)
		}
	}
	return out
}

// provider returns the fake provider for a service, creating it
func (e *fakeEnv) provider(name string) *fakeProvider {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.providerLocked(name)
}

func (e *fakeEnv) providerLocked(name string) *fakeProvider {
	p, ok := e.providers[name]
	if !ok {
		p = &fakeProvider{env: e, name: name}
		e.providers[name] = p
	}
	return p
}

// options returns the ProviderOptions a service's provider was created with
func (e *fakeEnv) options(name string) *providers.ProviderOptions {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.providerOpts[name]
}

// state loads the saved cluster state as project
func (e *fakeEnv) state(t *testing.T, project string) *state.ClusterState {
	t.Helper()
	st, err := state.Load(context.Background(), e.clientset, "c")
	if err != nil {
		t.Fatalf("loading state: %v", err)
	}
	if st == nil {
		t.Fatal("no state was saved")
	}
	st.SetProject(project)
	return st
}

// saveState stores st in the fake cluster
func (e *fakeEnv) saveState(t *testing.T, st *state.ClusterState) {
	t.Helper()
	if err := st.Save(context.Background(), e.clientset); err != nil {
		t.Fatal(err)
	}
}

// fakeCluster is a kind cluster manager. errs is keyed by method name.
type fakeCluster struct {
	env *fakeEnv

	exists             bool
	clusters           []string // ListClusters; defaults to the existing cluster
	kubeconfig         string
	externalKubeconfig string
	errs               map[string]error

	kubeconfigUpdated bool
}

func (c *fakeCluster) err(method string) error {
	c.env.mu.Lock()
	defer c.env.mu.Unlock()
	return c.errs[method]
}

func (c *fakeCluster) ClusterExists(name string) (bool, error) {
	if err := c.err("ClusterExists"); err != nil {
		return false, err
	}
	c.env.mu.Lock()
	defer c.env.mu.Unlock()
	return c.exists, nil
}

func (c *fakeCluster) ListClusters() ([]string, error) {
	if err := c.err("ListClusters"); err != nil {
		return nil, err
	}
	c.env.mu.Lock()
	defer c.env.mu.Unlock()
	return c.clusters, nil
}

func (c *fakeCluster) CreateCluster(_ context.Context, cfg *config.ClusterConfig) error {
	c.env.event("cluster.create %s", cfg.Name)
	if err := c.err("CreateCluster"); err != nil {
		return err
	}
	c.env.mu.Lock()
	defer c.env.mu.Unlock()
	c.exists = true
	return nil
}

func (c *fakeCluster) DeleteCluster(name string) error {
	c.env.event("cluster.delete %s", name)
	if err := c.err("DeleteCluster"); err != nil {
		return err
	}
	c.env.mu.Lock()
	defer c.env.mu.Unlock()
	c.exists = false
	return nil
}

func (c *fakeCluster) GetKubeConfig(string, bool) (string, error) {
	if err := c.err("GetKubeConfig"); err != nil {
		return "", err
	}
	return c.kubeconfig, nil
}

func (c *fakeCluster) GetKubeconfigForExternalCluster(*config.ClusterConfig) (string, error) {
	if err := c.err("GetKubeconfigForExternalCluster"); err != nil {
		return "", err
	}
	return c.externalKubeconfig, nil
}

func (c *fakeCluster) VerifyClusterAccess(context.Context, string) error {
	c.env.event("cluster.verify")
	return c.err("VerifyClusterAccess")
}

func (c *fakeCluster) UpdateKubeconfigFile(string) error {
	c.env.mu.Lock()
	c.kubeconfigUpdated = true
	c.env.mu.Unlock()
	return c.err("UpdateKubeconfigFile")
}

func (c *fakeCluster) LoadImage(_ context.Context, _, image string) error {
	c.env.event("image.load %s", image)
	return c.err("LoadImage")
}

func (c *fakeCluster) UntagImage(_ context.Context, _, image string) error {
	c.env.event("image.untag %s", image)
	return c.err("UntagImage")
}

// fakeImages answers image lookups from maps
type fakeImages struct {
	mu          sync.Mutex
	images      map[string][]string // per service
	info        map[string]*cluster.ImageInfo
	clusterHash map[string]string
	listed      []cluster.ClusterImage
	imagesErr   error // GetImagesForService
	listErr     error // ListClusterImages
	infoErr     map[string]error
	hashErr     map[string]error
}

func (f *fakeImages) GetImagesForService(_ context.Context, svc *config.ServiceConfig, _ string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.images[svc.Name], f.imagesErr
}

func (f *fakeImages) GetImageInfo(_ context.Context, image string) (*cluster.ImageInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.infoErr[image]; err != nil {
		return nil, err
	}
	if info, ok := f.info[image]; ok {
		return info, nil
	}
	return &cluster.ImageInfo{Reference: cluster.ParseImageReference(image)}, nil
}

func (f *fakeImages) GetClusterImageHash(_ context.Context, _, image string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.clusterHash[image], f.hashErr[image]
}

func (f *fakeImages) ListClusterImages(context.Context, string) ([]cluster.ClusterImage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.listed, f.listErr
}

// fakeProvider installs a service by remembering it. The *Err fields make
// the matching call fail; installed is what IsInstalled reports.
type fakeProvider struct {
	env  *fakeEnv
	name string

	installed    bool
	installErr   error
	uninstallErr error
	installedErr error
	status       *providers.ServiceStatus
	statusErr    error
	refs         []state.ResourceRef
	refsErr      error

	installs   int
	uninstalls int
}

var (
	_ providers.Provider         = (*fakeProvider)(nil)
	_ providers.ResourceRecorder = (*fakeProvider)(nil)
)

func (p *fakeProvider) Install(context.Context, *config.ServiceConfig) error {
	p.env.event("install %s", p.name)
	p.env.mu.Lock()
	defer p.env.mu.Unlock()
	p.installs++
	if p.installErr != nil {
		return p.installErr
	}
	p.installed = true
	return nil
}

func (p *fakeProvider) Uninstall(context.Context, *config.ServiceConfig) error {
	p.env.event("uninstall %s", p.name)
	p.env.mu.Lock()
	defer p.env.mu.Unlock()
	p.uninstalls++
	if p.uninstallErr != nil {
		return p.uninstallErr
	}
	p.installed = false
	return nil
}

func (p *fakeProvider) Status(context.Context, *config.ServiceConfig) (*providers.ServiceStatus, error) {
	p.env.mu.Lock()
	defer p.env.mu.Unlock()
	if p.statusErr != nil {
		return nil, p.statusErr
	}
	if p.status != nil {
		return p.status, nil
	}
	return &providers.ServiceStatus{Name: p.name, Installed: p.installed, Ready: p.installed, Message: "deployed"}, nil
}

func (p *fakeProvider) IsInstalled(context.Context, *config.ServiceConfig) (bool, error) {
	p.env.mu.Lock()
	defer p.env.mu.Unlock()
	return p.installed, p.installedErr
}

func (p *fakeProvider) ResourceRefs(*config.ServiceConfig) ([]state.ResourceRef, error) {
	p.env.mu.Lock()
	defer p.env.mu.Unlock()
	return p.refs, p.refsErr
}

// eventOrphanBackend logs orphan removal to the env's events
type eventOrphanBackend struct {
	env   *fakeEnv
	inner *fakeOrphanBackend
}

func (b eventOrphanBackend) helmReleaseExists(ctx context.Context, svc *config.ServiceConfig) (bool, error) {
	return b.inner.helmReleaseExists(ctx, svc)
}

func (b eventOrphanBackend) uninstallHelm(ctx context.Context, svc *config.ServiceConfig) error {
	b.env.event("orphan.uninstall %s/%s", svc.Namespace, svc.Name)
	return b.inner.uninstallHelm(ctx, svc)
}

func (b eventOrphanBackend) deleteManifests(ctx context.Context, name string, refs []state.ResourceRef) (int, error) {
	b.env.event("orphan.delete %s", name)
	return b.inner.deleteManifests(ctx, name, refs)
}

// resetFlags sets the global and command flags to their defaults, and
// restores their values after the test
func resetFlags(t *testing.T) {
	t.Helper()
	setVar(t, &configFiles, nil)
	setVar(t, &projectFlag, "")
	setVar(t, &verbose, false)
	setVar(t, &dryRun, false)
	setVar(t, &plain, true)

	setVar(t, &upWait, true)
	setVar(t, &upTimeout, "10m")
	setVar(t, &upNoWait, false)
	setVar(t, &upNoDeps, false)
	setVar(t, &upLabels, nil)
	setVar(t, &upRemoveOrphans, false)

	setVar(t, &downKeepCRDs, false)
	setVar(t, &downLabels, nil)
	setVar(t, &downNamespaceDeletionTimeout, 30*time.Second)
	setVar(t, &downDisabledOnly, false)
	setVar(t, &downDeleteNamespaces, false)
	setVar(t, &downRemoveOrphans, false)

	setVar(t, &statusLabels, nil)
	setVar(t, &statusAllProjects, false)
	setVar(t, &planLabels, nil)
	setVar(t, &destroyForce, false)
	setVar(t, &portForwardPod, "")
	setVar(t, &historyLimit, 20)
	setVar(t, &historyAllProjects, false)
	setVar(t, &historyOutput, "table")
	setVar(t, &packOutput, "")
}

// setVar sets *p to v for the rest of the test
func setVar[T any](t *testing.T, p *T, v T) {
	t.Helper()
	orig := *p
	*p = v
	t.Cleanup(func() { *p = orig })
}

// useConfig writes a kraze.yml and points -f at it
func useConfig(t *testing.T, content string) string {
	t.Helper()
	path := writeConfig(t, filepath.Join(t.TempDir(), "kraze.yml"), content)
	configFiles = []string{path}
	return path
}

// runCmd runs a command's RunE as Execute would, with a context, capturing
// stdout
func runCmd(t *testing.T, run func(*cobra.Command, []string) error, args ...string) (string, error) {
	t.Helper()
	return runCmdCtx(t, context.Background(), run, args...)
}

func runCmdCtx(t *testing.T, ctx context.Context, run func(*cobra.Command, []string) error, args ...string) (string, error) {
	t.Helper()
	cmd := &cobra.Command{}
	cmd.SetContext(ctx)
	var err error
	out := captureStdout(t, func() { err = run(cmd, args) })
	return out, err
}

// kindConfig is a kind-cluster project with a diamond of dependencies:
// a, then b and c, then d
const kindConfig = `project: web
cluster:
  name: c
services:
  a:
    type: helm
    chart: a
    repo: oci://registry.example/charts
    namespace: shop
    post_ready_delay: 0s
  b:
    type: helm
    chart: b
    repo: oci://registry.example/charts
    namespace: shop
    post_ready_delay: 0s
    depends_on: [a]
  c:
    type: manifests
    path: ./c.yaml
    namespace: jobs
    post_ready_delay: 0s
    depends_on: [a]
  d:
    type: helm
    chart: d
    repo: oci://registry.example/charts
    namespace: shop
    post_ready_delay: 0s
    depends_on: [b, c]
`

// externalConfig is the same project on an external cluster
var externalConfig = strings.Replace(kindConfig, "  name: c\n", "  name: c\n  external:\n    enabled: true\n    kubeconfig: /dev/null\n", 1)

// TestFakeDepsRestore checks withFakeDeps puts the real deps back
func TestFakeDepsRestore(t *testing.T) {
	var inner *fakeEnv
	t.Run("fake", func(t *testing.T) {
		inner = withFakeDeps(t)
		if _, ok := deps.newClusterManager().(*fakeCluster); !ok {
			t.Fatal("deps should be faked")
		}
	})
	if inner == nil {
		t.Fatal("subtest didn't run")
	}
	if _, ok := deps.newClusterManager().(*cluster.KindManager); !ok {
		t.Error("deps should be restored after the test")
	}
}

// assertEqual fails unless got equals want
func assertEqual[T comparable](t *testing.T, what string, got, want []T) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Errorf("%s = %v, want %v", what, got, want)
	}
}

// assertContains fails unless out contains every string in want
func assertContains(t *testing.T, out string, want ...string) {
	t.Helper()
	for _, w := range want {
		if !strings.Contains(out, w) {
			t.Errorf("output should contain %q, got:\n%s", w, out)
		}
	}
}

var errBoom = errors.New("boom")

// parseConfig parses a config from YAML
func parseConfig(t *testing.T, content string) *config.Config {
	t.Helper()
	path := writeConfig(t, filepath.Join(t.TempDir(), "kraze.yml"), content)
	cfg, err := config.ParseMultiple([]string{path})
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

// captureStderr runs fn while redirecting os.Stderr, returning what it wrote
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	orig := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stderr = w
	done := make(chan []byte)
	go func() {
		data, _ := io.ReadAll(r)
		done <- data
	}()
	fn()
	_ = w.Close()
	os.Stderr = orig
	return string(<-done)
}
