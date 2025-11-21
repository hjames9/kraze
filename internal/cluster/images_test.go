package cluster

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/hjames9/kraze/internal/config"
)

func TestParseImageReference(test *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected ImageReference
	}{
		{
			name:  "simple image with tag",
			input: "nginx:latest",
			expected: ImageReference{
				Registry:   "docker.io",
				Repository: "library/nginx",
				Tag:        "latest",
				Original:   "nginx:latest",
			},
		},
		{
			name:  "simple image without tag",
			input: "redis",
			expected: ImageReference{
				Registry:   "docker.io",
				Repository: "library/redis",
				Tag:        "latest",
				Original:   "redis",
			},
		},
		{
			name:  "image with registry and tag",
			input: "gcr.io/my-project/myapp:v1.2.3",
			expected: ImageReference{
				Registry:   "gcr.io",
				Repository: "my-project/myapp",
				Tag:        "v1.2.3",
				Original:   "gcr.io/my-project/myapp:v1.2.3",
			},
		},
		{
			name:  "docker hub with explicit registry",
			input: "docker.io/bitnami/redis:7.0",
			expected: ImageReference{
				Registry:   "docker.io",
				Repository: "bitnami/redis",
				Tag:        "7.0",
				Original:   "docker.io/bitnami/redis:7.0",
			},
		},
		{
			name:  "image with digest",
			input: "nginx@sha256:abc123def456",
			expected: ImageReference{
				Registry:   "docker.io",
				Repository: "library/nginx",
				Tag:        "latest",
				Digest:     "sha256:abc123def456",
				Original:   "nginx@sha256:abc123def456",
			},
		},
		{
			name:  "image with tag and digest",
			input: "redis:7.0@sha256:xyz789",
			expected: ImageReference{
				Registry:   "docker.io",
				Repository: "library/redis",
				Tag:        "7.0",
				Digest:     "sha256:xyz789",
				Original:   "redis:7.0@sha256:xyz789",
			},
		},
		{
			name:  "localhost registry",
			input: "localhost:5000/myapp:dev",
			expected: ImageReference{
				Registry:   "localhost:5000",
				Repository: "myapp",
				Tag:        "dev",
				Original:   "localhost:5000/myapp:dev",
			},
		},
		{
			name:  "private registry with port",
			input: "registry.example.com:443/team/app:v2",
			expected: ImageReference{
				Registry:   "registry.example.com:443",
				Repository: "team/app",
				Tag:        "v2",
				Original:   "registry.example.com:443/team/app:v2",
			},
		},
		{
			name:  "user/repo format",
			input: "myuser/myrepo:latest",
			expected: ImageReference{
				Registry:   "docker.io",
				Repository: "myuser/myrepo",
				Tag:        "latest",
				Original:   "myuser/myrepo:latest",
			},
		},
		{
			name:  "gcr.io without tag",
			input: "gcr.io/project/image",
			expected: ImageReference{
				Registry:   "gcr.io",
				Repository: "project/image",
				Tag:        "latest",
				Original:   "gcr.io/project/image",
			},
		},
	}

	for _, tt := range tests {
		test.Run(tt.name, func(test *testing.T) {
			result := ParseImageReference(tt.input)

			if result.Registry != tt.expected.Registry {
				test.Errorf("Registry: got %q, want %q", result.Registry, tt.expected.Registry)
			}
			if result.Repository != tt.expected.Repository {
				test.Errorf("Repository: got %q, want %q", result.Repository, tt.expected.Repository)
			}
			if result.Tag != tt.expected.Tag {
				test.Errorf("Tag: got %q, want %q", result.Tag, tt.expected.Tag)
			}
			if result.Digest != tt.expected.Digest {
				test.Errorf("Digest: got %q, want %q", result.Digest, tt.expected.Digest)
			}
			if result.Original != tt.expected.Original {
				test.Errorf("Original: got %q, want %q", result.Original, tt.expected.Original)
			}
		})
	}
}

func TestImageReferenceString(test *testing.T) {
	tests := []struct {
		name     string
		ref      ImageReference
		expected string
	}{
		{
			name: "simple image",
			ref: ImageReference{
				Registry:   "docker.io",
				Repository: "library/nginx",
				Tag:        "latest",
			},
			expected: "library/nginx:latest",
		},
		{
			name: "with custom registry",
			ref: ImageReference{
				Registry:   "gcr.io",
				Repository: "project/app",
				Tag:        "v1.0",
			},
			expected: "gcr.io/project/app:v1.0",
		},
		{
			name: "with digest",
			ref: ImageReference{
				Registry:   "docker.io",
				Repository: "library/redis",
				Tag:        "7.0",
				Digest:     "sha256:abc123",
			},
			expected: "library/redis:7.0@sha256:abc123",
		},
		{
			name: "original preserved",
			ref: ImageReference{
				Original: "myapp:dev",
			},
			expected: "myapp:dev",
		},
	}

	for _, tt := range tests {
		test.Run(tt.name, func(test *testing.T) {
			result := tt.ref.String()
			if result != tt.expected {
				test.Errorf("String(): got %q, want %q", result, tt.expected)
			}
		})
	}
}

func TestImageReferenceIsDockerHub(test *testing.T) {
	tests := []struct {
		name     string
		ref      ImageReference
		expected bool
	}{
		{
			name:     "docker.io registry",
			ref:      ImageReference{Registry: "docker.io"},
			expected: true,
		},
		{
			name:     "empty registry",
			ref:      ImageReference{Registry: ""},
			expected: true,
		},
		{
			name:     "gcr.io registry",
			ref:      ImageReference{Registry: "gcr.io"},
			expected: false,
		},
		{
			name:     "localhost registry",
			ref:      ImageReference{Registry: "localhost:5000"},
			expected: false,
		},
	}

	for _, tt := range tests {
		test.Run(tt.name, func(test *testing.T) {
			result := tt.ref.IsDockerHub()
			if result != tt.expected {
				test.Errorf("IsDockerHub(): got %v, want %v", result, tt.expected)
			}
		})
	}
}

func TestExtractImagesFromValues(test *testing.T) {
	// Create a temporary values file
	tmpDir := test.TempDir()
	valuesFile := filepath.Join(tmpDir, "values.yaml")

	valuesContent := `
image:
  registry: docker.io
  repository: myapp/backend
  tag: v1.2.3

redis:
  image:
    repository: redis
    tag: 7.0

postgres:
  image:
    repository: postgres
    tag: "15"

nginx:
  image:
    repository: nginx
    # No tag specified - should default to latest

sidecar:
  image:
    registry: gcr.io
    repository: project/sidecar
    tag: latest
`

	if err := os.WriteFile(valuesFile, []byte(valuesContent), 0644); err != nil {
		test.Fatalf("Failed to create test values file: %v", err)
	}

	im := NewImageManager(false)
	images, err := im.ExtractImagesFromValues(valuesFile)
	if err != nil {
		test.Fatalf("ExtractImagesFromValues() error: %v", err)
	}

	// We expect at least these images to be detected
	// Note: YAML parses 7.0 as float64, which becomes "7" when converted to string
	expectedImages := map[string]bool{
		"docker.io/myapp/backend:v1.2.3": false,
		"redis:7":                        false, // 7.0 becomes "7" after float conversion
		"postgres:15":                    false,
		"nginx:latest":                   false,
		"gcr.io/project/sidecar:latest":  false,
	}

	for _, img := range images {
		if _, exists := expectedImages[img]; exists {
			expectedImages[img] = true
		}
	}

	for img, found := range expectedImages {
		if !found {
			test.Errorf("Expected image %q not found in extracted images: %v", img, images)
		}
	}
}

func TestExtractImagesFromManifest(test *testing.T) {
	im := NewImageManager(false)

	manifest := `
apiVersion: apps/v1
kind: Deployment
metadata:
  name: myapp
spec:
  template:
    spec:
      containers:
      - name: app
        image: myapp:v1.0
      - name: sidecar
        image: nginx:alpine
      initContainers:
      - name: init
        image: busybox:latest
---
apiVersion: batch/v1
kind: Job
metadata:
  name: migration
spec:
  template:
    spec:
      containers:
      - name: migrate
        image: myapp/migrations:v2.0
`

	images := im.extractImagesFromManifest(manifest)

	expectedImages := []string{
		"myapp:v1.0",
		"nginx:alpine",
		"busybox:latest",
		"myapp/migrations:v2.0",
	}

	if len(images) != len(expectedImages) {
		test.Errorf("Expected %d images, got %d: %v", len(expectedImages), len(images), images)
	}

	for _, expected := range expectedImages {
		found := false
		for _, img := range images {
			if img == expected {
				found = true
				break
			}
		}
		if !found {
			test.Errorf("Expected image %q not found in: %v", expected, images)
		}
	}
}

func TestExtractImagesFromManifests(test *testing.T) {
	// Create temporary manifest files
	tmpDir := test.TempDir()

	deployment := `
apiVersion: apps/v1
kind: Deployment
metadata:
  name: app
spec:
  template:
    spec:
      containers:
      - name: app
        image: myapp:v1.0
`

	service := `
apiVersion: v1
kind: Service
metadata:
  name: app
spec:
  selector:
    app: myapp
  ports:
  - port: 80
`

	job := `
apiVersion: batch/v1
kind: Job
metadata:
  name: setup
spec:
  template:
    spec:
      containers:
      - name: setup
        image: setup-tool:latest
`

	deploymentFile := filepath.Join(tmpDir, "deployment.yaml")
	serviceFile := filepath.Join(tmpDir, "service.yaml")
	jobFile := filepath.Join(tmpDir, "job.yaml")

	if err := os.WriteFile(deploymentFile, []byte(deployment), 0644); err != nil {
		test.Fatalf("Failed to create deployment file: %v", err)
	}
	if err := os.WriteFile(serviceFile, []byte(service), 0644); err != nil {
		test.Fatalf("Failed to create service file: %v", err)
	}
	if err := os.WriteFile(jobFile, []byte(job), 0644); err != nil {
		test.Fatalf("Failed to create job file: %v", err)
	}

	im := NewImageManager(false)

	// Test with directory
	images, err := im.ExtractImagesFromManifests([]string{tmpDir})
	if err != nil {
		test.Fatalf("ExtractImagesFromManifests() error: %v", err)
	}

	expectedImages := []string{"myapp:v1.0", "setup-tool:latest"}
	if len(images) != len(expectedImages) {
		test.Errorf("Expected %d images, got %d: %v", len(expectedImages), len(images), images)
	}

	// Test with specific files
	images, err = im.ExtractImagesFromManifests([]string{deploymentFile, jobFile})
	if err != nil {
		test.Fatalf("ExtractImagesFromManifests() with files error: %v", err)
	}

	if len(images) != len(expectedImages) {
		test.Errorf("Expected %d images from files, got %d: %v", len(expectedImages), len(images), images)
	}
}

func TestDeduplicateImages(test *testing.T) {
	tests := []struct {
		name     string
		input    []string
		expected []string
	}{
		{
			name:     "no duplicates",
			input:    []string{"nginx:latest", "redis:7.0", "postgres:15"},
			expected: []string{"nginx:latest", "redis:7.0", "postgres:15"},
		},
		{
			name:     "with duplicates",
			input:    []string{"nginx:latest", "redis:7.0", "nginx:latest", "postgres:15", "redis:7.0"},
			expected: []string{"nginx:latest", "redis:7.0", "postgres:15"},
		},
		{
			name:     "all duplicates",
			input:    []string{"myapp:dev", "myapp:dev", "myapp:dev"},
			expected: []string{"myapp:dev"},
		},
		{
			name:     "empty list",
			input:    []string{},
			expected: []string{},
		},
	}

	for _, tt := range tests {
		test.Run(tt.name, func(test *testing.T) {
			result := DeduplicateImages(tt.input)

			if len(result) != len(tt.expected) {
				test.Errorf("DeduplicateImages(): got %d images, want %d", len(result), len(tt.expected))
			}

			// Check that all expected images are present
			for _, expected := range tt.expected {
				found := false
				for _, img := range result {
					if img == expected {
						found = true
						break
					}
				}
				if !found {
					test.Errorf("DeduplicateImages(): expected image %q not found in result: %v", expected, result)
				}
			}
		})
	}
}

func TestGetImagesForService_Manifests(test *testing.T) {
	// Create temporary manifest file
	tmpDir := test.TempDir()
	manifestFile := filepath.Join(tmpDir, "deployment.yaml")

	manifestContent := `
apiVersion: apps/v1
kind: Deployment
metadata:
  name: myapp
spec:
  template:
    spec:
      containers:
      - name: app
        image: myapp:v1.0
      - name: sidecar
        image: nginx:alpine
`

	if err := os.WriteFile(manifestFile, []byte(manifestContent), 0644); err != nil {
		test.Fatalf("Failed to create manifest file: %v", err)
	}

	svc := &config.ServiceConfig{
		Name:      "myapp",
		Type:      "manifests",
		Path:      manifestFile,
		Namespace: "default",
	}

	im := NewImageManager(false)
	ctx := context.Background()

	images, err := im.GetImagesForService(ctx, svc, "")
	if err != nil {
		test.Fatalf("GetImagesForService() error: %v", err)
	}

	expectedImages := []string{"myapp:v1.0", "nginx:alpine"}
	if len(images) != len(expectedImages) {
		test.Errorf("Expected %d images, got %d: %v", len(expectedImages), len(images), images)
	}

	for _, expected := range expectedImages {
		found := false
		for _, img := range images {
			if img == expected {
				found = true
				break
			}
		}
		if !found {
			test.Errorf("Expected image %q not found in: %v", expected, images)
		}
	}
}

func TestGetImagesForService_Helm(test *testing.T) {
	// Create temporary values file
	tmpDir := test.TempDir()
	valuesFile := filepath.Join(tmpDir, "values.yaml")

	valuesContent := `
image:
  repository: myapp
  tag: v1.0

database:
  image:
    repository: postgres
    tag: "15"
`

	if err := os.WriteFile(valuesFile, []byte(valuesContent), 0644); err != nil {
		test.Fatalf("Failed to create values file: %v", err)
	}

	svc := &config.ServiceConfig{
		Name:      "myapp",
		Type:      "helm",
		Chart:     "myapp",
		Repo:      "https://charts.example.com",
		Values:    valuesFile,
		Namespace: "default",
	}

	im := NewImageManager(false)
	ctx := context.Background()

	images, err := im.GetImagesForService(ctx, svc, "")
	if err != nil {
		test.Fatalf("GetImagesForService() error: %v", err)
	}

	// Should at least detect the images from values
	if len(images) < 2 {
		test.Errorf("Expected at least 2 images, got %d: %v", len(images), images)
	}

	// Check for specific images
	expectedImages := []string{"myapp:v1.0", "postgres:15"}
	for _, expected := range expectedImages {
		found := false
		for _, img := range images {
			if img == expected {
				found = true
				break
			}
		}
		if !found {
			test.Errorf("Expected image %q not found in: %v", expected, images)
		}
	}
}

func TestExtractImagesRecursive(test *testing.T) {
	im := NewImageManager(false)

	// Test nested structure
	data := map[string]interface{}{
		"app": map[string]interface{}{
			"image": map[string]interface{}{
				"repository": "myapp",
				"tag":        "v1.0",
			},
		},
		"sidecars": []interface{}{
			map[string]interface{}{
				"image": map[string]interface{}{
					"repository": "nginx",
					"tag":        "alpine",
				},
			},
			map[string]interface{}{
				"image": map[string]interface{}{
					"repository": "redis",
					"tag":        float64(7.0), // YAML numbers parse as float64
				},
			},
		},
	}

	images := make([]string, 0)
	im.extractImagesRecursive(data, &images)

	expectedImages := []string{"myapp:v1.0", "nginx:alpine", "redis:7"}
	if len(images) != len(expectedImages) {
		test.Errorf("Expected %d images, got %d: %v", len(expectedImages), len(images), images)
	}

	for _, expected := range expectedImages {
		found := false
		for _, img := range images {
			if img == expected {
				found = true
				break
			}
		}
		if !found {
			test.Errorf("Expected image %q not found in: %v", expected, images)
		}
	}
}
