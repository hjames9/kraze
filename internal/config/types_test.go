package config

import (
	"testing"
)

func TestServiceConfigGetNamespace(test *testing.T) {
	tests := []struct {
		name     string
		svc      ServiceConfig
		expected string
	}{
		{
			name:     "explicit namespace",
			svc:      ServiceConfig{Namespace: "custom"},
			expected: "custom",
		},
		{
			name:     "default namespace",
			svc:      ServiceConfig{},
			expected: "default",
		},
	}

	for _, tt := range tests {
		test.Run(tt.name, func(test *testing.T) {
			result := tt.svc.GetNamespace()
			if result != tt.expected {
				test.Errorf("Expected '%s', got '%s'", tt.expected, result)
			}
		})
	}
}

func TestServiceConfigIsHelm(test *testing.T) {
	tests := []struct {
		name     string
		svc      ServiceConfig
		expected bool
	}{
		{
			name:     "helm service",
			svc:      ServiceConfig{Type: "helm"},
			expected: true,
		},
		{
			name:     "manifests service",
			svc:      ServiceConfig{Type: "manifests"},
			expected: false,
		},
	}

	for _, tt := range tests {
		test.Run(tt.name, func(test *testing.T) {
			result := tt.svc.IsHelm()
			if result != tt.expected {
				test.Errorf("Expected %v, got %v", tt.expected, result)
			}
		})
	}
}

func TestServiceConfigIsManifests(test *testing.T) {
	tests := []struct {
		name     string
		svc      ServiceConfig
		expected bool
	}{
		{
			name:     "manifests service",
			svc:      ServiceConfig{Type: "manifests"},
			expected: true,
		},
		{
			name:     "helm service",
			svc:      ServiceConfig{Type: "helm"},
			expected: false,
		},
	}

	for _, tt := range tests {
		test.Run(tt.name, func(test *testing.T) {
			result := tt.svc.IsManifests()
			if result != tt.expected {
				test.Errorf("Expected %v, got %v", tt.expected, result)
			}
		})
	}
}

func TestServiceConfigIsLocalChart(test *testing.T) {
	tests := []struct {
		name     string
		svc      ServiceConfig
		expected bool
	}{
		{
			name:     "local chart with path",
			svc:      ServiceConfig{Type: "helm", Path: "./charts/mychart"},
			expected: true,
		},
		{
			name:     "remote chart",
			svc:      ServiceConfig{Type: "helm", Chart: "redis", Repo: "bitnami"},
			expected: false,
		},
		{
			name:     "manifests",
			svc:      ServiceConfig{Type: "manifests", Path: "./manifests"},
			expected: false,
		},
	}

	for _, tt := range tests {
		test.Run(tt.name, func(test *testing.T) {
			result := tt.svc.IsLocalChart()
			if result != tt.expected {
				test.Errorf("Expected %v, got %v", tt.expected, result)
			}
		})
	}
}

func TestServiceConfigIsRemoteChart(test *testing.T) {
	tests := []struct {
		name     string
		svc      ServiceConfig
		expected bool
	}{
		{
			name:     "remote chart",
			svc:      ServiceConfig{Type: "helm", Chart: "redis", Repo: "bitnami"},
			expected: true,
		},
		{
			name:     "local chart",
			svc:      ServiceConfig{Type: "helm", Path: "./charts/mychart"},
			expected: false,
		},
		{
			name:     "helm without chart specified",
			svc:      ServiceConfig{Type: "helm"},
			expected: false,
		},
	}

	for _, tt := range tests {
		test.Run(tt.name, func(test *testing.T) {
			result := tt.svc.IsRemoteChart()
			if result != tt.expected {
				test.Errorf("Expected %v, got %v", tt.expected, result)
			}
		})
	}
}

func TestGPUConfigIsNvidiaEnabled(test *testing.T) {
	tests := []struct {
		name     string
		gpu      *GPUConfig
		expected bool
	}{
		{name: "nil GPU config", gpu: nil, expected: false},
		{name: "no nvidia section", gpu: &GPUConfig{}, expected: false},
		{name: "nvidia disabled", gpu: &GPUConfig{Nvidia: &GPUVendorConfig{Enabled: false}}, expected: false},
		{name: "nvidia enabled", gpu: &GPUConfig{Nvidia: &GPUVendorConfig{Enabled: true}}, expected: true},
	}
	for _, tt := range tests {
		test.Run(tt.name, func(test *testing.T) {
			if result := tt.gpu.IsNvidiaEnabled(); result != tt.expected {
				test.Errorf("IsNvidiaEnabled() = %v, want %v", result, tt.expected)
			}
		})
	}
}

func TestGPUConfigIsAMDEnabled(test *testing.T) {
	tests := []struct {
		name     string
		gpu      *GPUConfig
		expected bool
	}{
		{name: "nil GPU config", gpu: nil, expected: false},
		{name: "no amd section", gpu: &GPUConfig{}, expected: false},
		{name: "amd disabled", gpu: &GPUConfig{AMD: &GPUVendorConfig{Enabled: false}}, expected: false},
		{name: "amd enabled", gpu: &GPUConfig{AMD: &GPUVendorConfig{Enabled: true}}, expected: true},
	}
	for _, tt := range tests {
		test.Run(tt.name, func(test *testing.T) {
			if result := tt.gpu.IsAMDEnabled(); result != tt.expected {
				test.Errorf("IsAMDEnabled() = %v, want %v", result, tt.expected)
			}
		})
	}
}

func TestGPUConfigIsAnyEnabled(test *testing.T) {
	tests := []struct {
		name     string
		gpu      *GPUConfig
		expected bool
	}{
		{name: "nil GPU config", gpu: nil, expected: false},
		{name: "neither enabled", gpu: &GPUConfig{}, expected: false},
		{name: "nvidia only", gpu: &GPUConfig{Nvidia: &GPUVendorConfig{Enabled: true}}, expected: true},
		{name: "amd only", gpu: &GPUConfig{AMD: &GPUVendorConfig{Enabled: true}}, expected: true},
		{
			name: "both enabled",
			gpu: &GPUConfig{
				Nvidia: &GPUVendorConfig{Enabled: true},
				AMD:    &GPUVendorConfig{Enabled: true},
			},
			expected: true,
		},
	}
	for _, tt := range tests {
		test.Run(tt.name, func(test *testing.T) {
			if result := tt.gpu.IsAnyEnabled(); result != tt.expected {
				test.Errorf("IsAnyEnabled() = %v, want %v", result, tt.expected)
			}
		})
	}
}

func TestConfigValidate(test *testing.T) {
	tests := []struct {
		name    string
		cfg     *Config
		wantErr bool
	}{
		{
			name: "valid config",
			cfg: &Config{
				Cluster: ClusterConfig{Name: "test"},
				Services: map[string]ServiceConfig{
					"redis": {Name: "redis", Type: "helm", Chart: "redis", Repo: "bitnami"},
				},
			},
			wantErr: false,
		},
		{
			name: "missing cluster name",
			cfg: &Config{
				Services: map[string]ServiceConfig{
					"redis": {Name: "redis", Type: "helm"},
				},
			},
			wantErr: true,
		},
		{
			name: "no services",
			cfg: &Config{
				Cluster:  ClusterConfig{Name: "test"},
				Services: map[string]ServiceConfig{},
			},
			wantErr: false, // Empty services is allowed
		},
		{
			name: "invalid service type",
			cfg: &Config{
				Cluster: ClusterConfig{Name: "test"},
				Services: map[string]ServiceConfig{
					"app": {Name: "app", Type: "invalid"},
				},
			},
			wantErr: true,
		},
		{
			name: "helm without chart or path",
			cfg: &Config{
				Cluster: ClusterConfig{Name: "test"},
				Services: map[string]ServiceConfig{
					"app": {Name: "app", Type: "helm"},
				},
			},
			wantErr: true,
		},
		{
			name: "manifests without path",
			cfg: &Config{
				Cluster: ClusterConfig{Name: "test"},
				Services: map[string]ServiceConfig{
					"app": {Name: "app", Type: "manifests"},
				},
			},
			wantErr: true,
		},
		{
			name: "nvidia gpu enabled",
			cfg: &Config{
				Cluster: ClusterConfig{
					Name: "test",
					GPU:  &GPUConfig{Nvidia: &GPUVendorConfig{Enabled: true}},
				},
				Services: map[string]ServiceConfig{},
			},
			wantErr: false,
		},
		{
			name: "amd gpu enabled",
			cfg: &Config{
				Cluster: ClusterConfig{
					Name: "test",
					GPU:  &GPUConfig{AMD: &GPUVendorConfig{Enabled: true}},
				},
				Services: map[string]ServiceConfig{},
			},
			wantErr: false,
		},
		{
			name: "both nvidia and amd enabled",
			cfg: &Config{
				Cluster: ClusterConfig{
					Name: "test",
					GPU: &GPUConfig{
						Nvidia: &GPUVendorConfig{Enabled: true},
						AMD:    &GPUVendorConfig{Enabled: true},
					},
				},
				Services: map[string]ServiceConfig{},
			},
			wantErr: false,
		},
		{
			name: "nvidia disabled",
			cfg: &Config{
				Cluster: ClusterConfig{
					Name: "test",
					GPU:  &GPUConfig{Nvidia: &GPUVendorConfig{Enabled: false}},
				},
				Services: map[string]ServiceConfig{},
			},
			wantErr: false,
		},
		{
			name: "nvidia gpu enabled on external cluster",
			cfg: &Config{
				Cluster: ClusterConfig{
					Name:     "test",
					GPU:      &GPUConfig{Nvidia: &GPUVendorConfig{Enabled: true}},
					External: &ExternalClusterConfig{Enabled: true},
				},
				Services: map[string]ServiceConfig{},
			},
			wantErr: true,
		},
		{
			name: "amd gpu enabled on external cluster",
			cfg: &Config{
				Cluster: ClusterConfig{
					Name:     "test",
					GPU:      &GPUConfig{AMD: &GPUVendorConfig{Enabled: true}},
					External: &ExternalClusterConfig{Enabled: true},
				},
				Services: map[string]ServiceConfig{},
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		test.Run(tt.name, func(test *testing.T) {
			err := tt.cfg.Validate()
			if (err != nil) != tt.wantErr {
				test.Errorf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}
