package state

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestNew(t *testing.T) {
	cs := New("test-cluster", false)

	if cs.ClusterName != "test-cluster" {
		t.Errorf("Expected cluster name 'test-cluster', got '%s'", cs.ClusterName)
	}

	if cs.IsExternal {
		t.Error("Expected IsExternal to be false")
	}

	if cs.Services == nil {
		t.Error("Expected Services map to be initialized")
	}

	if cs.LastUpdated.IsZero() {
		t.Error("Expected LastUpdated to be set")
	}

	if cs.Version != CurrentStateVersion {
		t.Errorf("Expected version %d, got %d", CurrentStateVersion, cs.Version)
	}
}

func TestSaveAndLoad(t *testing.T) {
	ctx := context.Background()
	clientset := fake.NewSimpleClientset()

	// Create and save state
	cs := New("test-cluster", false)
	cs.MarkServiceInstalled("redis")
	cs.MarkServiceInstalled("postgres")

	if err := cs.Save(ctx, clientset); err != nil {
		t.Fatalf("Failed to save state: %v", err)
	}

	// Verify ConfigMap exists
	cm, err := clientset.CoreV1().ConfigMaps(ConfigMapNamespace).Get(ctx, ConfigMapName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("ConfigMap was not created: %v", err)
	}

	// Verify ConfigMap has correct labels
	if cm.Labels["app.kubernetes.io/managed-by"] != "kraze" {
		t.Error("Expected ConfigMap to have managed-by label")
	}

	// Load state
	loaded, err := Load(ctx, clientset, "test-cluster")
	if err != nil {
		t.Fatalf("Failed to load state: %v", err)
	}

	if loaded.ClusterName != "test-cluster" {
		t.Errorf("Expected cluster name 'test-cluster', got '%s'", loaded.ClusterName)
	}

	if len(loaded.Services) != 2 {
		t.Errorf("Expected 2 services, got %d", len(loaded.Services))
	}

	if !loaded.IsServiceInstalled("redis") {
		t.Error("Expected redis to be installed")
	}

	if !loaded.IsServiceInstalled("postgres") {
		t.Error("Expected postgres to be installed")
	}
}

func TestLoadNonexistent(t *testing.T) {
	ctx := context.Background()
	clientset := fake.NewSimpleClientset()

	// Load when ConfigMap doesn't exist
	loaded, err := Load(ctx, clientset, "test-cluster")
	if err != nil {
		t.Errorf("Expected no error for nonexistent ConfigMap, got %v", err)
	}

	if loaded != nil {
		t.Error("Expected nil state for nonexistent ConfigMap")
	}
}

func TestLoadInvalidJSON(t *testing.T) {
	ctx := context.Background()
	clientset := fake.NewSimpleClientset()

	// Create ConfigMap with invalid JSON
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      ConfigMapName,
			Namespace: ConfigMapNamespace,
		},
		Data: map[string]string{
			ConfigMapDataKey: "invalid json",
		},
	}
	_, err := clientset.CoreV1().ConfigMaps(ConfigMapNamespace).Create(ctx, cm, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("Failed to create ConfigMap: %v", err)
	}

	_, err = Load(ctx, clientset, "test-cluster")
	if err == nil {
		t.Error("Expected error for invalid JSON, got nil")
	}
}

func TestDelete(t *testing.T) {
	ctx := context.Background()
	clientset := fake.NewSimpleClientset()

	// Create and save state
	cs := New("test-cluster", false)
	if err := cs.Save(ctx, clientset); err != nil {
		t.Fatalf("Failed to save state: %v", err)
	}

	// Verify ConfigMap exists
	_, err := clientset.CoreV1().ConfigMaps(ConfigMapNamespace).Get(ctx, ConfigMapName, metav1.GetOptions{})
	if err != nil {
		t.Fatal("ConfigMap should exist before delete")
	}

	// Delete state
	if err := Delete(ctx, clientset); err != nil {
		t.Fatalf("Failed to delete state: %v", err)
	}

	// Verify ConfigMap is gone
	_, err = clientset.CoreV1().ConfigMaps(ConfigMapNamespace).Get(ctx, ConfigMapName, metav1.GetOptions{})
	if err == nil {
		t.Error("ConfigMap should be deleted")
	}
}

func TestDeleteNonexistent(t *testing.T) {
	ctx := context.Background()
	clientset := fake.NewSimpleClientset()

	// Delete when ConfigMap doesn't exist (should not error)
	if err := Delete(ctx, clientset); err != nil {
		t.Errorf("Expected no error for deleting nonexistent ConfigMap, got %v", err)
	}
}

func TestMarkServiceInstalled(t *testing.T) {
	cs := New("test-cluster", false)

	cs.MarkServiceInstalled("backend")

	if !cs.IsServiceInstalled("backend") {
		t.Error("Expected backend to be installed")
	}

	svc, exists := cs.Services["backend"]
	if !exists {
		t.Fatal("Expected backend service to exist")
	}

	if svc.Name != "backend" {
		t.Errorf("Expected service name 'backend', got '%s'", svc.Name)
	}

	if !svc.Installed {
		t.Error("Expected Installed to be true")
	}

	if svc.UpdatedAt.IsZero() {
		t.Error("Expected UpdatedAt to be set")
	}
}

func TestMarkServiceInstalledWithNamespace(t *testing.T) {
	cs := New("test-cluster", false)

	cs.MarkServiceInstalledWithNamespace("backend", "default", true)

	svc, exists := cs.Services["backend"]
	if !exists {
		t.Fatal("Expected backend service to exist")
	}

	if svc.Namespace != "default" {
		t.Errorf("Expected namespace 'default', got '%s'", svc.Namespace)
	}

	if !svc.CreatedNamespace {
		t.Error("Expected CreatedNamespace to be true")
	}
}

func TestMarkServiceInstalledWithNamespacePreservesImageHashes(t *testing.T) {
	cs := New("test-cluster", false)

	// First install with image hashes
	imageHashes := map[string]string{
		"myapp:latest": "sha256:abc123",
	}
	cs.MarkServiceInstalledWithImages("backend", "default", true, imageHashes)

	// Update without image hashes (should preserve)
	cs.MarkServiceInstalledWithNamespace("backend", "default", false)

	svc := cs.Services["backend"]
	if len(svc.ImageHashes) != 1 {
		t.Errorf("Expected image hashes to be preserved, got %d hashes", len(svc.ImageHashes))
	}

	if svc.ImageHashes["myapp:latest"] != "sha256:abc123" {
		t.Error("Expected image hash to be preserved")
	}
}

func TestMarkServiceInstalledWithImages(t *testing.T) {
	cs := New("test-cluster", false)

	imageHashes := map[string]string{
		"myapp:latest":   "sha256:abc123",
		"postgres:15":    "sha256:def456",
	}

	cs.MarkServiceInstalledWithImages("backend", "default", true, imageHashes)

	svc, exists := cs.Services["backend"]
	if !exists {
		t.Fatal("Expected backend service to exist")
	}

	if len(svc.ImageHashes) != 2 {
		t.Errorf("Expected 2 image hashes, got %d", len(svc.ImageHashes))
	}

	if svc.ImageHashes["myapp:latest"] != "sha256:abc123" {
		t.Error("Expected correct image hash for myapp:latest")
	}
}

func TestMarkServiceUninstalled(t *testing.T) {
	cs := New("test-cluster", false)

	cs.MarkServiceInstalled("backend")
	if !cs.IsServiceInstalled("backend") {
		t.Fatal("Expected backend to be installed")
	}

	cs.MarkServiceUninstalled("backend")
	if cs.IsServiceInstalled("backend") {
		t.Error("Expected backend to be uninstalled")
	}

	if _, exists := cs.Services["backend"]; exists {
		t.Error("Expected backend service to be removed from map")
	}
}

func TestGetInstalledServices(t *testing.T) {
	cs := New("test-cluster", false)

	cs.MarkServiceInstalled("redis")
	cs.MarkServiceInstalled("postgres")
	cs.MarkServiceInstalled("backend")

	installed := cs.GetInstalledServices()

	if len(installed) != 3 {
		t.Errorf("Expected 3 installed services, got %d", len(installed))
	}

	// Check all services are present (order doesn't matter)
	serviceMap := make(map[string]bool)
	for _, name := range installed {
		serviceMap[name] = true
	}

	if !serviceMap["redis"] || !serviceMap["postgres"] || !serviceMap["backend"] {
		t.Error("Expected all services in installed list")
	}
}

func TestGetCreatedNamespaces(t *testing.T) {
	cs := New("test-cluster", false)

	// Service 1: created namespace "app"
	cs.MarkServiceInstalledWithNamespace("backend", "app", true)
	// Service 2: created namespace "app" (same namespace)
	cs.MarkServiceInstalledWithNamespace("frontend", "app", true)
	// Service 3: existing namespace "default"
	cs.MarkServiceInstalledWithNamespace("redis", "default", false)
	// Service 4: created namespace "data"
	cs.MarkServiceInstalledWithNamespace("postgres", "data", true)

	namespaces := cs.GetCreatedNamespaces()

	// Should only include namespaces where CreatedNamespace = true
	if len(namespaces) != 2 {
		t.Errorf("Expected 2 created namespaces, got %d", len(namespaces))
	}

	if namespaces["app"] != 2 {
		t.Errorf("Expected 2 services in 'app' namespace, got %d", namespaces["app"])
	}

	if namespaces["data"] != 1 {
		t.Errorf("Expected 1 service in 'data' namespace, got %d", namespaces["data"])
	}

	if _, exists := namespaces["default"]; exists {
		t.Error("Should not include 'default' namespace (not created by kraze)")
	}
}

func TestGetAllNamespacesUsed(t *testing.T) {
	cs := New("test-cluster", false)

	cs.MarkServiceInstalledWithNamespace("backend", "app", true)
	cs.MarkServiceInstalledWithNamespace("frontend", "app", false)
	cs.MarkServiceInstalledWithNamespace("redis", "data", true)

	namespaces := cs.GetAllNamespacesUsed()

	if len(namespaces) != 2 {
		t.Errorf("Expected 2 namespaces, got %d", len(namespaces))
	}

	if namespaces["app"] != 2 {
		t.Errorf("Expected 2 services in 'app', got %d", namespaces["app"])
	}

	if namespaces["data"] != 1 {
		t.Errorf("Expected 1 service in 'data', got %d", namespaces["data"])
	}
}

func TestGetAllNamespacesUsedForCleanup(t *testing.T) {
	cs := New("test-cluster", false)

	cs.MarkServiceInstalledWithNamespace("backend", "app", true)
	cs.MarkServiceInstalledWithNamespace("redis", "data", false)

	namespaces := cs.GetAllNamespacesUsedForCleanup()

	if len(namespaces) != 2 {
		t.Errorf("Expected 2 namespaces, got %d", len(namespaces))
	}

	// All counts should be 0 for cleanup
	if namespaces["app"] != 0 {
		t.Errorf("Expected count 0 for 'app', got %d", namespaces["app"])
	}

	if namespaces["data"] != 0 {
		t.Errorf("Expected count 0 for 'data', got %d", namespaces["data"])
	}
}

func TestGetNamespacesForServices(t *testing.T) {
	cs := New("test-cluster", false)

	// Three services in "app" namespace
	cs.MarkServiceInstalledWithNamespace("backend", "app", true)
	cs.MarkServiceInstalledWithNamespace("frontend", "app", true)
	cs.MarkServiceInstalledWithNamespace("api", "app", true)

	// One service in "data" namespace
	cs.MarkServiceInstalledWithNamespace("redis", "data", true)

	// Uninstalling backend and frontend (2 out of 3 in "app")
	namespaces := cs.GetNamespacesForServices([]string{"backend", "frontend"})

	// "app" namespace should have count 1 (api still using it)
	if namespaces["app"] != 1 {
		t.Errorf("Expected 1 other service in 'app', got %d", namespaces["app"])
	}

	// Uninstalling redis (only service in "data")
	namespaces = cs.GetNamespacesForServices([]string{"redis"})

	// "data" namespace should have count 0 (can be deleted)
	if namespaces["data"] != 0 {
		t.Errorf("Expected 0 other services in 'data', got %d", namespaces["data"])
	}
}

func TestGetImageHashes(t *testing.T) {
	cs := New("test-cluster", false)

	imageHashes := map[string]string{
		"myapp:latest": "sha256:abc123",
		"postgres:15":  "sha256:def456",
	}

	cs.MarkServiceInstalledWithImages("backend", "default", false, imageHashes)

	// Get hashes for existing service
	hashes := cs.GetImageHashes("backend")
	if len(hashes) != 2 {
		t.Errorf("Expected 2 hashes, got %d", len(hashes))
	}

	if hashes["myapp:latest"] != "sha256:abc123" {
		t.Error("Expected correct hash for myapp:latest")
	}

	// Get hashes for non-existent service
	emptyHashes := cs.GetImageHashes("nonexistent")
	if len(emptyHashes) != 0 {
		t.Errorf("Expected empty map for nonexistent service, got %d hashes", len(emptyHashes))
	}
}

func TestHasImageHashChanged(t *testing.T) {
	cs := New("test-cluster", false)

	imageHashes := map[string]string{
		"myapp:latest": "sha256:abc123",
	}
	cs.MarkServiceInstalledWithImages("backend", "default", false, imageHashes)

	// Same hash - not changed
	if cs.HasImageHashChanged("backend", "myapp:latest", "sha256:abc123") {
		t.Error("Expected hash to be unchanged")
	}

	// Different hash - changed
	if !cs.HasImageHashChanged("backend", "myapp:latest", "sha256:xyz789") {
		t.Error("Expected hash to be changed")
	}

	// New image - changed
	if !cs.HasImageHashChanged("backend", "newimage:v1", "sha256:new") {
		t.Error("Expected new image to be marked as changed")
	}

	// Nonexistent service - changed
	if !cs.HasImageHashChanged("nonexistent", "myapp:latest", "sha256:abc123") {
		t.Error("Expected nonexistent service to be marked as changed")
	}
}

func TestGetChangedImages(t *testing.T) {
	cs := New("test-cluster", false)

	storedHashes := map[string]string{
		"myapp:latest":  "sha256:abc123",
		"postgres:15":   "sha256:def456",
	}
	cs.MarkServiceInstalledWithImages("backend", "default", false, storedHashes)

	// Test with no changes
	currentHashes := map[string]string{
		"myapp:latest":  "sha256:abc123",
		"postgres:15":   "sha256:def456",
	}
	changed := cs.GetChangedImages("backend", currentHashes)
	if len(changed) != 0 {
		t.Errorf("Expected no changed images, got %d", len(changed))
	}

	// Test with one changed hash
	currentHashes = map[string]string{
		"myapp:latest":  "sha256:xyz789", // Changed
		"postgres:15":   "sha256:def456",
	}
	changed = cs.GetChangedImages("backend", currentHashes)
	if len(changed) != 1 {
		t.Errorf("Expected 1 changed image, got %d", len(changed))
	}
	if changed[0] != "myapp:latest" {
		t.Errorf("Expected 'myapp:latest' to be changed, got '%s'", changed[0])
	}

	// Test with new image
	currentHashes = map[string]string{
		"myapp:latest":  "sha256:abc123",
		"postgres:15":   "sha256:def456",
		"redis:7":       "sha256:newimage", // New
	}
	changed = cs.GetChangedImages("backend", currentHashes)
	if len(changed) != 1 {
		t.Errorf("Expected 1 changed image, got %d", len(changed))
	}
	if changed[0] != "redis:7" {
		t.Errorf("Expected 'redis:7' to be new, got '%s'", changed[0])
	}
}

func TestStatePersistence(t *testing.T) {
	ctx := context.Background()
	clientset := fake.NewSimpleClientset()

	// Create complex state
	cs := New("test-cluster", true)
	cs.MarkServiceInstalledWithImages("backend", "app", true, map[string]string{
		"myapp:latest": "sha256:abc123",
	})
	cs.MarkServiceInstalledWithNamespace("redis", "data", false)

	// Save
	if err := cs.Save(ctx, clientset); err != nil {
		t.Fatalf("Failed to save state: %v", err)
	}

	// Load and verify all fields
	loaded, err := Load(ctx, clientset, "test-cluster")
	if err != nil {
		t.Fatalf("Failed to load state: %v", err)
	}

	if loaded.ClusterName != "test-cluster" {
		t.Error("ClusterName not persisted")
	}

	if !loaded.IsExternal {
		t.Error("IsExternal not persisted")
	}

	if len(loaded.Services) != 2 {
		t.Errorf("Expected 2 services, got %d", len(loaded.Services))
	}

	backend := loaded.Services["backend"]
	if backend.Namespace != "app" {
		t.Error("Service namespace not persisted")
	}
	if !backend.CreatedNamespace {
		t.Error("CreatedNamespace flag not persisted")
	}
	if backend.ImageHashes["myapp:latest"] != "sha256:abc123" {
		t.Error("Image hashes not persisted")
	}
}

func TestMigration(t *testing.T) {
	ctx := context.Background()
	clientset := fake.NewSimpleClientset()

	// Create ConfigMap with v0 state (no version field)
	v0State := map[string]interface{}{
		"cluster_name": "test-cluster",
		"is_external":  false,
		"services":     map[string]interface{}{},
		"last_updated": time.Now().Format(time.RFC3339),
		// No version field
	}

	v0JSON, err := json.Marshal(v0State)
	if err != nil {
		t.Fatalf("Failed to marshal v0 state: %v", err)
	}

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      ConfigMapName,
			Namespace: ConfigMapNamespace,
		},
		Data: map[string]string{
			ConfigMapDataKey: string(v0JSON),
		},
	}
	_, err = clientset.CoreV1().ConfigMaps(ConfigMapNamespace).Create(ctx, cm, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("Failed to create ConfigMap: %v", err)
	}

	// Load should auto-migrate to v1
	loaded, err := Load(ctx, clientset, "test-cluster")
	if err != nil {
		t.Fatalf("Failed to load v0 state: %v", err)
	}

	if loaded.Version != CurrentStateVersion {
		t.Errorf("Expected version %d after migration, got %d", CurrentStateVersion, loaded.Version)
	}
}

func TestUpdateExistingConfigMap(t *testing.T) {
	ctx := context.Background()
	clientset := fake.NewSimpleClientset()

	// Create and save initial state
	cs1 := New("test-cluster", false)
	cs1.MarkServiceInstalled("redis")

	if err := cs1.Save(ctx, clientset); err != nil {
		t.Fatalf("Failed to save initial state: %v", err)
	}

	// Update state with new service
	cs2 := New("test-cluster", false)
	cs2.MarkServiceInstalled("redis")
	cs2.MarkServiceInstalled("postgres")

	if err := cs2.Save(ctx, clientset); err != nil {
		t.Fatalf("Failed to update state: %v", err)
	}

	// Load and verify both services
	loaded, err := Load(ctx, clientset, "test-cluster")
	if err != nil {
		t.Fatalf("Failed to load updated state: %v", err)
	}

	if len(loaded.Services) != 2 {
		t.Errorf("Expected 2 services after update, got %d", len(loaded.Services))
	}

	if !loaded.IsServiceInstalled("postgres") {
		t.Error("Expected postgres to be installed after update")
	}
}
