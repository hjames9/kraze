package providers

import (
	"testing"

	"github.com/hjames9/kraze/internal/config"
)

func TestNewProvider(test *testing.T) {
	tests := []struct {
		name        string
		service     *config.ServiceConfig
		expectError bool
	}{
		{
			name: "unsupported provider",
			service: &config.ServiceConfig{
				Name: "app",
				Type: "kustomize",
			},
			expectError: true,
		},
	}

	for _, tt := range tests {
		test.Run(tt.name, func(test *testing.T) {
			opts := &ProviderOptions{
				ClusterName: "test-cluster",
				KubeConfig:  "fake-kubeconfig",
				Verbose:     true,
			}

			_, err := NewProvider(tt.service, opts)

			if tt.expectError {
				if err == nil {
					test.Error("Expected error, got nil")
				}
				return
			}

			if err != nil {
				test.Errorf("Unexpected error: %v", err)
			}
		})
	}
}

// Note: Testing actual Helm and Manifests provider creation requires valid kubeconfig
// These are tested through integration tests or in actual cluster environments

func TestServiceStatus(test *testing.T) {
	status := &ServiceStatus{
		Name:      "test-service",
		Installed: true,
		Ready:     true,
		Message:   "Running",
	}

	if status.Name != "test-service" {
		test.Errorf("Expected name 'test-service', got '%s'", status.Name)
	}

	if !status.Installed {
		test.Error("Expected Installed to be true")
	}

	if !status.Ready {
		test.Error("Expected Ready to be true")
	}

	if status.Message != "Running" {
		test.Errorf("Expected message 'Running', got '%s'", status.Message)
	}
}
