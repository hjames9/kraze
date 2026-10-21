package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"go.yaml.in/yaml/v3"
)

// Parse reads and parses a kraze.yml configuration file
func Parse(configPath string) (*Config, error) {
	data, doc, err := readConfig(configPath)
	if err != nil {
		return nil, err
	}

	var config Config
	if err := unmarshalConfig(data, doc, &config); err != nil {
		return nil, err
	}

	// Set service names from map keys
	for name, svc := range config.Services {
		svc.Name = name
		config.Services[name] = svc
	}

	// Validate configuration
	if err := config.Validate(); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	if err := requireProject(config.Project, configPath); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalid, err)
	}

	// Resolve relative paths
	config.ResolvePaths(configPath)

	return &config, nil
}

// readConfig reads and parses a config file, then expands environment
// variables in its values (see expandEnvVarsInNode). It returns the file's
// text too, for unmarshalConfig's unknown-key check.
func readConfig(configPath string) ([]byte, *yaml.Node, error) {
	data, err := os.ReadFile(configPath)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to read config file: %w", err)
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, nil, fmt.Errorf("failed to parse YAML: %w", err)
	}
	expandEnvVarsInNode(&doc)
	return data, &doc, nil
}

// unmarshalConfig decodes a parsed, expanded config document (from
// readConfig) into a Config struct.
//
// Decoding is strict: unknown keys are rejected rather than silently discarded.
// A lenient decoder lets a typo ("extramounts", "depends_onn") or an unsupported
// key parse cleanly and do nothing, so `kraze validate` would report success for
// a config that does not mean what it says.
func unmarshalConfig(data []byte, doc *yaml.Node, config *Config) error {
	// An empty (or comment-only) file has no document. Leave the zero-value
	// Config in place and let Validate() report the missing required fields.
	if doc.Kind == 0 {
		return nil
	}
	if err := checkKnownFields(data); err != nil {
		return fmt.Errorf("failed to parse YAML: %w", err)
	}
	if err := doc.Decode(config); err != nil {
		return fmt.Errorf("failed to parse YAML: %w", friendlyYAMLError(err))
	}
	return nil
}

// checkKnownFields reports unknown keys. A yaml.Node can't be decoded
// strictly, so the file's text is decoded strictly as well and only its
// unknown-key errors are kept: before expansion a reference like
// `replicas: ${N}` doesn't fit its field yet, which the decode of the expanded
// document checks. Keys are expanded too, but a reference in a key of
// kraze's own schema can't name a field anyway.
func checkKnownFields(data []byte) error {
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	var probe Config
	typeErr := &yaml.TypeError{}
	if err := decoder.Decode(&probe); !errors.As(err, &typeErr) {
		return nil
	}
	var unknown []string
	for _, msg := range typeErr.Errors {
		if strings.Contains(msg, " not found in type ") {
			unknown = append(unknown, msg)
		}
	}
	if len(unknown) == 0 {
		return nil
	}
	return friendlyYAMLError(&yaml.TypeError{Errors: unknown})
}

// configSectionNames maps internal Go type names to the kraze.yml section a user
// would recognise, so strict-decoding errors don't leak implementation details.
var configSectionNames = map[string]string{
	"config.Config":                "top level",
	"config.ClusterConfig":         "cluster",
	"config.KindNode":              "cluster.config entry",
	"config.PortMapping":           "extraPortMappings entry",
	"config.Mount":                 "extraMounts entry",
	"config.NetworkingConfig":      "cluster.networking",
	"config.ProxyConfig":           "cluster.proxy",
	"config.GPUConfig":             "cluster.gpu",
	"config.GPUVendorConfig":       "cluster.gpu vendor",
	"config.ExternalClusterConfig": "cluster.external",
	"config.ServiceConfig":         "service",
}

// friendlyYAMLError rewrites yaml.v3's "field X not found in type config.Y"
// messages to name the kraze.yml section instead of the Go type.
func friendlyYAMLError(err error) error {
	typeErr := &yaml.TypeError{}
	if !errors.As(err, &typeErr) {
		return err
	}

	rewritten := make([]string, 0, len(typeErr.Errors))
	for _, msg := range typeErr.Errors {
		for goType, section := range configSectionNames {
			msg = strings.ReplaceAll(msg, "in type "+goType, "in "+section)
		}
		rewritten = append(rewritten, msg)
	}

	return fmt.Errorf("%s", strings.Join(rewritten, "\n  "))
}

// Validate performs validation on the entire configuration
func (cfg *Config) Validate() error {
	// Validate cluster config
	if cfg.Cluster.Name == "" {
		return &ValidationError{Field: "cluster.name", Message: "cluster name is required"}
	}

	if err := validateProjectName(cfg.Project); err != nil {
		return err
	}

	for _, registry := range cfg.Cluster.InsecureRegistries {
		if _, err := ParseInsecureRegistry(registry); err != nil {
			return &ValidationError{Field: "cluster.insecure_registries", Message: err.Error()}
		}
	}

	// Validate GPU config
	if cfg.Cluster.GPU.IsAnyEnabled() {
		if cfg.Cluster.IsExternal() {
			return &ValidationError{
				Field:   "cluster.gpu",
				Message: "GPU support is only available for kind clusters, not external clusters",
			}
		}
	}

	// Validate each service
	for _, svc := range cfg.Services {
		if err := svc.Validate(); err != nil {
			return fmt.Errorf("service '%s': %w", svc.Name, err)
		}
	}

	return cfg.validateCrossRefs()
}

// ResolvePaths resolves all relative paths in the configuration to absolute paths
// relative to the config file location
func (cfg *Config) ResolvePaths(configPath string) {
	configDir := filepath.Dir(configPath)

	for name, svc := range cfg.Services {
		// Resolve Helm values file paths
		if !svc.Values.IsEmpty() {
			resolvedFiles := make([]string, 0, len(svc.Values.Files()))
			for _, valuesFile := range svc.Values.Files() {
				if !filepath.IsAbs(valuesFile) {
					resolvedFiles = append(resolvedFiles, filepath.Join(configDir, valuesFile))
				} else {
					resolvedFiles = append(resolvedFiles, valuesFile)
				}
			}
			svc.Values = ValuesField{files: resolvedFiles}
		}

		// Resolve path (used by both Helm local charts and manifests)
		// Skip URL paths (http:// or https://)
		if svc.Path != "" && !filepath.IsAbs(svc.Path) && !IsHTTPURL(svc.Path) {
			svc.Path = filepath.Join(configDir, svc.Path)
		}

		// Resolve paths (multiple manifest files)
		for itr, path := range svc.Paths {
			if !filepath.IsAbs(path) && !IsHTTPURL(path) {
				svc.Paths[itr] = filepath.Join(configDir, path)
			}
		}

		cfg.Services[name] = svc
	}
}

// GetService returns a service by name
func (cfg *Config) GetService(name string) (*ServiceConfig, bool) {
	svc, ok := cfg.Services[name]
	return &svc, ok
}

// GetAllServiceNames returns all service names
func (cfg *Config) GetAllServiceNames() []string {
	names := make([]string, 0, len(cfg.Services))
	for name := range cfg.Services {
		names = append(names, name)
	}
	return names
}

// FilterServices returns services matching the given names
// If no names provided, returns all services
func (cfg *Config) FilterServices(names []string) (map[string]ServiceConfig, error) {
	if len(names) == 0 {
		return cfg.Services, nil
	}

	filtered := make(map[string]ServiceConfig)
	for _, name := range names {
		svc, ok := cfg.Services[name]
		if !ok {
			return nil, fmt.Errorf("service '%s' not found in configuration", name)
		}
		filtered[name] = svc
	}

	return filtered, nil
}

// FilterServicesWithDependencies returns services matching the given names plus all their dependencies
// This is used by 'up' to ensure dependencies are installed
func (cfg *Config) FilterServicesWithDependencies(names []string) (map[string]ServiceConfig, error) {
	if len(names) == 0 {
		return cfg.Services, nil
	}

	filtered := make(map[string]ServiceConfig)

	// Helper function to recursively add a service and its dependencies
	var addServiceWithDeps func(name string) error
	addServiceWithDeps = func(name string) error {
		// Skip if already added
		if _, exists := filtered[name]; exists {
			return nil
		}

		// Get the service
		svc, ok := cfg.Services[name]
		if !ok {
			return fmt.Errorf("service '%s' not found in configuration", name)
		}

		// Add the service
		filtered[name] = svc

		// Recursively add dependencies
		for _, dep := range svc.DependsOn {
			if err := addServiceWithDeps(dep); err != nil {
				return err
			}
		}

		return nil
	}

	// Add each requested service and its dependencies
	for _, name := range names {
		if err := addServiceWithDeps(name); err != nil {
			return nil, err
		}
	}

	return filtered, nil
}

// FilterServicesNoDependencies returns only the specified services without their dependencies
// This is useful for fast iteration when dependencies are already installed
func (cfg *Config) FilterServicesNoDependencies(names []string) (map[string]ServiceConfig, error) {
	if len(names) == 0 {
		return cfg.Services, nil
	}

	filtered := make(map[string]ServiceConfig)

	for _, name := range names {
		svc, ok := cfg.Services[name]
		if !ok {
			return nil, fmt.Errorf("service '%s' not found in configuration", name)
		}
		filtered[name] = svc
	}

	return filtered, nil
}

// FilterServicesByLabels returns services that match all the given label selectors
// Label selectors are in the format "key=value"
// If no labels provided, returns all services
func (cfg *Config) FilterServicesByLabels(labelSelectors []string) (map[string]ServiceConfig, error) {
	if len(labelSelectors) == 0 {
		return cfg.Services, nil
	}

	// Parse label selectors
	requiredLabels := make(map[string]string)
	for _, selector := range labelSelectors {
		parts := strings.SplitN(selector, "=", 2)
		if len(parts) != 2 {
			return nil, fmt.Errorf("invalid label selector '%s': must be in format 'key=value'", selector)
		}
		requiredLabels[parts[0]] = parts[1]
	}

	// Filter services that match all required labels
	filtered := make(map[string]ServiceConfig)
	for name, svc := range cfg.Services {
		if matchesLabels(svc.Labels, requiredLabels) {
			filtered[name] = svc
		}
	}

	if len(filtered) == 0 {
		return nil, fmt.Errorf("no services found matching label selectors: %v", labelSelectors)
	}

	return filtered, nil
}

// FilterServicesByLabelsWithDependencies returns services matching labels plus all their dependencies
func (cfg *Config) FilterServicesByLabelsWithDependencies(labelSelectors []string) (map[string]ServiceConfig, error) {
	// First get services matching labels
	matchingServices, err := cfg.FilterServicesByLabels(labelSelectors)
	if err != nil {
		return nil, err
	}

	// Get the names of matching services
	names := make([]string, 0, len(matchingServices))
	for name := range matchingServices {
		names = append(names, name)
	}

	// Use existing function to get services with dependencies
	return cfg.FilterServicesWithDependencies(names)
}

// matchesLabels checks if serviceLabels contains all requiredLabels with matching values
func matchesLabels(serviceLabels, requiredLabels map[string]string) bool {
	for key, requiredValue := range requiredLabels {
		serviceValue, exists := serviceLabels[key]
		if !exists || serviceValue != requiredValue {
			return false
		}
	}
	return true
}

// IsHTTPURL checks if a path is an HTTP or HTTPS URL
func IsHTTPURL(path string) bool {
	return strings.HasPrefix(path, "http://") || strings.HasPrefix(path, "https://")
}

// IsOCIURL checks if a path is an OCI URL
func IsOCIURL(path string) bool {
	return strings.HasPrefix(path, "oci://")
}

// projectNamePattern is a DNS label. Project names are joined to service names
// with "/" to key cluster state, so they must never contain one.
var projectNamePattern = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

// requireProject fails when a config sets no project. The error suggests a
// name derived from the config file's directory.
func requireProject(project, configPath string) error {
	if project != "" {
		return nil
	}
	return &ValidationError{
		Field: "project",
		Message: fmt.Sprintf("a project name is required; it names this config's services on the cluster (e.g. add `project: %s`)",
			suggestProjectName(configPath)),
	}
}

// suggestProjectName turns the name of a config file's directory into a
// valid project name
func suggestProjectName(configPath string) string {
	dir := filepath.Dir(configPath)
	if abs, err := filepath.Abs(dir); err == nil {
		dir = abs
	}
	name := strings.ToLower(filepath.Base(dir))
	name = invalidProjectChars.ReplaceAllString(name, "-")
	if len(name) > 63 {
		name = name[:63]
	}
	name = strings.Trim(name, "-")
	if name == "" || !projectNamePattern.MatchString(name) {
		return "my-project"
	}
	return name
}

var invalidProjectChars = regexp.MustCompile(`[^a-z0-9-]+`)

// validateProjectName checks the format of the top-level project key. A
// missing project is reported by requireProject.
func validateProjectName(project string) error {
	if project == "" {
		return nil
	}
	if len(project) > 63 || !projectNamePattern.MatchString(project) {
		return &ValidationError{
			Field:   "project",
			Message: fmt.Sprintf("'%s' must be a lowercase DNS label (a-z, 0-9 and '-', at most 63 characters, starting and ending with a letter or digit)", project),
		}
	}
	return nil
}

// Identity is the part of a config file that says which cluster and project it
// belongs to
type Identity struct {
	Project string
	Cluster string
	// External is true for an external (non-kind) cluster
	External bool
}

// ParseIdentity reads only a config file's project and cluster name. Unlike
// Parse it does no validation, so it works on one file of a multi-file config
// whose services depend on services defined in the other files.
func ParseIdentity(configPath string) (Identity, error) {
	_, doc, err := readConfig(configPath)
	if err != nil {
		return Identity{}, err
	}

	var partial struct {
		Project string `yaml:"project"`
		Cluster struct {
			Name     string                 `yaml:"name"`
			External *ExternalClusterConfig `yaml:"external"`
		} `yaml:"cluster"`
	}
	if doc.Kind != 0 {
		if err := doc.Decode(&partial); err != nil {
			return Identity{}, fmt.Errorf("failed to parse config: %w", err)
		}
	}
	external := partial.Cluster.External != nil && partial.Cluster.External.Enabled
	return Identity{Project: partial.Project, Cluster: partial.Cluster.Name, External: external}, nil
}
