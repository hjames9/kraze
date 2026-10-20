package cli

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hjames9/kraze/internal/cluster"
	"github.com/hjames9/kraze/internal/history"
	"github.com/hjames9/kraze/internal/providers"
	"github.com/hjames9/kraze/internal/state"
)

// --- status ---

func TestStatusClusterNotRunning(t *testing.T) {
	withFakeDeps(t)
	useConfig(t, kindConfig)

	out, err := runCmd(t, runStatus)
	if err != nil {
		t.Fatal(err)
	}
	assertContains(t, out, "Cluster 'c' is not running", "No services are currently deployed.")
}

// A cluster kraze hasn't recorded anything on has no state ConfigMap
func TestStatusWithoutState(t *testing.T) {
	env := withFakeDeps(t)
	useConfig(t, kindConfig)
	env.cluster.exists = true

	out, err := runCmd(t, runStatus)
	if err != nil {
		t.Fatal(err)
	}
	assertContains(t, out, "Summary: 0/4 services installed, 0 ready")
}

func TestStatusTable(t *testing.T) {
	env := withFakeDeps(t)
	installAll(t, env, kindConfig)
	refs := []state.ResourceRef{{APIVersion: "batch/v1", Kind: "Job", Name: "migrate", Completed: true}}
	st := env.state(t, "web")
	st.SetServiceSource("c", "manifests", refs)
	env.saveState(t, st)

	env.provider("b").status = &providers.ServiceStatus{Installed: true, Ready: false, Message: strings.Repeat("x", 50)}
	env.provider("d").statusErr = errBoom

	out, err := runCmd(t, runStatus)
	if err != nil {
		t.Fatal(err)
	}
	assertContains(t, out,
		"Cluster: c", "Project: web",
		strings.Repeat("x", 37)+"...", // long messages are truncated
		"Failed to get status: boom",
		"Summary: 3/4 services installed, 2 ready",
	)
	if strings.Contains(out, strings.Repeat("x", 38)) {
		t.Error("the message should be truncated to 40 characters")
	}
	// What up recorded reaches the provider (completed Jobs)
	if opts := env.options("c"); opts == nil || !slices.Equal(opts.Recorded, refs) {
		t.Errorf("recorded resources passed = %+v", opts)
	}
}

func TestStatusProviderErrorAndDisabled(t *testing.T) {
	env := withFakeDeps(t)
	useConfig(t, strings.Replace(kindConfig, "    depends_on: [b, c]\n", "    depends_on: [b, c]\n    enabled: false\n", 1))
	env.cluster.exists = true
	env.providerErr["a"] = errBoom

	out, err := runCmd(t, runStatus)
	if err != nil {
		t.Fatal(err)
	}
	assertContains(t, out, "Failed to create provider: boom", "DISABLED")
}

// Without a cluster client, status still reports what the providers see
func TestStatusWithoutClusterClient(t *testing.T) {
	env := withFakeDeps(t)
	installAll(t, env, kindConfig)
	env.clientsetErr = errBoom

	out, err := runCmd(t, runStatus)
	if err != nil {
		t.Fatal(err)
	}
	assertContains(t, out, "Summary: 4/4 services installed, 4 ready")
}

func TestStatusAllProjects(t *testing.T) {
	env := withFakeDeps(t)
	installAll(t, env, kindConfig)
	st := env.state(t, "billing")
	st.MarkServiceInstalledWithNamespace("ledger", "billing", true)
	env.saveState(t, st)
	statusAllProjects = true

	out, err := runCmd(t, runStatus)
	if err != nil {
		t.Fatal(err)
	}
	assertContains(t, out, "billing", "ledger", "web")
}

// An external cluster isn't a kind cluster: status must not look for one
func TestStatusExternalCluster(t *testing.T) {
	env := withFakeDeps(t)
	installAll(t, env, externalConfig)
	env.cluster.exists = false // no kind cluster of that name

	out, err := runCmd(t, runStatus)
	if err != nil {
		t.Fatal(err)
	}
	assertContains(t, out, "Summary: 4/4 services installed, 4 ready")
	assertEqual(t, "skipTLS per clientset", env.skipTLS, []bool{false})
	if opts := env.options("a"); opts.KubeConfig != "external-kubeconfig" {
		t.Errorf("provider got kubeconfig %q", opts.KubeConfig)
	}
	if env.dockerChecks != 0 {
		t.Error("an external cluster doesn't need Docker")
	}

	statusAllProjects = true
	env.skipTLS = nil
	if out, err := runCmd(t, runStatus); err != nil || !strings.Contains(out, "web") {
		t.Errorf("--all-projects: %v\n%s", err, out)
	}
	assertEqual(t, "--all-projects skipTLS", env.skipTLS, []bool{false})
}

// --- plan ---

func TestPlanNewCluster(t *testing.T) {
	env := withFakeDeps(t)
	useConfig(t, kindConfig)

	out, err := runCmd(t, runPlan)
	if err != nil {
		t.Fatal(err)
	}
	assertContains(t, out, "4 to add")
	if len(env.skipTLS) != 0 {
		t.Error("a missing cluster needs no client")
	}
}

func TestPlanExistingCluster(t *testing.T) {
	env := withFakeDeps(t)
	installAll(t, env, kindConfig)

	out, err := runCmd(t, runPlan)
	if err != nil {
		t.Fatal(err)
	}
	assertContains(t, out, "Plan: 4 no change")
	if len(env.events) != 0 {
		t.Errorf("plan must not change anything, got %v", env.events)
	}
}

func TestPlanListsOrphansAndMoves(t *testing.T) {
	env := withFakeDeps(t)
	installAll(t, env, kindConfig)
	st := env.state(t, "web")
	st.MarkServiceInstalledWithNamespace("old", "legacy", true)
	st.MarkServiceInstalledWithNamespace("a", "elsewhere", true)
	env.saveState(t, st)

	out, err := runCmd(t, runPlan)
	if err != nil {
		t.Fatal(err)
	}
	assertContains(t, out, "old", "elsewhere")
	// Plan claims and computes in memory only
	if saved := env.state(t, "web"); !saved.IsServiceInstalled("old") {
		t.Error("plan must not change the stored state")
	}
}

func TestPlanDegradesWithoutCluster(t *testing.T) {
	for name, setup := range map[string]func(env *fakeEnv){
		"cluster check fails": func(env *fakeEnv) { env.cluster.errs["ClusterExists"] = errBoom },
		"kubeconfig fails": func(env *fakeEnv) {
			env.cluster.exists = true
			env.cluster.errs["GetKubeConfig"] = errBoom
		},
		"client fails": func(env *fakeEnv) {
			env.cluster.exists = true
			env.clientsetErr = errBoom
		},
	} {
		t.Run(name, func(t *testing.T) {
			env := withFakeDeps(t)
			useConfig(t, kindConfig)
			setup(env)
			out, err := runCmd(t, runPlan)
			if err != nil {
				t.Fatalf("plan should treat the state as empty, got %v", err)
			}
			assertContains(t, out, "4 to add")
		})
	}
}

func TestPlanExternalClusterUnreachable(t *testing.T) {
	env := withFakeDeps(t)
	useConfig(t, externalConfig)
	env.cluster.errs["GetKubeconfigForExternalCluster"] = errBoom

	out, err := runCmd(t, runPlan)
	if err != nil {
		t.Fatal(err)
	}
	assertContains(t, out, "4 to add")
	if env.dockerChecks != 0 {
		t.Error("an external cluster doesn't need Docker")
	}
}

// --- init ---

const preloadConfig = `project: web
cluster:
  name: c
  preload_images:
    - good:1
    - bad:1
`

func TestInitKindCluster(t *testing.T) {
	env := withFakeDeps(t)
	path := useConfig(t, preloadConfig)
	env.cluster.errs["UpdateKubeconfigFile"] = errBoom
	loadErr := errors.New("no such image")
	failing := &failingLoadCluster{fakeCluster: env.cluster, fail: map[string]error{"bad:1": loadErr}}
	deps.newClusterManager = func() clusterManager { return failing }

	out, err := runCmd(t, runInit)
	if err != nil {
		t.Fatalf("init failed: %v\n%s", err, out)
	}
	assertContains(t, out,
		"Warning: failed to update kubeconfig", "kind export kubeconfig --name c",
		"Warning: failed to load image 'bad:1'", "Cluster initialized successfully")
	assertEqual(t, "cluster events", env.eventsWith("cluster."), []string{"cluster.create c"})
	assertEqual(t, "skipTLS per clientset", env.skipTLS, []bool{true})

	st := env.state(t, "web")
	if st.CreatedBy != "web" || st.ClusterConfigHash == "" {
		t.Errorf("init should record the cluster's creator and hash, got %q/%q", st.CreatedBy, st.ClusterConfigHash)
	}
	assertEqual(t, "config paths", st.GetConfigPaths(), []string{path})

	entry := env.lastHistory(t)
	// A failed preload doesn't fail init, but the run is recorded as failed
	if entry.Command != "init" || !entry.ClusterCreated || entry.Result != history.ResultFailed {
		t.Errorf("history entry = %+v", entry)
	}
	assertEqual(t, "history changes", changeActions(entry), []string{"bad:1:load-image:failed", "good:1:load-image:succeeded"})
}

func TestInitCreateFails(t *testing.T) {
	env := withFakeDeps(t)
	useConfig(t, preloadConfig)
	env.cluster.errs["CreateCluster"] = errBoom

	if _, err := runCmd(t, runInit); !errors.Is(err, errBoom) {
		t.Errorf("err = %v", err)
	}
	if len(env.skipTLS) != 0 {
		t.Error("nothing should be recorded for a cluster that wasn't created")
	}
}

func TestInitExternalKeepsOtherProjects(t *testing.T) {
	env := withFakeDeps(t)
	useConfig(t, externalConfig)
	other := state.New("c", true, false, 0, false, 0)
	other.SetProject("billing")
	other.MarkServiceInstalledWithNamespace("ledger", "billing", true)
	env.saveState(t, other)

	out, err := runCmd(t, runInit)
	if err != nil {
		t.Fatalf("init failed: %v\n%s", err, out)
	}
	assertContains(t, out, "External cluster is accessible", "External cluster initialized successfully")
	if env.dockerChecks != 0 || len(env.eventsWith("cluster.create")) != 0 {
		t.Error("an external cluster is neither created nor needs Docker")
	}
	assertEqual(t, "skipTLS per clientset", env.skipTLS, []bool{false})
	if !env.state(t, "billing").IsServiceInstalled("ledger") {
		t.Error("another project's state must be kept")
	}
	if env.state(t, "web").CreatedBy != "" {
		t.Error("an external cluster has no creator")
	}
}

func TestInitDryRun(t *testing.T) {
	for name, cfg := range map[string]string{"kind": preloadConfig, "external": externalConfig} {
		t.Run(name, func(t *testing.T) {
			env := withFakeDeps(t)
			useConfig(t, cfg)
			dryRun = true
			out, err := runCmd(t, runInit)
			if err != nil {
				t.Fatal(err)
			}
			assertContains(t, out, "[DRY RUN]")
			if len(env.events) != 0 || len(env.skipTLS) != 0 {
				t.Errorf("dry run reached the cluster: %v", env.events)
			}
		})
	}
}

// failingLoadCluster fails LoadImage for some images
type failingLoadCluster struct {
	*fakeCluster
	fail map[string]error
}

func (c *failingLoadCluster) LoadImage(ctx context.Context, clusterName, image string) error {
	if err := c.fakeCluster.LoadImage(ctx, clusterName, image); err != nil {
		return err
	}
	return c.fail[image]
}

// --- destroy ---

func TestDestroyKindCluster(t *testing.T) {
	env := withFakeDeps(t)
	installAll(t, env, kindConfig)

	out, err := runCmd(t, runDestroy)
	if err != nil {
		t.Fatal(err)
	}
	assertContains(t, out, "Cluster destroyed successfully")
	assertEqual(t, "cluster events", env.eventsWith("cluster."), []string{"cluster.delete c"})
}

func TestDestroyRefusesSharedCluster(t *testing.T) {
	env := withFakeDeps(t)
	installAll(t, env, kindConfig)
	st := env.state(t, "billing")
	st.MarkServiceInstalledWithNamespace("ledger", "billing", true)
	env.saveState(t, st)

	_, err := runCmd(t, runDestroy)
	if err == nil || !strings.Contains(err.Error(), "also used by project(s) billing") {
		t.Fatalf("err = %v", err)
	}
	if len(env.eventsWith("cluster.delete")) != 0 {
		t.Error("a shared cluster must not be deleted")
	}

	destroyForce = true
	if _, err := runCmd(t, runDestroy); err != nil {
		t.Fatal(err)
	}
	assertEqual(t, "cluster events", env.eventsWith("cluster."), []string{"cluster.delete c"})
}

// A state that can't be read doesn't block the destroy
func TestDestroyUnreadableState(t *testing.T) {
	for name, setup := range map[string]func(env *fakeEnv){
		"cluster gone":     func(env *fakeEnv) { env.cluster.exists = false },
		"no kubeconfig":    func(env *fakeEnv) { env.cluster.errs["GetKubeConfig"] = errBoom },
		"no client":        func(env *fakeEnv) { env.clientsetErr = errBoom },
		"cluster check":    func(env *fakeEnv) { env.cluster.errs["ClusterExists"] = errBoom },
		"state never made": func(env *fakeEnv) {},
	} {
		t.Run(name, func(t *testing.T) {
			env := withFakeDeps(t)
			useConfig(t, kindConfig)
			env.cluster.exists = true
			setup(env)
			if _, err := runCmd(t, runDestroy); err != nil {
				t.Fatal(err)
			}
			if len(env.eventsWith("cluster.delete")) != 1 {
				t.Error("the cluster should be deleted")
			}
		})
	}
}

func TestDestroyDeleteFails(t *testing.T) {
	env := withFakeDeps(t)
	useConfig(t, kindConfig)
	env.cluster.errs["DeleteCluster"] = errBoom

	if _, err := runCmd(t, runDestroy); !errors.Is(err, errBoom) {
		t.Errorf("err = %v", err)
	}
}

func TestDestroyExternalCluster(t *testing.T) {
	env := withFakeDeps(t)
	installAll(t, env, externalConfig)

	out, err := runCmd(t, runDestroy)
	if err != nil {
		t.Fatal(err)
	}
	assertContains(t, out, "preserving cluster, deleting state only", "State deleted successfully")
	assertEqual(t, "skipTLS per clientset", env.skipTLS, []bool{false})
	if len(env.eventsWith("cluster.delete")) != 0 || env.dockerChecks != 0 {
		t.Error("an external cluster must not be deleted")
	}
	if st, err := state.Load(context.Background(), env.clientset, "c"); err != nil || st != nil {
		t.Errorf("state should be deleted, got %v, %v", st, err)
	}
}

func TestDestroyDryRun(t *testing.T) {
	env := withFakeDeps(t)
	useConfig(t, kindConfig)
	dryRun = true

	out, err := runCmd(t, runDestroy)
	if err != nil {
		t.Fatal(err)
	}
	assertContains(t, out, "[DRY RUN] Would destroy kind cluster 'c'")
	if len(env.events) != 0 {
		t.Errorf("dry run changed something: %v", env.events)
	}
}

// --- load-image ---

func TestLoadImage(t *testing.T) {
	env := withFakeDeps(t)
	useConfig(t, kindConfig)
	env.cluster.exists = true

	out, err := runCmd(t, runLoadImage, "one:1", "two:2")
	if err != nil {
		t.Fatal(err)
	}
	assertContains(t, out, "Successfully loaded 2 image(s) into cluster 'c'")
	assertEqual(t, "image events", env.eventsWith("image."), []string{"image.load one:1", "image.load two:2"})
	entry := env.lastHistory(t)
	assertEqual(t, "history changes", changeActions(entry), []string{"one:1:load-image:succeeded", "two:2:load-image:succeeded"})
}

func TestLoadImageStopsAtFailure(t *testing.T) {
	env := withFakeDeps(t)
	useConfig(t, kindConfig)
	env.cluster.exists = true
	env.cluster.errs["LoadImage"] = errBoom

	if _, err := runCmd(t, runLoadImage, "one:1", "two:2"); !errors.Is(err, errBoom) {
		t.Fatalf("err = %v", err)
	}
	assertEqual(t, "image events", env.eventsWith("image."), []string{"image.load one:1"})
	if entry := env.lastHistory(t); entry.Result != history.ResultFailed {
		t.Errorf("history result = %s", entry.Result)
	}
}

func TestLoadImageErrors(t *testing.T) {
	for name, tc := range map[string]struct {
		setup func(env *fakeEnv)
		want  string
	}{
		"no docker":     {func(env *fakeEnv) { env.dockerErr = errBoom }, "boom"},
		"no cluster":    {func(env *fakeEnv) {}, "cluster 'c' does not exist"},
		"cluster check": {func(env *fakeEnv) { env.cluster.errs["ClusterExists"] = errBoom }, "failed to check cluster"},
	} {
		t.Run(name, func(t *testing.T) {
			env := withFakeDeps(t)
			useConfig(t, kindConfig)
			tc.setup(env)
			_, err := runCmd(t, runLoadImage, "one:1")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want %q", err, tc.want)
			}
		})
	}
}

// The client is only for history: without one, images still load
func TestLoadImageWithoutHistory(t *testing.T) {
	env := withFakeDeps(t)
	useConfig(t, kindConfig)
	env.cluster.exists = true
	env.cluster.errs["GetKubeConfig"] = errBoom

	if _, err := runCmd(t, runLoadImage, "one:1"); err != nil {
		t.Fatal(err)
	}
	assertEqual(t, "image events", env.eventsWith("image."), []string{"image.load one:1"})
	if len(env.history(t)) != 0 {
		t.Error("no history can be recorded without a client")
	}
}

func TestLoadImageDryRun(t *testing.T) {
	env := withFakeDeps(t)
	dryRun = true
	env.dockerErr = errBoom // a dry run doesn't need Docker

	out, err := runCmd(t, runLoadImage, "one:1")
	if err != nil {
		t.Fatal(err)
	}
	assertContains(t, out, "[DRY RUN] Would load 1 image(s)")
}

// --- port-forward ---

func TestPortForward(t *testing.T) {
	env := withFakeDeps(t)
	installAll(t, env, kindConfig)
	env.pods["a"] = []string{"a-1", "a-2"}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	out, err := runCmdCtx(t, ctx, runPortForward, "a", "8080:80", "9090")
	if err != nil {
		t.Fatal(err)
	}
	assertContains(t, out, "Multiple pods found, using 'a-1'", "localhost:8080 -> :80", "localhost:9090 -> :9090", "Stopping port-forward")
	assertEqual(t, "forwarded", env.forwarded, []string{"shop/a-1 8080:80,9090:9090"})
}

func TestPortForwardNamedPod(t *testing.T) {
	env := withFakeDeps(t)
	installAll(t, env, kindConfig)
	portForwardPod = "chosen"
	env.podsErr = errBoom // not consulted
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	if _, err := runCmdCtx(t, ctx, runPortForward, "a", "80"); err != nil {
		t.Fatal(err)
	}
	assertEqual(t, "forwarded", env.forwarded, []string{"shop/chosen 80:80"})
}

func TestPortForwardErrors(t *testing.T) {
	for name, tc := range map[string]struct {
		args  []string
		setup func(env *fakeEnv)
		want  string
	}{
		"bad port":         {[]string{"a", "x:80"}, nil, "invalid local port 'x'"},
		"unknown service":  {[]string{"nope", "80"}, nil, "service 'nope' not found"},
		"not running":      {[]string{"a", "80"}, func(env *fakeEnv) { env.cluster.exists = false }, "cluster 'c' is not running"},
		"not installed":    {[]string{"a", "80"}, func(env *fakeEnv) { env.provider("a").installed = false }, "service 'a' is not installed"},
		"no pods":          {[]string{"a", "80"}, func(env *fakeEnv) {}, "no pods found for service 'a'"},
		"pod lookup":       {[]string{"a", "80"}, func(env *fakeEnv) { env.podsErr = errBoom }, "failed to get pods for service"},
		"forward fails":    {[]string{"a", "80"}, func(env *fakeEnv) { env.pods["a"] = []string{"a-1"}; env.portForwardErr = errBoom }, "port-forward failed: boom"},
		"installed check":  {[]string{"a", "80"}, func(env *fakeEnv) { env.provider("a").installedErr = errBoom }, "failed to check if service is installed"},
		"provider factory": {[]string{"a", "80"}, func(env *fakeEnv) { env.providerErr["a"] = errBoom }, "failed to create provider"},
	} {
		t.Run(name, func(t *testing.T) {
			env := withFakeDeps(t)
			installAll(t, env, kindConfig)
			if tc.setup != nil {
				tc.setup(env)
			}
			_, err := runCmd(t, runPortForward, tc.args...)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestPortForwardExternalCluster(t *testing.T) {
	env := withFakeDeps(t)
	installAll(t, env, externalConfig)
	env.cluster.exists = false // no kind cluster of that name
	env.pods["a"] = []string{"a-1"}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	if _, err := runCmdCtx(t, ctx, runPortForward, "a", "80"); err != nil {
		t.Fatal(err)
	}
	assertEqual(t, "forwarded", env.forwarded, []string{"shop/a-1 80:80"})
}

// --- history ---

func TestHistoryCommand(t *testing.T) {
	env := withFakeDeps(t)
	installAll(t, env, kindConfig)

	out, err := runCmd(t, runHistory)
	if err != nil {
		t.Fatal(err)
	}
	assertContains(t, out, "Cluster: c", "Project: web", "up")

	historyOutput = "json"
	out, err = runCmd(t, runHistory)
	if err != nil {
		t.Fatal(err)
	}
	var entries []history.Entry
	if err := json.Unmarshal([]byte(out), &entries); err != nil || len(entries) != 1 || entries[0].Command != "up" {
		t.Errorf("json output = %q (%v)", out, err)
	}
}

func TestHistoryExternalClusterUnreachable(t *testing.T) {
	env := withFakeDeps(t)
	useConfig(t, externalConfig)
	env.cluster.errs["VerifyClusterAccess"] = errBoom

	if _, err := runCmd(t, runHistory); err == nil || !strings.Contains(err.Error(), "failed to access external cluster 'c'") {
		t.Errorf("err = %v", err)
	}
}

func TestHistoryDockerUnavailable(t *testing.T) {
	env := withFakeDeps(t)
	useConfig(t, kindConfig)
	env.dockerErr = errBoom

	if _, err := runCmd(t, runHistory); !errors.Is(err, errBoom) {
		t.Errorf("err = %v", err)
	}
}

func TestHistoryClusterNotRunning(t *testing.T) {
	withFakeDeps(t)
	useConfig(t, kindConfig)

	out, err := runCmd(t, runHistory)
	if err != nil {
		t.Fatal(err)
	}
	assertContains(t, out, "Cluster 'c' is not running")
}

func TestHistoryEmpty(t *testing.T) {
	env := withFakeDeps(t)
	useConfig(t, externalConfig)

	out, err := runCmd(t, runHistory)
	if err != nil {
		t.Fatal(err)
	}
	assertContains(t, out, "No history recorded for cluster 'c'")
	if env.dockerChecks != 0 {
		t.Error("an external cluster doesn't need Docker")
	}
}

// --- list-images ---

func TestListImages(t *testing.T) {
	env := withFakeDeps(t)
	useConfig(t, kindConfig)
	env.cluster.exists = true
	env.images.listed = []cluster.ClusterImage{
		{ID: "sha256:0123456789abcdef0123", RepoTags: []string{"app:1", "app:latest"}, Size: "1000"},
		{ID: "sha256:fedcba9876543210fedc", Size: "2000"},
	}

	out, err := runCmd(t, runListImages)
	if err != nil {
		t.Fatal(err)
	}
	assertContains(t, out, "app:1", "app:latest", "<none>", "0123456789ab")
}

func TestListImagesErrors(t *testing.T) {
	for name, tc := range map[string]struct {
		config string
		exists bool
		want   string
	}{
		"external":   {externalConfig, true, "only supported for kind clusters"},
		"no cluster": {kindConfig, false, "cluster 'c' does not exist"},
	} {
		t.Run(name, func(t *testing.T) {
			env := withFakeDeps(t)
			useConfig(t, tc.config)
			env.cluster.exists = tc.exists
			_, err := runCmd(t, runListImages)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want %q", err, tc.want)
			}
		})
	}
}
