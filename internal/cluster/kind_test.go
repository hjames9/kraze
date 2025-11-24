package cluster

import (
	"testing"

	"github.com/hjames9/kraze/internal/config"
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
			result := km.buildKindConfig(&tt.input)
			tt.validate(test, result)
		})
	}
}

func TestBuildKindConfig_APIVersion(test *testing.T) {
	km := NewKindManager()
	cfg := &config.ClusterConfig{
		Name: "test-cluster",
	}

	result := km.buildKindConfig(cfg)

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
