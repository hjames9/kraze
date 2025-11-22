package config

import "fmt"

// Config represents the complete kraze.yml structure
type Config struct {
	Cluster  ClusterConfig            `yaml:"cluster"`
	Services map[string]ServiceConfig `yaml:"services"`
}

// ClusterConfig represents the cluster configuration
type ClusterConfig struct {
	Name          string                  `yaml:"name"`
	Version       string                  `yaml:"version,omitempty"`
	Config        []KindNode              `yaml:"config,omitempty"`
	Networking    *NetworkingConfig       `yaml:"networking,omitempty"`
	PreloadImages []string                `yaml:"preload_images,omitempty"`
	External      *ExternalClusterConfig  `yaml:"external,omitempty"`
}

// KindNode represents a kind node configuration
type KindNode struct {
	Role              string            `yaml:"role"` // control-plane or worker
	Replicas          int               `yaml:"replicas,omitempty"`
	ExtraPortMappings []PortMapping     `yaml:"extraPortMappings,omitempty"`
	ExtraMounts       []Mount           `yaml:"extraMounts,omitempty"`
	Labels            map[string]string `yaml:"labels,omitempty"`
}

// PortMapping represents a port mapping from container to host
type PortMapping struct {
	ContainerPort int32  `yaml:"containerPort"`
	HostPort      int32  `yaml:"hostPort"`
	ListenAddress string `yaml:"listenAddress,omitempty"`
	Protocol      string `yaml:"protocol,omitempty"`
}

// Mount represents a volume mount
type Mount struct {
	HostPath      string `yaml:"hostPath"`
	ContainerPath string `yaml:"containerPath"`
	ReadOnly      bool   `yaml:"readOnly,omitempty"`
}

// NetworkingConfig represents networking configuration for the cluster
type NetworkingConfig struct {
	DisableDefaultCNI bool   `yaml:"disableDefaultCNI,omitempty"`
	PodSubnet         string `yaml:"podSubnet,omitempty"`
	ServiceSubnet     string `yaml:"serviceSubnet,omitempty"`
}

// ExternalClusterConfig represents configuration for using an existing cluster
type ExternalClusterConfig struct {
	Enabled    bool   `yaml:"enabled"`              // Use external cluster instead of creating one
	Kubeconfig string `yaml:"kubeconfig,omitempty"` // Path to kubeconfig (default: ~/.kube/config)
	Context    string `yaml:"context,omitempty"`    // Kubernetes context to use (default: current-context)
}

// IsExternal returns true if this cluster configuration is for an external cluster
func (c *ClusterConfig) IsExternal() bool {
	return c.External != nil && c.External.Enabled
}

// ValuesField represents a values file or array of values files
// Supports both: values: "single.yaml" and values: ["base.yaml", "override.yaml"]
type ValuesField struct {
	files []string
}

// UnmarshalYAML implements custom unmarshaling for string or []string
func (v *ValuesField) UnmarshalYAML(unmarshal func(interface{}) error) error {
	// Try unmarshaling as a string first
	var single string
	if err := unmarshal(&single); err == nil {
		if single != "" {
			v.files = []string{single}
		}
		return nil
	}

	// Try unmarshaling as []string
	var multiple []string
	if err := unmarshal(&multiple); err == nil {
		v.files = multiple
		return nil
	}

	return fmt.Errorf("values must be a string or array of strings")
}

// MarshalYAML implements custom marshaling
func (v ValuesField) MarshalYAML() (interface{}, error) {
	if len(v.files) == 0 {
		return nil, nil
	}
	if len(v.files) == 1 {
		return v.files[0], nil
	}
	return v.files, nil
}

// Files returns the values files as a slice
func (v ValuesField) Files() []string {
	return v.files
}

// IsEmpty returns true if no values files are specified
func (v ValuesField) IsEmpty() bool {
	return len(v.files) == 0
}

// ServiceConfig represents a service definition
type ServiceConfig struct {
	Name      string   `yaml:"-"`    // Set from map key
	Type      string   `yaml:"type"` // helm, manifests
	Namespace string   `yaml:"namespace,omitempty"`
	DependsOn []string `yaml:"depends_on,omitempty"`

	// Common fields
	CreateNamespace *bool             `yaml:"create_namespace,omitempty"` // Defaults to true
	Labels          map[string]string `yaml:"labels,omitempty"`
	Wait            *bool             `yaml:"wait,omitempty"`         // Wait for resources to be ready (defaults to CLI flag)
	WaitTimeout     string            `yaml:"wait_timeout,omitempty"` // Timeout for wait operations (e.g., "10m", "5m")

	// Helm-specific fields
	Repo         string      `yaml:"repo,omitempty"`          // Remote Helm repo URL
	Chart        string      `yaml:"chart,omitempty"`         // Chart name
	Version      string      `yaml:"version,omitempty"`       // Chart version
	Values       ValuesField `yaml:"values,omitempty"`        // Values file path(s) - string or []string
	ValuesInline string      `yaml:"values_inline,omitempty"` // Inline YAML values
	KeepCRDs     *bool       `yaml:"keep_crds,omitempty"`     // Keep CRDs on uninstall (nil = use default)

	// Path field used by both Helm (local chart) and Manifests (single file/dir)
	Path  string   `yaml:"path,omitempty"`  // Local chart path (Helm) or manifest file/directory (Manifests)
	Paths []string `yaml:"paths,omitempty"` // Multiple manifest files
}

// IsHelm returns true if this service is a Helm chart
func (srv *ServiceConfig) IsHelm() bool {
	return srv.Type == "helm"
}

// IsManifests returns true if this service uses raw manifests
func (srv *ServiceConfig) IsManifests() bool {
	return srv.Type == "manifests"
}

// IsLocalChart returns true if this is a local Helm chart (has path)
func (srv *ServiceConfig) IsLocalChart() bool {
	return srv.IsHelm() && srv.Path != ""
}

// IsRemoteChart returns true if this is a remote Helm chart (has repo)
func (srv *ServiceConfig) IsRemoteChart() bool {
	return srv.IsHelm() && srv.Repo != ""
}

// GetNamespace returns the namespace for this service, defaulting to "default"
func (srv *ServiceConfig) GetNamespace() string {
	if srv.Namespace != "" {
		return srv.Namespace
	}
	return "default"
}

// ShouldCreateNamespace returns whether to create the namespace, defaulting to true
func (srv *ServiceConfig) ShouldCreateNamespace() bool {
	if srv.CreateNamespace != nil {
		return *srv.CreateNamespace
	}
	return true // Default to true for local dev convenience
}

// Validate performs basic validation on the service config
func (srv *ServiceConfig) Validate() error {
	if srv.Name == "" {
		return &ValidationError{Field: "name", Message: "service name is required"}
	}

	if srv.Type == "" {
		return &ValidationError{Field: "type", Message: "service type is required"}
	}

	if srv.Type != "helm" && srv.Type != "manifests" {
		return &ValidationError{Field: "type", Message: "type must be 'helm' or 'manifests'"}
	}

	// Helm validation
	if srv.IsHelm() {
		if srv.IsLocalChart() && srv.IsRemoteChart() {
			return &ValidationError{Field: "helm", Message: "cannot specify both 'path' and 'repo' for helm chart"}
		}
		if !srv.IsLocalChart() && !srv.IsRemoteChart() {
			return &ValidationError{Field: "helm", Message: "must specify either 'path' or 'repo' for helm chart"}
		}
		if srv.IsRemoteChart() && srv.Chart == "" {
			return &ValidationError{Field: "chart", Message: "chart name is required for remote helm chart"}
		}

		// Values validation
		if !srv.Values.IsEmpty() && srv.ValuesInline != "" {
			return &ValidationError{Field: "values", Message: "cannot specify both 'values' and 'values_inline'"}
		}
	}

	// Manifests validation
	if srv.IsManifests() {
		if srv.Path == "" && len(srv.Paths) == 0 {
			return &ValidationError{Field: "manifests", Message: "must specify either 'path' or 'paths' for manifests"}
		}
	}

	return nil
}

// ValidationError represents a configuration validation error
type ValidationError struct {
	Field   string
	Message string
}

func (err *ValidationError) Error() string {
	if err.Field != "" {
		return err.Field + ": " + err.Message
	}
	return err.Message
}
