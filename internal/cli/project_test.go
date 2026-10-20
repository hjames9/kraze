package cli

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hjames9/kraze/internal/config"
	"github.com/hjames9/kraze/internal/providers"
	"github.com/hjames9/kraze/internal/state"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func sharedState() *state.ClusterState {
	st := state.New("shared", false, false, 0, false, 0)
	st.SetProject("billing")
	st.MarkServiceInstalledWithNamespace("postgres", "billing", true)
	st.MarkServiceInstalledWithNamespace("api", "common", true)
	return st
}

func TestCheckProjectAccessWarnsAboutUnclaimable(t *testing.T) {
	st := state.New("shared", false, false, 0, false, 0)
	st.MarkServiceInstalledWithNamespace("legacy", "default", false) // no project
	st.SetProject("web")
	cfg := &config.Config{Project: "web", Cluster: config.ClusterConfig{Name: "shared"}}

	out := captureStdout(t, func() { checkProjectAccess(st, cfg, map[string]bool{"api": true}) })
	if !strings.Contains(out, "legacy") {
		t.Errorf("expected a warning naming the unowned service, got %q", out)
	}

	// In the config but not part of this run (e.g. `up api --no-deps`): it
	// will be claimed by a later run, so no warning
	cfg.Services = map[string]config.ServiceConfig{"api": {Name: "api"}}
	configured := map[string]bool{"api": true, "legacy": true}
	if out := captureStdout(t, func() { checkProjectAccess(st, cfg, configured) }); out != "" {
		t.Errorf("a service in our config will be claimed, so no warning; got %q", out)
	}
}

func TestCheckServiceConflicts(t *testing.T) {
	st := sharedState()
	st.SetProject("web")

	ok := map[string]config.ServiceConfig{"postgres": {Name: "postgres", Namespace: "web"}}
	if err := checkServiceConflicts(st, ok, "web", nil); err != nil {
		t.Errorf("same name in another namespace should be allowed: %v", err)
	}

	clash := map[string]config.ServiceConfig{"postgres": {Name: "postgres", Namespace: "billing"}}
	err := checkServiceConflicts(st, clash, "web", nil)
	if err == nil || !strings.Contains(err.Error(), "owned by project billing") {
		t.Errorf("same name in the same namespace: got %v", err)
	}
}

func TestCheckGPUCompatibility(t *testing.T) {
	gpu := &config.GPUConfig{Nvidia: &config.GPUVendorConfig{Enabled: true}}
	st := state.New("shared", false, true, 0, false, 0) // cluster has NVIDIA
	st.SetClusterConfig("hash", "ml")

	tests := []struct {
		name    string
		project string
		gpu     *config.GPUConfig
		wantErr bool
	}{
		{name: "creator turning GPU off must recreate", project: "ml", gpu: nil, wantErr: true},
		{name: "creator unchanged", project: "ml", gpu: gpu},
		{name: "other project without GPU is fine", project: "web", gpu: nil},
		{name: "other project using the cluster's GPU is fine", project: "web", gpu: gpu},
		{name: "other project needing a missing vendor fails", project: "web",
			gpu: &config.GPUConfig{AMD: &config.GPUVendorConfig{Enabled: true}}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &config.Config{Project: tt.project, Cluster: config.ClusterConfig{Name: "shared", GPU: tt.gpu}}
			if err := checkGPUCompatibility(st, cfg); (err != nil) != tt.wantErr {
				t.Errorf("err = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestClusterConfigHashIgnoresName(t *testing.T) {
	a := config.ClusterConfig{Name: "one", Version: "1.37"}
	b := config.ClusterConfig{Name: "two", Version: "1.37"}
	c := config.ClusterConfig{Name: "one", Version: "1.36"}
	if clusterConfigHash(a) != clusterConfigHash(b) {
		t.Error("hash should not depend on the cluster name")
	}
	if clusterConfigHash(a) == clusterConfigHash(c) {
		t.Error("hash should change with cluster settings")
	}
}

func TestWarnClusterConfigDriftAdoptsLegacyHash(t *testing.T) {
	st := state.New("dev", false, false, 0, false, 0)
	cfg := &config.Config{Project: "app", Cluster: config.ClusterConfig{Name: "dev"}}
	warnClusterConfigDrift(st, cfg)
	if st.ClusterConfigHash != clusterConfigHash(cfg.Cluster) {
		t.Error("a state with no hash should adopt the current one")
	}
	if st.CreatedBy != "app" {
		t.Errorf("a state with no creator should adopt this project, got %q", st.CreatedBy)
	}
}

func TestCheckGPUCompatibilityUnknownCreatorIsExact(t *testing.T) {
	st := state.New("legacy", false, true, 0, false, 0) // older kraze: no CreatedBy
	cfg := &config.Config{Project: "app", Cluster: config.ClusterConfig{Name: "legacy"}}
	if err := checkGPUCompatibility(st, cfg); err == nil {
		t.Error("turning GPU off on a cluster with no recorded creator should require a recreate")
	}
}

func namespace(name string, managed bool) *corev1.Namespace {
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}
	if managed {
		ns.Labels = map[string]string{providers.ManagedByLabel: providers.ManagedByValue}
	}
	return ns
}

func TestNamespaceCleanupSelection(t *testing.T) {
	st := state.New("shared", false, false, 0, false, 0)
	st.SetProject("web")
	st.MarkServiceInstalledWithNamespace("other", "common", false) // web still uses common
	st.SetProject("billing")
	st.MarkServiceInstalledWithNamespace("api", "billing", false)        // label decides
	st.MarkServiceInstalledWithNamespace("worker", "flagged", true)      // state flag decides
	st.MarkServiceInstalledWithNamespace("legacy", "preexisting", false) // neither: keep
	st.MarkServiceInstalledWithNamespace("shared-api", "common", true)   // still in use: skip

	services := []*config.ServiceConfig{
		{Name: "api", Namespace: "billing"},
		{Name: "worker", Namespace: "flagged"},
		{Name: "legacy", Namespace: "preexisting"},
		{Name: "shared-api", Namespace: "common"},
		{Name: "unrecorded", Namespace: "labeled-only"}, // not in state; label decides
		{Name: "gone", Namespace: "deleted"},            // namespace no longer exists
	}

	clientset := fake.NewSimpleClientset(
		namespace("billing", true),
		namespace("flagged", false),
		namespace("preexisting", false),
		namespace("common", true),
		namespace("labeled-only", true),
	)

	candidates := collectNamespaceCandidates(st, services)
	toDelete, kept, skipped := selectNamespacesToDelete(context.Background(), clientset, candidates, false, func(string, ...interface{}) {})

	if want := []string{"billing", "flagged", "labeled-only"}; !slices.Equal(toDelete, want) {
		t.Errorf("toDelete = %v, want %v", toDelete, want)
	}
	if want := []string{"preexisting"}; !slices.Equal(keptNames(kept), want) {
		t.Errorf("kept = %v, want %v", kept, want)
	}
	if skipped != 1 {
		t.Errorf("skipped = %d, want 1 (common is still used by web)", skipped)
	}
}

func keptNames(kept []keptNamespace) []string {
	names := make([]string, 0, len(kept))
	for _, k := range kept {
		names = append(names, k.name)
	}
	return names
}

func TestNamespaceCleanupDeleteUnmanaged(t *testing.T) {
	unmanagedNamespaceSettle = 0
	t.Cleanup(func() { unmanagedNamespaceSettle = 20 * time.Second })

	st := state.New("shared", false, false, 0, false, 0)
	st.SetProject("web")
	st.MarkServiceInstalledWithNamespace("other", "shared-ns", false) // another project still uses it
	st.SetProject("billing")

	services := []*config.ServiceConfig{
		{Name: "api", Namespace: "legacy"},      // unlabeled, empty: deleted
		{Name: "worker", Namespace: "busy"},     // unlabeled, foreign pod left: kept
		{Name: "dns", Namespace: "kube-system"}, // system: kept
		{Name: "cache", Namespace: "shared-ns"}, // used by web: skipped
		{Name: "db", Namespace: "managed"},      // labeled: deleted as usual
	}
	clientset := fake.NewSimpleClientset(
		namespace("legacy", false),
		namespace("busy", false),
		namespace("kube-system", false),
		namespace("shared-ns", false),
		namespace("managed", true),
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "someone-elses", Namespace: "busy"}},
	)

	candidates := collectNamespaceCandidates(st, services)
	toDelete, kept, skipped := selectNamespacesToDelete(context.Background(), clientset, candidates, true, func(string, ...interface{}) {})

	if want := []string{"legacy", "managed"}; !slices.Equal(toDelete, want) {
		t.Errorf("toDelete = %v, want %v", toDelete, want)
	}
	if want := []string{"busy", "kube-system"}; !slices.Equal(keptNames(kept), want) {
		t.Errorf("kept = %v, want %v", kept, want)
	}
	if !strings.Contains(kept[0].reason, "1 pod(s)") {
		t.Errorf("busy should be kept because of its pod, got %q", kept[0].reason)
	}
	if skipped != 1 {
		t.Errorf("skipped = %d, want 1", skipped)
	}
}

func TestEnsureNamespaceLabelsWhatItCreates(t *testing.T) {
	ctx := context.Background()
	clientset := fake.NewSimpleClientset(namespace("existing", false))

	created, err := providers.EnsureNamespace(ctx, clientset, "fresh")
	if err != nil || !created {
		t.Fatalf("EnsureNamespace(fresh) = %v, %v", created, err)
	}
	if _, managed, _ := providers.NamespaceInfo(ctx, clientset, "fresh"); !managed {
		t.Error("a namespace kraze creates should carry the managed-by label")
	}

	created, err = providers.EnsureNamespace(ctx, clientset, "existing")
	if err != nil || created {
		t.Errorf("EnsureNamespace(existing) = %v, %v; want false, nil", created, err)
	}
	if _, managed, _ := providers.NamespaceInfo(ctx, clientset, "existing"); managed {
		t.Error("an existing namespace must not be labeled")
	}

	if created, _ := providers.EnsureNamespace(ctx, clientset, "default"); created {
		t.Error("system namespaces are never created")
	}
}

func TestNamespaceCleanupWaitsForUninstalledWorkloads(t *testing.T) {
	unmanagedNamespaceSettle = 10 * time.Second
	t.Cleanup(func() { unmanagedNamespaceSettle = 20 * time.Second })

	st := state.New("c", false, false, 0, false, 0)
	st.SetProject("billing")
	clientset := fake.NewSimpleClientset(
		namespace("legacy", false),
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "api-0", Namespace: "legacy"}},
	)

	// The just-uninstalled service's pod goes away while kraze waits
	go func() {
		time.Sleep(1500 * time.Millisecond)
		_ = clientset.CoreV1().Pods("legacy").Delete(context.Background(), "api-0", metav1.DeleteOptions{})
	}()

	var mu sync.Mutex
	var logged []string
	verbosef := func(format string, args ...interface{}) {
		mu.Lock()
		defer mu.Unlock()
		logged = append(logged, fmt.Sprintf(format, args...))
	}

	start := time.Now()
	candidates := collectNamespaceCandidates(st, []*config.ServiceConfig{{Name: "api", Namespace: "legacy"}})
	toDelete, kept, _ := selectNamespacesToDelete(context.Background(), clientset, candidates, true, verbosef)

	if !slices.Equal(toDelete, []string{"legacy"}) || len(kept) != 0 {
		t.Errorf("toDelete = %v, kept = %v; want legacy deleted once its pod is gone", toDelete, kept)
	}
	if elapsed := time.Since(start); elapsed >= unmanagedNamespaceSettle {
		t.Errorf("waited %v; should stop as soon as the workloads are gone", elapsed)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(logged) != 1 || !strings.Contains(logged[0], "Waiting up to") || !strings.Contains(logged[0], "1 pod(s)") {
		t.Errorf("expected one verbose waiting message naming the pod, got %v", logged)
	}
}

func TestWarnClusterConfigDrift(t *testing.T) {
	created := config.ClusterConfig{Name: "dev", Version: "v1.36.0"}
	changed := config.ClusterConfig{Name: "dev", Version: "v1.37.0"}

	tests := []struct {
		name      string
		createdBy string
		cluster   config.ClusterConfig
		want      string // "" means no warning
	}{
		{name: "same settings", createdBy: "billing", cluster: created},
		{name: "changed, creator recorded", createdBy: "billing", cluster: changed, want: "by project 'billing'"},
		{name: "changed, creator unknown", cluster: changed, want: "by a different configuration"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := state.New("dev", false, false, 0, false, 0)
			st.SetClusterConfig(clusterConfigHash(created), tt.createdBy)
			cfg := &config.Config{Project: "web", Cluster: tt.cluster}

			out := captureStdout(t, func() { warnClusterConfigDrift(st, cfg) })
			if tt.want == "" {
				if out != "" {
					t.Errorf("expected no warning, got %q", out)
				}
			} else if !strings.Contains(out, "cluster 'dev'") || !strings.Contains(out, tt.want) {
				t.Errorf("expected a warning containing %q, got %q", tt.want, out)
			}

			// The recorded settings describe the cluster as created; a later
			// config must never replace them
			if st.ClusterConfigHash != clusterConfigHash(created) || st.CreatedBy != tt.createdBy {
				t.Errorf("drift changed the recorded creation settings: %q by %q", st.ClusterConfigHash, st.CreatedBy)
			}
		})
	}
}

func TestPrintProjectsTable(t *testing.T) {
	st := projectServices("shared", false, map[string][]string{
		"web": {"frontend"}, "billing": {"db", "api"}, "": {"old"},
	})
	out := captureStdout(t, func() { printProjectsTable("shared", st) })

	// Sorted by project, then name; unowned entries are labelled
	rows := []string{"(no project)", "billing              api", "billing              db", "web                  frontend"}
	last := -1
	for _, row := range rows {
		i := strings.Index(out, row)
		if i < 0 {
			t.Fatalf("missing row %q in:\n%s", row, out)
		}
		if i < last {
			t.Errorf("row %q is out of order in:\n%s", row, out)
		}
		last = i
	}

	empty := captureStdout(t, func() { printProjectsTable("shared", nil) })
	if !strings.Contains(empty, "No services are recorded") {
		t.Errorf("a cluster with no state should say so, got %q", empty)
	}
}

func TestCheckServiceConflictsDetectsProjectRename(t *testing.T) {
	cfgPath := "/src/shop/kraze.yml"
	st := state.New("dev", false, false, 0, false, 0)
	st.SetProject("web")
	st.MarkServiceInstalledWithNamespace("frontend", "shop", true)
	st.SetConfigPaths([]string{cfgPath})
	st.SetProject("storefront")

	services := map[string]config.ServiceConfig{"frontend": {Name: "frontend", Namespace: "shop"}}
	err := checkServiceConflicts(st, services, "storefront", []string{cfgPath})
	if err == nil || !strings.Contains(err.Error(), "renamed to 'storefront'") || !strings.Contains(err.Error(), "set `project: web` again") {
		t.Errorf("expected a project-rename hint, got %v", err)
	}

	// A different config with the same service is a real clash, not a rename
	err = checkServiceConflicts(st, services, "storefront", []string{"/src/other/kraze.yml"})
	if err == nil || strings.Contains(err.Error(), "project: web") || !strings.Contains(err.Error(), "rename the service") {
		t.Errorf("expected the plain conflict hint, got %v", err)
	}
}
