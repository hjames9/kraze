package cli

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/hjames9/kraze/internal/history"
	"github.com/hjames9/kraze/internal/state"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// installAll runs a full `up` of config into an existing kind cluster, then
// clears the events so a test sees only what comes after
func installAll(t *testing.T, env *fakeEnv, config string) {
	t.Helper()
	useConfig(t, config)
	env.cluster.exists = true
	if out, err := runCmd(t, runUp); err != nil {
		t.Fatalf("up failed: %v\n%s", err, out)
	}
	env.mu.Lock()
	env.events = nil
	env.skipTLS = nil
	env.dockerChecks = 0
	env.mu.Unlock()
}

func uninstallOrder(env *fakeEnv) []string {
	var out []string
	for _, ev := range env.eventsWith("uninstall ") {
		out = append(out, strings.TrimPrefix(ev, "uninstall "))
	}
	return out
}

func TestDownUninstallsInReverseOrder(t *testing.T) {
	env := withFakeDeps(t)
	installAll(t, env, kindConfig)

	out, err := runCmd(t, runDown)
	if err != nil {
		t.Fatalf("down failed: %v\n%s", err, out)
	}
	order := uninstallOrder(env)
	if len(order) != 4 || order[0] != "d" || order[3] != "a" {
		t.Errorf("uninstall order = %v, want d first and a last", order)
	}
	st := env.state(t, "web")
	for _, name := range []string{"a", "b", "c", "d"} {
		if st.IsServiceInstalled(name) {
			t.Errorf("%s should be gone from state", name)
		}
	}

	// Both namespaces were created by kraze and nothing else uses them
	slices.Sort(env.deletedNamespaces)
	assertEqual(t, "deleted namespaces", env.deletedNamespaces, []string{"jobs", "shop"})
	slices.Sort(env.waitedNamespaces)
	assertEqual(t, "waited for", env.waitedNamespaces, []string{"jobs", "shop"})

	entry := env.lastHistory(t)
	if entry.Command != "down" || entry.Result != history.ResultSucceeded {
		t.Errorf("history entry = %+v", entry)
	}
	assertEqual(t, "history changes", changeActions(entry),
		[]string{"a:uninstall:succeeded", "b:uninstall:succeeded", "c:uninstall:succeeded", "d:uninstall:succeeded"})
}

func TestDownNoNamespaceWaitWithZeroTimeout(t *testing.T) {
	env := withFakeDeps(t)
	installAll(t, env, kindConfig)
	downNamespaceDeletionTimeout = 0

	out, err := runCmd(t, runDown)
	if err != nil {
		t.Fatal(err)
	}
	if len(env.deletedNamespaces) != 2 || len(env.waitedNamespaces) != 0 {
		t.Errorf("deleted %v, waited %v: a zero timeout deletes without waiting", env.deletedNamespaces, env.waitedNamespaces)
	}
	assertContains(t, out, "Initiated deletion of 2 namespace(s)")
}

func TestDownSkipsServicesNotInstalled(t *testing.T) {
	env := withFakeDeps(t)
	installAll(t, env, kindConfig)
	env.provider("b").installed = false // removed behind kraze's back

	if _, err := runCmd(t, runDown); err != nil {
		t.Fatal(err)
	}
	if slices.Contains(uninstallOrder(env), "b") {
		t.Error("a service that isn't installed must not be uninstalled")
	}
}

func TestDownUninstallsWhenInstalledCheckFails(t *testing.T) {
	env := withFakeDeps(t)
	installAll(t, env, kindConfig)
	env.provider("b").installedErr = errBoom

	if _, err := runCmd(t, runDown); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(uninstallOrder(env), "b") {
		t.Error("a failed installed-check should still try the uninstall")
	}
}

// A release can disappear between the installed check and the uninstall
// (deleted namespace, someone else's helm uninstall): that's a removal
func TestDownReleaseGoneDuringUninstall(t *testing.T) {
	env := withFakeDeps(t)
	installAll(t, env, kindConfig)
	env.provider("b").uninstallErr = releaseGone("b")

	if _, err := runCmd(t, runDown); err != nil {
		t.Fatal(err)
	}
	if env.state(t, "web").IsServiceInstalled("b") {
		t.Error("a release already gone should be forgotten")
	}
	if !slices.Contains(changeActions(env.lastHistory(t)), "b:uninstall:succeeded") {
		t.Errorf("history changes = %v", changeActions(env.lastHistory(t)))
	}
	if !slices.Contains(env.deletedNamespaces, "shop") {
		t.Errorf("namespace cleanup should go on, deleted %v", env.deletedNamespaces)
	}
}

func TestDownUninstallFailureKeepsEntry(t *testing.T) {
	env := withFakeDeps(t)
	installAll(t, env, kindConfig)
	env.provider("b").uninstallErr = errBoom

	if _, err := runCmd(t, runDown); err != nil {
		t.Fatal(err)
	}
	st := env.state(t, "web")
	if !st.IsServiceInstalled("b") {
		t.Error("a failed uninstall must keep the entry")
	}
	if st.IsServiceInstalled("a") {
		t.Error("the services after the failure should still be uninstalled")
	}
	if slices.Contains(env.deletedNamespaces, "shop") {
		t.Error("b's namespace must be kept while b may still be there")
	}
	if !slices.Contains(changeActions(env.lastHistory(t)), "b:uninstall:failed") {
		t.Errorf("history changes = %v", changeActions(env.lastHistory(t)))
	}
}

func TestDownMissingCluster(t *testing.T) {
	env := withFakeDeps(t)
	useConfig(t, kindConfig)

	out, err := runCmd(t, runDown)
	if err != nil {
		t.Fatal(err)
	}
	assertContains(t, out, "Cluster 'c' does not exist, nothing to uninstall")
	if len(env.skipTLS) != 0 {
		t.Error("no client should be created for a missing cluster")
	}
}

func TestDownExternalCluster(t *testing.T) {
	env := withFakeDeps(t)
	installAll(t, env, externalConfig)

	if _, err := runCmd(t, runDown); err != nil {
		t.Fatal(err)
	}
	if env.dockerChecks != 0 {
		t.Error("an external cluster doesn't need Docker")
	}
	assertEqual(t, "cluster events", env.eventsWith("cluster."), []string{"cluster.verify"})
	assertEqual(t, "skipTLS per clientset", env.skipTLS, []bool{false})
	if len(uninstallOrder(env)) != 4 {
		t.Errorf("uninstalled %v", uninstallOrder(env))
	}
}

func TestDownKeepCRDs(t *testing.T) {
	env := withFakeDeps(t)
	installAll(t, env, kindConfig)
	downKeepCRDs = true

	if _, err := runCmd(t, runDown); err != nil {
		t.Fatal(err)
	}
	if opts := env.options("a"); opts == nil || !opts.KeepCRDs {
		t.Errorf("--keep-crds should reach the provider, got %+v", opts)
	}
}

// A service that moved namespace is removed where it was recorded, not where
// the config now says
func TestDownMovedServiceRemovedByRecord(t *testing.T) {
	env := withFakeDeps(t)
	useConfig(t, kindConfig)
	env.cluster.exists = true
	st := state.New("c", false, false, 0, false, 0)
	st.SetProject("web")
	st.MarkServiceInstalledWithNamespace("a", "elsewhere", true)
	st.SetServiceSource("a", "helm", nil)
	env.saveState(t, st)

	if _, err := runCmd(t, runDown); err != nil {
		t.Fatal(err)
	}
	assertEqual(t, "orphan events", env.eventsWith("orphan."), []string{"orphan.uninstall elsewhere/a"})
	if slices.Contains(uninstallOrder(env), "a") {
		t.Error("a must not be uninstalled from its configured namespace")
	}
	if env.state(t, "web").IsServiceInstalled("a") {
		t.Error("a's old entry should be gone")
	}
}

func TestDownRemoveOrphansAfterServices(t *testing.T) {
	env := withFakeDeps(t)
	installAll(t, env, kindConfig)
	st := env.state(t, "web")
	st.MarkServiceInstalledWithNamespace("old", "legacy", true)
	st.SetServiceSource("old", "helm", nil)
	env.saveState(t, st)
	downRemoveOrphans = true

	if _, err := runCmd(t, runDown); err != nil {
		t.Fatal(err)
	}
	events := env.events
	orphanAt := slices.Index(events, "orphan.uninstall legacy/old")
	lastUninstall := slices.Index(events, "uninstall a")
	if orphanAt < 0 || orphanAt < lastUninstall {
		t.Errorf("events = %v, want the orphan removed after the configured services", events)
	}
	if env.state(t, "web").IsServiceInstalled("old") {
		t.Error("the orphan should be gone from state")
	}
}

func TestDownCancelled(t *testing.T) {
	env := withFakeDeps(t)
	installAll(t, env, kindConfig)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := runCmdCtx(t, ctx, runDown)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if len(uninstallOrder(env)) != 0 || len(env.deletedNamespaces) != 0 {
		t.Errorf("nothing should be removed after cancellation: %v, %v", env.events, env.deletedNamespaces)
	}
	if entry := env.lastHistory(t); entry.Result != history.ResultInterrupted {
		t.Errorf("history result = %s, want interrupted", entry.Result)
	}
}

func TestDownToleratesUnreadableState(t *testing.T) {
	env := withFakeDeps(t)
	installAll(t, env, kindConfig)
	cm, err := env.clientset.CoreV1().ConfigMaps(state.ConfigMapNamespace).Get(context.Background(), state.ConfigMapName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	cm.Data[state.ConfigMapDataKey] = "{not json"
	if _, err := env.clientset.CoreV1().ConfigMaps(state.ConfigMapNamespace).Update(context.Background(), cm, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}

	if _, err := runCmd(t, runDown); err != nil {
		t.Fatalf("down should go on without state, got %v", err)
	}
	if len(uninstallOrder(env)) != 4 {
		t.Errorf("uninstalled %v", uninstallOrder(env))
	}
}

func TestDownMovedServiceFailureKeepsNamespace(t *testing.T) {
	env := withFakeDeps(t)
	useConfig(t, kindConfig)
	env.cluster.exists = true
	if _, err := env.clientset.CoreV1().Namespaces().Create(context.Background(), namespace("elsewhere", true), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	st := state.New("c", false, false, 0, false, 0)
	st.SetProject("web")
	st.MarkServiceInstalledWithNamespace("a", "elsewhere", true)
	st.SetServiceSource("a", "helm", nil)
	env.saveState(t, st)
	env.orphans.failUninstall["a"] = true

	if _, err := runCmd(t, runDown); err != nil {
		t.Fatal(err)
	}
	if !env.state(t, "web").IsServiceInstalled("a") {
		t.Error("a failed removal must keep the entry")
	}
	if slices.Contains(env.deletedNamespaces, "elsewhere") {
		t.Error("the namespace of a failed removal must be kept")
	}
}
