package graph

import (
	"fmt"
	"math/rand"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/hjames9/kraze/internal/config"
)

// Property tests: random dependency graphs, checked against what an ordering
// must satisfy rather than against expected outputs. Seeded, so failures
// reproduce.

const propertyRuns = 500

// randomDAG returns services named s0..s(n-1), shuffled so names don't follow
// the order; each depends only on services generated before it (so there is
// no cycle), sometimes listing a dependency twice
func randomDAG(rng *rand.Rand) map[string]config.ServiceConfig {
	n := 1 + rng.Intn(12)
	names := make([]string, n)
	for i, p := range rng.Perm(n) {
		names[i] = fmt.Sprintf("s%d", p)
	}
	services := make(map[string]config.ServiceConfig, n)
	for i, name := range names {
		var deps []string
		for j := 0; j < i; j++ {
			if rng.Intn(3) == 0 {
				deps = append(deps, names[j])
				if rng.Intn(5) == 0 {
					deps = append(deps, names[j]) // duplicate
				}
			}
		}
		services[name] = config.ServiceConfig{Name: name, DependsOn: deps}
	}
	return services
}

func names(services []*config.ServiceConfig) []string {
	out := make([]string, len(services))
	for i, svc := range services {
		out[i] = svc.Name
	}
	return out
}

// checkOrder fails unless order holds every service once, each after all of
// its dependencies
func checkOrder(t *testing.T, services map[string]config.ServiceConfig, order []string) {
	t.Helper()
	if len(order) != len(services) {
		t.Fatalf("order %v has %d services, want %d", order, len(order), len(services))
	}
	pos := make(map[string]int, len(order))
	for i, name := range order {
		if _, dup := pos[name]; dup {
			t.Fatalf("%s appears twice in %v", name, order)
		}
		pos[name] = i
	}
	for name, svc := range services {
		for _, dep := range svc.DependsOn {
			if pos[dep] >= pos[name] {
				t.Fatalf("%s comes before its dependency %s in %v", name, dep, order)
			}
		}
	}
}

func TestPropertySortsRespectDependencies(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	for run := 0; run < propertyRuns; run++ {
		services := randomDAG(rng)
		graph := NewDependencyGraph(services)

		order, err := graph.TopologicalSort()
		if err != nil {
			t.Fatalf("run %d: TopologicalSort(%v): %v", run, services, err)
		}
		checkOrder(t, services, names(order))

		reversed, err := graph.ReverseTopologicalSort()
		if err != nil {
			t.Fatalf("run %d: ReverseTopologicalSort: %v", run, err)
		}
		r := names(reversed)
		slices.Reverse(r)
		checkOrder(t, services, r)

		levels, err := graph.TopologicalSortByLevel()
		if err != nil {
			t.Fatalf("run %d: TopologicalSortByLevel(%v): %v", run, services, err)
		}
		levelOf := map[string]int{}
		var flat []string
		for i, level := range levels {
			for _, svc := range level {
				levelOf[svc.Name] = i
				flat = append(flat, svc.Name)
			}
		}
		checkOrder(t, services, flat)
		for name, svc := range services {
			// A service is in the earliest level its dependencies allow
			want := 0
			for _, dep := range svc.DependsOn {
				want = max(want, levelOf[dep]+1)
			}
			if levelOf[name] != want {
				t.Fatalf("run %d: %s is in level %d, want %d (levels %v)", run, name, levelOf[name], want, levels)
			}
		}
	}
}

func TestPropertyCyclesAreReportedByEverySort(t *testing.T) {
	rng := rand.New(rand.NewSource(2))
	for run := 0; run < propertyRuns; run++ {
		services := randomDAG(rng)
		// Close a cycle: a service depends (directly or through others) on
		// one that depends on it
		var withDeps []string
		for name, svc := range services {
			if len(svc.DependsOn) > 0 {
				withDeps = append(withDeps, name)
			}
		}
		if len(withDeps) == 0 {
			continue
		}
		sort.Strings(withDeps)
		from := withDeps[rng.Intn(len(withDeps))]
		to := services[from].DependsOn[rng.Intn(len(services[from].DependsOn))]
		dep := services[to]
		dep.DependsOn = append(dep.DependsOn, from)
		services[to] = dep

		graph := NewDependencyGraph(services)
		if _, err := graph.TopologicalSort(); err == nil || !strings.Contains(err.Error(), "circular dependency") {
			t.Fatalf("run %d: TopologicalSort should report the cycle, got %v", run, err)
		}
		if _, err := graph.TopologicalSortByLevel(); err == nil || !strings.Contains(err.Error(), "circular dependency") {
			t.Fatalf("run %d: TopologicalSortByLevel should report the cycle, got %v", run, err)
		}

		// The reported cycle must be a real one: each service depends on the next
		cycle := graph.detectCycle()
		if len(cycle) < 2 || cycle[0] != cycle[len(cycle)-1] {
			t.Fatalf("run %d: %v is not a closed cycle", run, cycle)
		}
		for i := 0; i+1 < len(cycle); i++ {
			if !slices.Contains(services[cycle[i]].DependsOn, cycle[i+1]) {
				t.Fatalf("run %d: reported cycle %v, but %s doesn't depend on %s", run, cycle, cycle[i], cycle[i+1])
			}
		}
	}
}

// transitiveDeps returns roots plus everything they depend on, directly or not
func transitiveDeps(services map[string]config.ServiceConfig, roots []string) map[string]bool {
	seen := map[string]bool{}
	var visit func(string)
	visit = func(name string) {
		if seen[name] {
			return
		}
		seen[name] = true
		for _, dep := range services[name].DependsOn {
			visit(dep)
		}
	}
	for _, root := range roots {
		visit(root)
	}
	return seen
}

func TestPropertyFiltersReturnTheDependencyClosure(t *testing.T) {
	rng := rand.New(rand.NewSource(3))
	for run := 0; run < propertyRuns; run++ {
		services := randomDAG(rng)
		var all []string
		for name := range services {
			all = append(all, name)
		}
		sort.Strings(all)
		rng.Shuffle(len(all), func(i, j int) { all[i], all[j] = all[j], all[i] })
		roots := all[:1+rng.Intn(len(all))]
		want := transitiveDeps(services, roots)

		filtered, err := NewDependencyGraph(services).FilterServices(roots)
		if err != nil {
			t.Fatal(err)
		}
		if got := len(filtered.services); got != len(want) {
			t.Fatalf("run %d: graph filter kept %d services, want %d", run, got, len(want))
		}
		order, err := filtered.TopologicalSort()
		if err != nil {
			t.Fatalf("run %d: sorting a filtered graph: %v", run, err)
		}
		for _, svc := range order {
			if !want[svc.Name] {
				t.Fatalf("run %d: graph filter kept %s, which %v don't need", run, svc.Name, roots)
			}
		}

		// The config-level filter that `up <services>` uses must agree
		for name := range services {
			svc := services[name]
			svc.Labels = map[string]string{"picked": fmt.Sprint(slices.Contains(roots, name))}
			services[name] = svc
		}
		cfg := &config.Config{Services: services}
		withDeps, err := cfg.FilterServicesWithDependencies(roots)
		if err != nil {
			t.Fatal(err)
		}
		byLabel, err := cfg.FilterServicesByLabelsWithDependencies([]string{"picked=true"})
		if err != nil {
			t.Fatal(err)
		}
		for _, got := range []map[string]config.ServiceConfig{withDeps, byLabel} {
			if len(got) != len(want) {
				t.Fatalf("run %d: config filter kept %d services, want %d", run, len(got), len(want))
			}
			for name := range got {
				if !want[name] {
					t.Fatalf("run %d: config filter kept %s, which %v don't need", run, name, roots)
				}
			}
		}
	}
}

// A dependency outside the graph (not part of this run, e.g. filtered out)
// is treated as already satisfied by the sorts; Validate reports it
func TestSortsIgnoreDependenciesOutsideTheGraph(t *testing.T) {
	services := map[string]config.ServiceConfig{
		"api": {Name: "api", DependsOn: []string{"db", "elsewhere"}},
		"db":  {Name: "db"},
	}
	graph := NewDependencyGraph(services)
	order, err := graph.TopologicalSort()
	if err != nil {
		t.Fatal(err)
	}
	if got := names(order); !slices.Equal(got, []string{"db", "api"}) {
		t.Errorf("order = %v", got)
	}
	levels, err := graph.TopologicalSortByLevel()
	if err != nil || len(levels) != 2 {
		t.Errorf("levels = %v, %v", levels, err)
	}
	if err := graph.Validate(); err == nil || !strings.Contains(err.Error(), "elsewhere") {
		t.Errorf("Validate should report the missing dependency, got %v", err)
	}
}
