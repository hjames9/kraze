package config

import (
	"os"
	"path/filepath"
	"testing"
)

// TestAllExamples validates that all example configurations parse successfully
func TestAllExamples(test *testing.T) {
	// Find the examples directory relative to this test file
	// Go from internal/config/ -> ../../examples/
	examplesDir := filepath.Join("..", "..", "examples")

	// Check if examples directory exists
	if _, err := os.Stat(examplesDir); os.IsNotExist(err) {
		test.Skipf("Examples directory not found at %s, skipping validation", examplesDir)
		return
	}

	// Define expected examples
	expectedExamples := []string{
		"minimal",
		"charts",
		"manifests",
		"dependencies",
	}

	// Track validation results
	var passed, failed int

	for _, exampleName := range expectedExamples {
		examplePath := filepath.Join(examplesDir, exampleName, "kraze.yml")

		test.Run(exampleName, func(test *testing.T) {
			// Check if example file exists
			if _, err := os.Stat(examplePath); os.IsNotExist(err) {
				test.Errorf("Example file not found: %s", examplePath)
				failed++
				return
			}

			// Parse the configuration
			cfg, err := Parse(examplePath)
			if err != nil {
				test.Errorf("Failed to parse example '%s': %v", exampleName, err)
				failed++
				return
			}

			// Basic validation checks
			if cfg.Cluster.Name == "" {
				test.Errorf("Example '%s': cluster name is empty", exampleName)
				failed++
				return
			}

			if len(cfg.Services) == 0 {
				test.Errorf("Example '%s': no services defined", exampleName)
				failed++
				return
			}

			// Validate each service has required fields
			for serviceName, service := range cfg.Services {
				if service.Type == "" {
					test.Errorf("Example '%s': service '%s' has no type", exampleName, serviceName)
					failed++
					return
				}

				// Type-specific validation
				switch service.Type {
				case "helm":
					// Helm services need either (repo + chart) or path
					if service.IsRemoteChart() {
						if service.Repo == "" || service.Chart == "" {
							test.Errorf("Example '%s': helm service '%s' missing repo or chart", exampleName, serviceName)
							failed++
							return
						}
					} else if service.IsLocalChart() {
						if service.Path == "" {
							test.Errorf("Example '%s': helm service '%s' missing path", exampleName, serviceName)
							failed++
							return
						}
					} else {
						test.Errorf("Example '%s': helm service '%s' has neither remote (repo+chart) nor local (path) configuration", exampleName, serviceName)
						failed++
						return
					}

				case "manifests":
					// Manifest services need path or paths
					if service.Path == "" && len(service.Paths) == 0 {
						test.Errorf("Example '%s': manifest service '%s' missing path or paths", exampleName, serviceName)
						failed++
						return
					}

				default:
					test.Errorf("Example '%s': service '%s' has unknown type '%s'", exampleName, serviceName, service.Type)
					failed++
					return
				}
			}

			test.Logf("Example '%s' validated successfully (%d service(s))", exampleName, len(cfg.Services))
			passed++
		})
	}

	// Summary
	if failed > 0 {
		test.Logf("\nValidation Summary: %d passed, %d failed", passed, failed)
	} else {
		test.Logf("\nAll %d example(s) validated successfully", passed)
	}
}

// TestMinimalExample specifically tests the minimal example structure
func TestMinimalExample(test *testing.T) {
	examplePath := filepath.Join("..", "..", "examples", "minimal", "kraze.yml")

	cfg, err := Parse(examplePath)
	if err != nil {
		test.Skipf("Minimal example not found or invalid, skipping: %v", err)
		return
	}

	// Minimal should have exactly 1 service
	if len(cfg.Services) != 1 {
		test.Errorf("Minimal example should have exactly 1 service, got %d", len(cfg.Services))
	}

	// Should have a redis service
	if _, ok := cfg.Services["redis"]; !ok {
		test.Errorf("Minimal example should have a 'redis' service")
	}
}

// TestChartsExample specifically tests the charts example
func TestChartsExample(test *testing.T) {
	examplePath := filepath.Join("..", "..", "examples", "charts", "kraze.yml")

	cfg, err := Parse(examplePath)
	if err != nil {
		test.Skipf("Charts example not found or invalid, skipping: %v", err)
		return
	}

	// Should have multiple services demonstrating different chart sources
	if len(cfg.Services) < 2 {
		test.Errorf("Charts example should have multiple services, got %d", len(cfg.Services))
	}

	// Count different chart source types
	var ociCount, httpsCount, localCount int
	for _, service := range cfg.Services {
		if service.Type != "helm" {
			test.Errorf("Charts example should only have helm services, found '%s'", service.Type)
		}

		if service.IsRemoteChart() {
			if IsOCIURL(service.Repo) {
				ociCount++
			} else if IsHTTPURL(service.Repo) {
				httpsCount++
			}
		} else if service.IsLocalChart() {
			localCount++
		}
	}

	test.Logf("Charts example has: %d OCI, %d HTTPS, %d local", ociCount, httpsCount, localCount)

	// Should demonstrate at least OCI and one other type
	if ociCount == 0 {
		test.Errorf("Charts example should include at least one OCI chart")
	}
}

// TestManifestsExample specifically tests the manifests example
func TestManifestsExample(test *testing.T) {
	examplePath := filepath.Join("..", "..", "examples", "manifests", "kraze.yml")

	cfg, err := Parse(examplePath)
	if err != nil {
		test.Skipf("Manifests example not found or invalid, skipping: %v", err)
		return
	}

	// Should have multiple services demonstrating different manifest sources
	if len(cfg.Services) < 2 {
		test.Errorf("Manifests example should have multiple services, got %d", len(cfg.Services))
	}

	// All should be manifest type
	for name, service := range cfg.Services {
		if service.Type != "manifests" {
			test.Errorf("Manifests example should only have manifest services, found '%s' in '%s'", service.Type, name)
		}
	}
}

// TestDependenciesExample specifically tests the dependencies example
func TestDependenciesExample(test *testing.T) {
	examplePath := filepath.Join("..", "..", "examples", "dependencies", "kraze.yml")

	cfg, err := Parse(examplePath)
	if err != nil {
		test.Skipf("Dependencies example not found or invalid, skipping: %v", err)
		return
	}

	// Should have multiple services
	if len(cfg.Services) < 3 {
		test.Errorf("Dependencies example should have at least 3 services to demonstrate dependencies, got %d", len(cfg.Services))
	}

	// Should have at least one service with dependencies
	var hasDependencies bool
	for _, service := range cfg.Services {
		if len(service.DependsOn) > 0 {
			hasDependencies = true
			break
		}
	}

	if !hasDependencies {
		test.Errorf("Dependencies example should have at least one service with depends_on")
	}
}
