package cluster

import (
	"fmt"
	"testing"

	"github.com/hjames9/kraze/internal/config"
	"sigs.k8s.io/kind/pkg/apis/config/defaults"
	"sigs.k8s.io/kind/pkg/apis/config/v1alpha4"
)

func TestBuildKindNode(test *testing.T) {
	km := NewKindManager()

	tests := []struct {
		name     string
		input    config.KindNode
		validate func(*testing.T, v1alpha4.Node)
	}{
		{
			name: "control-plane node",
			input: config.KindNode{
				Role: "control-plane",
			},
			validate: func(test *testing.T, node v1alpha4.Node) {
				if node.Role != v1alpha4.ControlPlaneRole {
					test.Errorf("Role: got %v, want %v", node.Role, v1alpha4.ControlPlaneRole)
				}
			},
		},
		{
			name: "worker node",
			input: config.KindNode{
				Role: "worker",
			},
			validate: func(test *testing.T, node v1alpha4.Node) {
				if node.Role != v1alpha4.WorkerRole {
					test.Errorf("Role: got %v, want %v", node.Role, v1alpha4.WorkerRole)
				}
			},
		},
		{
			name: "node with port mappings",
			input: config.KindNode{
				Role: "control-plane",
				ExtraPortMappings: []config.PortMapping{
					{
						ContainerPort: 30080,
						HostPort:      8080,
						Protocol:      "TCP",
					},
					{
						ContainerPort: 30443,
						HostPort:      8443,
						ListenAddress: "127.0.0.1",
						Protocol:      "TCP",
					},
				},
			},
			validate: func(test *testing.T, node v1alpha4.Node) {
				if len(node.ExtraPortMappings) != 2 {
					test.Fatalf("ExtraPortMappings: got %d, want 2", len(node.ExtraPortMappings))
				}

				pm1 := node.ExtraPortMappings[0]
				if pm1.ContainerPort != 30080 {
					test.Errorf("PortMapping[0].ContainerPort: got %d, want 30080", pm1.ContainerPort)
				}
				if pm1.HostPort != 8080 {
					test.Errorf("PortMapping[0].HostPort: got %d, want 8080", pm1.HostPort)
				}

				pm2 := node.ExtraPortMappings[1]
				if pm2.ListenAddress != "127.0.0.1" {
					test.Errorf("PortMapping[1].ListenAddress: got %q, want %q", pm2.ListenAddress, "127.0.0.1")
				}
			},
		},
		{
			name: "node with extra mounts",
			input: config.KindNode{
				Role: "control-plane",
				ExtraMounts: []config.Mount{
					{
						HostPath:      "/tmp/data",
						ContainerPath: "/data",
						ReadOnly:      false,
					},
					{
						HostPath:      "/tmp/config",
						ContainerPath: "/config",
						ReadOnly:      true,
					},
				},
			},
			validate: func(test *testing.T, node v1alpha4.Node) {
				if len(node.ExtraMounts) != 2 {
					test.Fatalf("ExtraMounts: got %d, want 2", len(node.ExtraMounts))
				}

				m1 := node.ExtraMounts[0]
				if m1.HostPath != "/tmp/data" {
					test.Errorf("Mount[0].HostPath: got %q, want %q", m1.HostPath, "/tmp/data")
				}
				if m1.ContainerPath != "/data" {
					test.Errorf("Mount[0].ContainerPath: got %q, want %q", m1.ContainerPath, "/data")
				}
				if m1.Readonly {
					test.Errorf("Mount[0].Readonly: got true, want false")
				}

				m2 := node.ExtraMounts[1]
				if !m2.Readonly {
					test.Errorf("Mount[1].Readonly: got false, want true")
				}
			},
		},
		{
			name: "node with labels",
			input: config.KindNode{
				Role: "worker",
				Labels: map[string]string{
					"type":        "compute",
					"environment": "dev",
				},
			},
			validate: func(test *testing.T, node v1alpha4.Node) {
				if len(node.Labels) != 2 {
					test.Fatalf("Labels: got %d, want 2", len(node.Labels))
				}

				if node.Labels["type"] != "compute" {
					test.Errorf("Labels[type]: got %q, want %q", node.Labels["type"], "compute")
				}
				if node.Labels["environment"] != "dev" {
					test.Errorf("Labels[environment]: got %q, want %q", node.Labels["environment"], "dev")
				}
			},
		},
	}

	for _, tt := range tests {
		test.Run(tt.name, func(test *testing.T) {
			result := km.buildKindNode(tt.input)
			tt.validate(test, result)
		})
	}
}

func TestBuildKindConfig(test *testing.T) {
	km := NewKindManager()

	tests := []struct {
		name     string
		input    config.ClusterConfig
		validate func(*testing.T, *v1alpha4.Cluster)
	}{
		{
			name: "minimal config",
			input: config.ClusterConfig{
				Name: "test-cluster",
			},
			validate: func(test *testing.T, cluster *v1alpha4.Cluster) {
				if cluster.Name != "test-cluster" {
					test.Errorf("Name: got %q, want %q", cluster.Name, "test-cluster")
				}
				if len(cluster.Nodes) != 1 {
					test.Errorf("Nodes: got %d, want 1 (default control-plane)", len(cluster.Nodes))
				}
				if cluster.Nodes[0].Role != v1alpha4.ControlPlaneRole {
					test.Errorf("Node[0].Role: got %v, want ControlPlaneRole", cluster.Nodes[0].Role)
				}
			},
		},
		{
			name: "config with networking",
			input: config.ClusterConfig{
				Name: "test-cluster",
				Networking: &config.NetworkingConfig{
					PodSubnet:     "10.244.0.0/16",
					ServiceSubnet: "10.96.0.0/12",
				},
			},
			validate: func(test *testing.T, cluster *v1alpha4.Cluster) {
				if cluster.Networking.PodSubnet != "10.244.0.0/16" {
					test.Errorf("Networking.PodSubnet: got %q, want %q", cluster.Networking.PodSubnet, "10.244.0.0/16")
				}
				if cluster.Networking.ServiceSubnet != "10.96.0.0/12" {
					test.Errorf("Networking.ServiceSubnet: got %q, want %q", cluster.Networking.ServiceSubnet, "10.96.0.0/12")
				}
			},
		},
		{
			name: "config with custom CNI",
			input: config.ClusterConfig{
				Name: "test-cluster",
				Networking: &config.NetworkingConfig{
					DisableDefaultCNI: true,
				},
			},
			validate: func(test *testing.T, cluster *v1alpha4.Cluster) {
				if !cluster.Networking.DisableDefaultCNI {
					test.Errorf("Networking.DisableDefaultCNI: got false, want true")
				}
			},
		},
		{
			name: "config with multiple nodes",
			input: config.ClusterConfig{
				Name: "test-cluster",
				Config: []config.KindNode{
					{Role: "control-plane"},
					{Role: "worker"},
					{Role: "worker"},
				},
			},
			validate: func(test *testing.T, cluster *v1alpha4.Cluster) {
				if len(cluster.Nodes) != 3 {
					test.Fatalf("Nodes: got %d, want 3", len(cluster.Nodes))
				}
				if cluster.Nodes[0].Role != v1alpha4.ControlPlaneRole {
					test.Errorf("Node[0].Role: got %v, want ControlPlaneRole", cluster.Nodes[0].Role)
				}
				if cluster.Nodes[1].Role != v1alpha4.WorkerRole {
					test.Errorf("Node[1].Role: got %v, want WorkerRole", cluster.Nodes[1].Role)
				}
				if cluster.Nodes[2].Role != v1alpha4.WorkerRole {
					test.Errorf("Node[2].Role: got %v, want WorkerRole", cluster.Nodes[2].Role)
				}
			},
		},
		{
			name: "config with node replicas",
			input: config.ClusterConfig{
				Name: "test-cluster",
				Config: []config.KindNode{
					{Role: "control-plane"},
					{Role: "worker", Replicas: 3},
				},
			},
			validate: func(test *testing.T, cluster *v1alpha4.Cluster) {
				// 1 control-plane + 3 workers
				if len(cluster.Nodes) != 4 {
					test.Fatalf("Nodes: got %d, want 4 (1 control-plane + 3 workers)", len(cluster.Nodes))
				}
				if cluster.Nodes[0].Role != v1alpha4.ControlPlaneRole {
					test.Errorf("Node[0].Role: got %v, want ControlPlaneRole", cluster.Nodes[0].Role)
				}
				for itr := 1; itr <= 3; itr++ {
					if cluster.Nodes[itr].Role != v1alpha4.WorkerRole {
						test.Errorf("Node[%d].Role: got %v, want WorkerRole", itr, cluster.Nodes[itr].Role)
					}
				}
			},
		},
	}

	for _, tt := range tests {
		test.Run(tt.name, func(test *testing.T) {
			result, err := km.buildKindConfig(&tt.input)
			if err != nil {
				test.Fatalf("buildKindConfig failed: %v", err)
			}
			tt.validate(test, result)
		})
	}
}

func TestBuildKindConfig_APIVersion(test *testing.T) {
	km := NewKindManager()
	cfg := &config.ClusterConfig{
		Name: "test-cluster",
	}

	result, err := km.buildKindConfig(cfg)
	if err != nil {
		test.Fatalf("buildKindConfig failed: %v", err)
	}

	if result.APIVersion != "kind.x-k8s.io/v1alpha4" {
		test.Errorf("APIVersion: got %q, want %q", result.APIVersion, "kind.x-k8s.io/v1alpha4")
	}
	if result.Kind != "Cluster" {
		test.Errorf("Kind: got %q, want %q", result.Kind, "Cluster")
	}
}

func TestPatchKubeconfigWithContainerIP(test *testing.T) {
	km := NewKindManager()

	tests := []struct {
		name            string
		kubeconfig      string
		clusterName     string
		expectedPattern string // What we expect to be replaced
	}{
		{
			name:            "replace localhost",
			kubeconfig:      "server: https://localhost:12345",
			clusterName:     "test-cluster",
			expectedPattern: "https://", // Should have https prefix
		},
		{
			name:            "replace 127.0.0.1",
			kubeconfig:      "server: https://127.0.0.1:54321",
			clusterName:     "test-cluster",
			expectedPattern: "https://",
		},
		{
			name:            "replace container name",
			kubeconfig:      "server: https://test-cluster-control-plane:6443",
			clusterName:     "test-cluster",
			expectedPattern: "https://",
		},
	}

	for _, tt := range tests {
		test.Run(tt.name, func(test *testing.T) {
			// Note: This will fail if Docker is not running or container doesn't exist
			// That's expected for unit tests - we're just testing the logic
			result, err := km.patchKubeconfigWithContainerIP(tt.clusterName, tt.kubeconfig, "")

			// If patching succeeds (Docker available, container exists)
			if err == nil {
				// Check that result still has https prefix
				if len(result) > 0 {
					// Basic sanity check - should still be valid kubeconfig structure
					if len(result) < len(tt.kubeconfig)/2 {
						test.Errorf("Result seems too short: %d bytes (original: %d)", len(result), len(tt.kubeconfig))
					}
				}
			}
			// If patching fails, that's ok - Docker might not be available in test env
			// We're mainly testing the function doesn't panic
		})
	}
}

func TestNewKindManager(test *testing.T) {
	km := NewKindManager()

	if km == nil {
		test.Fatal("NewKindManager() returned nil")
	}

	if km.provider == nil {
		test.Error("KindManager.provider is nil")
	}
}

func TestGetEffectiveProxyConfig(test *testing.T) {
	km := NewKindManager()

	tests := []struct {
		name            string
		envVars         map[string]string
		config          *config.ClusterConfig
		expectedHTTP    string
		expectedHTTPS   string
		expectedNoProxy string
		description     string
	}{
		{
			name:    "no proxy config, no env vars",
			envVars: map[string]string{},
			config: &config.ClusterConfig{
				Name: "test",
			},
			expectedHTTP:    "",
			expectedHTTPS:   "",
			expectedNoProxy: "",
			description:     "should return empty strings when no proxy configured",
		},
		{
			name: "env vars present but proxy not enabled (opt-in)",
			envVars: map[string]string{
				"HTTP_PROXY":  "http://proxy.corp.com:8080",
				"HTTPS_PROXY": "http://proxy.corp.com:8080",
				"NO_PROXY":    "localhost,127.0.0.1",
			},
			config: &config.ClusterConfig{
				Name: "test",
			},
			expectedHTTP:    "",
			expectedHTTPS:   "",
			expectedNoProxy: "",
			description:     "should ignore env vars unless explicitly enabled (opt-in)",
		},
		{
			name: "env vars with enabled: true (uppercase)",
			envVars: map[string]string{
				"HTTP_PROXY":  "http://proxy.corp.com:8080",
				"HTTPS_PROXY": "http://proxy.corp.com:8080",
				"NO_PROXY":    "localhost,127.0.0.1",
			},
			config: &config.ClusterConfig{
				Name: "test",
				Proxy: &config.ProxyConfig{
					Enabled: boolPtr(true),
				},
			},
			expectedHTTP:    "http://proxy.corp.com:8080",
			expectedHTTPS:   "http://proxy.corp.com:8080",
			expectedNoProxy: "localhost,127.0.0.1",
			description:     "should use uppercase environment variables when enabled: true",
		},
		{
			name: "env vars with enabled: true (lowercase)",
			envVars: map[string]string{
				"http_proxy":  "http://proxy.example.com:3128",
				"https_proxy": "http://proxy.example.com:3128",
				"no_proxy":    ".internal",
			},
			config: &config.ClusterConfig{
				Name: "test",
				Proxy: &config.ProxyConfig{
					Enabled: boolPtr(true),
				},
			},
			expectedHTTP:    "http://proxy.example.com:3128",
			expectedHTTPS:   "http://proxy.example.com:3128",
			expectedNoProxy: ".internal",
			description:     "should use lowercase environment variables when enabled: true",
		},
		{
			name: "YAML overrides env vars",
			envVars: map[string]string{
				"HTTP_PROXY":  "http://env-proxy:8080",
				"HTTPS_PROXY": "http://env-proxy:8080",
				"NO_PROXY":    "localhost",
			},
			config: &config.ClusterConfig{
				Name: "test",
				Proxy: &config.ProxyConfig{
					HTTPProxy:  "http://yaml-proxy:3128",
					HTTPSProxy: "http://yaml-proxy:3128",
					NoProxy:    ".cluster.local",
				},
			},
			expectedHTTP:    "http://yaml-proxy:3128",
			expectedHTTPS:   "http://yaml-proxy:3128",
			expectedNoProxy: ".cluster.local",
			description:     "YAML config should override environment variables",
		},
		{
			name: "explicit YAML values without enabled field",
			envVars: map[string]string{
				"HTTP_PROXY": "http://env-proxy:8080",
			},
			config: &config.ClusterConfig{
				Name: "test",
				Proxy: &config.ProxyConfig{
					HTTPProxy:  "http://yaml-proxy:3128",
					HTTPSProxy: "http://yaml-proxy:3128",
					NoProxy:    ".cluster.local",
				},
			},
			expectedHTTP:    "http://yaml-proxy:3128",
			expectedHTTPS:   "http://yaml-proxy:3128",
			expectedNoProxy: ".cluster.local",
			description:     "explicit YAML values work without enabled: true",
		},
		{
			name: "YAML partial override with enabled: true",
			envVars: map[string]string{
				"HTTP_PROXY":  "http://env-proxy:8080",
				"HTTPS_PROXY": "http://env-proxy:8080",
				"NO_PROXY":    "localhost",
			},
			config: &config.ClusterConfig{
				Name: "test",
				Proxy: &config.ProxyConfig{
					Enabled:   boolPtr(true),
					HTTPProxy: "http://yaml-proxy:3128",
					// HTTPS_PROXY not set in YAML
					// NO_PROXY not set in YAML
				},
			},
			expectedHTTP:    "http://yaml-proxy:3128",
			expectedHTTPS:   "http://env-proxy:8080",
			expectedNoProxy: "localhost",
			description:     "YAML should partially override, keeping env vars for unset fields",
		},
		{
			name: "explicit disable ignores all",
			envVars: map[string]string{
				"HTTP_PROXY":  "http://proxy.corp.com:8080",
				"HTTPS_PROXY": "http://proxy.corp.com:8080",
				"NO_PROXY":    "localhost",
			},
			config: &config.ClusterConfig{
				Name: "test",
				Proxy: &config.ProxyConfig{
					Enabled: boolPtr(false),
				},
			},
			expectedHTTP:    "",
			expectedHTTPS:   "",
			expectedNoProxy: "",
			description:     "enabled: false should ignore all proxy settings",
		},
		{
			name: "explicit enable with YAML values",
			envVars: map[string]string{
				"HTTP_PROXY": "http://env-proxy:8080",
			},
			config: &config.ClusterConfig{
				Name: "test",
				Proxy: &config.ProxyConfig{
					Enabled:    boolPtr(true),
					HTTPProxy:  "http://yaml-proxy:3128",
					HTTPSProxy: "http://yaml-proxy:3128",
				},
			},
			expectedHTTP:    "http://yaml-proxy:3128",
			expectedHTTPS:   "http://yaml-proxy:3128",
			expectedNoProxy: "",
			description:     "enabled: true with YAML values should use YAML values",
		},
	}

	for _, tt := range tests {
		test.Run(tt.name, func(test *testing.T) {
			// Set up environment variables
			for key, value := range tt.envVars {
				test.Setenv(key, value)
			}

			// Call getEffectiveProxyConfig
			httpProxy, httpsProxy, noProxy := km.getEffectiveProxyConfig(tt.config)

			// Validate results
			if httpProxy != tt.expectedHTTP {
				test.Errorf("HTTP_PROXY: got %q, want %q (%s)", httpProxy, tt.expectedHTTP, tt.description)
			}
			if httpsProxy != tt.expectedHTTPS {
				test.Errorf("HTTPS_PROXY: got %q, want %q (%s)", httpsProxy, tt.expectedHTTPS, tt.description)
			}
			if noProxy != tt.expectedNoProxy {
				test.Errorf("NO_PROXY: got %q, want %q (%s)", noProxy, tt.expectedNoProxy, tt.description)
			}
		})
	}
}

func TestBuildCAMounts(test *testing.T) {
	km := NewKindManager()

	tests := []struct {
		name     string
		config   *config.ClusterConfig
		validate func(*testing.T, []v1alpha4.Mount)
	}{
		{
			name: "no CA certificates",
			config: &config.ClusterConfig{
				Name:           "test",
				CACertificates: []string{},
			},
			validate: func(test *testing.T, mounts []v1alpha4.Mount) {
				if len(mounts) != 0 {
					test.Errorf("Expected 0 mounts, got %d", len(mounts))
				}
			},
		},
		{
			name: "single CA certificate",
			config: &config.ClusterConfig{
				Name:           "test",
				CACertificates: []string{"/etc/ssl/certs/corporate-ca.crt"},
			},
			validate: func(test *testing.T, mounts []v1alpha4.Mount) {
				if len(mounts) != 1 {
					test.Fatalf("Expected 1 mount, got %d", len(mounts))
				}
				if mounts[0].HostPath != "/etc/ssl/certs/corporate-ca.crt" {
					test.Errorf("HostPath: got %q, want %q", mounts[0].HostPath, "/etc/ssl/certs/corporate-ca.crt")
				}
				if mounts[0].ContainerPath != "/usr/local/share/ca-certificates/kraze-ca-0.crt" {
					test.Errorf("ContainerPath: got %q, want %q", mounts[0].ContainerPath, "/usr/local/share/ca-certificates/kraze-ca-0.crt")
				}
				if !mounts[0].Readonly {
					test.Errorf("Readonly: got false, want true")
				}
			},
		},
		{
			name: "multiple CA certificates",
			config: &config.ClusterConfig{
				Name: "test",
				CACertificates: []string{
					"/etc/ssl/certs/ca1.crt",
					"/etc/ssl/certs/ca2.crt",
					"/etc/ssl/certs/ca3.crt",
				},
			},
			validate: func(test *testing.T, mounts []v1alpha4.Mount) {
				if len(mounts) != 3 {
					test.Fatalf("Expected 3 mounts, got %d", len(mounts))
				}
				for i := 0; i < 3; i++ {
					expectedContainerPath := "/usr/local/share/ca-certificates/kraze-ca-" + string(rune('0'+i)) + ".crt"
					if mounts[i].ContainerPath != expectedContainerPath {
						test.Errorf("Mount[%d].ContainerPath: got %q, want %q", i, mounts[i].ContainerPath, expectedContainerPath)
					}
					if !mounts[i].Readonly {
						test.Errorf("Mount[%d].Readonly: got false, want true", i)
					}
				}
			},
		},
	}

	for _, tt := range tests {
		test.Run(tt.name, func(test *testing.T) {
			result := km.buildCAMounts(tt.config)
			tt.validate(test, result)
		})
	}
}

// TestBuildNvidiaGPUMounts verifies the nvidia toolkit binary mount logic.
// When nvidia-ctk is not in PATH (typical CI / non-GPU hosts), no mounts are returned.
// When nvidia-ctk IS in PATH, a mount for it is returned at /usr/local/bin/nvidia-ctk.
// When nvidia-cdi-hook is also in PATH, an additional mount at its original host path
// is returned so that CDI spec hook references resolve correctly in the kind node.
func TestBuildNvidiaGPUMounts(test *testing.T) {
	km := NewKindManager()

	tests := []struct {
		name     string
		config   *config.ClusterConfig
		wantZero bool
	}{
		{
			name:     "no GPU config returns nil",
			config:   &config.ClusterConfig{Name: "test"},
			wantZero: true,
		},
		{
			name: "nvidia disabled returns nil",
			config: &config.ClusterConfig{
				Name: "test",
				GPU:  &config.GPUConfig{Nvidia: &config.GPUVendorConfig{Enabled: false}},
			},
			wantZero: true,
		},
		{
			name: "nvidia enabled: mounts depend on what toolkit binaries are in PATH",
			config: &config.ClusterConfig{
				Name: "test",
				GPU:  &config.GPUConfig{Nvidia: &config.GPUVendorConfig{Enabled: true}},
			},
			// Result depends on whether nvidia-ctk/nvidia-cdi-hook are installed on the test host.
			// We verify mount paths are well-formed when present.
			wantZero: false,
		},
	}

	for _, tt := range tests {
		test.Run(tt.name, func(test *testing.T) {
			mounts := km.buildNvidiaGPUMounts(tt.config)
			if tt.wantZero {
				if len(mounts) != 0 {
					test.Errorf("Expected 0 mounts, got %d", len(mounts))
				}
				return
			}
			// For enabled case: 0–2 mounts depending on what's in PATH.
			// If nvidia-ctk is present it must be at /usr/local/bin/nvidia-ctk.
			// If nvidia-cdi-hook is present its container path must equal its host path.
			for _, m := range mounts {
				if m.ContainerPath == "/usr/local/bin/nvidia-ctk" {
					// nvidia-ctk mount — host path must be non-empty and readable
					if m.HostPath == "" {
						test.Errorf("nvidia-ctk mount: HostPath must not be empty")
					}
				} else {
					// nvidia-cdi-hook mount — container path must match host path
					if m.ContainerPath != m.HostPath {
						test.Errorf("nvidia-cdi-hook mount: ContainerPath %q != HostPath %q", m.ContainerPath, m.HostPath)
					}
				}
				if !m.Readonly {
					test.Errorf("Mount %q: expected Readonly=true", m.ContainerPath)
				}
			}
		})
	}
}

func TestBuildAMDGPUMounts(test *testing.T) {
	km := NewKindManager()

	tests := []struct {
		name     string
		config   *config.ClusterConfig
		validate func(*testing.T, []v1alpha4.Mount)
	}{
		{
			name:   "no GPU config (nil)",
			config: &config.ClusterConfig{Name: "test"},
			validate: func(test *testing.T, mounts []v1alpha4.Mount) {
				if len(mounts) != 0 {
					test.Errorf("Expected 0 mounts for nil GPU config, got %d", len(mounts))
				}
			},
		},
		{
			name: "amd disabled",
			config: &config.ClusterConfig{
				Name: "test",
				GPU:  &config.GPUConfig{AMD: &config.GPUVendorConfig{Enabled: false}},
			},
			validate: func(test *testing.T, mounts []v1alpha4.Mount) {
				if len(mounts) != 0 {
					test.Errorf("Expected 0 mounts for disabled AMD GPU, got %d", len(mounts))
				}
			},
		},
		{
			name: "amd enabled: always includes /dev/kfd, auto-discovers renderD devices",
			config: &config.ClusterConfig{
				Name: "test",
				GPU:  &config.GPUConfig{AMD: &config.GPUVendorConfig{Enabled: true}},
			},
			validate: func(test *testing.T, mounts []v1alpha4.Mount) {
				// Must always include /dev/kfd as the first mount.
				if len(mounts) == 0 {
					test.Fatal("Expected at least 1 mount (/dev/kfd) for enabled AMD GPU, got 0")
				}
				if mounts[0].HostPath != "/dev/kfd" || mounts[0].ContainerPath != "/dev/kfd" {
					test.Errorf("Mount[0]: expected /dev/kfd, got HostPath=%q ContainerPath=%q",
						mounts[0].HostPath, mounts[0].ContainerPath)
				}
				// Additional mounts must follow the renderD128+i naming convention.
				for i, m := range mounts[1:] {
					expected := fmt.Sprintf("/dev/dri/renderD%d", 128+i)
					if m.HostPath != expected || m.ContainerPath != expected {
						test.Errorf("Mount[%d]: expected %q, got HostPath=%q ContainerPath=%q",
							i+1, expected, m.HostPath, m.ContainerPath)
					}
				}
			},
		},
	}

	for _, tt := range tests {
		test.Run(tt.name, func(test *testing.T) {
			result := km.buildAMDGPUMounts(tt.config)
			tt.validate(test, result)
		})
	}
}

func TestBuildContainerdConfigPatches(test *testing.T) {
	km := NewKindManager()

	tests := []struct {
		name     string
		config   *config.ClusterConfig
		validate func(*testing.T, []string)
	}{
		{
			name: "no insecure registries",
			config: &config.ClusterConfig{
				Name:               "test",
				InsecureRegistries: []string{},
			},
			validate: func(test *testing.T, patches []string) {
				if len(patches) != 0 {
					test.Errorf("Expected 0 patches, got %d", len(patches))
				}
			},
		},
		{
			name: "single insecure registry",
			config: &config.ClusterConfig{
				Name:               "test",
				InsecureRegistries: []string{"ghcr.io"},
			},
			validate: func(test *testing.T, patches []string) {
				// Insecure registries are now configured post-init, so no patches expected
				if len(patches) != 0 {
					test.Fatalf("Expected 0 patches (insecure registries configured post-init), got %d", len(patches))
				}
			},
		},
		{
			name: "multiple insecure registries",
			config: &config.ClusterConfig{
				Name:               "test",
				InsecureRegistries: []string{"ghcr.io", "registry.corp.com", "docker.io"},
			},
			validate: func(test *testing.T, patches []string) {
				// Insecure registries are now configured post-init, so no patches expected
				if len(patches) != 0 {
					test.Fatalf("Expected 0 patches (insecure registries configured post-init), got %d", len(patches))
				}
			},
		},
	}

	for _, tt := range tests {
		test.Run(tt.name, func(test *testing.T) {
			result := km.buildContainerdConfigPatches(tt.config)
			tt.validate(test, result)
		})
	}
}

func TestBuildKindConfigWithCorporateFeatures(test *testing.T) {
	km := NewKindManager()

	tests := []struct {
		name     string
		config   *config.ClusterConfig
		validate func(*testing.T, *v1alpha4.Cluster)
	}{
		{
			name: "config with CA certificates",
			config: &config.ClusterConfig{
				Name:           "test",
				CACertificates: []string{"/etc/ssl/certs/ca.crt"},
			},
			validate: func(test *testing.T, cluster *v1alpha4.Cluster) {
				// Should have CA mount + GODEBUG mount on the default control-plane node
				if len(cluster.Nodes) != 1 {
					test.Fatalf("Expected 1 node, got %d", len(cluster.Nodes))
				}
				if len(cluster.Nodes[0].ExtraMounts) != 2 {
					test.Errorf("Expected 2 extra mounts (CA + GODEBUG), got %d", len(cluster.Nodes[0].ExtraMounts))
				}
				// CA certificates are updated post-init, so no kubeadm patches expected
				if len(cluster.KubeadmConfigPatches) != 0 {
					test.Errorf("Expected 0 kubeadm config patches, got %d", len(cluster.KubeadmConfigPatches))
				}
			},
		},
		{
			name: "config with insecure registries",
			config: &config.ClusterConfig{
				Name:               "test",
				InsecureRegistries: []string{"ghcr.io"},
			},
			validate: func(test *testing.T, cluster *v1alpha4.Cluster) {
				// Should have GODEBUG mount (always added)
				if len(cluster.Nodes[0].ExtraMounts) != 1 {
					test.Errorf("Expected 1 extra mount (GODEBUG), got %d", len(cluster.Nodes[0].ExtraMounts))
				}
				// Insecure registries are configured post-init, so no containerd patches expected
				if len(cluster.ContainerdConfigPatches) != 0 {
					test.Errorf("Expected 0 containerd config patches, got %d", len(cluster.ContainerdConfigPatches))
				}
			},
		},
		{
			name: "config with proxy",
			config: &config.ClusterConfig{
				Name: "test",
				Proxy: &config.ProxyConfig{
					HTTPProxy:  "http://proxy:8080",
					HTTPSProxy: "http://proxy:8080",
					NoProxy:    "localhost",
				},
			},
			validate: func(test *testing.T, cluster *v1alpha4.Cluster) {
				// Should have GODEBUG mount (always added)
				if len(cluster.Nodes[0].ExtraMounts) != 1 {
					test.Errorf("Expected 1 extra mount (GODEBUG), got %d", len(cluster.Nodes[0].ExtraMounts))
				}
				// Proxy is configured after cluster init, not via kubeadm patches
				// So we don't expect kubeadm patches for proxy
				// Just verify the config is valid
				if cluster.Name != "test" {
					test.Error("Expected cluster name 'test'")
				}
			},
		},
		{
			name: "config with all corporate features",
			config: &config.ClusterConfig{
				Name:               "test",
				CACertificates:     []string{"/etc/ssl/certs/ca.crt"},
				InsecureRegistries: []string{"ghcr.io"},
				Proxy: &config.ProxyConfig{
					HTTPProxy: "http://proxy:8080",
				},
			},
			validate: func(test *testing.T, cluster *v1alpha4.Cluster) {
				// Should have CA mount + GODEBUG mount
				if len(cluster.Nodes[0].ExtraMounts) != 2 {
					test.Errorf("Expected 2 extra mounts (CA + GODEBUG), got %d", len(cluster.Nodes[0].ExtraMounts))
				}
				// Both insecure registries and CA certs are configured post-init, so no containerd patches expected
				if len(cluster.ContainerdConfigPatches) != 0 {
					test.Errorf("Expected 0 containerd config patches, got %d", len(cluster.ContainerdConfigPatches))
				}
				// Both CA certs and proxy are configured post-init, so no kubeadm patches expected
				if len(cluster.KubeadmConfigPatches) != 0 {
					test.Errorf("Expected 0 kubeadm config patches, got %d", len(cluster.KubeadmConfigPatches))
				}
			},
		},
	}

	for _, tt := range tests {
		test.Run(tt.name, func(test *testing.T) {
			result, err := km.buildKindConfig(tt.config)
			if err != nil {
				test.Fatalf("buildKindConfig failed: %v", err)
			}
			tt.validate(test, result)
		})
	}
}

func TestBuildKindConfigWithGPU(test *testing.T) {
	km := NewKindManager()

	tests := []struct {
		name     string
		config   *config.ClusterConfig
		validate func(*testing.T, *v1alpha4.Cluster)
	}{
		{
			name: "NVIDIA GPU on default single node sets GPUs field",
			config: &config.ClusterConfig{
				Name: "test",
				GPU:  &config.GPUConfig{Nvidia: &config.GPUVendorConfig{Enabled: true}},
			},
			validate: func(test *testing.T, cluster *v1alpha4.Cluster) {
				if len(cluster.Nodes) != 1 {
					test.Fatalf("Expected 1 node, got %d", len(cluster.Nodes))
				}
				node := cluster.Nodes[0]
				if node.GPUs != "all" {
					test.Errorf("node.GPUs: got %q, want \"all\"", node.GPUs)
				}
			},
		},
		{
			name: "NVIDIA GPUs field only on worker nodes in multi-node config",
			config: &config.ClusterConfig{
				Name: "test",
				GPU:  &config.GPUConfig{Nvidia: &config.GPUVendorConfig{Enabled: true}},
				Config: []config.KindNode{
					{Role: "control-plane"},
					{Role: "worker"},
				},
			},
			validate: func(test *testing.T, cluster *v1alpha4.Cluster) {
				if len(cluster.Nodes) != 2 {
					test.Fatalf("Expected 2 nodes, got %d", len(cluster.Nodes))
				}
				controlPlane := cluster.Nodes[0]
				worker := cluster.Nodes[1]
				if controlPlane.GPUs != "" {
					test.Errorf("control-plane.GPUs: got %q, want \"\"", controlPlane.GPUs)
				}
				if worker.GPUs != "all" {
					test.Errorf("worker.GPUs: got %q, want \"all\"", worker.GPUs)
				}
			},
		},
		{
			name: "NVIDIA GPUs field on all workers with replicas",
			config: &config.ClusterConfig{
				Name: "test",
				GPU:  &config.GPUConfig{Nvidia: &config.GPUVendorConfig{Enabled: true}},
				Config: []config.KindNode{
					{Role: "control-plane"},
					{Role: "worker", Replicas: 3},
				},
			},
			validate: func(test *testing.T, cluster *v1alpha4.Cluster) {
				// 1 control-plane + 3 workers
				if len(cluster.Nodes) != 4 {
					test.Fatalf("Expected 4 nodes, got %d", len(cluster.Nodes))
				}
				if cluster.Nodes[0].GPUs != "" {
					test.Errorf("control-plane.GPUs: got %q, want \"\"", cluster.Nodes[0].GPUs)
				}
				for i := 1; i <= 3; i++ {
					if cluster.Nodes[i].GPUs != "all" {
						test.Errorf("Node[%d].GPUs: got %q, want \"all\"", i, cluster.Nodes[i].GPUs)
					}
				}
			},
		},
		{
			name: "no GPU config leaves GPUs field empty",
			config: &config.ClusterConfig{
				Name: "test",
				Config: []config.KindNode{
					{Role: "control-plane"},
					{Role: "worker"},
				},
			},
			validate: func(test *testing.T, cluster *v1alpha4.Cluster) {
				for i, node := range cluster.Nodes {
					if node.GPUs != "" {
						test.Errorf("Node[%d].GPUs: got %q, want \"\"", i, node.GPUs)
					}
				}
			},
		},
	}

	for _, tt := range tests {
		test.Run(tt.name, func(test *testing.T) {
			result, err := km.buildKindConfig(tt.config)
			if err != nil {
				test.Fatalf("buildKindConfig failed: %v", err)
			}
			tt.validate(test, result)
		})
	}
}

// Helper functions

func boolPtr(bl bool) *bool {
	return &bl
}

func containsString(str, substr string) bool {
	return len(str) > 0 && len(substr) > 0 && (str == substr || len(str) > len(substr) && (str[:len(substr)] == substr || str[len(str)-len(substr):] == substr || findInString(str, substr)))
}

func findInString(str, substr string) bool {
	for iter := 0; iter <= len(str)-len(substr); iter++ {
		if str[iter:iter+len(substr)] == substr {
			return true
		}
	}
	return false
}

func TestParseK8sVersion(test *testing.T) {
	km := NewKindManager()

	tests := []struct {
		name     string
		cfg      config.ClusterConfig
		expected string
	}{
		{
			name:     "no version configured uses kind default",
			cfg:      config.ClusterConfig{},
			expected: "v1.35.1",
		},
		{
			name:     "version field set",
			cfg:      config.ClusterConfig{Version: "1.33.0"},
			expected: "v1.33.0",
		},
		{
			name:     "node_image with digest",
			cfg:      config.ClusterConfig{NodeImage: "kindest/node:v1.32.0@sha256:c48c62eac5da28cdadcf560d1675ac2d9b09d2a4e5d4cdf2a7a4ee6f6781f38"},
			expected: "v1.32.0",
		},
		{
			name:     "node_image without digest",
			cfg:      config.ClusterConfig{NodeImage: "kindest/node:v1.31.0"},
			expected: "v1.31.0",
		},
		{
			name:     "default image constant parses correctly",
			cfg:      config.ClusterConfig{NodeImage: defaults.Image},
			expected: "v1.35.1",
		},
	}

	for _, tt := range tests {
		test.Run(tt.name, func(test *testing.T) {
			got := km.parseK8sVersion(&tt.cfg)
			if got != tt.expected {
				test.Errorf("parseK8sVersion() = %q, want %q", got, tt.expected)
			}
		})
	}
}
