package providers

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/hjames9/kraze/internal/config"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestParseManifest(test *testing.T) {
	// Create a minimal provider just for testing parse logic
	mp := &ManifestsProvider{}

	tests := []struct {
		name        string
		manifest    string
		expectError bool
		validate    func(*testing.T, *unstructured.Unstructured)
	}{
		{
			name: "simple deployment",
			manifest: `
apiVersion: apps/v1
kind: Deployment
metadata:
  name: myapp
  namespace: default
spec:
  replicas: 3
`,
			expectError: false,
			validate: func(test *testing.T, obj *unstructured.Unstructured) {
				if obj.GetKind() != "Deployment" {
					test.Errorf("Kind: got %q, want %q", obj.GetKind(), "Deployment")
				}
				if obj.GetName() != "myapp" {
					test.Errorf("Name: got %q, want %q", obj.GetName(), "myapp")
				}
				if obj.GetNamespace() != "default" {
					test.Errorf("Namespace: got %q, want %q", obj.GetNamespace(), "default")
				}
			},
		},
		{
			name: "service",
			manifest: `
apiVersion: v1
kind: Service
metadata:
  name: myservice
spec:
  type: ClusterIP
  ports:
  - port: 80
`,
			expectError: false,
			validate: func(test *testing.T, obj *unstructured.Unstructured) {
				if obj.GetKind() != "Service" {
					test.Errorf("Kind: got %q, want %q", obj.GetKind(), "Service")
				}
				if obj.GetAPIVersion() != "v1" {
					test.Errorf("APIVersion: got %q, want %q", obj.GetAPIVersion(), "v1")
				}
			},
		},
		{
			name:        "invalid YAML",
			manifest:    `{invalid yaml content`,
			expectError: true,
		},
		{
			name:        "empty manifest",
			manifest:    "",
			expectError: false,
			validate: func(test *testing.T, obj *unstructured.Unstructured) {
				if obj != nil {
					test.Error("Expected nil object for empty manifest")
				}
			},
		},
		{
			name: "configmap",
			manifest: `
apiVersion: v1
kind: ConfigMap
metadata:
  name: myconfig
data:
  key1: value1
  key2: value2
`,
			expectError: false,
			validate: func(test *testing.T, obj *unstructured.Unstructured) {
				if obj.GetKind() != "ConfigMap" {
					test.Errorf("Kind: got %q, want %q", obj.GetKind(), "ConfigMap")
				}
				// Check data field exists
				data, found, err := unstructured.NestedMap(obj.Object, "data")
				if err != nil {
					test.Errorf("Failed to get data field: %v", err)
				}
				if !found {
					test.Error("ConfigMap should have data field")
				}
				if len(data) != 2 {
					test.Errorf("ConfigMap data: got %d keys, want 2", len(data))
				}
			},
		},
	}

	for _, tt := range tests {
		test.Run(tt.name, func(test *testing.T) {
			obj, err := mp.parseManifest(tt.manifest)

			if tt.expectError {
				if err == nil {
					test.Error("Expected error but got none")
				}
				return
			}

			if err != nil {
				test.Fatalf("Unexpected error: %v", err)
			}

			if tt.validate != nil {
				tt.validate(test, obj)
			}
		})
	}
}

func TestAddTrackingLabels(test *testing.T) {
	mp := &ManifestsProvider{}

	service := &config.ServiceConfig{
		Name: "test-service",
	}

	obj := &unstructured.Unstructured{}
	obj.SetKind("Deployment")
	obj.SetName("myapp")

	mp.addTrackingLabels(obj, service)

	labels := obj.GetLabels()
	if labels == nil {
		test.Fatal("Labels should not be nil after adding tracking labels")
	}

	if labels[managedByLabel] != "kraze" {
		test.Errorf("managedByLabel: got %q, want %q", labels[managedByLabel], "kraze")
	}

	if labels[serviceLabel] != "test-service" {
		test.Errorf("serviceLabel: got %q, want %q", labels[serviceLabel], "test-service")
	}
}

func TestAddTrackingLabels_PreservesExisting(test *testing.T) {
	mp := &ManifestsProvider{}

	service := &config.ServiceConfig{
		Name: "test-service",
	}

	obj := &unstructured.Unstructured{}
	obj.SetKind("Deployment")
	obj.SetName("myapp")
	obj.SetLabels(map[string]string{
		"app":  "myapp",
		"tier": "backend",
	})

	mp.addTrackingLabels(obj, service)

	labels := obj.GetLabels()

	// Check original labels are preserved
	if labels["app"] != "myapp" {
		test.Errorf("app label: got %q, want %q", labels["app"], "myapp")
	}
	if labels["tier"] != "backend" {
		test.Errorf("tier label: got %q, want %q", labels["tier"], "backend")
	}

	// Check tracking labels are added
	if labels[managedByLabel] != "kraze" {
		test.Errorf("managedByLabel: got %q, want %q", labels[managedByLabel], "kraze")
	}
	if labels[serviceLabel] != "test-service" {
		test.Errorf("serviceLabel: got %q, want %q", labels[serviceLabel], "test-service")
	}
}

func TestLoadManifests(test *testing.T) {
	mp := &ManifestsProvider{}

	// Create temporary directory with test manifests
	tmpDir := test.TempDir()

	deployment := `
apiVersion: apps/v1
kind: Deployment
metadata:
  name: app
spec:
  replicas: 1
`

	service := `
apiVersion: v1
kind: Service
metadata:
  name: app
spec:
  type: ClusterIP
`

	deploymentFile := filepath.Join(tmpDir, "deployment.yaml")
	serviceFile := filepath.Join(tmpDir, "service.yaml")

	if err := os.WriteFile(deploymentFile, []byte(deployment), 0644); err != nil {
		test.Fatalf("Failed to create deployment file: %v", err)
	}
	if err := os.WriteFile(serviceFile, []byte(service), 0644); err != nil {
		test.Fatalf("Failed to create service file: %v", err)
	}

	tests := []struct {
		name          string
		serviceConfig *config.ServiceConfig
		expectError   bool
		expectCount   int
	}{
		{
			name: "single file",
			serviceConfig: &config.ServiceConfig{
				Type: "manifests",
				Path: deploymentFile,
			},
			expectError: false,
			expectCount: 1,
		},
		{
			name: "directory",
			serviceConfig: &config.ServiceConfig{
				Type: "manifests",
				Path: tmpDir,
			},
			expectError: false,
			expectCount: 2, // deployment + service
		},
		{
			name: "multiple paths",
			serviceConfig: &config.ServiceConfig{
				Type:  "manifests",
				Paths: []string{deploymentFile, serviceFile},
			},
			expectError: false,
			expectCount: 2,
		},
		{
			name: "nonexistent file",
			serviceConfig: &config.ServiceConfig{
				Type: "manifests",
				Path: "/nonexistent/file.yaml",
			},
			expectError: true,
		},
	}

	for _, tt := range tests {
		test.Run(tt.name, func(test *testing.T) {
			manifests, err := mp.loadManifests(tt.serviceConfig)

			if tt.expectError {
				if err == nil {
					test.Error("Expected error but got none")
				}
				return
			}

			if err != nil {
				test.Fatalf("Unexpected error: %v", err)
			}

			if len(manifests) != tt.expectCount {
				test.Errorf("Manifest count: got %d, want %d", len(manifests), tt.expectCount)
			}
		})
	}
}

func TestLoadManifests_MultiDocument(test *testing.T) {
	mp := &ManifestsProvider{}

	// Create temporary file with multi-document YAML
	tmpDir := test.TempDir()
	multiDocFile := filepath.Join(tmpDir, "multi.yaml")

	multiDoc := `
apiVersion: apps/v1
kind: Deployment
metadata:
  name: app1
---
apiVersion: v1
kind: Service
metadata:
  name: svc1
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: config1
`

	if err := os.WriteFile(multiDocFile, []byte(multiDoc), 0644); err != nil {
		test.Fatalf("Failed to create multi-doc file: %v", err)
	}

	serviceConfig := &config.ServiceConfig{
		Type: "manifests",
		Path: multiDocFile,
	}

	manifests, err := mp.loadManifests(serviceConfig)
	if err != nil {
		test.Fatalf("loadManifests() error: %v", err)
	}

	// Should have 3 documents
	if len(manifests) != 3 {
		test.Errorf("Manifest count: got %d, want 3", len(manifests))
	}

	// Verify each manifest can be parsed
	for itr, manifest := range manifests {
		obj, err := mp.parseManifest(manifest)
		if err != nil {
			test.Errorf("Document %d parse error: %v", itr+1, err)
		}
		if obj == nil {
			test.Errorf("Document %d: got nil object", itr+1)
		}
	}
}

func TestSplitYAML(test *testing.T) {
	tests := []struct {
		name        string
		input       string
		expectCount int
	}{
		{
			name: "single document",
			input: `
apiVersion: v1
kind: Service
metadata:
  name: test
`,
			expectCount: 1,
		},
		{
			name: "two documents",
			input: `
apiVersion: v1
kind: Service
metadata:
  name: svc1
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: config1
`,
			expectCount: 2,
		},
		{
			name: "three documents with empty",
			input: `
apiVersion: v1
kind: Service
metadata:
  name: svc1
---
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: config1
`,
			expectCount: 2, // Empty document should be skipped
		},
		{
			name:        "empty input",
			input:       "",
			expectCount: 0,
		},
		{
			name: "document with comments",
			input: `
# This is a comment
apiVersion: v1
kind: Service
metadata:
  name: svc1
---
# Another comment
apiVersion: v1
kind: ConfigMap
metadata:
  name: config1
`,
			expectCount: 2,
		},
	}

	mp := &ManifestsProvider{}

	for _, tt := range tests {
		test.Run(tt.name, func(test *testing.T) {
			result := mp.splitYAML(tt.input)

			if len(result) != tt.expectCount {
				test.Errorf("splitYAML(): got %d documents, want %d", len(result), tt.expectCount)
			}
		})
	}
}
