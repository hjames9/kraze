package cli

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/hjames9/kraze/internal/state"
)

// withClusters replaces kind cluster discovery for the duration of a test
func withClusters(t *testing.T, clusters ...discoveredCluster) {
	t.Helper()
	orig := discoverClusterStates
	discoverClusterStates = func(context.Context, string) []discoveredCluster { return clusters }
	t.Cleanup(func() { discoverClusterStates = orig })
}

// clusterWithProjects builds a discovered cluster whose state stores config
// paths per project
func clusterWithProjects(name string, projectPaths map[string][]string) discoveredCluster {
	st := state.New(name, false, false, 0, false, 0)
	for project, paths := range projectPaths {
		st.SetProject(project)
		st.SetConfigPaths(paths)
	}
	return discoveredCluster{name: name, state: st}
}

// inDir runs the test from dir with no -f and no --project
func inDir(t *testing.T, dir string) {
	t.Helper()
	orig, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chdir(orig) }) //nolint:errcheck

	origFiles, origProject := configFiles, projectFlag
	configFiles, projectFlag = nil, ""
	t.Cleanup(func() { configFiles, projectFlag = origFiles, origProject })
}

func writeConfig(t *testing.T, path, content string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestResolveConfigCwdBeatsOtherProjectsState(t *testing.T) {
	root := t.TempDir()
	billing := writeConfig(t, filepath.Join(root, "billing", "kraze.yml"), "project: billing\ncluster:\n  name: shared\n")
	web := writeConfig(t, filepath.Join(root, "web", "kraze.yml"), "project: web\ncluster:\n  name: shared\n")

	// web ran `up` last, so a single-list state would have pointed at web
	withClusters(t, clusterWithProjects("shared", map[string][]string{
		"billing": {billing},
		"web":     {web},
	}))
	inDir(t, filepath.Join(root, "billing"))

	got, err := resolveConfigFiles(rootCmd)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, []string{billing}) {
		t.Errorf("got %v, want [%s]", got, billing)
	}
}

func TestResolveConfigCwdUsesStoredMultiFileSet(t *testing.T) {
	dir := t.TempDir()
	main := writeConfig(t, filepath.Join(dir, "kraze.yml"),
		"project: app\ncluster:\n  name: dev\nservices:\n  api:\n    type: manifests\n    path: x.yaml\n    depends_on: [db]\n")
	extra := writeConfig(t, filepath.Join(dir, "extra.yml"), "cluster:\n  name: dev\n")

	withClusters(t, clusterWithProjects("dev", map[string][]string{"app": {main, extra}}))
	inDir(t, dir)

	// kraze.yml alone fails validation (db lives in extra.yml); resolution
	// must still recognise it as part of the stored set
	got, err := resolveConfigFiles(rootCmd)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, []string{main, extra}) {
		t.Errorf("got %v, want [%s %s]", got, main, extra)
	}
}

func TestResolveConfigCwdIgnoresStoredSetOfAnotherProject(t *testing.T) {
	dir := t.TempDir()
	main := writeConfig(t, filepath.Join(dir, "kraze.yml"), "project: app\ncluster:\n  name: dev\n")
	extra := writeConfig(t, filepath.Join(dir, "extra.yml"), "cluster:\n  name: dev\n")

	withClusters(t, clusterWithProjects("dev", map[string][]string{"other": {main, extra}}))
	inDir(t, dir)

	got, err := resolveConfigFiles(rootCmd)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, []string{main}) {
		t.Errorf("got %v, want [%s]", got, main)
	}
}

func TestResolveConfigDiscovery(t *testing.T) {
	root := t.TempDir()
	billing := writeConfig(t, filepath.Join(root, "billing", "kraze.yml"), "project: billing\ncluster:\n  name: shared\n")
	web := writeConfig(t, filepath.Join(root, "web", "kraze.yml"), "project: web\ncluster:\n  name: shared\n")
	shared := clusterWithProjects("shared", map[string][]string{"billing": {billing}, "web": {web}})

	t.Run("single project", func(t *testing.T) {
		withClusters(t, clusterWithProjects("shared", map[string][]string{"billing": {billing}}))
		inDir(t, t.TempDir())
		got, err := resolveConfigFiles(rootCmd)
		if err != nil || !slices.Equal(got, []string{billing}) {
			t.Errorf("got %v, %v", got, err)
		}
	})

	t.Run("several projects is ambiguous", func(t *testing.T) {
		withClusters(t, shared)
		inDir(t, t.TempDir())
		_, err := resolveConfigFiles(rootCmd)
		if err == nil || !strings.Contains(err.Error(), "--project") || !strings.Contains(err.Error(), "project: web") {
			t.Errorf("expected an ambiguity error naming the projects, got %v", err)
		}
	})

	t.Run("--project selects one", func(t *testing.T) {
		withClusters(t, shared)
		inDir(t, t.TempDir())
		projectFlag = "web"
		got, err := resolveConfigFiles(rootCmd)
		if err != nil || !slices.Equal(got, []string{web}) {
			t.Errorf("got %v, %v", got, err)
		}
	})

	t.Run("--project skips the cwd kraze.yml", func(t *testing.T) {
		withClusters(t, shared)
		inDir(t, filepath.Join(root, "billing"))
		projectFlag = "web"
		got, err := resolveConfigFiles(rootCmd)
		if err != nil || !slices.Equal(got, []string{web}) {
			t.Errorf("got %v, %v", got, err)
		}
	})

	t.Run("unknown --project", func(t *testing.T) {
		withClusters(t, shared)
		inDir(t, t.TempDir())
		projectFlag = "nope"
		if _, err := resolveConfigFiles(rootCmd); err == nil || !strings.Contains(err.Error(), "'nope'") {
			t.Errorf("expected an error naming the project, got %v", err)
		}
	})
}

func TestResolveConfigCwdWithoutClusters(t *testing.T) {
	dir := t.TempDir()
	writeConfig(t, filepath.Join(dir, "kraze.yml"), "cluster:\n  name: test\n")
	withClusters(t)
	inDir(t, dir)

	got, err := resolveConfigFiles(rootCmd)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := filepath.Abs("kraze.yml")
	if !slices.Equal(got, []string{want}) {
		t.Errorf("got %v, want [%s]", got, want)
	}
}

func TestResolveConfigFallback(t *testing.T) {
	withClusters(t)
	inDir(t, t.TempDir())

	got, err := resolveConfigFiles(rootCmd)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, []string{"kraze.yml"}) {
		t.Errorf("got %v, want [kraze.yml]", got)
	}
}
