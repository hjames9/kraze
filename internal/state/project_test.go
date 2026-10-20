package state

import (
	"context"
	"encoding/json"
	"slices"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

// loadAs loads the saved state acting as project
func loadAs(t *testing.T, clientset *fake.Clientset, project string) *ClusterState {
	t.Helper()
	st, err := Load(context.Background(), clientset, "test-cluster")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if st == nil {
		t.Fatal("Load returned no state")
	}
	st.SetProject(project)
	return st
}

func mustSave(t *testing.T, st *ClusterState, clientset *fake.Clientset) {
	t.Helper()
	if err := st.Save(context.Background(), clientset); err != nil {
		t.Fatalf("Save: %v", err)
	}
}

func TestProjectsScopeServiceKeys(t *testing.T) {
	clientset := fake.NewSimpleClientset()

	billing := New("test-cluster", false, false, 0, false, 0)
	billing.SetProject("billing")
	billing.MarkServiceInstalledWithNamespace("postgres", "billing", true)
	mustSave(t, billing, clientset)

	web := loadAs(t, clientset, "web")
	if web.IsServiceInstalled("postgres") {
		t.Error("web should not see billing's postgres as its own")
	}
	web.MarkServiceInstalledWithNamespace("postgres", "web", true)
	mustSave(t, web, clientset)

	loaded := loadAs(t, clientset, "billing")
	if len(loaded.Services) != 2 {
		t.Fatalf("expected both projects' postgres, got keys %v", keys(loaded.Services))
	}
	if svc, _ := loaded.GetService("postgres"); svc.Namespace != "billing" || svc.Project != "billing" {
		t.Errorf("billing's postgres = %+v", svc)
	}
	if _, ok := loaded.Services["web/postgres"]; !ok {
		t.Errorf("expected key web/postgres, got %v", keys(loaded.Services))
	}

	loaded.MarkServiceUninstalled("postgres")
	mustSave(t, loaded, clientset)
	if after := loadAs(t, clientset, "web"); !after.IsServiceInstalled("postgres") {
		t.Error("uninstalling billing's postgres removed web's")
	}
}

func TestAnonymousProjectUsesBareKeys(t *testing.T) {
	st := New("test-cluster", false, false, 0, false, 0)
	st.MarkServiceInstalledWithNamespace("redis", "data", true)
	if _, ok := st.Services["redis"]; !ok {
		t.Errorf("anonymous project should key by bare name, got %v", keys(st.Services))
	}
}

func TestClaim(t *testing.T) {
	st := New("test-cluster", false, false, 0, false, 0)
	st.MarkServiceInstalledWithNamespace("redis", "data", true) // anonymous / legacy
	st.SetImageHashes("redis", map[string]string{"redis:7": "sha256:abc"})

	st.SetProject("cache")
	if !st.Claim("redis") {
		t.Fatal("expected redis to be claimed")
	}
	if _, ok := st.Services["redis"]; ok {
		t.Error("bare key should be gone after claim")
	}
	svc, ok := st.GetService("redis")
	if !ok || svc.Project != "cache" || !svc.CreatedNamespace || svc.ImageHashes["redis:7"] != "sha256:abc" {
		t.Errorf("claimed entry = %+v, %v", svc, ok)
	}

	if st.Claim("redis") {
		t.Error("claiming twice should do nothing")
	}
	if st.Claim("missing") {
		t.Error("claiming a service with no entry should do nothing")
	}

	st.SetProject("")
	st.MarkServiceInstalledWithNamespace("other", "data", false)
	if st.Claim("other") {
		t.Error("the anonymous project never claims")
	}
}

func TestClaimPersists(t *testing.T) {
	clientset := fake.NewSimpleClientset()
	legacy := New("test-cluster", false, false, 0, false, 0)
	legacy.MarkServiceInstalledWithNamespace("redis", "data", true)
	mustSave(t, legacy, clientset)

	st := loadAs(t, clientset, "cache")
	st.Claim("redis")
	mustSave(t, st, clientset)

	loaded := loadAs(t, clientset, "cache")
	if _, ok := loaded.Services["redis"]; ok {
		t.Error("bare key survived the claim after save")
	}
	if !loaded.IsServiceInstalled("redis") {
		t.Error("claimed entry missing after save")
	}
}

func TestOtherProjectsAndUnowned(t *testing.T) {
	st := New("test-cluster", false, false, 0, false, 0)
	st.MarkServiceInstalledWithNamespace("legacy", "default", false)
	st.SetProject("web")
	st.MarkServiceInstalledWithNamespace("frontend", "web", true)
	st.SetProject("billing")
	st.SetConfigPaths([]string{"/src/billing/kraze.yml"})

	if got := st.OtherProjects(); !slices.Equal(got, []string{"web"}) {
		t.Errorf("OtherProjects as billing = %v, want [web]", got)
	}
	st.SetProject("")
	if got := st.OtherProjects(); !slices.Equal(got, []string{"web"}) {
		t.Errorf("OtherProjects as anonymous = %v, want [web] (billing has config paths but no services)", got)
	}
	if got := st.UnownedServices(); !slices.Equal(got, []string{"legacy"}) {
		t.Errorf("UnownedServices = %v, want [legacy]", got)
	}
}

func TestConflictingOwner(t *testing.T) {
	st := New("test-cluster", false, false, 0, false, 0)
	st.MarkServiceInstalledWithNamespace("cache", "shared", false) // unowned
	st.SetProject("billing")
	st.MarkServiceInstalledWithNamespace("postgres", "shared", false)

	st.SetProject("web")
	if owner, ok := st.ConflictingOwner("postgres", "shared"); !ok || owner != "billing" {
		t.Errorf("same name, same namespace: got %q, %v; want billing, true", owner, ok)
	}
	if _, ok := st.ConflictingOwner("postgres", "web"); ok {
		t.Error("same name in a different namespace is not a conflict")
	}
	if _, ok := st.ConflictingOwner("cache", "shared"); ok {
		t.Error("an unowned entry is claimable by a named project, not a conflict")
	}

	st.SetProject("billing")
	if _, ok := st.ConflictingOwner("postgres", "shared"); ok {
		t.Error("a project never conflicts with itself")
	}

	st.SetProject("")
	if owner, ok := st.ConflictingOwner("postgres", "shared"); !ok || owner != "billing" {
		t.Errorf("anonymous vs billing: got %q, %v", owner, ok)
	}
}

func TestNamespaceCountsSpanProjects(t *testing.T) {
	st := New("test-cluster", false, false, 0, false, 0)
	st.SetProject("billing")
	st.MarkServiceInstalledWithNamespace("api", "shared", true)
	st.SetProject("web")
	st.MarkServiceInstalledWithNamespace("frontend", "shared", false)

	st.SetProject("billing")
	counts := st.GetNamespacesForServices([]string{"api"})
	if counts["shared"] != 1 {
		t.Errorf("shared namespace should still be used by web's frontend, got count %d", counts["shared"])
	}
	if got := st.CountOtherServicesInNamespace("shared", []string{"api"}); got != 1 {
		t.Errorf("CountOtherServicesInNamespace = %d, want 1", got)
	}
	if !st.NamespaceCreatedByKraze("shared") {
		t.Error("billing's entry records that kraze created 'shared'")
	}
}

func TestCreatedNamespaceIsSticky(t *testing.T) {
	st := New("test-cluster", false, false, 0, false, 0)
	st.MarkServiceInstalledWithNamespace("backend", "app", true)
	// An upgrade: the namespace exists now, so up reports it wasn't created
	st.MarkServiceInstalledWithNamespace("backend", "app", false)
	if svc, _ := st.GetService("backend"); !svc.CreatedNamespace {
		t.Error("CreatedNamespace was reset by an upgrade")
	}

	// Moving the service to a different namespace does not carry the flag over
	st.MarkServiceInstalledWithNamespace("backend", "other", false)
	if svc, _ := st.GetService("backend"); svc.CreatedNamespace {
		t.Error("CreatedNamespace carried over to a different namespace")
	}
}

func TestRemoveProject(t *testing.T) {
	clientset := fake.NewSimpleClientset()
	st := New("test-cluster", false, false, 0, false, 0)
	st.SetProject("billing")
	st.MarkServiceInstalledWithNamespace("api", "billing", true)
	st.SetConfigPaths([]string{"/src/billing/kraze.yml"})
	st.SetProject("web")
	st.MarkServiceInstalledWithNamespace("frontend", "web", true)
	mustSave(t, st, clientset)

	billing := loadAs(t, clientset, "billing")
	billing.RemoveProject()
	mustSave(t, billing, clientset)

	loaded := loadAs(t, clientset, "web")
	if got := keys(loaded.Services); !slices.Equal(got, []string{"web/frontend"}) {
		t.Errorf("services after RemoveProject = %v", got)
	}
	if _, ok := loaded.Projects["billing"]; ok {
		t.Error("billing's project metadata survived RemoveProject")
	}
}

func TestConfigPathsArePerProject(t *testing.T) {
	st := New("test-cluster", false, false, 0, false, 0)
	st.SetProject("billing")
	st.SetConfigPaths([]string{"/src/billing/kraze.yml"})
	st.SetProject("web")
	st.SetConfigPaths([]string{"/src/web/kraze.yml", "/src/web/extra.yml"})

	if got := st.GetConfigPaths(); len(got) != 2 {
		t.Errorf("web's paths = %v", got)
	}
	all := st.AllProjectConfigPaths()
	if len(all) != 2 || all["billing"][0] != "/src/billing/kraze.yml" {
		t.Errorf("AllProjectConfigPaths = %v", all)
	}
}

func TestSetConfigPathsMovesFilesBetweenProjects(t *testing.T) {
	st := New("test-cluster", false, false, 0, false, 0)
	st.SetConfigPaths([]string{"/src/app/kraze.yml"}) // anonymous, before `project:` was added
	st.SetProject("other")
	st.SetConfigPaths([]string{"/src/other/kraze.yml", "/src/shared.yml"})

	st.SetProject("app")
	st.SetConfigPaths([]string{"/src/app/kraze.yml", "/src/shared.yml"})

	all := st.AllProjectConfigPaths()
	if _, ok := all[""]; ok {
		t.Errorf("anonymous entry should be gone once its only file moved to app: %v", all)
	}
	if !slices.Equal(all["other"], []string{"/src/other/kraze.yml"}) {
		t.Errorf("other's paths = %v, want only its own file", all["other"])
	}
}

func TestMigrationV3ToV4(t *testing.T) {
	ctx := context.Background()
	clientset := fake.NewSimpleClientset()

	v3State := map[string]interface{}{
		"version":      3,
		"cluster_name": "test-cluster",
		"config_paths": []string{"/src/app/kraze.yml"},
		"services": map[string]interface{}{
			"redis": map[string]interface{}{"name": "redis", "installed": true, "namespace": "data", "created_namespace": true},
		},
		"last_updated": time.Now().Format(time.RFC3339),
	}
	createRawState(t, clientset, v3State)

	loaded, err := Load(ctx, clientset, "test-cluster")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.Version != CurrentStateVersion {
		t.Errorf("version = %d, want %d", loaded.Version, CurrentStateVersion)
	}
	if loaded.ConfigPaths != nil {
		t.Error("legacy config_paths should be cleared after migration")
	}
	if got := loaded.GetConfigPaths(); !slices.Equal(got, []string{"/src/app/kraze.yml"}) {
		t.Errorf("anonymous project's paths = %v", got)
	}
	if !loaded.IsServiceInstalled("redis") {
		t.Error("legacy service should belong to the anonymous project")
	}

	// Saving drops the legacy field
	mustSave(t, loaded, clientset)
	cm, _ := clientset.CoreV1().ConfigMaps(ConfigMapNamespace).Get(ctx, ConfigMapName, metav1.GetOptions{})
	var raw map[string]interface{}
	if err := json.Unmarshal([]byte(cm.Data[ConfigMapDataKey]), &raw); err != nil {
		t.Fatal(err)
	}
	if _, ok := raw["config_paths"]; ok {
		t.Error("saved state still has top-level config_paths")
	}
}

func TestSaveMergesConcurrentWriters(t *testing.T) {
	clientset := fake.NewSimpleClientset()
	base := New("test-cluster", false, false, 0, false, 0)
	base.SetProject("billing")
	base.MarkServiceInstalledWithNamespace("old", "billing", true)
	mustSave(t, base, clientset)

	// Two runs load the same state, then each changes something different
	billing := loadAs(t, clientset, "billing")
	web := loadAs(t, clientset, "web")

	billing.MarkServiceInstalledWithNamespace("api", "billing", true)
	billing.MarkServiceUninstalled("old")
	web.MarkServiceInstalledWithNamespace("frontend", "web", true)
	web.SetConfigPaths([]string{"/src/web/kraze.yml"})

	mustSave(t, billing, clientset)
	mustSave(t, web, clientset) // must not resurrect "old" or drop "api"

	loaded := loadAs(t, clientset, "")
	if got := keys(loaded.Services); !slices.Equal(got, []string{"billing/api", "web/frontend"}) {
		t.Errorf("merged services = %v, want [billing/api web/frontend]", got)
	}
	if web.Services["billing/api"].Name != "api" {
		t.Error("after Save, the in-memory state should include other runs' changes")
	}
}

func TestSaveRetriesOnConflict(t *testing.T) {
	clientset := fake.NewSimpleClientset()
	st := New("test-cluster", false, false, 0, false, 0)
	mustSave(t, st, clientset)

	conflicts := 0
	clientset.PrependReactor("update", "configmaps", func(k8stesting.Action) (bool, runtime.Object, error) {
		if conflicts == 0 {
			conflicts++
			return true, nil, apierrors.NewConflict(schema.GroupResource{Resource: "configmaps"}, ConfigMapName, nil)
		}
		return false, nil, nil
	})

	st.MarkServiceInstalledWithNamespace("redis", "data", true)
	mustSave(t, st, clientset)
	if conflicts != 1 {
		t.Fatalf("reactor saw %d conflicts", conflicts)
	}
	if !loadAs(t, clientset, "").IsServiceInstalled("redis") {
		t.Error("change lost after a conflict retry")
	}
}

func TestNewStateDoesNotOverrideExistingClusterFields(t *testing.T) {
	clientset := fake.NewSimpleClientset()
	creator := New("test-cluster", false, true, 0, false, 0)
	creator.SetClusterConfig("hash-a", "billing")
	mustSave(t, creator, clientset)

	// A run that found no state (before the creator saved) and made its own
	late := New("test-cluster", false, false, 0, false, 0)
	late.SetProject("web")
	late.MarkServiceInstalledWithNamespace("frontend", "web", true)
	mustSave(t, late, clientset)

	loaded := loadAs(t, clientset, "web")
	if !loaded.NvidiaGPUEnabled || loaded.ClusterConfigHash != "hash-a" || loaded.CreatedBy != "billing" {
		t.Errorf("cluster fields overridden: gpu=%v hash=%q createdBy=%q",
			loaded.NvidiaGPUEnabled, loaded.ClusterConfigHash, loaded.CreatedBy)
	}
	if !loaded.IsServiceInstalled("frontend") {
		t.Error("late run's service missing")
	}
}

func createRawState(t *testing.T, clientset *fake.Clientset, raw map[string]interface{}) {
	t.Helper()
	data, err := json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	_, err = clientset.CoreV1().ConfigMaps(ConfigMapNamespace).Create(context.Background(), &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: ConfigMapName, Namespace: ConfigMapNamespace},
		Data:       map[string]string{ConfigMapDataKey: string(data)},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
}

func keys(m map[string]ServiceMetadata) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

func TestOrphans(t *testing.T) {
	st := New("test-cluster", false, false, 0, false, 0)
	st.MarkServiceInstalledWithNamespace("legacy", "default", false) // unowned
	st.SetProject("web")
	st.MarkServiceInstalledWithNamespace("frontend", "web", false)
	st.SetProject("billing")
	st.MarkServiceInstalledWithNamespace("api", "billing", false)
	st.MarkServiceInstalledWithNamespace("worker", "billing", false)
	st.MarkServiceInstalledWithNamespace("cron", "billing", false)

	var names []string
	for _, svc := range st.Orphans(map[string]bool{"api": true}) {
		names = append(names, svc.Name)
	}
	if want := []string{"cron", "worker"}; !slices.Equal(names, want) {
		t.Errorf("Orphans = %v, want %v (only billing's, never unowned or other projects')", names, want)
	}
}

func TestServiceSourceSurvivesSaveAndUpgrade(t *testing.T) {
	clientset := fake.NewSimpleClientset()
	st := New("test-cluster", false, false, 0, false, 0)
	st.SetProject("web")
	st.MarkServiceInstalledWithNamespace("site", "web", true)
	refs := []ResourceRef{{APIVersion: "v1", Kind: "ConfigMap", Namespace: "web", Name: "site"},
		{APIVersion: "batch/v1", Kind: "Job", Namespace: "web", Name: "init", Completed: true}}
	st.SetServiceSource("site", "manifests", refs)
	mustSave(t, st, clientset)

	loaded := loadAs(t, clientset, "web")
	loaded.MarkServiceInstalledWithNamespace("site", "web", false) // an upgrade
	svc, _ := loaded.GetService("site")
	if svc.Type != "manifests" || !slices.Equal(svc.Resources, refs) {
		t.Errorf("source lost: type=%q resources=%v", svc.Type, svc.Resources)
	}

	loaded.SetServiceSource("missing", "helm", nil) // not installed: no entry is created
	if _, ok := loaded.GetService("missing"); ok {
		t.Error("SetServiceSource must not create an entry")
	}
}
