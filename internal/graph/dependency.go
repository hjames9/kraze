package graph

import (
	"fmt"
	"slices"
	"sort"
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
		// A dependency listed twice is still one edge
		var deps []string
		for _, dep := range svc.DependsOn {
			if !slices.Contains(deps, dep) {
				deps = append(deps, dep)
			}
		}
		graph.edges[name] = deps
	}

	return graph
}

// TopologicalSort returns services in dependency order (dependencies first)
// Returns an error if there are cyclic dependencies
func (graph *DependencyGraph) TopologicalSort() ([]*config.ServiceConfig, error) {
	levels, err := graph.TopologicalSortByLevel()
	if err != nil {
		return nil, err
	}
	var result []*config.ServiceConfig
	for _, level := range levels {
		result = append(result, level...)
	}
	return result, nil
}

// TopologicalSortByLevel returns services grouped by dependency level
// Services in the same level have no dependencies on each other and can be installed in parallel
// Level 0 has no dependencies, Level 1 depends only on Level 0, etc.
// Each service is in the earliest level its dependencies allow, and each level
// is sorted by name. A dependency that isn't in the graph (not part of this
// run) counts as satisfied; Validate reports it.
func (graph *DependencyGraph) TopologicalSortByLevel() ([][]*config.ServiceConfig, error) {
	// Detect cycles first
	if cycle := graph.detectCycle(); cycle != nil {
		return nil, fmt.Errorf("circular dependency detected: %s", formatCycle(cycle))
	}

	// Kahn's algorithm, one level at a time: unmet counts each service's
	// dependencies not yet placed, dependents the reverse edges
	unmet := make(map[string]int, len(graph.services))
	dependents := make(map[string][]string)
	var current []string
	for name := range graph.services {
		for _, dep := range graph.edges[name] {
			if _, ok := graph.services[dep]; ok {
				unmet[name]++
				dependents[dep] = append(dependents[dep], name)
			}
		}
		if unmet[name] == 0 {
			current = append(current, name)
		}
	}

	var levels [][]*config.ServiceConfig
	processed := 0
	for len(current) > 0 {
		sort.Strings(current)
		level := make([]*config.ServiceConfig, 0, len(current))
		var next []string
		for _, name := range current {
			level = append(level, graph.services[name])
			for _, dependent := range dependents[name] {
				unmet[dependent]--
				if unmet[dependent] == 0 {
					next = append(next, dependent)
				}
			}
		}
		levels = append(levels, level)
		processed += len(current)
		current = next
	}

	if processed != len(graph.services) {
		// Unreachable after cycle detection
		return nil, fmt.Errorf("failed to resolve all dependencies")
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
