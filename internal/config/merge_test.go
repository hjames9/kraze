package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeTemp writes content to a temp file in dir and returns its path.
func writeTemp(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("failed to write %s: %v", name, err)
	}
	return path
}

func TestParseMultipleSingleFile(t *testing.T) {
	dir := t.TempDir()
	path := writeTemp(t, dir, "kraze.yml", `
cluster:
  name: dev
services:
  redis:
    type: manifests
    path: .
`)
	cfg, err := ParseMultiple([]string{path})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Cluster.Name != "dev" {
		t.Errorf("expected cluster name 'dev', got %q", cfg.Cluster.Name)
	}
	if _, ok := cfg.Services["redis"]; !ok {
		t.Error("expected redis service")
	}
}

func TestParseMultipleMergesServices(t *testing.T) {
	dir := t.TempDir()
	a := writeTemp(t, dir, "a.yml", `
cluster:
  name: dev
services:
  redis:
    type: manifests
    path: .
`)
	b := writeTemp(t, dir, "b.yml", `
cluster:
  name: dev
services:
  postgres:
    type: manifests
    path: .
`)
	cfg, err := ParseMultiple([]string{a, b})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cfg.Services) != 2 {
		t.Errorf("expected 2 services, got %d", len(cfg.Services))
	}
	if _, ok := cfg.Services["redis"]; !ok {
		t.Error("expected redis service")
	}
	if _, ok := cfg.Services["postgres"]; !ok {
		t.Error("expected postgres service")
	}
}

func TestParseMultipleDuplicateServiceError(t *testing.T) {
	dir := t.TempDir()
	a := writeTemp(t, dir, "a.yml", `
cluster:
  name: dev
services:
  redis:
    type: manifests
    path: .
`)
	b := writeTemp(t, dir, "b.yml", `
cluster:
  name: dev
services:
  redis:
    type: manifests
    path: .
`)
	_, err := ParseMultiple([]string{a, b})
	if err == nil {
		t.Error("expected error for duplicate service name, got nil")
	}
}

func TestParseMultipleCrossFileDependency(t *testing.T) {
	dir := t.TempDir()
	a := writeTemp(t, dir, "a.yml", `
cluster:
  name: dev
services:
  redis:
    type: manifests
    path: .
`)
	b := writeTemp(t, dir, "b.yml", `
cluster:
  name: dev
services:
  app:
    type: manifests
    path: .
    depends_on: [redis]
`)
	cfg, err := ParseMultiple([]string{a, b})
	if err != nil {
		t.Fatalf("cross-file dependency should be valid, got error: %v", err)
	}
	app := cfg.Services["app"]
	if len(app.DependsOn) != 1 || app.DependsOn[0] != "redis" {
		t.Errorf("expected depends_on=[redis], got %v", app.DependsOn)
	}
}

func TestParseMultipleClusterNameFirstFileWins(t *testing.T) {
	dir := t.TempDir()
	a := writeTemp(t, dir, "a.yml", `
cluster:
  name: primary
services:
  redis:
    type: manifests
    path: .
`)
	b := writeTemp(t, dir, "b.yml", `
cluster:
  name: secondary
services:
  postgres:
    type: manifests
    path: .
`)
	cfg, err := ParseMultiple([]string{a, b})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Cluster.Name != "primary" {
		t.Errorf("expected cluster name 'primary' (first file wins), got %q", cfg.Cluster.Name)
	}
}

func TestParseMultipleCACertificatesUnion(t *testing.T) {
	dir := t.TempDir()
	a := writeTemp(t, dir, "a.yml", `
cluster:
  name: dev
  ca_certificates:
    - /etc/ssl/certs/corp-ca.crt
    - /etc/ssl/certs/shared.crt
services:
  redis:
    type: manifests
    path: .
`)
	b := writeTemp(t, dir, "b.yml", `
cluster:
  name: dev
  ca_certificates:
    - /etc/ssl/certs/ml-ca.crt
    - /etc/ssl/certs/shared.crt
services:
  postgres:
    type: manifests
    path: .
`)
	cfg, err := ParseMultiple([]string{a, b})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// shared.crt should appear only once
	if len(cfg.Cluster.CACertificates) != 3 {
		t.Errorf("expected 3 deduplicated CA certs, got %d: %v", len(cfg.Cluster.CACertificates), cfg.Cluster.CACertificates)
	}
}

func TestParseMultipleGPUOrLogic(t *testing.T) {
	dir := t.TempDir()
	a := writeTemp(t, dir, "a.yml", `
cluster:
  name: dev
  gpu:
    amd:
      enabled: true
      count: 2
services:
  redis:
    type: manifests
    path: .
`)
	b := writeTemp(t, dir, "b.yml", `
cluster:
  name: dev
services:
  postgres:
    type: manifests
    path: .
`)
	cfg, err := ParseMultiple([]string{a, b})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !cfg.Cluster.GPU.IsAMDEnabled() {
		t.Error("expected AMD GPU to be enabled after merge")
	}
	if cfg.Cluster.GPU.AMD.Count != 2 {
		t.Errorf("expected AMD count 2, got %d", cfg.Cluster.GPU.AMD.Count)
	}
}

func TestParseMultipleGPUMaxCount(t *testing.T) {
	dir := t.TempDir()
	a := writeTemp(t, dir, "a.yml", `
cluster:
  name: dev
  gpu:
    amd:
      enabled: true
      count: 2
services:
  redis:
    type: manifests
    path: .
`)
	b := writeTemp(t, dir, "b.yml", `
cluster:
  name: dev
  gpu:
    amd:
      enabled: true
      count: 4
services:
  postgres:
    type: manifests
    path: .
`)
	cfg, err := ParseMultiple([]string{a, b})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Cluster.GPU.AMD.Count != 4 {
		t.Errorf("expected max AMD count 4, got %d", cfg.Cluster.GPU.AMD.Count)
	}
}

func TestParseMultipleProxyConflictError(t *testing.T) {
	dir := t.TempDir()
	boolTrue := true
	boolFalse := false
	_ = boolTrue
	_ = boolFalse

	a := writeTemp(t, dir, "a.yml", `
cluster:
  name: dev
  proxy:
    enabled: true
    http_proxy: http://proxy.corp.com:8080
services:
  redis:
    type: manifests
    path: .
`)
	b := writeTemp(t, dir, "b.yml", `
cluster:
  name: dev
  proxy:
    enabled: false
services:
  postgres:
    type: manifests
    path: .
`)
	_, err := ParseMultiple([]string{a, b})
	if err == nil {
		t.Error("expected error for proxy.enabled conflict, got nil")
	}
}

func TestParseMultipleNoProxyUnion(t *testing.T) {
	dir := t.TempDir()
	a := writeTemp(t, dir, "a.yml", `
cluster:
  name: dev
  proxy:
    http_proxy: http://proxy.corp.com:8080
    no_proxy: localhost,127.0.0.1
services:
  redis:
    type: manifests
    path: .
`)
	b := writeTemp(t, dir, "b.yml", `
cluster:
  name: dev
  proxy:
    http_proxy: http://proxy.corp.com:8080
    no_proxy: 127.0.0.1,.svc.cluster.local
services:
  postgres:
    type: manifests
    path: .
`)
	cfg, err := ParseMultiple([]string{a, b})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// 127.0.0.1 appears in both — should be deduplicated
	noProxy := cfg.Cluster.Proxy.NoProxy
	count := 0
	for _, part := range splitNoProxy(noProxy) {
		if part == "127.0.0.1" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("expected 127.0.0.1 to appear exactly once in no_proxy, got %d times in %q", count, noProxy)
	}
}

func TestParseMultipleVersionConflictError(t *testing.T) {
	dir := t.TempDir()
	a := writeTemp(t, dir, "a.yml", `
cluster:
  name: dev
  version: "1.29"
services:
  redis:
    type: manifests
    path: .
`)
	b := writeTemp(t, dir, "b.yml", `
cluster:
  name: dev
  version: "1.30"
services:
  postgres:
    type: manifests
    path: .
`)
	_, err := ParseMultiple([]string{a, b})
	if err == nil {
		t.Error("expected error for cluster.version conflict, got nil")
	}
}

func TestParseMultipleEmptyPathsError(t *testing.T) {
	_, err := ParseMultiple([]string{})
	if err == nil {
		t.Error("expected error for empty paths, got nil")
	}
}

func TestParseMultipleKindNodePortMappingsUnion(t *testing.T) {
	dir := t.TempDir()
	a := writeTemp(t, dir, "a.yml", `
cluster:
  name: dev
  config:
    - role: control-plane
      extraPortMappings:
        - containerPort: 30080
          hostPort: 8080
services:
  redis:
    type: manifests
    path: .
`)
	b := writeTemp(t, dir, "b.yml", `
cluster:
  name: dev
  config:
    - role: control-plane
      extraPortMappings:
        - containerPort: 30443
          hostPort: 8443
services:
  postgres:
    type: manifests
    path: .
`)
	cfg, err := ParseMultiple([]string{a, b})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cfg.Cluster.Config) != 1 {
		t.Fatalf("expected 1 node, got %d", len(cfg.Cluster.Config))
	}
	if len(cfg.Cluster.Config[0].ExtraPortMappings) != 2 {
		t.Errorf("expected 2 port mappings after union, got %d", len(cfg.Cluster.Config[0].ExtraPortMappings))
	}
}

func TestParseMultipleKindNodePortMappingsDeduplicated(t *testing.T) {
	dir := t.TempDir()
	a := writeTemp(t, dir, "a.yml", `
cluster:
  name: dev
  config:
    - role: control-plane
      extraPortMappings:
        - containerPort: 30080
          hostPort: 8080
services:
  redis:
    type: manifests
    path: .
`)
	b := writeTemp(t, dir, "b.yml", `
cluster:
  name: dev
  config:
    - role: control-plane
      extraPortMappings:
        - containerPort: 30080
          hostPort: 8080
services:
  postgres:
    type: manifests
    path: .
`)
	cfg, err := ParseMultiple([]string{a, b})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cfg.Cluster.Config[0].ExtraPortMappings) != 1 {
		t.Errorf("expected identical port mapping to be deduplicated, got %d", len(cfg.Cluster.Config[0].ExtraPortMappings))
	}
}

func TestParseMultipleKindNodePortMappingsConflictError(t *testing.T) {
	dir := t.TempDir()
	a := writeTemp(t, dir, "a.yml", `
cluster:
  name: dev
  config:
    - role: control-plane
      extraPortMappings:
        - containerPort: 30080
          hostPort: 8080
services:
  redis:
    type: manifests
    path: .
`)
	b := writeTemp(t, dir, "b.yml", `
cluster:
  name: dev
  config:
    - role: control-plane
      extraPortMappings:
        - containerPort: 30080
          hostPort: 9090
services:
  postgres:
    type: manifests
    path: .
`)
	_, err := ParseMultiple([]string{a, b})
	if err == nil {
		t.Error("expected error for conflicting hostPort on same containerPort, got nil")
	}
}

func TestParseMultipleKindNodeMountsUnion(t *testing.T) {
	dir := t.TempDir()
	a := writeTemp(t, dir, "a.yml", `
cluster:
  name: dev
  config:
    - role: control-plane
      extraMounts:
        - hostPath: /host/data
          containerPath: /data
services:
  redis:
    type: manifests
    path: .
`)
	b := writeTemp(t, dir, "b.yml", `
cluster:
  name: dev
  config:
    - role: control-plane
      extraMounts:
        - hostPath: /host/models
          containerPath: /models
services:
  postgres:
    type: manifests
    path: .
`)
	cfg, err := ParseMultiple([]string{a, b})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cfg.Cluster.Config[0].ExtraMounts) != 2 {
		t.Errorf("expected 2 mounts after union, got %d", len(cfg.Cluster.Config[0].ExtraMounts))
	}
}

func TestParseMultipleKindNodeMountsConflictError(t *testing.T) {
	dir := t.TempDir()
	a := writeTemp(t, dir, "a.yml", `
cluster:
  name: dev
  config:
    - role: control-plane
      extraMounts:
        - hostPath: /host/data
          containerPath: /data
services:
  redis:
    type: manifests
    path: .
`)
	b := writeTemp(t, dir, "b.yml", `
cluster:
  name: dev
  config:
    - role: control-plane
      extraMounts:
        - hostPath: /host/other
          containerPath: /data
services:
  postgres:
    type: manifests
    path: .
`)
	_, err := ParseMultiple([]string{a, b})
	if err == nil {
		t.Error("expected error for conflicting hostPath on same containerPath, got nil")
	}
}

func TestParseMultipleKindNodeLabelsUnion(t *testing.T) {
	dir := t.TempDir()
	a := writeTemp(t, dir, "a.yml", `
cluster:
  name: dev
  config:
    - role: control-plane
      labels:
        app: foo
services:
  redis:
    type: manifests
    path: .
`)
	b := writeTemp(t, dir, "b.yml", `
cluster:
  name: dev
  config:
    - role: control-plane
      labels:
        env: dev
services:
  postgres:
    type: manifests
    path: .
`)
	cfg, err := ParseMultiple([]string{a, b})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	labels := cfg.Cluster.Config[0].Labels
	if labels["app"] != "foo" || labels["env"] != "dev" {
		t.Errorf("expected merged labels, got %v", labels)
	}
}

func TestParseMultipleKindNodeLabelsConflictError(t *testing.T) {
	dir := t.TempDir()
	a := writeTemp(t, dir, "a.yml", `
cluster:
  name: dev
  config:
    - role: control-plane
      labels:
        app: foo
services:
  redis:
    type: manifests
    path: .
`)
	b := writeTemp(t, dir, "b.yml", `
cluster:
  name: dev
  config:
    - role: control-plane
      labels:
        app: bar
services:
  postgres:
    type: manifests
    path: .
`)
	_, err := ParseMultiple([]string{a, b})
	if err == nil {
		t.Error("expected error for conflicting label value, got nil")
	}
}

func TestParseMultipleKindNodeDistinctRoles(t *testing.T) {
	dir := t.TempDir()
	a := writeTemp(t, dir, "a.yml", `
cluster:
  name: dev
  config:
    - role: control-plane
      extraPortMappings:
        - containerPort: 30080
          hostPort: 8080
services:
  redis:
    type: manifests
    path: .
`)
	b := writeTemp(t, dir, "b.yml", `
cluster:
  name: dev
  config:
    - role: worker
      extraMounts:
        - hostPath: /host/data
          containerPath: /data
services:
  postgres:
    type: manifests
    path: .
`)
	cfg, err := ParseMultiple([]string{a, b})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cfg.Cluster.Config) != 2 {
		t.Errorf("expected 2 nodes (control-plane + worker), got %d", len(cfg.Cluster.Config))
	}
}

// splitNoProxy splits a comma-separated no_proxy string into trimmed entries.
func splitNoProxy(s string) []string {
	if s == "" {
		return nil
	}
	raw := strings.Split(s, ",")
	result := make([]string, 0, len(raw))
	for _, p := range raw {
		p = strings.TrimSpace(p)
		if p != "" {
			result = append(result, p)
		}
	}
	return result
}
