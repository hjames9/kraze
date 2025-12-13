package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParse(test *testing.T) {
	// Create a temporary config file
	tmpDir := test.TempDir()
	configFile := filepath.Join(tmpDir, "kraze.yml")

	configContent := `
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

	cfg.ResolvePaths(configFile)

	expected := filepath.Join(tmpDir, "manifests")
	if cfg.Services["api"].Path != expected {
		test.Errorf("Expected path '%s', got '%s'", expected, cfg.Services["api"].Path)
	}
}
