package cli

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/hjames9/kraze/internal/cluster"
	"github.com/hjames9/kraze/internal/history"
	"github.com/hjames9/kraze/internal/state"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"
)

// failVerb makes the fake cluster refuse verb on resource
func (e *fakeEnv) failVerb(verb, resource string) {
	e.clientset.PrependReactor(verb, resource, func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("forbidden")
	})
}

// What up can't do for a service is only a warning: the install goes on
func TestUpToleratesSideFailures(t *testing.T) {
	env := withFakeDeps(t)
	useConfig(t, kindConfig+"  off:\n    type: helm\n    chart: off\n    repo: oci://r/c\n    enabled: false\n")
	env.cluster.exists = true
	verbose = true

	env.images.images["a"] = []string{"noinfo:1", "nohash:1", "stale:1"}
	env.images.infoErr["noinfo:1"] = errBoom
	env.images.info["nohash:1"] = &cluster.ImageInfo{SHA256: "sha256:aaaaaaaaaaaaaaaa", InLocalDaemon: true}
	env.images.hashErr["nohash:1"] = errBoom
	env.images.info["stale:1"] = &cluster.ImageInfo{SHA256: "sha256:bbbbbbbbbbbbbbbb", InLocalDaemon: true}
	env.images.clusterHash["stale:1"] = "sha256:cccccccccccccccc"
	env.cluster.errs["UntagImage"] = errBoom
	env.cluster.errs["LoadImage"] = errBoom
	env.cluster.errs["UpdateKubeconfigFile"] = errBoom
	env.failVerb("list", "pods")

	out, err := runCmd(t, runUp)
	if err != nil {
		t.Fatalf("up should go on: %v\n%s", err, out)
	}
	assertContains(t, out,
		"Filtered out 1 disabled service(s)",
		"failed to get info for image 'noinfo:1'",
		"failed to get cluster image hash for 'nohash:1'",
		"failed to untag old image 'stale:1'",
		"failed to load image",
		"failed to list pods in 'shop'",
	)
	if len(installOrder(env)) != 4 {
		t.Errorf("installed %v", installOrder(env))
	}
}

func TestUpToleratesUnsavableState(t *testing.T) {
	env := withFakeDeps(t)
	useConfig(t, kindConfig)
	verbose = true
	env.images.images["a"] = []string{"app:1"}
	env.images.info["app:1"] = &cluster.ImageInfo{SHA256: "sha256:dddddddddddddddd", InLocalDaemon: true}
	env.images.imagesErr = nil

	// The cluster is new: creating the kubeconfig entry fails, then no state
	// or history can be written
	env.cluster.errs["UpdateKubeconfigFile"] = errBoom
	env.failVerb("create", "configmaps")
	env.failVerb("update", "configmaps")

	out, err := runCmd(t, runUp)
	if err != nil {
		t.Fatalf("up should go on: %v\n%s", err, out)
	}
	assertContains(t, out,
		"Warning: failed to update kubeconfig",
		"failed to store config paths in cluster state",
		"failed to save cluster state",
		"failed to record history",
	)
}

func TestUpImageDetectionFailure(t *testing.T) {
	env := withFakeDeps(t)
	useConfig(t, kindConfig)
	env.cluster.exists = true
	verbose = true
	env.images.imagesErr = errBoom

	out, err := runCmd(t, runUp)
	if err != nil {
		t.Fatal(err)
	}
	assertContains(t, out, "failed to extract images for 'a'")
}

func TestDownToleratesSideFailures(t *testing.T) {
	env := withFakeDeps(t)
	installAll(t, env, kindConfig)
	verbose = true
	env.providerErr["b"] = errBoom
	env.deleteNamespaceErr = errBoom

	out, err := runCmd(t, runDown)
	if err != nil {
		t.Fatal(err)
	}
	assertContains(t, out, "failed to create provider for 'b'", "failed to delete namespace")

	// A namespace that keeps terminating
	env = withFakeDeps(t)
	installAll(t, env, kindConfig)
	env.waitNamespaceErr = errBoom
	out, err = runCmd(t, runDown)
	if err != nil {
		t.Fatal(err)
	}
	assertContains(t, out, "still terminating after", "(2 still terminating)")
}

func TestDownKeepsAndReportsNamespaces(t *testing.T) {
	env := withFakeDeps(t)
	useConfig(t, kindConfig)
	env.cluster.exists = true
	verbose = true
	// Installed into namespaces kraze didn't create
	for _, ns := range []string{"shop", "jobs"} {
		if _, err := env.clientset.CoreV1().Namespaces().Create(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := runCmd(t, runUp); err != nil {
		t.Fatal(err)
	}

	out, err := runCmd(t, runDown)
	if err != nil {
		t.Fatal(err)
	}
	assertContains(t, out, "Kept namespace 'jobs' (not created by kraze; --delete-namespaces deletes it)")
	if len(env.deletedNamespaces) != 0 {
		t.Errorf("deleted %v", env.deletedNamespaces)
	}

	// Namespaces that can't be read are warned about and kept
	env = withFakeDeps(t)
	installAll(t, env, kindConfig)
	env.failVerb("get", "namespaces")
	out, err = runCmd(t, runDown)
	if err != nil {
		t.Fatal(err)
	}
	assertContains(t, out, "Warning: failed to get namespace")
	if len(env.deletedNamespaces) != 0 {
		t.Errorf("deleted %v", env.deletedNamespaces)
	}
}

func TestDownToleratesUnsavableState(t *testing.T) {
	env := withFakeDeps(t)
	installAll(t, env, kindConfig)
	verbose = true
	st := env.state(t, "web")
	st.MarkServiceInstalledWithNamespace("a", "elsewhere", true) // moved
	env.saveState(t, st)
	env.failVerb("update", "configmaps")

	out, err := runCmd(t, runDown)
	if err != nil {
		t.Fatal(err)
	}
	assertContains(t, out, "failed to save cluster state")
}

func TestStatusAndPlanTolerateUnreadableCluster(t *testing.T) {
	env := withFakeDeps(t)
	installAll(t, env, kindConfig)
	verbose = true
	env.failVerb("get", "configmaps")
	env.failVerb("list", "namespaces")

	out, err := runCmd(t, runStatus)
	if err != nil {
		t.Fatal(err)
	}
	assertContains(t, out, "failed to load cluster state")

	out, err = runCmd(t, runPlan)
	if err != nil {
		t.Fatal(err)
	}
	assertContains(t, out, "failed to list namespaces", "failed to load cluster state")
}

func TestPlanNothingToDo(t *testing.T) {
	withFakeDeps(t)
	useConfig(t, "project: web\ncluster:\n  name: c\n")
	out, err := runCmd(t, runPlan)
	if err != nil {
		t.Fatal(err)
	}
	assertContains(t, out, "No changes")
}

func TestLoadImageVerbose(t *testing.T) {
	env := withFakeDeps(t)
	useConfig(t, kindConfig)
	env.cluster.exists = true
	env.clientsetErr = errBoom
	verbose = true

	out, err := runCmd(t, runLoadImage, "app:1")
	if err != nil {
		t.Fatal(err)
	}
	assertContains(t, out, "Loading 1 image(s) into cluster 'c'", "history will not be recorded")
}

func TestInitDryRunDetails(t *testing.T) {
	withFakeDeps(t)
	dryRun = true
	for cfg, want := range map[string]string{
		"project: web\ncluster:\n  name: c\n  version: 1.37.0\n":                           "Node image: kindest/node:v1.37.0",
		"project: web\ncluster:\n  name: c\n  node_image: kindest/node:v1.36.0@sha256:a\n": "Node image: kindest/node:v1.36.0@sha256:a",
	} {
		useConfig(t, cfg)
		out, err := runCmd(t, runInit)
		if err != nil || !strings.Contains(out, want) {
			t.Errorf("err = %v, output:\n%s", err, out)
		}
	}
	useConfig(t, externalConfig)
	if out, _ := runCmd(t, runDestroy); !strings.Contains(out, "Would delete state for external cluster 'c'") {
		t.Errorf("destroy dry run on an external cluster:\n%s", out)
	}
}

func TestVersionCommandOutput(t *testing.T) {
	resetFlags(t)
	setVar(t, &version, "1.2.3")
	setVar(t, &gitCommit, "abc123")
	setVar(t, &buildDate, "today")
	out := captureStdout(t, func() { _ = versionCmd.RunE(versionCmd, nil) })
	assertContains(t, out, "kraze version: 1.2.3", "abc123", "today")
}

// Discovery reads each kind cluster's state, optionally only one cluster's
func TestDiscoverClusterStates(t *testing.T) {
	env := withFakeDeps(t)
	env.cluster.clusters = []string{"c", "other"}
	st := state.New("c", false, false, 0, false, 0)
	st.SetProject("web")
	st.SetConfigPaths([]string{"/p/kraze.yml"})
	env.saveState(t, st)

	found := discoverClusterStates(context.Background(), "")
	if len(found) != 2 || found[0].name != "c" || found[0].state == nil {
		t.Fatalf("found %+v", found)
	}
	if only := discoverClusterStates(context.Background(), "other"); len(only) != 1 || only[0].name != "other" {
		t.Errorf("only other: %+v", only)
	}

	env.cluster.errs["ListClusters"] = errBoom
	if found := discoverClusterStates(context.Background(), ""); found != nil {
		t.Errorf("no clusters can be listed: %+v", found)
	}
	env.cluster.errs = map[string]error{"GetKubeConfig": errBoom}
	if found := discoverClusterStates(context.Background(), ""); len(found) != 2 || found[0].state != nil {
		t.Errorf("unreadable clusters are found without state: %+v", found)
	}
}

func TestFormatHistoryShowsRunErrors(t *testing.T) {
	out := formatHistory([]history.Entry{{Command: "up", Result: history.ResultFailed, Error: "cluster unreachable"}}, true, false)
	assertContains(t, out, "error: cluster unreachable")
}

func TestClaimServicesVerbose(t *testing.T) {
	resetFlags(t)
	verbose = true
	st := state.New("c", false, false, 0, false, 0)
	st.SetProject("")
	st.MarkServiceInstalledWithNamespace("a", "shop", true)
	st.SetProject("web")
	cfg := parseConfig(t, kindConfig)
	out := captureStdout(t, func() { claimServices(st, cfg.Services) })
	assertContains(t, out, "Claimed unowned service 'a' for project 'web'")
}

func TestStoredConfigSetFallbacks(t *testing.T) {
	resetFlags(t)
	dir := t.TempDir()
	kind := writeConfig(t, dir+"/kraze.yml", "project: web\ncluster:\n  name: c\n")
	external := writeConfig(t, dir+"/ext.yml", "project: web\ncluster:\n  name: c\n  external:\n    enabled: true\n")

	if got := storedConfigSetFor(context.Background(), external); got != nil {
		t.Errorf("an external cluster has no stored set, got %v", got)
	}
	// The cluster has no state
	withClusters(t, discoveredCluster{name: "c"})
	if got := storedConfigSetFor(context.Background(), kind); got != nil {
		t.Errorf("no state, got %v", got)
	}
	// The stored set names a file that's gone
	withClusters(t, clusterWithProjects("c", map[string][]string{"web": {kind, dir + "/gone.yml"}}))
	if got := storedConfigSetFor(context.Background(), kind); got != nil {
		t.Errorf("a set with a missing file, got %v", got)
	}
}

// Execute runs a command with the interrupt-aware context
func TestExecute(t *testing.T) {
	resetFlags(t)
	rootCmd.SetArgs([]string{"version"})
	t.Cleanup(func() { rootCmd.SetArgs(nil) })
	var err error
	out := captureStdout(t, func() { err = Execute() })
	if err != nil || !strings.Contains(out, "kraze version:") {
		t.Errorf("err = %v, output:\n%s", err, out)
	}
}

func TestUpCancelledBeforeLoadingImages(t *testing.T) {
	env := withFakeDeps(t)
	useConfig(t, kindConfig)
	env.cluster.exists = true
	env.images.images["a"] = []string{"app:1"}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := runCmdCtx(t, ctx, runUp); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v", err)
	}
	if len(env.eventsWith("image.")) != 0 {
		t.Error("no images should be loaded after cancellation")
	}
}

// On an external cluster an image whose hash matches state is reported as unchanged
func TestUpExternalImageUnchanged(t *testing.T) {
	env := withFakeDeps(t)
	useConfig(t, externalConfig)
	verbose = true
	env.images.images["a"] = []string{"app:1"}
	env.images.info["app:1"] = &cluster.ImageInfo{SHA256: "sha256:eeeeeeeeeeeeeeee", InLocalDaemon: true}
	if _, err := runCmd(t, runUp); err != nil {
		t.Fatal(err)
	}
	out, err := runCmd(t, runUp)
	if err != nil {
		t.Fatal(err)
	}
	assertContains(t, out, "Image 'app:1' unchanged (hash matches state), skipping")
}
