package cli

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hjames9/kraze/internal/cluster"
	"github.com/hjames9/kraze/internal/history"
	"github.com/hjames9/kraze/internal/providers"
	"github.com/hjames9/kraze/internal/state"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// history returns the recorded history entries, oldest first
func (e *fakeEnv) history(t *testing.T) []history.Entry {
	t.Helper()
	entries, err := history.Load(context.Background(), e.clientset)
	if err != nil {
		t.Fatal(err)
	}
	return entries
}

// lastHistory returns the most recent history entry
func (e *fakeEnv) lastHistory(t *testing.T) history.Entry {
	t.Helper()
	entries := e.history(t)
	if len(entries) == 0 {
		t.Fatal("no history recorded")
	}
	return entries[len(entries)-1]
}

// changeActions returns a history entry's changes as "name:action:result"
func changeActions(entry history.Entry) []string {
	var out []string
	for _, c := range entry.Changes {
		out = append(out, fmt.Sprintf("%s:%s:%s", c.Name, c.Action, c.Result))
	}
	slices.Sort(out)
	return out
}

// installLevels groups the install events into dependency levels by the
// order they happened: services installed in parallel come out sorted
func installOrder(env *fakeEnv) []string {
	var out []string
	for _, ev := range env.eventsWith("install ") {
		out = append(out, strings.TrimPrefix(ev, "install "))
	}
	return out
}

func TestUpCreatesMissingCluster(t *testing.T) {
	env := withFakeDeps(t)
	path := useConfig(t, kindConfig)

	out, err := runCmd(t, runUp)
	if err != nil {
		t.Fatalf("up failed: %v\n%s", err, out)
	}
	assertContains(t, out, "Cluster 'c' does not exist, creating it...")

	if got := env.eventsWith("cluster."); !slices.Equal(got, []string{"cluster.create c"}) {
		t.Errorf("cluster events = %v, want only a create", got)
	}
	if !env.cluster.kubeconfigUpdated {
		t.Error("~/.kube/config should be updated for a new cluster")
	}
	if env.dockerChecks != 1 {
		t.Errorf("docker checked %d times, want 1", env.dockerChecks)
	}
	assertEqual(t, "skipTLS per clientset", env.skipTLS, []bool{true})

	// a, then b and c in parallel, then d
	order := installOrder(env)
	if len(order) != 4 || order[0] != "a" || order[3] != "d" {
		t.Errorf("install order = %v, want a first and d last", order)
	}
	// Two pauses between three levels; post_ready_delay is 0s
	assertEqual(t, "sleeps", env.sleeps, []time.Duration{3 * time.Second, 3 * time.Second})

	st := env.state(t, "web")
	for name, want := range map[string]struct{ ns, typ string }{
		"a": {"shop", "helm"}, "b": {"shop", "helm"}, "c": {"jobs", "manifests"}, "d": {"shop", "helm"},
	} {
		meta, ok := st.GetService(name)
		if !ok || !meta.Installed || meta.Namespace != want.ns || meta.Type != want.typ || !meta.CreatedNamespace {
			t.Errorf("state for %s = %+v, want installed %s in %s with created namespace", name, meta, want.typ, want.ns)
		}
	}
	if st.CreatedBy != "web" || st.ClusterConfigHash == "" {
		t.Errorf("a new cluster should record its creator and config hash, got %q/%q", st.CreatedBy, st.ClusterConfigHash)
	}
	assertEqual(t, "config paths", st.GetConfigPaths(), []string{path})

	// Namespaces kraze creates are labeled, so down may delete them
	for _, ns := range []string{"shop", "jobs"} {
		obj, err := env.clientset.CoreV1().Namespaces().Get(context.Background(), ns, metav1.GetOptions{})
		if err != nil {
			t.Errorf("namespace %s: %v", ns, err)
		} else if obj.Labels[providers.ManagedByLabel] != providers.ManagedByValue {
			t.Errorf("namespace %s labels = %v, want the managed-by label", ns, obj.Labels)
		}
	}

	entry := env.lastHistory(t)
	if entry.Command != "up" || entry.Result != history.ResultSucceeded || !entry.ClusterCreated || entry.Project != "web" {
		t.Errorf("history entry = %+v", entry)
	}
	assertEqual(t, "history changes", changeActions(entry),
		[]string{"a:install:succeeded", "b:install:succeeded", "c:install:succeeded", "d:install:succeeded"})
}

func TestUpExistingClusterUpgrades(t *testing.T) {
	env := withFakeDeps(t)
	useConfig(t, kindConfig)
	env.cluster.exists = true

	if out, err := runCmd(t, runUp); err != nil {
		t.Fatalf("first up: %v\n%s", err, out)
	}
	if out, err := runCmd(t, runUp); err != nil {
		t.Fatalf("second up: %v\n%s", err, out)
	}

	if got := env.eventsWith("cluster."); len(got) != 0 {
		t.Errorf("an existing cluster must not be created, got %v", got)
	}
	if env.cluster.kubeconfigUpdated {
		t.Error("~/.kube/config is only updated for a new cluster")
	}
	entries := env.history(t)
	if len(entries) != 2 || entries[0].ClusterCreated {
		t.Fatalf("history = %+v", entries)
	}
	assertEqual(t, "second run changes", changeActions(entries[1]),
		[]string{"a:upgrade:succeeded", "b:upgrade:succeeded", "c:upgrade:succeeded", "d:upgrade:succeeded"})
}

func TestUpExternalCluster(t *testing.T) {
	env := withFakeDeps(t)
	useConfig(t, externalConfig)
	env.images.images["a"] = []string{"app:dev"}
	env.images.info["app:dev"] = &cluster.ImageInfo{SHA256: "sha256:0123456789abcdef", InLocalDaemon: true}

	out, err := runCmd(t, runUp)
	if err != nil {
		t.Fatalf("up failed: %v\n%s", err, out)
	}
	if env.dockerChecks != 0 {
		t.Error("an external cluster doesn't need Docker")
	}
	assertEqual(t, "cluster events", env.eventsWith("cluster."), []string{"cluster.verify"})
	assertEqual(t, "skipTLS per clientset", env.skipTLS, []bool{false})
	if got := env.eventsWith("image."); len(got) != 0 {
		t.Errorf("images are never loaded into an external cluster, got %v", got)
	}
	if opts := env.options("a"); opts == nil || opts.KubeConfig != "external-kubeconfig" {
		t.Errorf("provider should get the external kubeconfig, got %+v", opts)
	}
	// Hashes are still recorded
	meta, _ := env.state(t, "web").GetService("a")
	if meta.ImageHashes["app:dev"] != "sha256:0123456789abcdef" {
		t.Errorf("image hashes = %v", meta.ImageHashes)
	}
}

func TestUpExternalClusterUnreachable(t *testing.T) {
	env := withFakeDeps(t)
	useConfig(t, externalConfig)
	env.cluster.errs["VerifyClusterAccess"] = errBoom

	_, err := runCmd(t, runUp)
	if err == nil || !strings.Contains(err.Error(), "failed to access external cluster 'c'") {
		t.Errorf("err = %v", err)
	}
	if len(installOrder(env)) != 0 {
		t.Error("nothing should be installed")
	}
}

func TestUpDockerUnavailable(t *testing.T) {
	env := withFakeDeps(t)
	useConfig(t, kindConfig)
	env.dockerErr = errBoom

	if _, err := runCmd(t, runUp); !errors.Is(err, errBoom) {
		t.Errorf("err = %v, want the Docker error", err)
	}
	if len(env.events) != 0 {
		t.Errorf("nothing should happen without Docker, got %v", env.events)
	}
}

func TestUpFailedInstall(t *testing.T) {
	for name, tc := range map[string]struct {
		err       error
		leftovers bool // IsInstalled after the failure
		recorded  bool
	}{
		"left objects behind": {err: errBoom, leftovers: true, recorded: true},
		"applied nothing":     {err: errBoom, leftovers: false, recorded: false},
		"ownership conflict":  {err: fmt.Errorf("x: %w", providers.ErrOwnershipConflict), leftovers: true, recorded: false},
	} {
		t.Run(name, func(t *testing.T) {
			env := withFakeDeps(t)
			useConfig(t, kindConfig)
			env.cluster.exists = true
			b := env.provider("b")
			b.installErr = tc.err
			b.installed = tc.leftovers

			_, err := runCmd(t, runUp)
			if err == nil || !strings.Contains(err.Error(), "failed to install service 'b' in level 1") {
				t.Fatalf("err = %v", err)
			}
			if slices.Contains(installOrder(env), "d") {
				t.Error("a later level must not run after a failure")
			}
			st := env.state(t, "web")
			if got := st.IsServiceInstalled("b"); got != tc.recorded {
				t.Errorf("b recorded in state = %v, want %v", got, tc.recorded)
			}
			if !st.IsServiceInstalled("c") {
				t.Error("c, installed in parallel with b, should be recorded")
			}
			entry := env.lastHistory(t)
			if entry.Result != history.ResultFailed || !slices.Contains(changeActions(entry), "b:install:failed") {
				t.Errorf("history entry = %+v", entry)
			}
		})
	}
}

func TestUpRecordsResourceRefs(t *testing.T) {
	env := withFakeDeps(t)
	useConfig(t, kindConfig)
	env.cluster.exists = true
	refs := []state.ResourceRef{{APIVersion: "batch/v1", Kind: "Job", Namespace: "jobs", Name: "migrate"}}
	env.provider("c").refs = refs

	if out, err := runCmd(t, runUp); err != nil {
		t.Fatalf("up failed: %v\n%s", err, out)
	}
	meta, _ := env.state(t, "web").GetService("c")
	assertEqual(t, "recorded resources", meta.Resources, refs)
}

// Orphans are removed before installing: a renamed service's objects still
// carry the old name, so installing the new name first would fail
func TestUpRemoveOrphansBeforeInstalling(t *testing.T) {
	env := withFakeDeps(t)
	useConfig(t, kindConfig)
	env.cluster.exists = true
	upRemoveOrphans = true

	st := state.New("c", false, false, 0, false, 0)
	st.SetProject("web")
	st.MarkServiceInstalledWithNamespace("old-a", "shop", true)
	st.SetServiceSource("old-a", "helm", nil)
	env.saveState(t, st)

	if out, err := runCmd(t, runUp); err != nil {
		t.Fatalf("up failed: %v\n%s", err, out)
	}
	if len(env.events) == 0 || env.events[0] != "orphan.uninstall shop/old-a" {
		t.Errorf("events = %v, want the orphan removed first", env.events)
	}
	if env.state(t, "web").IsServiceInstalled("old-a") {
		t.Error("the orphan should be gone from state")
	}
	if len(env.deletedNamespaces) != 0 {
		t.Errorf("shop is still configured and must be kept, deleted %v", env.deletedNamespaces)
	}
}

func TestUpOrphanHintWithoutRemoveOrphans(t *testing.T) {
	env := withFakeDeps(t)
	useConfig(t, kindConfig)
	env.cluster.exists = true
	st := state.New("c", false, false, 0, false, 0)
	st.SetProject("web")
	st.MarkServiceInstalledWithNamespace("old-a", "shop", true)
	env.saveState(t, st)

	out, err := runCmd(t, runUp)
	if err != nil {
		t.Fatal(err)
	}
	assertContains(t, out, "no longer in its config: old-a", "--remove-orphans")
	if got := env.eventsWith("orphan."); len(got) != 0 {
		t.Errorf("orphans are only removed with --remove-orphans, got %v", got)
	}
}

func TestUpRefusesMovedService(t *testing.T) {
	env := withFakeDeps(t)
	useConfig(t, kindConfig)
	env.cluster.exists = true
	st := state.New("c", false, false, 0, false, 0)
	st.SetProject("web")
	st.MarkServiceInstalledWithNamespace("a", "elsewhere", true)
	st.SetServiceSource("a", "helm", nil)
	env.saveState(t, st)

	_, err := runCmd(t, runUp)
	if err == nil || !strings.Contains(err.Error(), "a (helm in 'elsewhere', now helm in 'shop')") {
		t.Errorf("err = %v", err)
	}
	if len(installOrder(env)) != 0 {
		t.Error("nothing should be installed")
	}

	// --remove-orphans removes the old installation by its record first
	upRemoveOrphans = true
	if out, err := runCmd(t, runUp); err != nil {
		t.Fatalf("up --remove-orphans: %v\n%s", err, out)
	}
	if got := env.eventsWith("orphan."); !slices.Equal(got, []string{"orphan.uninstall elsewhere/a"}) {
		t.Errorf("orphan events = %v", got)
	}
	meta, _ := env.state(t, "web").GetService("a")
	if meta.Namespace != "shop" {
		t.Errorf("a should now be recorded in shop, got %+v", meta)
	}
}

func TestUpRefusesAnotherProjectsService(t *testing.T) {
	env := withFakeDeps(t)
	useConfig(t, kindConfig)
	env.cluster.exists = true
	st := state.New("c", false, false, 0, false, 0)
	st.SetProject("billing")
	st.MarkServiceInstalledWithNamespace("a", "shop", true)
	env.saveState(t, st)

	if _, err := runCmd(t, runUp); err == nil || !strings.Contains(err.Error(), "billing") {
		t.Errorf("err = %v, want a conflict with billing", err)
	}
	if len(installOrder(env)) != 0 {
		t.Error("nothing should be installed")
	}
}

func TestUpLoadsChangedLocalImages(t *testing.T) {
	env := withFakeDeps(t)
	useConfig(t, kindConfig)
	env.cluster.exists = true
	env.images.images["a"] = []string{"changed:dev", "same:dev", "missing:dev", "registry/redis:7"}
	env.images.info["changed:dev"] = &cluster.ImageInfo{SHA256: "sha256:new0000000000000", InLocalDaemon: true}
	env.images.info["same:dev"] = &cluster.ImageInfo{SHA256: "sha256:same000000000000", InLocalDaemon: true}
	env.images.info["missing:dev"] = &cluster.ImageInfo{SHA256: "sha256:miss000000000000", InLocalDaemon: true}
	env.images.clusterHash["changed:dev"] = "sha256:old0000000000000"
	env.images.clusterHash["same:dev"] = "sha256:same000000000000"
	// registry/redis:7 isn't in the local daemon: kind pulls it

	if out, err := runCmd(t, runUp); err != nil {
		t.Fatalf("up failed: %v\n%s", err, out)
	}
	assertEqual(t, "image events", env.eventsWith("image."),
		[]string{"image.untag changed:dev", "image.load changed:dev", "image.load missing:dev"})
	meta, _ := env.state(t, "web").GetService("a")
	if len(meta.ImageHashes) != 3 || meta.ImageHashes["changed:dev"] != "sha256:new0000000000000" {
		t.Errorf("image hashes = %v", meta.ImageHashes)
	}
}

func TestUpPassesWaitOptions(t *testing.T) {
	env := withFakeDeps(t)
	useConfig(t, strings.Replace(kindConfig, "    namespace: jobs\n", "    namespace: jobs\n    wait: false\n    wait_timeout: 2m\n", 1))
	env.cluster.exists = true
	upTimeout = "7m"

	if out, err := runCmd(t, runUp); err != nil {
		t.Fatalf("up failed: %v\n%s", err, out)
	}
	if opts := env.options("a"); !opts.Wait || opts.Timeout != "7m" || opts.Project != "web" || !opts.Quiet {
		t.Errorf("a options = %+v", opts)
	}
	if opts := env.options("c"); opts.Wait || opts.Timeout != "2m" {
		t.Errorf("c's own wait settings should win, got %+v", opts)
	}

	upNoWait = true
	if _, err := runCmd(t, runUp); err != nil {
		t.Fatal(err)
	}
	if opts := env.options("a"); opts.Wait {
		t.Error("--no-wait should turn waiting off")
	}
}

func TestUpPostReadyDelay(t *testing.T) {
	env := withFakeDeps(t)
	useConfig(t, `project: web
cluster:
  name: c
services:
  only:
    type: helm
    chart: x
    repo: oci://registry.example/charts
`)
	env.cluster.exists = true

	if _, err := runCmd(t, runUp); err != nil {
		t.Fatal(err)
	}
	assertEqual(t, "sleeps", env.sleeps, []time.Duration{3 * time.Second})
}

func TestUpCancelled(t *testing.T) {
	env := withFakeDeps(t)
	useConfig(t, kindConfig)
	env.cluster.exists = true
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := runCmdCtx(t, ctx, runUp)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if len(installOrder(env)) != 0 {
		t.Error("nothing should be installed after cancellation")
	}
	if entry := env.lastHistory(t); entry.Result != history.ResultInterrupted {
		t.Errorf("history result = %s, want interrupted", entry.Result)
	}
}

func TestUpDryRunTouchesNothing(t *testing.T) {
	env := withFakeDeps(t)
	useConfig(t, kindConfig)
	dryRun = true

	out, err := runCmd(t, runUp)
	if err != nil {
		t.Fatal(err)
	}
	assertContains(t, out, "[DRY RUN] Would install 4 service(s)")
	if len(env.events) != 0 || len(env.skipTLS) != 0 {
		t.Errorf("dry run reached the cluster: %v", env.events)
	}
}

func TestUpNoDepsInstallsOnlyNamed(t *testing.T) {
	env := withFakeDeps(t)
	useConfig(t, kindConfig)
	env.cluster.exists = true
	upNoDeps = true

	if _, err := runCmd(t, runUp, "d"); err != nil {
		t.Fatal(err)
	}
	assertEqual(t, "installed", installOrder(env), []string{"d"})
}

// Unowned entries are only foreign if the whole config lacks them, not just
// the services named on the command line
func TestUpNamedServicesWarnOnlyAboutForeignEntries(t *testing.T) {
	env := withFakeDeps(t)
	useConfig(t, kindConfig)
	env.cluster.exists = true
	legacy := state.New("c", false, false, 0, false, 0)
	legacy.SetProject("") // entries from before projects existed
	legacy.MarkServiceInstalledWithNamespace("d", "shop", true)
	legacy.MarkServiceInstalledWithNamespace("stranger", "elsewhere", true)
	env.saveState(t, legacy)

	out, err := runCmd(t, runUp, "a")
	if err != nil {
		t.Fatal(err)
	}
	assertContains(t, out, "has service(s) with no project: stranger.")
	if strings.Contains(out, "no project: d") || strings.Contains(out, ", d") {
		t.Errorf("d is in the config and must not be called foreign:\n%s", out)
	}
	if !slices.Contains(env.state(t, "").UnownedServices(), "d") {
		t.Error("d wasn't installed by this run and stays unclaimed")
	}
}

// GPU support is fixed when the cluster is created: an existing cluster
// without it refuses a config that needs it, before installing anything.
// External clusters aren't checked.
func TestUpRefusesGPUMismatch(t *testing.T) {
	gpuConfig := strings.Replace(kindConfig, "  name: c\n", "  name: c\n  gpu:\n    nvidia:\n      enabled: true\n", 1)

	env := withFakeDeps(t)
	useConfig(t, gpuConfig)
	env.cluster.exists = true
	st := state.New("c", false, false, 0, false, 0) // created without GPUs
	st.SetProject("web")
	st.SetClusterConfig("hash", "web")
	env.saveState(t, st)

	_, err := runCmd(t, runUp)
	if err == nil || !strings.Contains(err.Error(), "GPU configuration has changed since cluster 'c' was created") {
		t.Fatalf("err = %v", err)
	}
	if len(installOrder(env)) != 0 {
		t.Error("nothing should be installed")
	}

	// A new cluster records what it was created with
	env = withFakeDeps(t)
	useConfig(t, gpuConfig)
	if out, err := runCmd(t, runUp); err != nil {
		t.Fatalf("up on a new cluster: %v\n%s", err, out)
	}
	if !env.state(t, "web").NvidiaGPUEnabled {
		t.Error("a new cluster's state should record NVIDIA GPUs")
	}

}
