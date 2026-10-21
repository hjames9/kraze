package graph

import (
	"strings"
	"testing"

	"github.com/hjames9/kraze/internal/config"
)

func TestValidGraphAndUnfilteredFilter(t *testing.T) {
	graph := NewDependencyGraph(map[string]config.ServiceConfig{
		"db":  {Name: "db"},
		"api": {Name: "api", DependsOn: []string{"db"}},
	})
	if err := graph.Validate(); err != nil {
		t.Errorf("a valid graph: %v", err)
	}
	if all, err := graph.FilterServices(nil); err != nil || all != graph {
		t.Errorf("no names should return the whole graph, got %v, %v", all, err)
	}
}

func TestGraphErrors(t *testing.T) {
	cycle := NewDependencyGraph(map[string]config.ServiceConfig{
		"a": {Name: "a", DependsOn: []string{"b"}},
		"b": {Name: "b", DependsOn: []string{"a"}},
	})
	if _, err := cycle.ReverseTopologicalSort(); err == nil {
		t.Error("a cycle can't be sorted")
	}
	graph := NewDependencyGraph(map[string]config.ServiceConfig{
		"api": {Name: "api", DependsOn: []string{"ghost"}},
		"web": {Name: "web", DependsOn: []string{"api"}},
	})
	if _, err := graph.FilterServices([]string{"nope"}); err == nil || err.Error() != "service 'nope' not found" {
		t.Errorf("err = %v", err)
	}
	if _, err := graph.FilterServices([]string{"web"}); err == nil || !strings.Contains(err.Error(), "'ghost' not found") {
		t.Errorf("a missing transitive dependency: %v", err)
	}
}
