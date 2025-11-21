package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// Parse reads and parses a kraze.yml configuration file
func Parse(configPath string) (*Config, error) {
	// Read the file
	data, err := os.ReadFile(configPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read config file: %w", err)
	}

	// Expand environment variables before parsing
	data = ExpandEnvVarsInBytes(data)

	// Parse YAML
	var config Config
	if err := yaml.Unmarshal(data, &config); err != nil {
		return nil, fmt.Errorf("failed to parse YAML: %w", err)
	}

	// Set service names from map keys
	for name, svc := range config.Services {
		svc.Name = name
		config.Services[name] = svc
	}

	// Validate configuration
	if err := config.Validate(); err != nil {
		return nil, fmt.Errorf("validation failed: %w", err)
	}

	// Resolve relative paths
	if err := config.ResolvePaths(configPath); err != nil {
		return nil, fmt.Errorf("failed to resolve paths: %w", err)
	}

	return &config, nil
}

// Validate performs validation on the entire configuration
func (cfg *Config) Validate() error {
	// Validate cluster config
	if cfg.Cluster.Name == "" {
		return &ValidationError{Field: "cluster.name", Message: "cluster name is required"}
	}

	// Validate each service
	for _, svc := range cfg.Services {
		if err := svc.Validate(); err != nil {
			return fmt.Errorf("service '%s': %w", svc.Name, err)
		}
	}

	// Check for dependency cycles (will be implemented in graph package)
	// For now, just check that dependencies exist
	for _, svc := range cfg.Services {
		for _, dep := range svc.DependsOn {
			if _, exists := cfg.Services[dep]; !exists {
				return &ValidationError{
					Field:   fmt.Sprintf("service '%s' depends_on", svc.Name),
					Message: fmt.Sprintf("dependency '%s' not found in services", dep),
				}
			}
		}
	}

	return nil
}

// ResolvePaths resolves all relative paths in the configuration to absolute paths
// relative to the config file location
func (cfg *Config) ResolvePaths(configPath string) error {
	configDir := filepath.Dir(configPath)

	for name, svc := range cfg.Services {
		// Resolve Helm values file path
		if svc.Values != "" && !filepath.IsAbs(svc.Values) {
			svc.Values = filepath.Join(configDir, svc.Values)
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

	return nil
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

// IsHTTPURL checks if a path is an HTTP or HTTPS URL
func IsHTTPURL(path string) bool {
	return strings.HasPrefix(path, "http://") || strings.HasPrefix(path, "https://")
}

// IsOCIURL checks if a path is an OCI URL
func IsOCIURL(path string) bool {
	return strings.HasPrefix(path, "oci://")
}
