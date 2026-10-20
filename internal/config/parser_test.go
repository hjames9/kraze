package config

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

func TestParse(test *testing.T) {
	// Create a temporary config file
	tmpDir := test.TempDir()
	configFile := filepath.Join(tmpDir, "kraze.yml")

	configContent := `
project: test
cluster:
  name: test-cluster
  config:
    - role: control-plane
      extraPortMappings:
        - containerPort: 80
          hostPort: 8080

services:
  redis:
    type: helm
    chart: redis
    repo: bitnami
    namespace: data

  api:
    type: manifests
    path: ./manifests
    namespace: app
    depends_on:
      - redis
`

	if err := os.WriteFile(configFile, []byte(configContent), 0644); err != nil {
		test.Fatalf("Failed to write test config: %v", err)
	}

	cfg, err := Parse(configFile)
	if err != nil {
		test.Fatalf("Failed to parse config: %v", err)
	}

	// Verify cluster config
	if cfg.Cluster.Name != "test-cluster" {
		test.Errorf("Expected cluster name 'test-cluster', got '%s'", cfg.Cluster.Name)
	}

	if len(cfg.Cluster.Config) != 1 {
		test.Errorf("Expected 1 node config, got %d", len(cfg.Cluster.Config))
	}

	// Verify services
	if len(cfg.Services) != 2 {
		test.Errorf("Expected 2 services, got %d", len(cfg.Services))
	}

	redis, ok := cfg.Services["redis"]
	if !ok {
		test.Fatal("Expected 'redis' service")
	}

	if redis.Type != "helm" {
		test.Errorf("Expected redis type 'helm', got '%s'", redis.Type)
	}

	if redis.Chart != "redis" {
		test.Errorf("Expected redis chart 'redis', got '%s'", redis.Chart)
	}

	api, ok := cfg.Services["api"]
	if !ok {
		test.Fatal("Expected 'api' service")
	}

	if len(api.DependsOn) != 1 || api.DependsOn[0] != "redis" {
		test.Errorf("Expected api to depend on redis, got %v", api.DependsOn)
	}
}

func TestParseInvalidYAML(test *testing.T) {
	tmpDir := test.TempDir()
	configFile := filepath.Join(tmpDir, "invalid.yml")

	if err := os.WriteFile(configFile, []byte("invalid: yaml: content:"), 0644); err != nil {
		test.Fatalf("Failed to write test config: %v", err)
	}

	_, err := Parse(configFile)
	if err == nil {
		test.Error("Expected error parsing invalid YAML, got nil")
	}
}

func TestParseMissingFile(test *testing.T) {
	_, err := Parse("/nonexistent/file.yml")
	if err == nil {
		test.Error("Expected error for missing file, got nil")
	}
}

func TestFilterServices(test *testing.T) {
	cfg := &Config{
		Services: map[string]ServiceConfig{
			"redis":    {Name: "redis", Type: "helm"},
			"postgres": {Name: "postgres", Type: "helm"},
			"api":      {Name: "api", Type: "manifests", DependsOn: []string{"redis"}},
		},
	}

	// Filter to just api (should include redis as dependency)
	filtered, err := cfg.FilterServices([]string{"api"})
	if err != nil {
		test.Fatalf("FilterServices failed: %v", err)
	}

	if len(filtered) != 1 {
		test.Errorf("Expected 1 service after filter, got %d", len(filtered))
	}

	if _, ok := filtered["api"]; !ok {
		test.Error("Expected 'api' in filtered services")
	}
}

func TestFilterServicesNonexistent(test *testing.T) {
	cfg := &Config{
		Services: map[string]ServiceConfig{
			"redis": {Name: "redis", Type: "helm"},
		},
	}

	_, err := cfg.FilterServices([]string{"nonexistent"})
	if err == nil {
		test.Error("Expected error for nonexistent service, got nil")
	}
}

func TestFilterServicesEmpty(test *testing.T) {
	cfg := &Config{
		Services: map[string]ServiceConfig{
			"redis": {Name: "redis", Type: "helm"},
		},
	}

	filtered, err := cfg.FilterServices([]string{})
	if err != nil {
		test.Fatalf("FilterServices failed: %v", err)
	}

	if len(filtered) != 1 {
		test.Errorf("Expected all services when filter is empty, got %d", len(filtered))
	}
}

func TestFilterServicesNoDependencies(test *testing.T) {
	cfg := &Config{
		Services: map[string]ServiceConfig{
			"redis":    {Name: "redis", Type: "helm"},
			"postgres": {Name: "postgres", Type: "helm"},
			"api":      {Name: "api", Type: "manifests", DependsOn: []string{"redis", "postgres"}},
			"frontend": {Name: "frontend", Type: "manifests", DependsOn: []string{"api"}},
		},
	}

	// Test filtering api without dependencies (should only include api, not redis or postgres)
	filtered, err := cfg.FilterServicesNoDependencies([]string{"api"})
	if err != nil {
		test.Fatalf("FilterServicesNoDependencies failed: %v", err)
	}

	if len(filtered) != 1 {
		test.Errorf("Expected 1 service after filter (no deps), got %d", len(filtered))
	}

	if _, ok := filtered["api"]; !ok {
		test.Error("Expected 'api' in filtered services")
	}

	if _, ok := filtered["redis"]; ok {
		test.Error("Did not expect 'redis' in filtered services (dependency should be excluded)")
	}

	if _, ok := filtered["postgres"]; ok {
		test.Error("Did not expect 'postgres' in filtered services (dependency should be excluded)")
	}
}

func TestFilterServicesNoDependenciesMultiple(test *testing.T) {
	cfg := &Config{
		Services: map[string]ServiceConfig{
			"redis":    {Name: "redis", Type: "helm"},
			"postgres": {Name: "postgres", Type: "helm"},
			"api":      {Name: "api", Type: "manifests", DependsOn: []string{"redis"}},
			"frontend": {Name: "frontend", Type: "manifests", DependsOn: []string{"api"}},
		},
	}

	// Test filtering multiple services without dependencies
	filtered, err := cfg.FilterServicesNoDependencies([]string{"redis", "frontend"})
	if err != nil {
		test.Fatalf("FilterServicesNoDependencies failed: %v", err)
	}

	if len(filtered) != 2 {
		test.Errorf("Expected 2 services after filter (no deps), got %d", len(filtered))
	}

	if _, ok := filtered["redis"]; !ok {
		test.Error("Expected 'redis' in filtered services")
	}

	if _, ok := filtered["frontend"]; !ok {
		test.Error("Expected 'frontend' in filtered services")
	}

	if _, ok := filtered["api"]; ok {
		test.Error("Did not expect 'api' in filtered services (dependency should be excluded)")
	}
}

func TestFilterServicesNoDependenciesEmpty(test *testing.T) {
	cfg := &Config{
		Services: map[string]ServiceConfig{
			"redis": {Name: "redis", Type: "helm"},
			"api":   {Name: "api", Type: "manifests", DependsOn: []string{"redis"}},
		},
	}

	// Empty filter should return all services
	filtered, err := cfg.FilterServicesNoDependencies([]string{})
	if err != nil {
		test.Fatalf("FilterServicesNoDependencies failed: %v", err)
	}

	if len(filtered) != 2 {
		test.Errorf("Expected all services when filter is empty, got %d", len(filtered))
	}
}

func TestFilterServicesNoDependenciesNonexistent(test *testing.T) {
	cfg := &Config{
		Services: map[string]ServiceConfig{
			"redis": {Name: "redis", Type: "helm"},
		},
	}

	// Nonexistent service should return error
	_, err := cfg.FilterServicesNoDependencies([]string{"nonexistent"})
	if err == nil {
		test.Error("Expected error for nonexistent service, got nil")
	}
}

func TestResolvePaths(test *testing.T) {
	tmpDir := test.TempDir()
	configFile := filepath.Join(tmpDir, "kraze.yml")

	cfg := &Config{
		Services: map[string]ServiceConfig{
			"api": {
				Name: "api",
				Type: "manifests",
				Path: "./manifests",
			},
		},
	}

	if err := cfg.ResolvePaths(configFile); err != nil {
		test.Fatalf("ResolvePaths failed: %v", err)
	}

	expected := filepath.Join(tmpDir, "manifests")
	if cfg.Services["api"].Path != expected {
		test.Errorf("Expected path '%s', got '%s'", expected, cfg.Services["api"].Path)
	}
}

// TestParseNodeKubeadmConfigPatches is the regression guard for node-level
// kubeadmConfigPatches. The YAML decoder is lenient, so before KindNode carried
// this field the key parsed "successfully" and was silently discarded - config
// that did nothing still validated clean. These assertions fail loudly if the
// field is ever removed or renamed.
func TestParseNodeKubeadmConfigPatches(test *testing.T) {
	tmpDir := test.TempDir()
	configFile := filepath.Join(tmpDir, "kraze.yml")

	configContent := `
project: test
cluster:
  name: dev-cluster
  config:
    - role: control-plane
      kubeadmConfigPatches:
        - |
          kind: ClusterConfiguration
          apiVersion: kubeadm.k8s.io/v1beta4
          controllerManager:
            extraArgs:
              - name: leader-elect-lease-duration
                value: 180s
        - |
          kind: ClusterConfiguration
          apiVersion: kubeadm.k8s.io/v1beta4
          scheduler:
            extraArgs:
              - name: leader-elect-renew-deadline
                value: 120s
      extraMounts:
        - hostPath: /mnt/cache
          containerPath: /mnt/cache
    - role: worker

services: {}
`

	if err := os.WriteFile(configFile, []byte(configContent), 0644); err != nil {
		test.Fatalf("Failed to write test config: %v", err)
	}

	cfg, err := Parse(configFile)
	if err != nil {
		test.Fatalf("Failed to parse config: %v", err)
	}

	if len(cfg.Cluster.Config) != 2 {
		test.Fatalf("Expected 2 node configs, got %d", len(cfg.Cluster.Config))
	}

	want := []string{
		"kind: ClusterConfiguration\napiVersion: kubeadm.k8s.io/v1beta4\ncontrollerManager:\n  extraArgs:\n    - name: leader-elect-lease-duration\n      value: 180s\n",
		"kind: ClusterConfiguration\napiVersion: kubeadm.k8s.io/v1beta4\nscheduler:\n  extraArgs:\n    - name: leader-elect-renew-deadline\n      value: 120s\n",
	}

	got := cfg.Cluster.Config[0].KubeadmConfigPatches
	if !reflect.DeepEqual(got, want) {
		test.Errorf("KubeadmConfigPatches: got %#v, want %#v", got, want)
	}

	// Sibling keys on the same node must still decode
	if len(cfg.Cluster.Config[0].ExtraMounts) != 1 {
		test.Fatalf("Expected 1 extra mount, got %d", len(cfg.Cluster.Config[0].ExtraMounts))
	}
	if cfg.Cluster.Config[0].ExtraMounts[0].HostPath != "/mnt/cache" {
		test.Errorf("ExtraMounts[0].HostPath: got %q, want %q",
			cfg.Cluster.Config[0].ExtraMounts[0].HostPath, "/mnt/cache")
	}

	// A node that declares no patches must stay nil, not an empty slice
	if cfg.Cluster.Config[1].KubeadmConfigPatches != nil {
		test.Errorf("Node[1].KubeadmConfigPatches: got %#v, want nil", cfg.Cluster.Config[1].KubeadmConfigPatches)
	}

	// Round-trip: re-marshalling must preserve the patches and, thanks to
	// omitempty, must not emit the key for the node that has none.
	out, err := yaml.Marshal(cfg)
	if err != nil {
		test.Fatalf("Failed to marshal config: %v", err)
	}

	var reparsed Config
	if err := yaml.Unmarshal(out, &reparsed); err != nil {
		test.Fatalf("Failed to re-parse marshalled config: %v", err)
	}
	if !reflect.DeepEqual(reparsed.Cluster.Config[0].KubeadmConfigPatches, want) {
		test.Errorf("After round-trip: got %#v, want %#v",
			reparsed.Cluster.Config[0].KubeadmConfigPatches, want)
	}
	if strings.Count(string(out), "kubeadmConfigPatches") != 1 {
		test.Errorf("Expected exactly 1 kubeadmConfigPatches key in marshalled output (omitempty), got %d:\n%s",
			strings.Count(string(out), "kubeadmConfigPatches"), out)
	}
}

func TestProjectValidation(t *testing.T) {
	tests := []struct {
		project string
		valid   bool
	}{
		{"", true},
		{"billing", true},
		{"web-2", true},
		{"a", true},
		{strings.Repeat("a", 63), true},
		{strings.Repeat("a", 64), false},
		{"Billing", false},
		{"team/billing", false},
		{"-billing", false},
		{"billing-", false},
		{"billing_api", false},
	}
	for _, tt := range tests {
		cfg := &Config{Project: tt.project, Cluster: ClusterConfig{Name: "c"}}
		err := cfg.Validate()
		if (err == nil) != tt.valid {
			t.Errorf("project %q: err = %v, want valid=%v", tt.project, err, tt.valid)
		}
	}
}

func TestParseRequiresProject(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "My_App")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "kraze.yml")
	if err := os.WriteFile(path, []byte("cluster:\n  name: dev\nservices: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Parse(path)
	if err == nil || !strings.Contains(err.Error(), "project name is required") {
		t.Fatalf("expected a missing project error, got %v", err)
	}
	if !strings.Contains(err.Error(), "project: my-app") {
		t.Errorf("error should suggest a name from the directory, got %v", err)
	}
	if !errors.Is(err, ErrInvalid) || strings.Count(err.Error(), "validation failed") != 1 {
		t.Errorf("a validation error should wrap ErrInvalid exactly once, got %v", err)
	}

	// A YAML error isn't a validation error
	bad := filepath.Join(dir, "bad.yml")
	if err := os.WriteFile(bad, []byte("project: app\ncluster: [\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Parse(bad); err == nil || errors.Is(err, ErrInvalid) {
		t.Errorf("a YAML error should not wrap ErrInvalid, got %v", err)
	}
}

func TestSuggestProjectName(t *testing.T) {
	tests := map[string]string{
		"/home/me/billing/kraze.yml": "billing",
		"/home/me/My App!/kraze.yml": "my-app",
		"/home/me/__/kraze.yml":      "my-project",
		"/home/me/-svc-/kraze.yml":   "svc",
	}
	for path, want := range tests {
		if got := suggestProjectName(path); got != want {
			t.Errorf("suggestProjectName(%q) = %q, want %q", path, got, want)
		}
	}
}

func TestParseIdentity(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "kraze.yml")
	// Depends on a service that isn't here: Parse would fail, ParseIdentity must not
	content := "project: billing\ncluster:\n  name: ${KRAZE_TEST_CLUSTER:-dev}\nservices:\n  api:\n    type: manifests\n    path: x.yaml\n    depends_on: [db]\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	id, err := ParseIdentity(path)
	if err != nil {
		t.Fatal(err)
	}
	if id.Project != "billing" || id.Cluster != "dev" || id.External {
		t.Errorf("ParseIdentity = %+v", id)
	}
}
