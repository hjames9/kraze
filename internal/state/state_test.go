package state

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestNew(test *testing.T) {
	st := New("test-cluster", false)

	if st.ClusterName != "test-cluster" {
		test.Errorf("Expected cluster name 'test-cluster', got '%s'", st.ClusterName)
	}

	if st.Services == nil {
		test.Error("Expected Services map to be initialized")
	}

	if st.LastUpdated.IsZero() {
		test.Error("Expected LastUpdated to be set")
	}
}

func TestSaveAndLoad(test *testing.T) {
	tmpDir := test.TempDir()
	stateFile := filepath.Join(tmpDir, StateFileName)

	// Create and save state
	st := New("test-cluster", false)
	st.MarkServiceInstalled("redis")
	st.MarkServiceInstalled("postgres")

	if err := st.Save(stateFile); err != nil {
		test.Fatalf("Failed to save state: %v", err)
	}

	// Verify file exists
	if _, err := os.Stat(stateFile); os.IsNotExist(err) {
		test.Fatal("State file was not created")
	}

	// Load state
	loaded, err := Load(stateFile)
	if err != nil {
		test.Fatalf("Failed to load state: %v", err)
	}

	if loaded.ClusterName != "test-cluster" {
		test.Errorf("Expected cluster name 'test-cluster', got '%s'", loaded.ClusterName)
	}

	if len(loaded.Services) != 2 {
		test.Errorf("Expected 2 services, got %d", len(loaded.Services))
	}

	if !loaded.IsServiceInstalled("redis") {
		test.Error("Expected redis to be installed")
	}

	if !loaded.IsServiceInstalled("postgres") {
		test.Error("Expected postgres to be installed")
	}
}

func TestLoadNonexistent(test *testing.T) {
	tmpDir := test.TempDir()
	stateFile := filepath.Join(tmpDir, "nonexistent.state")

	loaded, err := Load(stateFile)
	if err != nil {
		test.Errorf("Expected no error for nonexistent file, got %v", err)
	}

	if loaded != nil {
		test.Error("Expected nil state for nonexistent file")
	}
}

func TestLoadInvalidJSON(test *testing.T) {
	tmpDir := test.TempDir()
	stateFile := filepath.Join(tmpDir, StateFileName)

	if err := os.WriteFile(stateFile, []byte("invalid json"), 0644); err != nil {
		test.Fatalf("Failed to write invalid state: %v", err)
	}

	_, err := Load(stateFile)
	if err == nil {
		test.Error("Expected error for invalid JSON, got nil")
	}
}

func TestDelete(test *testing.T) {
	tmpDir := test.TempDir()
	stateFile := filepath.Join(tmpDir, StateFileName)

	// Create state file
	st := New("test-cluster", false)
	if err := st.Save(stateFile); err != nil {
		test.Fatalf("Failed to save state: %v", err)
	}

	// Delete it
	if err := Delete(stateFile); err != nil {
		test.Fatalf("Failed to delete state: %v", err)
	}

	// Verify it's gone
	if _, err := os.Stat(stateFile); !os.IsNotExist(err) {
		test.Error("State file should not exist after delete")
	}

	// Deleting nonexistent file should not error
	if err := Delete(stateFile); err != nil {
		test.Errorf("Expected no error deleting nonexistent file, got %v", err)
	}
}

func TestMarkServiceInstalled(test *testing.T) {
	st := New("test-cluster", false)

	st.MarkServiceInstalled("redis")

	if !st.IsServiceInstalled("redis") {
		test.Error("Expected redis to be installed")
	}

	svc, ok := st.Services["redis"]
	if !ok {
		test.Fatal("Expected redis in services map")
	}

	if svc.Name != "redis" {
		test.Errorf("Expected service name 'redis', got '%s'", svc.Name)
	}

	if !svc.Installed {
		test.Error("Expected service to be marked as installed")
	}

	if svc.UpdatedAt.IsZero() {
		test.Error("Expected UpdatedAt to be set")
	}
}

func TestMarkServiceUninstalled(test *testing.T) {
	st := New("test-cluster", false)

	st.MarkServiceInstalled("redis")
	if !st.IsServiceInstalled("redis") {
		test.Error("Expected redis to be installed")
	}

	st.MarkServiceUninstalled("redis")
	if st.IsServiceInstalled("redis") {
		test.Error("Expected redis to be uninstalled")
	}

	if _, ok := st.Services["redis"]; ok {
		test.Error("Expected redis to be removed from services map")
	}
}

func TestIsServiceInstalled(test *testing.T) {
	st := New("test-cluster", false)

	if st.IsServiceInstalled("redis") {
		test.Error("Expected redis to not be installed initially")
	}

	st.MarkServiceInstalled("redis")

	if !st.IsServiceInstalled("redis") {
		test.Error("Expected redis to be installed")
	}
}

func TestGetInstalledServices(test *testing.T) {
	st := New("test-cluster", false)

	st.MarkServiceInstalled("redis")
	st.MarkServiceInstalled("postgres")
	st.MarkServiceInstalled("api")

	installed := st.GetInstalledServices()

	if len(installed) != 3 {
		test.Errorf("Expected 3 installed services, got %d", len(installed))
	}

	// Verify all services are present (order doesn't matter)
	found := make(map[string]bool)
	for _, name := range installed {
		found[name] = true
	}

	for _, expected := range []string{"redis", "postgres", "api"} {
		if !found[expected] {
			test.Errorf("Expected '%s' in installed services", expected)
		}
	}
}

func TestGetNamespacesForServices(test *testing.T) {
	st := New("test-cluster", false)

	// Setup: Install 3 services in 2 namespaces
	st.MarkServiceInstalledWithNamespace("argocd", "argocd", true)
	st.MarkServiceInstalledWithNamespace("redis", "data", false)
	st.MarkServiceInstalledWithNamespace("postgres", "data", true)

	// Test 1: Uninstalling argocd (only service in argocd namespace)
	// Should return argocd namespace with count 0 (no other services using it)
	namespaces := st.GetNamespacesForServices([]string{"argocd"})
	if len(namespaces) != 1 {
		test.Errorf("Expected 1 namespace, got %d", len(namespaces))
	}
	if count, exists := namespaces["argocd"]; !exists || count != 0 {
		test.Errorf("Expected argocd namespace with count 0, got count %d, exists %v", count, exists)
	}

	// Test 2: Uninstalling redis (one of two services in data namespace)
	// Should return data namespace with count 1 (postgres still using it)
	namespaces = st.GetNamespacesForServices([]string{"redis"})
	if len(namespaces) != 1 {
		test.Errorf("Expected 1 namespace, got %d", len(namespaces))
	}
	if count, exists := namespaces["data"]; !exists || count != 1 {
		test.Errorf("Expected data namespace with count 1, got count %d, exists %v", count, exists)
	}

	// Test 3: Uninstalling both redis and postgres (all services in data namespace)
	// Should return data namespace with count 0 (no other services using it)
	namespaces = st.GetNamespacesForServices([]string{"redis", "postgres"})
	if len(namespaces) != 1 {
		test.Errorf("Expected 1 namespace, got %d", len(namespaces))
	}
	if count, exists := namespaces["data"]; !exists || count != 0 {
		test.Errorf("Expected data namespace with count 0, got count %d, exists %v", count, exists)
	}

	// Test 4: Uninstalling all three services
	// Should return both namespaces with count 0
	namespaces = st.GetNamespacesForServices([]string{"argocd", "redis", "postgres"})
	if len(namespaces) != 2 {
		test.Errorf("Expected 2 namespaces, got %d", len(namespaces))
	}
	if count, exists := namespaces["argocd"]; !exists || count != 0 {
		test.Errorf("Expected argocd namespace with count 0, got count %d, exists %v", count, exists)
	}
	if count, exists := namespaces["data"]; !exists || count != 0 {
		test.Errorf("Expected data namespace with count 0, got count %d, exists %v", count, exists)
	}
}

func TestGetAllNamespacesUsedForCleanup(test *testing.T) {
	st := New("test-cluster", false)

	// Setup: Install 3 services in 2 namespaces
	// This simulates the state BEFORE we start uninstalling
	st.MarkServiceInstalledWithNamespace("argocd", "argocd", true)
	st.MarkServiceInstalledWithNamespace("metallb", "metallb-system", true)
	st.MarkServiceInstalledWithNamespace("metallb-l2", "metallb-system", false)

	// Test: GetAllNamespacesUsedForCleanup should return ALL namespaces with count 0
	// This is used when uninstalling all services (called BEFORE actual uninstall)
	namespaces := st.GetAllNamespacesUsedForCleanup()

	// Should include both namespaces with count 0
	// (count is 0 because we're going to uninstall everything)
	if len(namespaces) != 2 {
		test.Errorf("Expected 2 namespaces, got %d", len(namespaces))
	}

	if count, exists := namespaces["argocd"]; !exists || count != 0 {
		test.Errorf("Expected argocd namespace with count 0, got count %d, exists %v", count, exists)
	}

	if count, exists := namespaces["metallb-system"]; !exists || count != 0 {
		test.Errorf("Expected metallb-system namespace with count 0, got count %d, exists %v", count, exists)
	}
}

func TestGetAllNamespacesUsed(test *testing.T) {
	st := New("test-cluster", false)

	// Setup: Install 3 services in 2 namespaces, then uninstall one
	st.MarkServiceInstalledWithNamespace("argocd", "argocd", true)
	st.MarkServiceInstalledWithNamespace("metallb", "metallb-system", true)
	st.MarkServiceInstalledWithNamespace("metallb-l2", "metallb-system", false)

	// Uninstall argocd
	st.MarkServiceUninstalled("argocd")

	// Test: GetAllNamespacesUsed should only return namespaces with INSTALLED services
	namespaces := st.GetAllNamespacesUsed()

	// Should only include metallb-system (2 installed services)
	// argocd namespace should NOT be included (argocd is uninstalled)
	if len(namespaces) != 1 {
		test.Errorf("Expected 1 namespace, got %d", len(namespaces))
	}

	if count, exists := namespaces["metallb-system"]; !exists || count != 2 {
		test.Errorf("Expected metallb-system namespace with count 2, got count %d, exists %v", count, exists)
	}

	if _, exists := namespaces["argocd"]; exists {
		test.Errorf("Did not expect argocd namespace (service is uninstalled)")
	}
}

func TestGetStateFilePath(test *testing.T) {
	path := GetStateFilePath("/test/dir")
	expected := filepath.Join("/test/dir", StateFileName)

	if path != expected {
		test.Errorf("Expected '%s', got '%s'", expected, path)
	}
}

func TestSaveUpdatesTimestamp(test *testing.T) {
	tmpDir := test.TempDir()
	stateFile := filepath.Join(tmpDir, StateFileName)

	st := New("test-cluster", false)
	originalTime := st.LastUpdated

	// Wait a bit to ensure timestamp difference
	time.Sleep(10 * time.Millisecond)

	if err := st.Save(stateFile); err != nil {
		test.Fatalf("Failed to save state: %v", err)
	}

	if !st.LastUpdated.After(originalTime) {
		test.Error("Expected LastUpdated to be updated on save")
	}
}

func TestStateVersioning(test *testing.T) {
	// New state should have current version
	st := New("test-cluster", false)
	if st.Version != CurrentStateVersion {
		test.Errorf("Expected version %d, got %d", CurrentStateVersion, st.Version)
	}

	// Saved state should have current version
	tmpDir := test.TempDir()
	stateFile := filepath.Join(tmpDir, StateFileName)

	if err := st.Save(stateFile); err != nil {
		test.Fatalf("Failed to save state: %v", err)
	}

	loaded, err := Load(stateFile)
	if err != nil {
		test.Fatalf("Failed to load state: %v", err)
	}

	if loaded.Version != CurrentStateVersion {
		test.Errorf("Expected loaded version %d, got %d", CurrentStateVersion, loaded.Version)
	}
}

func TestMigrateFromV0(test *testing.T) {
	tmpDir := test.TempDir()
	stateFile := filepath.Join(tmpDir, StateFileName)

	// Create a v0 state file (no version field)
	v0State := `{
  "cluster_name": "test-cluster",
  "is_external": false,
  "services": {
    "redis": {
      "name": "redis",
      "installed": true,
      "updated_at": "2024-01-01T00:00:00Z",
      "namespace": "data",
      "created_namespace": true
    }
  },
  "last_updated": "2024-01-01T00:00:00Z"
}`

	if err := os.WriteFile(stateFile, []byte(v0State), 0644); err != nil {
		test.Fatalf("Failed to write v0 state: %v", err)
	}

	// Load and verify it migrates to v1
	loaded, err := Load(stateFile)
	if err != nil {
		test.Fatalf("Failed to load v0 state: %v", err)
	}

	if loaded.Version != 1 {
		test.Errorf("Expected migrated version to be 1, got %d", loaded.Version)
	}

	if loaded.ClusterName != "test-cluster" {
		test.Errorf("Expected cluster name 'test-cluster', got '%s'", loaded.ClusterName)
	}

	if !loaded.IsServiceInstalled("redis") {
		test.Error("Expected redis to be installed after migration")
	}

	// Save and verify version is persisted
	if err := loaded.Save(stateFile); err != nil {
		test.Fatalf("Failed to save migrated state: %v", err)
	}

	reloaded, err := Load(stateFile)
	if err != nil {
		test.Fatalf("Failed to reload state: %v", err)
	}

	if reloaded.Version != CurrentStateVersion {
		test.Errorf("Expected reloaded version %d, got %d", CurrentStateVersion, reloaded.Version)
	}
}

func TestLoadNewerVersion(test *testing.T) {
	tmpDir := test.TempDir()
	stateFile := filepath.Join(tmpDir, StateFileName)

	// Create a state file with a future version
	futureState := `{
  "version": 999,
  "cluster_name": "test-cluster",
  "is_external": false,
  "services": {},
  "last_updated": "2024-01-01T00:00:00Z"
}`

	if err := os.WriteFile(stateFile, []byte(futureState), 0644); err != nil {
		test.Fatalf("Failed to write future state: %v", err)
	}

	// Load should fail with helpful error
	_, err := Load(stateFile)
	if err == nil {
		test.Error("Expected error loading newer version, got nil")
	}

	if err != nil && err.Error() != "failed to migrate state file: state file version 999 is newer than supported version 1 - please upgrade kraze" {
		test.Errorf("Expected upgrade error message, got: %v", err)
	}
}
