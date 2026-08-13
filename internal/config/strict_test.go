package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// writeConfig writes content to a temp kraze.yml and returns its path.
func writeConfig(test *testing.T, content string) string {
	test.Helper()
	path := filepath.Join(test.TempDir(), "kraze.yml")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		test.Fatalf("Failed to write test config: %v", err)
	}
	return path
}

// TestParseRejectsUnknownFields covers strict decoding. Before KnownFields was
// enabled these configs all parsed clean and silently did nothing, so `kraze
// validate` reported success for configuration that had no effect.
func TestParseRejectsUnknownFields(test *testing.T) {
	tests := []struct {
		name        string
		config      string
		wantInError string
	}{
		{
			name: "unknown top-level key",
			config: `
cluster:
  name: dev
clusters:
  name: oops
services: {}
`,
			wantInError: "clusters",
		},
		{
			name: "unknown cluster key",
			config: `
cluster:
  name: dev
  node_imagee: kindest/node:v1.34.0
services: {}
`,
			wantInError: "node_imagee",
		},
		{
			name: "misspelled node key (wrong case)",
			config: `
cluster:
  name: dev
  config:
    - role: control-plane
      extramounts:
        - hostPath: /mnt/cache
          containerPath: /mnt/cache
services: {}
`,
			wantInError: "extramounts",
		},
		{
			name: "unknown key inside extraMounts entry",
			config: `
cluster:
  name: dev
  config:
    - role: control-plane
      extraMounts:
        - hostPath: /mnt/cache
          containerPath: /mnt/cache
          readonly: true
services: {}
`,
			wantInError: "readonly",
		},
		{
			name: "unknown key inside extraPortMappings entry",
			config: `
cluster:
  name: dev
  config:
    - role: control-plane
      extraPortMappings:
        - containerPort: 80
          hostport: 8080
services: {}
`,
			wantInError: "hostport",
		},
		{
			name: "misspelled kubeadmConfigPatches",
			config: `
cluster:
  name: dev
  config:
    - role: control-plane
      kubeadmConfigPatch:
        - |
          kind: ClusterConfiguration
services: {}
`,
			wantInError: "kubeadmConfigPatch",
		},
		{
			name: "unknown service key",
			config: `
cluster:
  name: dev
services:
  api:
    type: manifests
    path: ./k8s
    depends_onn:
      - redis
`,
			wantInError: "depends_onn",
		},
		{
			name: "unknown networking key",
			config: `
cluster:
  name: dev
  networking:
    podsubnet: 10.244.0.0/16
services: {}
`,
			wantInError: "podsubnet",
		},
		{
			name: "unknown proxy key",
			config: `
cluster:
  name: dev
  proxy:
    httpProxy: http://proxy:8080
services: {}
`,
			wantInError: "httpProxy",
		},
		{
			name: "unknown gpu vendor key",
			config: `
cluster:
  name: dev
  gpu:
    intel:
      enabled: true
services: {}
`,
			wantInError: "intel",
		},
		{
			name: "unknown external cluster key",
			config: `
cluster:
  name: dev
  external:
    enabled: true
    kube_config: ~/.kube/config
services: {}
`,
			wantInError: "kube_config",
		},
	}

	for _, tt := range tests {
		test.Run(tt.name, func(test *testing.T) {
			_, err := Parse(writeConfig(test, tt.config))
			if err == nil {
				test.Fatalf("Expected an error for unknown field %q, got nil", tt.wantInError)
			}
			if !strings.Contains(err.Error(), tt.wantInError) {
				test.Errorf("Error should name the offending key %q, got: %v", tt.wantInError, err)
			}
			// Go type names must not leak into user-facing errors
			if strings.Contains(err.Error(), "config.") {
				test.Errorf("Error leaks a Go type name, got: %v", err)
			}
		})
	}
}

// TestParseReportsAllUnknownFields verifies the decoder reports every unknown
// key at once rather than stopping at the first, so a user fixes one at a time.
func TestParseReportsAllUnknownFields(test *testing.T) {
	cfg := `
cluster:
  name: dev
  bogus_cluster_key: 1
  config:
    - role: control-plane
      bogus_node_key: 2
services:
  api:
    type: manifests
    path: ./k8s
    bogus_service_key: 3
`
	_, err := Parse(writeConfig(test, cfg))
	if err == nil {
		test.Fatal("Expected an error, got nil")
	}

	for _, key := range []string{"bogus_cluster_key", "bogus_node_key", "bogus_service_key"} {
		if !strings.Contains(err.Error(), key) {
			test.Errorf("Expected error to mention %q, got: %v", key, err)
		}
	}
}

// TestParseAcceptsEveryDocumentedKey is the counterweight to the rejection
// tests: strictness must not reject configuration that is actually supported.
func TestParseAcceptsEveryDocumentedKey(test *testing.T) {
	cfg := `
cluster:
  name: dev-cluster
  version: "1.34.0"
  node_image: kindest/node:v1.34.0
  network: dev
  ipv4_address: 172.1.0.2
  subnet: 172.1.0.0/16
  ca_certificates:
    - /etc/ssl/certs/corporate-ca.crt
  insecure_registries:
    - registry.corp.com
  preload_images:
    - redis:7
  proxy:
    enabled: true
    http_proxy: http://proxy:8080
    https_proxy: http://proxy:8080
    no_proxy: localhost,127.0.0.1
  gpu:
    nvidia:
      enabled: true
    amd:
      enabled: false
  networking:
    disableDefaultCNI: true
    podSubnet: 10.244.0.0/16
    serviceSubnet: 10.96.0.0/12
  config:
    - role: control-plane
      replicas: 1
      labels:
        type: cp
      extraPortMappings:
        - containerPort: 30080
          hostPort: 8080
          listenAddress: 127.0.0.1
          protocol: TCP
      extraMounts:
        - hostPath: /mnt/cache
          containerPath: /mnt/cache
          readOnly: true
      kubeadmConfigPatches:
        - |
          kind: ClusterConfiguration
          apiVersion: kubeadm.k8s.io/v1beta4

services:
  redis:
    type: helm
    repo: oci://registry-1.docker.io/bitnamicharts
    chart: redis
    version: 23.2.6
    namespace: data
    create_namespace: true
    enabled: true
    keep_crds: false
    wait: true
    wait_timeout: 15m
    post_ready_delay: 5s
    labels:
      tier: data
    values:
      - base.yml
      - over.yml
    images:
      - myorg/custom:latest
  postgres:
    type: helm
    repo: https://charts.bitnami.com/bitnami
    chart: postgresql
    namespace: data
    values_inline: |
      replicaCount: 1
  api:
    type: manifests
    path: ./k8s
    paths:
      - ./extra.yml
    depends_on:
      - redis
`
	if _, err := Parse(writeConfig(test, cfg)); err != nil {
		test.Fatalf("A config using every supported key must parse, got: %v", err)
	}
}

// TestParseEmptyConfigFallsThroughToValidation ensures an empty or comment-only
// file surfaces the actionable "cluster name is required" validation error
// rather than an EOF parse error from the strict decoder.
func TestParseEmptyConfigFallsThroughToValidation(test *testing.T) {
	for _, tt := range []struct{ name, content string }{
		{"empty file", ""},
		{"comments only", "# nothing here\n"},
	} {
		test.Run(tt.name, func(test *testing.T) {
			_, err := Parse(writeConfig(test, tt.content))
			if err == nil {
				test.Fatal("Expected a validation error, got nil")
			}
			if !strings.Contains(err.Error(), "cluster name is required") {
				test.Errorf("Expected the missing-cluster-name validation error, got: %v", err)
			}
			if strings.Contains(err.Error(), "EOF") {
				test.Errorf("Empty config leaked an EOF parse error: %v", err)
			}
		})
	}
}

// TestParseMultipleRejectsUnknownFields confirms the merge path (ParseMultiple)
// decodes strictly too, not just the single-file Parse path.
func TestParseMultipleRejectsUnknownFields(test *testing.T) {
	dir := test.TempDir()

	good := filepath.Join(dir, "a.yml")
	if err := os.WriteFile(good, []byte("cluster:\n  name: dev\nservices:\n  a:\n    type: manifests\n    path: ./k8s\n"), 0644); err != nil {
		test.Fatal(err)
	}
	bad := filepath.Join(dir, "b.yml")
	if err := os.WriteFile(bad, []byte("cluster:\n  name: dev\nservices:\n  b:\n    type: manifests\n    path: ./k8s\n    waitt: true\n"), 0644); err != nil {
		test.Fatal(err)
	}

	_, err := ParseMultiple([]string{good, bad})
	if err == nil {
		test.Fatal("Expected an error for the unknown key in the second file, got nil")
	}
	if !strings.Contains(err.Error(), "waitt") {
		test.Errorf("Expected error to mention 'waitt', got: %v", err)
	}
}

// obsoleteUnmarshaler matches the yaml.v3 legacy custom-unmarshaler interface
// (as implemented by ValuesField).
type obsoleteUnmarshaler interface {
	UnmarshalYAML(unmarshal func(interface{}) error) error
}

// TestConfigSectionNamesIsComplete walks the Config type graph and requires
// every decoded struct to have a friendly section name. Without this, adding a
// new config struct would silently reintroduce "in type config.Foo" Go type
// names into user-facing validation errors.
func TestConfigSectionNamesIsComplete(test *testing.T) {
	seen := map[reflect.Type]bool{}

	var walk func(reflect.Type)
	walk = func(typ reflect.Type) {
		for typ.Kind() == reflect.Pointer || typ.Kind() == reflect.Slice || typ.Kind() == reflect.Map {
			typ = typ.Elem()
		}
		if typ.Kind() != reflect.Struct || seen[typ] {
			return
		}
		seen[typ] = true

		// Types with a custom unmarshaler never reach the strict field check.
		if reflect.PointerTo(typ).Implements(reflect.TypeOf((*obsoleteUnmarshaler)(nil)).Elem()) {
			return
		}

		name := typ.String()
		if _, ok := configSectionNames[name]; !ok {
			test.Errorf("Type %s is decoded from kraze.yml but has no entry in configSectionNames; "+
				"add one so strict-decoding errors name the config section instead of the Go type", name)
		}

		for i := 0; i < typ.NumField(); i++ {
			if typ.Field(i).Tag.Get("yaml") != "-" {
				walk(typ.Field(i).Type)
			}
		}
	}

	walk(reflect.TypeOf(Config{}))
}
