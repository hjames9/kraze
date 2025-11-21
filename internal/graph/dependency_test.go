package graph

import (
	"testing"

	"github.com/hjames9/kraze/internal/config"
)

func TestTopologicalSort(test *testing.T) {
	// Create test services
	services := map[string]config.ServiceConfig{
		"redis": {
			Name: "redis",
			Type: "helm",
		},
		"postgres": {
			Name: "postgres",
			Type: "helm",
		},
		"api": {
			Name:      "api",
			Type:      "helm",
			DependsOn: []string{"redis", "postgres"},
		},
		"worker": {
			Name:      "worker",
			Type:      "helm",
			DependsOn: []string{"redis"},
		},
	}

	graph := NewDependencyGraph(services)

	sorted, err := graph.TopologicalSort()
	if err != nil {
		test.Fatalf("Expected no error, got: %v", err)
	}

	if len(sorted) != 4 {
		test.Fatalf("Expected 4 services, got %d", len(sorted))
	}

	// Verify dependencies come before dependents
	indices := make(map[string]int)
	for itr, svc := range sorted {
		indices[svc.Name] = itr
	}

	// redis and postgres should come before api
	if indices["redis"] >= indices["api"] {
		test.Error("redis should come before api")
	}
	if indices["postgres"] >= indices["api"] {
		test.Error("postgres should come before api")
	}

	// redis should come before worker
	if indices["redis"] >= indices["worker"] {
		test.Error("redis should come before worker")
	}
}

func TestCycleDetection(test *testing.T) {
	// Create services with circular dependency
	services := map[string]config.ServiceConfig{
		"a": {
			Name:      "a",
			Type:      "helm",
			DependsOn: []string{"b"},
		},
		"b": {
			Name:      "b",
			Type:      "helm",
			DependsOn: []string{"c"},
		},
		"c": {
			Name:      "c",
			Type:      "helm",
			DependsOn: []string{"a"}, // Creates cycle: a -> b -> c -> a
		},
	}

	graph := NewDependencyGraph(services)

	_, err := graph.TopologicalSort()
	if err == nil {
		test.Fatal("Expected cycle detection error, got nil")
	}

	if err := graph.Validate(); err == nil {
		test.Fatal("Expected validation error for cycle, got nil")
	}
}

func TestFilterServices(test *testing.T) {
	services := map[string]config.ServiceConfig{
		"redis": {
			Name: "redis",
			Type: "helm",
		},
		"postgres": {
			Name: "postgres",
			Type: "helm",
		},
		"api": {
			Name:      "api",
			Type:      "helm",
			DependsOn: []string{"redis"},
		},
		"worker": {
			Name:      "worker",
			Type:      "helm",
			DependsOn: []string{"postgres"},
		},
	}

	graph := NewDependencyGraph(services)

	// Filter to just "api" (should include redis as dependency)
	filtered, err := graph.FilterServices([]string{"api"})
	if err != nil {
		test.Fatalf("Expected no error, got: %v", err)
	}

	if len(filtered.services) != 2 {
		test.Fatalf("Expected 2 services (api + redis), got %d", len(filtered.services))
	}

	if _, exists := filtered.services["api"]; !exists {
		test.Error("Expected 'api' in filtered graph")
	}
	if _, exists := filtered.services["redis"]; !exists {
		test.Error("Expected 'redis' in filtered graph")
	}
}

func TestReverseTopologicalSort(test *testing.T) {
	services := map[string]config.ServiceConfig{
		"db": {
			Name: "db",
			Type: "helm",
		},
		"api": {
			Name:      "api",
			Type:      "helm",
			DependsOn: []string{"db"},
		},
	}

	graph := NewDependencyGraph(services)

	reversed, err := graph.ReverseTopologicalSort()
	if err != nil {
		test.Fatalf("Expected no error, got: %v", err)
	}

	// In reverse order, api should come before db
	if reversed[0].Name != "api" {
		test.Errorf("Expected 'api' first in reverse order, got '%s'", reversed[0].Name)
	}
	if reversed[1].Name != "db" {
		test.Errorf("Expected 'db' second in reverse order, got '%s'", reversed[1].Name)
	}
}
