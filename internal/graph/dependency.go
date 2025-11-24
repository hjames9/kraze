package graph

import (
	"fmt"
	"strings"

	"github.com/hjames9/kraze/internal/config"
)

// DependencyGraph represents a directed graph of service dependencies
type DependencyGraph struct {
	services map[string]*config.ServiceConfig
	edges    map[string][]string // service -> dependencies
}

// NewDependencyGraph creates a new dependency graph from services
func NewDependencyGraph(services map[string]config.ServiceConfig) *DependencyGraph {
	graph := &DependencyGraph{
		services: make(map[string]*config.ServiceConfig),
		edges:    make(map[string][]string),
	}

	// Convert map to pointers and build edges
	for name, svc := range services {
		svcCopy := svc
		graph.services[name] = &svcCopy
		graph.edges[name] = svc.DependsOn
	}

	return graph
}

// TopologicalSort returns services in dependency order (dependencies first)
// Returns an error if there are cyclic dependencies
func (graph *DependencyGraph) TopologicalSort() ([]*config.ServiceConfig, error) {
	// Detect cycles first
	if cycle := graph.detectCycle(); cycle != nil {
		return nil, fmt.Errorf("circular dependency detected: %s", formatCycle(cycle))
	}

	// Kahn's algorithm for topological sort
	// Calculate in-degrees: how many dependencies each service has
	inDegree := make(map[string]int)
	for name, deps := range graph.edges {
		if _, exists := inDegree[name]; !exists {
			inDegree[name] = 0
		}
		inDegree[name] += len(deps)
	}

	// Ensure all services are in inDegree map
	for name := range graph.services {
		if _, exists := inDegree[name]; !exists {
			inDegree[name] = 0
		}
	}

	// Queue of services with no dependencies (in-degree = 0)
	queue := []string{}
	for name, degree := range inDegree {
		if degree == 0 {
			queue = append(queue, name)
		}
	}

	// Process queue
	result := []*config.ServiceConfig{}

	for len(queue) > 0 {
		// Pop from queue
		current := queue[0]
		queue = queue[1:]

		result = append(result, graph.services[current])

		// For each service that depends on current, decrement its in-degree
		for name, deps := range graph.edges {
			for _, dep := range deps {
				if dep == current {
					inDegree[name]--
					if inDegree[name] == 0 {
						queue = append(queue, name)
					}
					break
				}
			}
		}
	}

	if len(result) != len(graph.services) {
		return nil, fmt.Errorf("failed to resolve all dependencies (possible cycle)")
	}

	return result, nil
}

// TopologicalSortByLevel returns services grouped by dependency level
// Services in the same level have no dependencies on each other and can be installed in parallel
// Level 0 has no dependencies, Level 1 depends only on Level 0, etc.
func (graph *DependencyGraph) TopologicalSortByLevel() ([][]*config.ServiceConfig, error) {
	// Detect cycles first
	if cycle := graph.detectCycle(); cycle != nil {
		return nil, fmt.Errorf("circular dependency detected: %s", formatCycle(cycle))
	}

	// Calculate in-degrees
	inDegree := make(map[string]int)
	for name, deps := range graph.edges {
		if _, exists := inDegree[name]; !exists {
			inDegree[name] = 0
		}
		inDegree[name] += len(deps)
	}

	// Ensure all services are in inDegree map
	for name := range graph.services {
		if _, exists := inDegree[name]; !exists {
			inDegree[name] = 0
		}
	}

	// Process level by level
	var levels [][]*config.ServiceConfig
	processed := 0

	for processed < len(graph.services) {
		// Find all services with in-degree 0 (no remaining dependencies)
		currentLevel := []*config.ServiceConfig{}
		currentLevelNames := []string{}

		for name, degree := range inDegree {
			if degree == 0 {
				currentLevel = append(currentLevel, graph.services[name])
				currentLevelNames = append(currentLevelNames, name)
			}
		}

		if len(currentLevel) == 0 {
			// No progress possible - shouldn't happen after cycle detection
			return nil, fmt.Errorf("failed to resolve all dependencies")
		}

		levels = append(levels, currentLevel)
		processed += len(currentLevel)

		// Remove processed services and update in-degrees
		for _, name := range currentLevelNames {
			delete(inDegree, name)
		}

		// Decrement in-degrees for services that depended on current level
		for name, deps := range graph.edges {
			if _, exists := inDegree[name]; !exists {
				continue // Already processed
			}
			for _, dep := range deps {
				for _, levelName := range currentLevelNames {
					if dep == levelName {
						inDegree[name]--
						break
					}
				}
			}
		}
	}

	return levels, nil
}

// ReverseTopologicalSort returns services in reverse dependency order
// (dependents first, for safe uninstallation)
func (graph *DependencyGraph) ReverseTopologicalSort() ([]*config.ServiceConfig, error) {
	sorted, err := graph.TopologicalSort()
	if err != nil {
		return nil, err
	}

	// Reverse the slice
	reversed := make([]*config.ServiceConfig, len(sorted))
	for itr := range sorted {
		reversed[len(sorted)-1-itr] = sorted[itr]
	}

	return reversed, nil
}

// FilterServices returns a subgraph containing only the specified services
// and their dependencies
func (graph *DependencyGraph) FilterServices(serviceNames []string) (*DependencyGraph, error) {
	if len(serviceNames) == 0 {
		// Return full graph if no filter
		return graph, nil
	}

	// Find all services needed (including transitive dependencies)
	needed := make(map[string]bool)
	var findDeps func(string) error
	findDeps = func(name string) error {
		if needed[name] {
			return nil // Already processed
		}

		if _, exists := graph.services[name]; !exists {
			return fmt.Errorf("service '%s' not found", name)
		}

		needed[name] = true

		// Add dependencies
		for _, dep := range graph.edges[name] {
			if err := findDeps(dep); err != nil {
				return err
			}
		}

		return nil
	}

	// Find all needed services
	for _, name := range serviceNames {
		if err := findDeps(name); err != nil {
			return nil, err
		}
	}

	// Build filtered graph
	filtered := &DependencyGraph{
		services: make(map[string]*config.ServiceConfig),
		edges:    make(map[string][]string),
	}

	for name := range needed {
		filtered.services[name] = graph.services[name]
		filtered.edges[name] = graph.edges[name]
	}

	return filtered, nil
}

// detectCycle detects if there's a cycle in the dependency graph
// Returns the cycle path if found, nil otherwise
func (graph *DependencyGraph) detectCycle() []string {
	visited := make(map[string]bool)
	recStack := make(map[string]bool)

	var dfs func(string, []string) []string
	dfs = func(node string, path []string) []string {
		visited[node] = true
		recStack[node] = true
		path = append(path, node)

		for _, dep := range graph.edges[node] {
			if !visited[dep] {
				if cycle := dfs(dep, path); cycle != nil {
					return cycle
				}
			} else if recStack[dep] {
				// Found a cycle
				cycleStart := -1
				for itr, pt := range path {
					if pt == dep {
						cycleStart = itr
						break
					}
				}
				if cycleStart >= 0 {
					return append(path[cycleStart:], dep)
				}
			}
		}

		recStack[node] = false
		return nil
	}

	for node := range graph.services {
		if !visited[node] {
			if cycle := dfs(node, []string{}); cycle != nil {
				return cycle
			}
		}
	}

	return nil
}

// formatCycle formats a cycle path for error messages
func formatCycle(cycle []string) string {
	return strings.Join(cycle, " -> ")
}

// Validate checks if the dependency graph is valid
func (graph *DependencyGraph) Validate() error {
	// Check if all dependencies exist
	for name, deps := range graph.edges {
		for _, dep := range deps {
			if _, exists := graph.services[dep]; !exists {
				return fmt.Errorf("service '%s' depends on '%s' which does not exist", name, dep)
			}
		}
	}

	// Check for cycles
	if cycle := graph.detectCycle(); cycle != nil {
		return fmt.Errorf("circular dependency detected: %s", formatCycle(cycle))
	}

	return nil
}
