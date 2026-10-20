package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/hjames9/kraze/internal/config"
	"github.com/hjames9/kraze/internal/providers"
	"github.com/hjames9/kraze/internal/state"
	"k8s.io/client-go/kubernetes/fake"
)

// fakeOrphanBackend records what removeOrphans asks it to do
type fakeOrphanBackend struct {
	helmReleases  map[string]bool // releases that exist, by name
	failUninstall map[string]bool
	uninstallErr  map[string]error // returned by uninstallHelm, by name
	uninstalled   []string
	deleted       map[string][]state.ResourceRef
	keepCRDs      map[string]bool
}

func (b *fakeOrphanBackend) helmReleaseExists(_ context.Context, svc *config.ServiceConfig) (bool, error) {
	return b.helmReleases[svc.Name], nil
}

func (b *fakeOrphanBackend) uninstallHelm(_ context.Context, svc *config.ServiceConfig) error {
	if b.failUninstall[svc.Name] {
		return errors.New("boom")
	}
	if err := b.uninstallErr[svc.Name]; err != nil {
		return err
	}
	b.uninstalled = append(b.uninstalled, svc.Name)
	b.keepCRDs[svc.Name] = svc.KeepCRDs != nil && *svc.KeepCRDs
	return nil
}

func (b *fakeOrphanBackend) deleteManifests(_ context.Context, name string, refs []state.ResourceRef) (int, error) {
	b.deleted[name] = refs
	return len(refs), nil
}

func TestRemoveOrphans(t *testing.T) {
	var mu sync.Mutex
	var deletedNamespaces []string
	deleteNamespace = func(_ context.Context, _, ns string) error {
		mu.Lock()
		defer mu.Unlock()
		deletedNamespaces = append(deletedNamespaces, ns)
		return nil
	}
	timeout := downNamespaceDeletionTimeout
	downNamespaceDeletionTimeout = 0
	t.Cleanup(func() {
		deleteNamespace = providers.DeleteNamespace
		downNamespaceDeletionTimeout = timeout
	})

	refs := []state.ResourceRef{{APIVersion: "v1", Kind: "ConfigMap", Namespace: "man", Name: "cfg"}}
	st := state.New("c", false, false, 0, false, 0)
	st.SetProject("billing")
	install := func(name, ns, serviceType string, resources []state.ResourceRef) {
		st.MarkServiceInstalledWithNamespace(name, ns, true)
		st.SetServiceSource(name, serviceType, resources)
	}
	install("chart", "charts", "helm", nil)
	install("manifest", "man", "manifests", refs)
	install("legacy-chart", "legacy-charts", "", nil)
	install("legacy-manifest", "legacy-man", "", nil)
	install("broken", "broken", "helm", nil)
	install("kept", "charts", "helm", nil) // in config, shares chart's namespace

	clientset := fake.NewSimpleClientset(
		namespace("charts", true), namespace("man", true), namespace("legacy-charts", true),
		namespace("legacy-man", true), namespace("broken", true),
	)
	backend := &fakeOrphanBackend{
		helmReleases:  map[string]bool{"legacy-chart": true},
		failUninstall: map[string]bool{"broken": true},
		deleted:       map[string][]state.ResourceRef{},
		keepCRDs:      map[string]bool{},
	}

	orphans := st.Orphans(map[string]bool{"kept": true})
	var err error
	out := captureStdout(t, func() {
		err = removeOrphans(context.Background(), st, clientset, "", orphans, nil, backend, false, nil, func(string, ...interface{}) {})
	})

	if err == nil || !strings.Contains(err.Error(), "broken") {
		t.Errorf("expected an error naming the failed orphan, got %v", err)
	}
	slices.Sort(backend.uninstalled)
	if want := []string{"chart", "legacy-chart"}; !slices.Equal(backend.uninstalled, want) {
		t.Errorf("helm uninstalled = %v, want %v", backend.uninstalled, want)
	}
	if !backend.keepCRDs["chart"] {
		t.Error("orphan removal must keep CRDs")
	}
	if got := backend.deleted["manifest"]; !slices.Equal(got, refs) {
		t.Errorf("manifests deleted with refs %v, want %v", got, refs)
	}
	if !strings.Contains(out, "kraze.service=legacy-manifest") {
		t.Errorf("expected a kubectl hint for the unknown legacy orphan, got:\n%s", out)
	}

	for _, gone := range []string{"chart", "manifest", "legacy-chart", "legacy-manifest"} {
		if st.IsServiceInstalled(gone) {
			t.Errorf("%s should be gone from state", gone)
		}
	}
	for _, still := range []string{"broken", "kept"} {
		if !st.IsServiceInstalled(still) {
			t.Errorf("%s should still be in state", still)
		}
	}

	// Only the namespaces of fully removed orphans: not the forgotten legacy
	// manifests service's (its objects remain), the failed one's, or one still
	// used by a configured service
	slices.Sort(deletedNamespaces)
	if want := []string{"legacy-charts", "man"}; !slices.Equal(deletedNamespaces, want) {
		t.Errorf("deleted namespaces = %v, want %v", deletedNamespaces, want)
	}
}

// A renamed service usually keeps its namespace; when `up --remove-orphans`
// removes the old name, the new one isn't installed yet but needs it
func TestRemoveOrphansKeepsConfiguredNamespaces(t *testing.T) {
	var deleted []string
	deleteNamespace = func(_ context.Context, _, ns string) error {
		deleted = append(deleted, ns)
		return nil
	}
	timeout := downNamespaceDeletionTimeout
	downNamespaceDeletionTimeout = 0
	t.Cleanup(func() {
		deleteNamespace = providers.DeleteNamespace
		downNamespaceDeletionTimeout = timeout
	})

	st := state.New("c", false, false, 0, false, 0)
	st.SetProject("web")
	st.MarkServiceInstalledWithNamespace("api", "shop", true)
	st.SetServiceSource("api", "helm", nil)
	st.MarkServiceInstalledWithNamespace("old-worker", "jobs", true)
	st.SetServiceSource("old-worker", "helm", nil)
	clientset := fake.NewSimpleClientset(namespace("shop", true), namespace("jobs", true))
	backend := &fakeOrphanBackend{deleted: map[string][]state.ResourceRef{}, keepCRDs: map[string]bool{}}

	cfg := &config.Config{Services: map[string]config.ServiceConfig{"backend": {Name: "backend", Namespace: "shop"}}}
	orphans := st.Orphans(configuredServices(cfg))
	captureStdout(t, func() {
		if err := removeOrphans(context.Background(), st, clientset, "", orphans, configuredNamespaces(cfg), backend, false, nil, func(string, ...interface{}) {}); err != nil {
			t.Error(err)
		}
	})

	if !slices.Equal(deleted, []string{"jobs"}) {
		t.Errorf("deleted namespaces = %v, want only jobs (shop is still in the config)", deleted)
	}
}

func TestOrphanHint(t *testing.T) {
	out := captureStdout(t, func() {
		printOrphanHint([]state.ServiceMetadata{{Name: "a"}, {Name: "b"}})
	})
	if !strings.Contains(out, "a, b") || !strings.Contains(out, "--remove-orphans") {
		t.Errorf("hint = %q", out)
	}
	if out := captureStdout(t, func() { printOrphanHint(nil) }); out != "" {
		t.Errorf("no orphans should print nothing, got %q", out)
	}
}

// writeDownTestConfig writes downTestConfig (an external cluster, so no
// command below needs Docker) and points -f at it
func writeDownTestConfig(t *testing.T) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "kraze.yml")
	if err := os.WriteFile(path, []byte(downTestConfig), 0o644); err != nil {
		t.Fatal(err)
	}
	saved := configFiles
	configFiles = []string{path}
	t.Cleanup(func() { configFiles = saved })
}

func TestRemoveOrphansRejectsPartialRuns(t *testing.T) {
	writeDownTestConfig(t)
	savedUp, savedUpLabels := upRemoveOrphans, upLabels
	savedDown, savedDownLabels, savedDisabled := downRemoveOrphans, downLabels, downDisabledOnly
	t.Cleanup(func() {
		upRemoveOrphans, upLabels = savedUp, savedUpLabels
		downRemoveOrphans, downLabels, downDisabledOnly = savedDown, savedDownLabels, savedDisabled
	})
	upRemoveOrphans, downRemoveOrphans = true, true

	tests := []struct {
		name     string
		run      func(args []string) error
		args     []string
		labels   []string
		disabled bool
	}{
		{name: "up with service names", run: func(a []string) error { return runUp(upCmd, a) }, args: []string{"alpha"}},
		{name: "up with --label", run: func(a []string) error { return runUp(upCmd, a) }, labels: []string{"tier=backend"}},
		{name: "down with service names", run: func(a []string) error { return runDown(downCmd, a) }, args: []string{"alpha"}},
		{name: "down with --label", run: func(a []string) error { return runDown(downCmd, a) }, labels: []string{"tier=backend"}},
		{name: "down with --disabled", run: func(a []string) error { return runDown(downCmd, a) }, disabled: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			upLabels, downLabels, downDisabledOnly = tt.labels, tt.labels, tt.disabled
			err := tt.run(tt.args)
			if err == nil || !strings.Contains(err.Error(), "--remove-orphans can't be combined") {
				t.Errorf("expected --remove-orphans to be rejected, got %v", err)
			}
		})
	}
}

func TestDownRemoveOrphansDryRun(t *testing.T) {
	writeDownTestConfig(t)
	savedOrphans, savedDryRun, savedLabels, savedDisabled := downRemoveOrphans, dryRun, downLabels, downDisabledOnly
	t.Cleanup(func() {
		downRemoveOrphans, dryRun, downLabels, downDisabledOnly = savedOrphans, savedDryRun, savedLabels, savedDisabled
	})
	downRemoveOrphans, dryRun, downLabels, downDisabledOnly = true, true, nil, false

	var err error
	out := captureStdout(t, func() { err = runDown(downCmd, nil) })
	if err != nil {
		t.Fatalf("a full down accepts --remove-orphans: %v", err)
	}
	if !strings.Contains(out, "--remove-orphans") {
		t.Errorf("dry run should mention orphan removal, got:\n%s", out)
	}
}

func TestPrintOrphanPlan(t *testing.T) {
	var count int
	out := captureStdout(t, func() {
		count = printOrphanPlan([]state.ServiceMetadata{
			{Name: "old-chart", Type: "helm", Namespace: "charts"},
			{Name: "ancient", Namespace: "default"},
		})
	})
	if count != 2 {
		t.Errorf("count = %d, want 2", count)
	}
	for _, want := range []string{"kraze up --remove-orphans", "old-chart", "helm in namespace 'charts'", "ancient", "type unknown"} {
		if !strings.Contains(out, want) {
			t.Errorf("orphan plan missing %q:\n%s", want, out)
		}
	}

	if out := captureStdout(t, func() { count = printOrphanPlan(nil) }); out != "" || count != 0 {
		t.Errorf("no orphans should print nothing, got %d and %q", count, out)
	}

	summary := captureStdout(t, func() { printPlanSummary(1, 0, 0, 0, 0, 2) })
	if !strings.Contains(summary, "2 orphaned") {
		t.Errorf("summary should count orphans, got %q", summary)
	}
}

func TestMovedInstalls(t *testing.T) {
	st := state.New("c", false, false, 0, false, 0)
	st.SetProject("web")
	install := func(name, ns, serviceType string) {
		st.MarkServiceInstalledWithNamespace(name, ns, true)
		st.SetServiceSource(name, serviceType, nil)
	}
	install("same", "app", "helm")
	install("new-namespace", "jobs", "manifests")
	install("new-type", "app", "manifests")
	install("legacy-same", "app", "")  // older kraze recorded no type
	install("legacy-moved", "old", "") // but always the namespace

	services := map[string]config.ServiceConfig{
		"same":          {Name: "same", Type: "helm", Namespace: "app"},
		"new-namespace": {Name: "new-namespace", Type: "manifests", Namespace: "work"},
		"new-type":      {Name: "new-type", Type: "helm", Namespace: "app"},
		"legacy-same":   {Name: "legacy-same", Type: "helm", Namespace: "app"},
		"legacy-moved":  {Name: "legacy-moved", Type: "helm", Namespace: "app"},
		"not-installed": {Name: "not-installed", Type: "helm", Namespace: "elsewhere"},
	}
	moved := movedInstalls(st, services)
	if got, want := orphanNames(moved), []string{"legacy-moved", "new-namespace", "new-type"}; !slices.Equal(got, want) {
		t.Fatalf("moved = %v, want %v", got, want)
	}
	if moved[1].Namespace != "jobs" {
		t.Errorf("a moved entry must carry where it was installed, got namespace %q", moved[1].Namespace)
	}

	err := movedServicesError(moved, services)
	for _, want := range []string{
		"new-namespace (manifests in 'jobs', now manifests in 'work')",
		"new-type (manifests in 'app', now helm in 'app')",
		"legacy-moved (installed in 'old', now helm in 'app')",
		"kraze up --remove-orphans",
	} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("error should contain %q, got %v", want, err)
		}
	}
}

// A moved service's old installation is removed by what was recorded: the
// old namespace and type, not the config's
func TestRemoveOrphansRemovesMovedInstallByRecord(t *testing.T) {
	deleteNamespace = func(context.Context, string, string) error { return nil }
	timeout := downNamespaceDeletionTimeout
	downNamespaceDeletionTimeout = 0
	t.Cleanup(func() {
		deleteNamespace = providers.DeleteNamespace
		downNamespaceDeletionTimeout = timeout
	})

	refs := []state.ResourceRef{{APIVersion: "v1", Kind: "ConfigMap", Namespace: "jobs", Name: "worker-cfg"}}
	st := state.New("c", false, false, 0, false, 0)
	st.SetProject("web")
	st.MarkServiceInstalledWithNamespace("worker", "jobs", true)
	st.SetServiceSource("worker", "manifests", refs)

	services := map[string]config.ServiceConfig{"worker": {Name: "worker", Type: "helm", Namespace: "work"}}
	backend := &fakeOrphanBackend{deleted: map[string][]state.ResourceRef{}, keepCRDs: map[string]bool{}}
	captureStdout(t, func() {
		err := removeOrphans(context.Background(), st, fake.NewSimpleClientset(), "", movedInstalls(st, services),
			map[string]bool{"work": true}, backend, false, nil, func(string, ...interface{}) {})
		if err != nil {
			t.Error(err)
		}
	})
	if got := backend.deleted["worker"]; !slices.Equal(got, refs) {
		t.Errorf("deleted %v, want the recorded manifests objects %v", got, refs)
	}
	if len(backend.uninstalled) != 0 {
		t.Errorf("must not uninstall as the new type, got %v", backend.uninstalled)
	}
	if st.IsServiceInstalled("worker") {
		t.Error("the old entry should be gone so the new installation is recorded afresh")
	}
}
