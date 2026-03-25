package providers

import (
	"testing"

	"github.com/hjames9/kraze/internal/config"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
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

func TestIsPodReady(test *testing.T) {
	tests := []struct {
		name       string
		phase      string
		conditions []map[string]interface{}
		wantReady  bool
	}{
		{
			name:      "Succeeded is ready (batch/job pod completed)",
			phase:     "Succeeded",
			wantReady: true,
		},
		{
			name:  "Running with Ready=True is ready (service pod)",
			phase: "Running",
			conditions: []map[string]interface{}{
				{"type": "Ready", "status": "True"},
			},
			wantReady: true,
		},
		{
			name:  "Running with Ready=False is not ready",
			phase: "Running",
			conditions: []map[string]interface{}{
				{"type": "Ready", "status": "False"},
			},
			wantReady: false,
		},
		{
			name:      "Running with no conditions is not ready",
			phase:     "Running",
			wantReady: false,
		},
		{
			name:      "Pending is not ready",
			phase:     "Pending",
			wantReady: false,
		},
		{
			name:      "Failed is not ready",
			phase:     "Failed",
			wantReady: false,
		},
		{
			name:      "Unknown is not ready",
			phase:     "Unknown",
			wantReady: false,
		},
	}

	for _, tt := range tests {
		test.Run(tt.name, func(test *testing.T) {
			obj := &unstructured.Unstructured{}
			status := map[string]interface{}{
				"phase": tt.phase,
			}
			if len(tt.conditions) > 0 {
				conds := make([]interface{}, len(tt.conditions))
				for i, c := range tt.conditions {
					conds[i] = c
				}
				status["conditions"] = conds
			}

			ready, err := isPodReady(obj, status)
			if err != nil {
				test.Fatalf("unexpected error: %v", err)
			}
			if ready != tt.wantReady {
				test.Errorf("isPodReady(%q) = %v, want %v", tt.phase, ready, tt.wantReady)
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
