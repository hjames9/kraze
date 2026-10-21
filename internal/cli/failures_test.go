package cli

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hjames9/kraze/internal/state"
	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"
)

// cycleConfig has services that depend on each other
const cycleConfig = `project: web
cluster:
  name: c
services:
  a:
    type: helm
    chart: a
    repo: oci://r/c
    depends_on: [b]
  b:
    type: helm
    chart: b
    repo: oci://r/c
    depends_on: [a]
    enabled: true
`

// legacyOrphan saves a state with an orphan of the given type in "old"
func legacyOrphan(t *testing.T, env *fakeEnv, serviceType string) {
	t.Helper()
	st := state.New("c", false, false, 0, false, 0)
	st.SetProject("web")
	st.MarkServiceInstalledWithNamespace("gone", "old", true)
	if serviceType != "" {
		st.SetServiceSource("gone", serviceType, nil)
	}
	env.saveState(t, st)
}

// Each case sets up one failure and names what the command must report
func TestCommandFailures(t *testing.T) {
	tests := []struct {
		name   string
		run    func(*cobra.Command, []string) error
		args   []string
		config string
		setup  func(t *testing.T, env *fakeEnv)
		want   string
	}{
		{name: "up: dependency cycle", run: runUp, want: "circular dependency"},
		{name: "down: dependency cycle", run: runDown, want: "failed to resolve dependencies"},
		{name: "down --disabled: dependency cycle", run: runDown, setup: func(t *testing.T, env *fakeEnv) { downDisabledOnly = true }, want: "failed to resolve dependencies"},
		{name: "plan: dependency cycle", run: runPlan, want: "dependency validation failed"},
		{name: "up: cluster can't be created", run: runUp, config: kindConfig,
			setup: func(t *testing.T, env *fakeEnv) {
				env.cluster.exists = false
				env.cluster.errs["CreateCluster"] = errBoom
			},
			want: "failed to create cluster"},
		{name: "up: state can't be read", run: runUp, config: kindConfig,
			setup: func(t *testing.T, env *fakeEnv) { env.failVerb("get", "configmaps") }, want: "failed to load cluster state"},
		{name: "up: provider can't be created", run: runUp, config: kindConfig,
			setup: func(t *testing.T, env *fakeEnv) { env.providerErr["a"] = errBoom }, want: "failed to create provider for 'a'"},
		{name: "up: namespace can't be created", run: runUp, config: kindConfig,
			setup: func(t *testing.T, env *fakeEnv) { env.failVerb("create", "namespaces") }, want: "failed to create namespace 'shop'"},
		{name: "up --remove-orphans: orphan can't be removed", run: runUp, config: kindConfig,
			setup: func(t *testing.T, env *fakeEnv) {
				upRemoveOrphans = true
				legacyOrphan(t, env, "helm")
				env.orphans.failUninstall["gone"] = true
			}, want: "could not remove old installation(s) of: gone"},
		{name: "down --remove-orphans: orphan can't be removed", run: runDown, config: kindConfig,
			setup: func(t *testing.T, env *fakeEnv) {
				downRemoveOrphans = true
				legacyOrphan(t, env, "helm")
				env.orphans.failUninstall["gone"] = true
			}, want: "could not remove old installation(s) of: gone"},
		{name: "down --remove-orphans: legacy orphan lookup fails", run: runDown, config: kindConfig,
			setup: func(t *testing.T, env *fakeEnv) {
				downRemoveOrphans = true
				legacyOrphan(t, env, "")
				env.orphans.existsErr = errBoom
			}, want: "could not remove old installation(s) of: gone"},
		{name: "init: external cluster unreachable", run: runInit, config: externalConfig,
			setup: func(t *testing.T, env *fakeEnv) { env.cluster.errs["VerifyClusterAccess"] = errBoom }, want: "failed to access external cluster 'c'"},
		{name: "init: kubeconfig after creating", run: runInit, config: kindConfig,
			setup: func(t *testing.T, env *fakeEnv) { env.cluster.errs["GetKubeConfig"] = errBoom }, want: "failed to get kubeconfig"},
		{name: "init: state can't be read", run: runInit, config: kindConfig,
			setup: func(t *testing.T, env *fakeEnv) { env.failVerb("get", "configmaps") }, want: "failed to load cluster state"},
		{name: "init: state can't be saved", run: runInit, config: kindConfig,
			setup: func(t *testing.T, env *fakeEnv) { env.failVerb("create", "configmaps") }, want: "failed to save cluster state"},
		{name: "list-images: cluster check", run: runListImages, config: kindConfig,
			setup: func(t *testing.T, env *fakeEnv) { env.cluster.errs["ClusterExists"] = errBoom }, want: "failed to check cluster"},
		{name: "list-images: images can't be listed", run: runListImages, config: kindConfig,
			setup: func(t *testing.T, env *fakeEnv) { env.cluster.exists = true; env.images.listErr = errBoom }, want: "failed to list images"},
		{name: "history: history can't be read", run: runHistory, config: kindConfig,
			setup: func(t *testing.T, env *fakeEnv) { env.cluster.exists = true; env.failVerb("get", "configmaps") }, want: "failed to read history"},
		{name: "status --all-projects: no client", run: runStatus, config: kindConfig,
			setup: func(t *testing.T, env *fakeEnv) {
				env.cluster.exists = true
				statusAllProjects = true
				env.clientsetErr = errBoom
			}, want: "failed to create Kubernetes client"},
		{name: "status --all-projects: state can't be read", run: runStatus, config: kindConfig,
			setup: func(t *testing.T, env *fakeEnv) {
				env.cluster.exists = true
				statusAllProjects = true
				env.failVerb("get", "configmaps")
			}, want: "failed to load cluster state"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := withFakeDeps(t)
			env.cluster.exists = true
			cfg := tt.config
			if cfg == "" {
				cfg = cycleConfig
			}
			useConfig(t, cfg)
			if tt.setup != nil {
				tt.setup(t, env)
			}
			_, err := runCmd(t, tt.run, tt.args...)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("err = %v, want %q", err, tt.want)
			}
		})
	}
}

// Failures up survives with a warning
func TestUpMoreWarnings(t *testing.T) {
	env := withFakeDeps(t)
	useConfig(t, strings.Replace(kindConfig, "    post_ready_delay: 0s\n  b:", "    post_ready_delay: soon\n  b:", 1))
	env.cluster.exists = true
	verbose = true
	env.provider("c").refsErr = errBoom
	env.failVerb("get", "namespaces")
	// A pod stuck pulling one of a's images, which can't be deleted
	env.images.images["a"] = []string{"app:1"}
	if _, err := env.clientset.CoreV1().Pods("shop").Create(context.Background(), &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "a-1", Namespace: "shop"},
		Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
			Image: "app:1", State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff"}},
		}}},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	env.failVerb("delete", "pods")

	out, err := runCmd(t, runUp)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	assertContains(t, out,
		"using default 3s delay",
		"failed to record resources of 'c'",
		"Warning: failed to get namespace",
		"failed to delete pod 'a-1'",
	)
}

func TestOrphanRemovalEdges(t *testing.T) {
	// Cancelled before the next orphan
	env := withFakeDeps(t)
	useConfig(t, kindConfig)
	env.cluster.exists = true
	legacyOrphan(t, env, "helm")
	st := env.state(t, "web")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := removeOrphans(ctx, st, env.clientset, "", st.Orphans(nil), nil, env.orphans, false, nil, func(string, ...interface{}) {})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v", err)
	}

	// The state can't be saved after a removal: a warning
	var warnings []string
	env.failVerb("update", "configmaps")
	captureStdout(t, func() {
		err = removeOrphans(context.Background(), st, env.clientset, "", st.Orphans(nil), nil, env.orphans, false, nil,
			func(format string, args ...interface{}) { warnings = append(warnings, format) })
	})
	if err != nil || len(warnings) == 0 || !strings.Contains(strings.Join(warnings, "\n"), "failed to save cluster state") {
		t.Errorf("err = %v, warnings %v", err, warnings)
	}

	// A legacy orphan in down that isn't a Helm release is forgotten
	env = withFakeDeps(t)
	useConfig(t, kindConfig)
	env.cluster.exists = true
	moved := state.New("c", false, false, 0, false, 0)
	moved.SetProject("web")
	moved.MarkServiceInstalledWithNamespace("a", "elsewhere", true) // no type: an older kraze's
	env.saveState(t, moved)
	out, err := runCmd(t, runDown)
	if err != nil {
		t.Fatal(err)
	}
	assertContains(t, out, "Forgotten; its objects are unknown")
}

// down --delete-namespaces waits for the workloads of an unlabeled namespace
func TestDownDeleteNamespacesWorkloadFailures(t *testing.T) {
	env := withFakeDeps(t)
	useConfig(t, kindConfig)
	env.cluster.exists = true
	for _, ns := range []string{"shop", "jobs"} {
		if _, err := env.clientset.CoreV1().Namespaces().Create(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := runCmd(t, runUp); err != nil {
		t.Fatal(err)
	}
	downDeleteNamespaces = true
	env.failVerb("list", "pods")
	out, err := runCmd(t, runDown)
	if err != nil {
		t.Fatal(err)
	}
	assertContains(t, out, "Warning:")
	if len(env.deletedNamespaces) != 0 {
		t.Errorf("namespaces whose workloads can't be listed must be kept, deleted %v", env.deletedNamespaces)
	}

	// Cancelled while waiting for workloads to go
	setVar(t, &unmanagedNamespaceSettle, time.Minute)
	env.clientset.PrependReactor("list", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, &corev1.PodList{Items: []corev1.Pod{{ObjectMeta: metav1.ObjectMeta{Name: "p"}}}}, nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := settledWorkloads(ctx, env.clientset, "shop", func(string, ...interface{}) {}); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v", err)
	}
}

func TestResolveConfigNoValidStoredPath(t *testing.T) {
	inDir(t, t.TempDir())
	withClusters(t,
		discoveredCluster{name: "plain"}, // a kind cluster kraze never used
		clusterWithProjects("shared", map[string][]string{"web": {filepath.Join(t.TempDir(), "gone.yml")}}),
	)
	var err error
	out := captureStdout(t, func() { _, err = resolveConfigFiles(&cobra.Command{}) })
	if err == nil || !strings.Contains(err.Error(), "no valid config path found") {
		t.Errorf("err = %v", err)
	}
	assertContains(t, out, "does not exist on this machine")

	// The commands report it
	for _, run := range []func(*cobra.Command, []string) error{runPack, validateCmd.RunE} {
		if _, err := runCmd(t, run); err == nil || !strings.Contains(err.Error(), "no valid config path found") {
			t.Errorf("err = %v", err)
		}
	}
	if names, _ := getServiceNames(&cobra.Command{}, nil, ""); names != nil {
		t.Errorf("no config, no completions: %v", names)
	}
}

func TestPlanMissingNamespace(t *testing.T) {
	env := withFakeDeps(t)
	useConfig(t, "project: web\ncluster:\n  name: c\nservices:\n  a:\n    type: helm\n    chart: a\n    repo: oci://r/c\n    namespace: absent\n    create_namespace: false\n")
	env.cluster.exists = true
	out, err := runCmd(t, runPlan)
	if err != nil {
		t.Fatal(err)
	}
	assertContains(t, out, "Namespace: absent")
}

func TestRealImageManagerFactory(t *testing.T) {
	if realDeps().newImageManager(false) == nil {
		t.Error("the real image manager should be created")
	}
}

// A config that can't be found is reported by every command that loads one
func TestCommandsReportUnresolvableConfig(t *testing.T) {
	for _, c := range commandRuns {
		t.Run(c.name, func(t *testing.T) {
			withFakeDeps(t)
			inDir(t, t.TempDir())
			withClusters(t, clusterWithProjects("shared", map[string][]string{"web": {filepath.Join(t.TempDir(), "gone.yml")}}))
			if _, err := runCmd(t, c.run, c.args...); err == nil || !strings.Contains(err.Error(), "no valid config path found") {
				t.Errorf("err = %v", err)
			}
		})
	}
}
