package config

// Config represents the complete kraze.yml structure
type Config struct {
	Cluster  ClusterConfig            `yaml:"cluster"`
	Services map[string]ServiceConfig `yaml:"services"`
}

// ClusterConfig represents the cluster configuration
type ClusterConfig struct {
	Name          string            `yaml:"name"`
	Version       string            `yaml:"version,omitempty"`
	Config        []KindNode        `yaml:"config,omitempty"`
	Networking    *NetworkingConfig `yaml:"networking,omitempty"`
	PreloadImages []string          `yaml:"preload_images,omitempty"`
	// External *ExternalClusterConfig `yaml:"external,omitempty"`
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

// ServiceConfig represents a service definition
type ServiceConfig struct {
	Name      string   `yaml:"-"`    // Set from map key
	Type      string   `yaml:"type"` // helm, manifests
	Namespace string   `yaml:"namespace,omitempty"`
	DependsOn []string `yaml:"depends_on,omitempty"`

	// Common fields
	CreateNamespace *bool             `yaml:"create_namespace,omitempty"` // Defaults to true
	Labels          map[string]string `yaml:"labels,omitempty"`

	// Helm-specific fields
	Repo         string `yaml:"repo,omitempty"`          // Remote Helm repo URL
	Chart        string `yaml:"chart,omitempty"`         // Chart name
	Version      string `yaml:"version,omitempty"`       // Chart version
	Values       string `yaml:"values,omitempty"`        // Values file path
	ValuesInline string `yaml:"values_inline,omitempty"` // Inline YAML values
	KeepCRDs     *bool  `yaml:"keep_crds,omitempty"`     // Keep CRDs on uninstall (nil = use default)

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
