package state

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestNew(test *testing.T) {
	st := New("test-cluster")

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
	st := New("test-cluster")
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
	st := New("test-cluster")
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
	st := New("test-cluster")

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
	st := New("test-cluster")

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
	st := New("test-cluster")

	if st.IsServiceInstalled("redis") {
		test.Error("Expected redis to not be installed initially")
	}

	st.MarkServiceInstalled("redis")

	if !st.IsServiceInstalled("redis") {
		test.Error("Expected redis to be installed")
	}
}

func TestGetInstalledServices(test *testing.T) {
	st := New("test-cluster")

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

	st := New("test-cluster")
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
