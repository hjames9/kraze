package cluster

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hjames9/kraze/internal/config"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/dynamic"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"sigs.k8s.io/kind/pkg/cluster"
	"sigs.k8s.io/kind/pkg/cluster/images"
	"sigs.k8s.io/kind/pkg/cluster/nodes"
)

// errNotFaked is what fakeRunner returns for a command no rule matches, as
// a failing command would
var errNotFaked = errors.New("exit status 1")

// runCall is a command fakeRunner ran
type runCall struct {
	argv  string // name and arguments, space-separated
	stdin string
}

// runRule answers the commands whose argv starts with prefix
type runRule struct {
	prefix string
	out    string
	err    error
	// run, if set, answers instead of out and err
	run func(ctx context.Context) ([]byte, error)
}

// fakeRunner answers commands from rules, the first match winning, and
// records them
type fakeRunner struct {
	mu    sync.Mutex
	rules []runRule
	calls []runCall
}

func (f *fakeRunner) on(prefix, out string, err error) *fakeRunner {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rules = append(f.rules, runRule{prefix: prefix, out: out, err: err})
	return f
}

func (f *fakeRunner) onRun(prefix string, run func(ctx context.Context) ([]byte, error)) *fakeRunner {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rules = append(f.rules, runRule{prefix: prefix, run: run})
	return f
}

func (f *fakeRunner) answer(ctx context.Context, stdin, name string, args []string) ([]byte, error) {
	argv := strings.Join(append([]string{name}, args...), " ")
	f.mu.Lock()
	f.calls = append(f.calls, runCall{argv: argv, stdin: stdin})
	var rule *runRule
	for i := range f.rules {
		if strings.HasPrefix(argv, f.rules[i].prefix) {
			rule = &f.rules[i]
			break
		}
	}
	f.mu.Unlock()
	if rule == nil {
		return nil, errNotFaked
	}
	if rule.run != nil {
		return rule.run(ctx)
	}
	return []byte(rule.out), rule.err
}

func (f *fakeRunner) Output(ctx context.Context, name string, args ...string) ([]byte, error) {
	return f.answer(ctx, "", name, args)
}

func (f *fakeRunner) CombinedOutput(ctx context.Context, stdin, name string, args ...string) ([]byte, error) {
	return f.answer(ctx, stdin, name, args)
}

// ran returns the commands run, in order
func (f *fakeRunner) ran() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.calls))
	for i, c := range f.calls {
		out[i] = c.argv
	}
	return out
}

// ranWith returns the calls whose argv starts with prefix
func (f *fakeRunner) ranWith(prefix string) []runCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []runCall
	for _, c := range f.calls {
		if strings.HasPrefix(c.argv, prefix) {
			out = append(out, c)
		}
	}
	return out
}

// fakeNode is a kind node; kraze only uses its name
type fakeNode struct {
	nodes.Node
	name string
}

func (n fakeNode) String() string { return n.name }

// fakeProvider is a kind provider holding clusters by name
type fakeProvider struct {
	mu         sync.Mutex
	clusters   []string
	nodes      []string // node names of every cluster
	kubeconfig string
	createErr  error
	listErr    error
	deleteErr  error
	kubeErr    error // KubeConfig
	nodesErr   error // ListInternalNodes
	created    []string
	deleted    []string
}

var _ kindProvider = (*fakeProvider)(nil)

func (p *fakeProvider) Create(name string, _ ...cluster.CreateOption) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.created = append(p.created, name)
	if p.createErr != nil {
		return p.createErr
	}
	p.clusters = append(p.clusters, name)
	return nil
}

func (p *fakeProvider) Delete(name, _ string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.deleted = append(p.deleted, name)
	return p.deleteErr
}

func (p *fakeProvider) List() ([]string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.clusters, p.listErr
}

func (p *fakeProvider) KubeConfig(string, bool) (string, error) {
	return p.kubeconfig, p.kubeErr
}

func (p *fakeProvider) ListInternalNodes(string) ([]nodes.Node, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.nodesErr != nil {
		return nil, p.nodesErr
	}
	var out []nodes.Node
	for _, n := range p.nodes {
		out = append(out, fakeNode{name: n})
	}
	return out, nil
}

// testHost is a host with the given files, binaries on PATH and home
// directory, which records sleeps instead of sleeping
type testHost struct {
	mu     sync.Mutex
	files  map[string]string
	bins   map[string]string
	home   string
	sleeps []time.Duration
	// statErr fails stat (with something other than not-exist) for a path;
	// readErr fails readFile; homeErr fails homeDir
	statErr map[string]error
	readErr map[string]error
	homeErr error
	hostErr error
	// existing is the user's kubeconfig before UpdateKubeconfigFile (nil: none)
	existing *clientcmdapi.Config
}

type fakeFileInfo struct{ fs.FileInfo }

func (h *testHost) env(t *testing.T) hostEnv {
	return hostEnv{
		stat: func(path string) (fs.FileInfo, error) {
			if err := h.statErr[path]; err != nil {
				return nil, err
			}
			if _, ok := h.files[path]; ok {
				return fakeFileInfo{}, nil
			}
			return nil, fs.ErrNotExist
		},
		readFile: func(path string) ([]byte, error) {
			if err := h.readErr[path]; err != nil {
				return nil, err
			}
			if content, ok := h.files[path]; ok {
				return []byte(content), nil
			}
			return nil, fs.ErrNotExist
		},
		hostname: func() (string, error) { return "devbox", h.hostErr },
		lookPath: func(file string) (string, error) {
			if path, ok := h.bins[file]; ok {
				return path, nil
			}
			return "", fmt.Errorf("%s: not found", file)
		},
		homeDir: func() (string, error) { return h.home, h.homeErr },
		sleep: func(d time.Duration) {
			h.mu.Lock()
			defer h.mu.Unlock()
			h.sleeps = append(h.sleeps, d)
		},
		kubeconfigPath: filepath.Join(h.home, ".kube", "config"),
		loadKubeconfig: func() (*clientcmdapi.Config, error) {
			if h.existing == nil {
				return nil, os.ErrNotExist
			}
			return h.existing, nil
		},
	}
}

// testKind is a KindManager on fakes
type testKind struct {
	*KindManager
	run      *fakeRunner
	provider *fakeProvider
	host     *testHost
	dynamic  *dynamicfake.FakeDynamicClient
	loaded   []string // archives loadImages was given
	loadErr  error    // per-node load error
}

func newTestKind(t *testing.T) *testKind {
	t.Helper()
	tk := &testKind{
		run:      &fakeRunner{},
		provider: &fakeProvider{nodes: []string{"c-control-plane"}, kubeconfig: "server: https://c-control-plane:6443\n"},
		host:     &testHost{files: map[string]string{}, bins: map[string]string{}, home: t.TempDir()},
		dynamic:  dynamicfake.NewSimpleDynamicClient(runtime.NewScheme()),
	}
	tk.KindManager = &KindManager{
		provider: tk.provider,
		run:      tk.run,
		host:     tk.host.env(t),
		clients: clusterClients{
			clientset: func(*rest.Config) (kubernetes.Interface, error) { return fake.NewSimpleClientset(), nil },
			dynamic:   func(*rest.Config) (dynamic.Interface, error) { return tk.dynamic, nil },
		},
		loadImages: func(archive string, ns []nodes.Node) ([]*images.LoadResult, error) {
			tk.loaded = append(tk.loaded, archive)
			var results []*images.LoadResult
			for _, n := range ns {
				results = append(results, &images.LoadResult{Node: n, Success: tk.loadErr == nil, Error: tk.loadErr})
			}
			return results, nil
		},
	}
	return tk
}

func newTestImages() (*ImageManager, *fakeRunner) {
	run := &fakeRunner{}
	return &ImageManager{run: run, pullChart: func(*config.ServiceConfig, string) (string, error) {
		return "", errors.New("offline")
	}}, run
}
