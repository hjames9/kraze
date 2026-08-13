package config

import (
	"os"
	"path/filepath"
	"slices"
	"sort"
	"testing"
)

// requiredExamples are the examples documented in CLAUDE.md. They must always
// be present; every other example directory is discovered automatically.
var requiredExamples = []string{
	"minimal",
	"charts",
	"manifests",
	"dependencies",
	"corporate-network",
}

// discoverExamples returns the names of every example directory holding a
// top-level kraze.yml - the same set `make validate-examples` iterates over.
//
// Discovery is deliberate: this list used to be hardcoded, so examples added
// later (corporate-network, external-cluster, labels, vllm-rag-amd) were never
// covered by the Go tests even though CLAUDE.md promises they are picked up
// automatically.
func discoverExamples(test *testing.T, examplesDir string) []string {
	test.Helper()

	matches, err := filepath.Glob(filepath.Join(examplesDir, "*", "kraze.yml"))
	if err != nil {
		test.Fatalf("Failed to scan examples directory: %v", err)
	}

	names := make([]string, 0, len(matches))
	for _, match := range matches {
		names = append(names, filepath.Base(filepath.Dir(match)))
	}
	sort.Strings(names)
	return names
}

// assertReferencedPathsExist checks that every local file or directory a config
// points at is actually present.
//
// `kraze validate` only checks schema and cross-references, so a config can
// report "valid" while referencing a values file or manifest that was never
// committed - the failure then only surfaces at `kraze up`. This caught
// examples/explicit-images referencing two files that did not exist.
func assertReferencedPathsExist(test *testing.T, label string, cfg *Config) {
	test.Helper()

	check := func(serviceName, field, path string) {
		// Remote references are resolved at install time, not from disk.
		if path == "" || IsHTTPURL(path) || IsOCIURL(path) {
			return
		}
		if _, err := os.Stat(path); err != nil {
			test.Errorf("%s: service %q references a %s that does not exist: %s",
				label, serviceName, field, path)
		}
	}

	for name, svc := range cfg.Services {
		check(name, "path", svc.Path)
		for _, path := range svc.Paths {
			check(name, "paths entry", path)
		}
		for _, valuesFile := range svc.Values.Files() {
			check(name, "values file", valuesFile)
		}
	}
}

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

	discovered := discoverExamples(test, examplesDir)
	if len(discovered) == 0 {
		test.Fatalf("No examples with a kraze.yml found under %s", examplesDir)
	}

	// The documented examples must not disappear.
	for _, required := range requiredExamples {
		if !slices.Contains(discovered, required) {
			test.Errorf("Documented example %q is missing from %s", required, examplesDir)
		}
	}

	// Track validation results
	var passed, failed int

	for _, exampleName := range discovered {
		examplePath := filepath.Join(examplesDir, exampleName, "kraze.yml")

		test.Run(exampleName, func(test *testing.T) {
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

			// Every local file the example points at must actually exist,
			// otherwise the example validates but cannot be deployed.
			assertReferencedPathsExist(test, "Example '"+exampleName+"'", cfg)

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

// TestMultiConfigExample validates the multi-config example using ParseMultiple
func TestMultiConfigExample(test *testing.T) {
	appPath := filepath.Join("..", "..", "examples", "multi-config", "app", "kraze.yml")
	mlPath := filepath.Join("..", "..", "examples", "multi-config", "ml-stack", "kraze.yml")

	cfg, err := ParseMultiple([]string{appPath, mlPath})
	if err != nil {
		test.Fatalf("Failed to parse multi-config example: %v", err)
	}

	// The multi-config example is not covered by TestAllExamples (it has no
	// top-level kraze.yml and is only valid when merged), so check its paths here.
	assertReferencedPathsExist(test, "Example 'multi-config'", cfg)

	if cfg.Cluster.Name == "" {
		test.Error("Expected cluster name to be set")
	}

	// Should contain services from both files
	if len(cfg.Services) < 4 {
		test.Errorf("Expected at least 4 services from merged configs, got %d", len(cfg.Services))
	}

	// App stack services
	for _, name := range []string{"redis", "postgres"} {
		if _, ok := cfg.Services[name]; !ok {
			test.Errorf("Expected app service %q to be present", name)
		}
	}

	// ML stack services
	for _, name := range []string{"vllm", "openwebui"} {
		if _, ok := cfg.Services[name]; !ok {
			test.Errorf("Expected ml-stack service %q to be present", name)
		}
	}

	// Cross-file dependency: pgvector depends_on postgres (defined in app config)
	if pgvector, ok := cfg.Services["pgvector"]; ok {
		found := false
		for _, dep := range pgvector.DependsOn {
			if dep == "postgres" {
				found = true
			}
		}
		if !found {
			test.Error("Expected pgvector to depend on postgres (cross-file dependency)")
		}
	}

	// GPU should be enabled from ml-stack config
	if !cfg.Cluster.GPU.IsAMDEnabled() {
		test.Error("Expected AMD GPU to be enabled after merging ml-stack config")
	}

	// cluster.config: both files define a control-plane node — they should merge into one.
	if len(cfg.Cluster.Config) != 1 {
		test.Fatalf("Expected 1 merged kind node, got %d", len(cfg.Cluster.Config))
	}
	node := cfg.Cluster.Config[0]

	// extraPortMappings: app contributes 2 (redis, postgres), ml-stack contributes 2 (vllm, openwebui).
	if len(node.ExtraPortMappings) != 4 {
		test.Errorf("Expected 4 merged port mappings, got %d", len(node.ExtraPortMappings))
	}

	// extraMounts: only ml-stack contributes a mount.
	if len(node.ExtraMounts) != 1 {
		test.Errorf("Expected 1 merged mount, got %d", len(node.ExtraMounts))
	}
	if len(node.ExtraMounts) > 0 && node.ExtraMounts[0].ContainerPath != "/var/lib/model-cache" {
		test.Errorf("Expected mount at /var/lib/model-cache, got %q", node.ExtraMounts[0].ContainerPath)
	}

	// labels: app contributes team=app, ml-stack contributes stack=ml.
	if node.Labels["team"] != "app" {
		test.Errorf("Expected label team=app, got %q", node.Labels["team"])
	}
	if node.Labels["stack"] != "ml" {
		test.Errorf("Expected label stack=ml, got %q", node.Labels["stack"])
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
