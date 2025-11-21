package config

import (
	"os"
	"testing"
)

func TestExpandEnvVars(test *testing.T) {
	tests := []struct {
		name     string
		input    string
		envVars  map[string]string
		expected string
	}{
		{
			name:     "simple substitution",
			input:    "cluster-${ENV}",
			envVars:  map[string]string{"ENV": "dev"},
			expected: "cluster-dev",
		},
		{
			name:     "substitution with default",
			input:    "cluster-${ENV:-production}",
			envVars:  map[string]string{},
			expected: "cluster-production",
		},
		{
			name:     "substitution overrides default",
			input:    "cluster-${ENV:-production}",
			envVars:  map[string]string{"ENV": "dev"},
			expected: "cluster-dev",
		},
		{
			name:     "multiple substitutions",
			input:    "${PREFIX}-${SUFFIX}",
			envVars:  map[string]string{"PREFIX": "app", "SUFFIX": "v1"},
			expected: "app-v1",
		},
		{
			name:     "no substitution needed",
			input:    "static-value",
			envVars:  map[string]string{},
			expected: "static-value",
		},
		{
			name:     "empty default",
			input:    "value-${MISSING:-}",
			envVars:  map[string]string{},
			expected: "value-",
		},
		{
			name:     "numeric in var name",
			input:    "${VAR_123}",
			envVars:  map[string]string{"VAR_123": "test"},
			expected: "test",
		},
	}

	for _, tt := range tests {
		test.Run(tt.name, func(test *testing.T) {
			// Set environment variables
			for key, val := range tt.envVars {
				os.Setenv(key, val)
				defer os.Unsetenv(key)
			}

			result := ExpandEnvVars(tt.input)
			if result != tt.expected {
				test.Errorf("Expected '%s', got '%s'", tt.expected, result)
			}
		})
	}
}

func TestExpandEnvVarsInStruct(test *testing.T) {
	os.Setenv("CLUSTER_NAME", "test-cluster")
	os.Setenv("REDIS_NAMESPACE", "data")
	defer os.Unsetenv("CLUSTER_NAME")
	defer os.Unsetenv("REDIS_NAMESPACE")

	// Test that expansion works when manually applied
	clusterName := ExpandEnvVars("${CLUSTER_NAME}")
	if clusterName != "test-cluster" {
		test.Errorf("Expected cluster name 'test-cluster', got '%s'", clusterName)
	}

	namespace := ExpandEnvVars("${REDIS_NAMESPACE:-default}")
	if namespace != "data" {
		test.Errorf("Expected namespace 'data', got '%s'", namespace)
	}

	chart := ExpandEnvVars("${CHART:-redis}")
	if chart != "redis" {
		test.Errorf("Expected chart 'redis' (from default), got '%s'", chart)
	}
}
