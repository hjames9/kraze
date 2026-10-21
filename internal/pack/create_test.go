package pack

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hjames9/kraze/internal/config"
	v2chart "helm.sh/helm/v4/pkg/chart/v2"
	chartutil "helm.sh/helm/v4/pkg/chart/v2/util"
	repov1 "helm.sh/helm/v4/pkg/repo/v1"
)

// isolateHelm keeps Helm's repositories and cache in a temp dir, away from
// the developer's ~/.config/helm and ~/.cache/helm
func isolateHelm(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HELM_CONFIG_HOME", filepath.Join(home, "config"))
	t.Setenv("HELM_CACHE_HOME", filepath.Join(home, "cache"))
	t.Setenv("HELM_DATA_HOME", filepath.Join(home, "data"))
}

// serveAssets serves a chart repository holding mychart 1.0.0, plus manifests
// under /manifests/, and returns its URL
func serveAssets(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	chart := &v2chart.Chart{Metadata: &v2chart.Metadata{APIVersion: "v2", Name: "mychart", Version: "1.0.0"}}
	if _, err := chartutil.Save(chart, dir); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "manifests", "remote.yaml"), "apiVersion: v1\nkind: ConfigMap\nmetadata: {name: remote}\n")
	writeFile(t, filepath.Join(dir, "manifests", "a.yaml"), "apiVersion: v1\nkind: ConfigMap\nmetadata: {name: a}\n")
	server := httptest.NewServer(http.FileServer(http.Dir(dir)))
	t.Cleanup(server.Close)
	index, err := repov1.IndexDirectory(dir, server.URL)
	if err != nil {
		t.Fatal(err)
	}
	if err := index.WriteFile(filepath.Join(dir, "index.yaml"), 0o644); err != nil {
		t.Fatal(err)
	}
	return server.URL
}

// packProject writes a project with every kind of asset and returns its
// config path
func packProject(t *testing.T, server string) string {
	t.Helper()
	project := t.TempDir()
	writeFile(t, filepath.Join(project, "charts", "app", "Chart.yaml"), "apiVersion: v2\nname: app\nversion: 0.1.0\n")
	writeFile(t, filepath.Join(project, "values", "app.yaml"), "replicas: 1\n")
	writeFile(t, filepath.Join(project, "manifests", "local.yaml"), "apiVersion: v1\nkind: ConfigMap\nmetadata: {name: local}\n")
	writeFile(t, filepath.Join(project, "manifests", "b.yaml"), "apiVersion: v1\nkind: ConfigMap\nmetadata: {name: b}\n")
	cfgPath := filepath.Join(project, "kraze.yml")
	writeFile(t, cfgPath, `project: web
cluster:
  name: packed
services:
  local-chart:
    type: helm
    path: ./charts/app
    values: ./values/app.yaml
  remote-chart:
    type: helm
    repo: `+server+`
    chart: mychart
    version: 1.0.0
  local-manifest:
    type: manifests
    path: ./manifests/local.yaml
  remote-manifest:
    type: manifests
    path: `+server+`/manifests/remote.yaml
  mixed-manifests:
    type: manifests
    paths:
      - `+server+`/manifests/a.yaml
      - ./manifests/b.yaml
  disabled-remote:
    type: helm
    enabled: false
    repo: `+server+`
    chart: not-in-the-repo
`)
	return cfgPath
}

// resolve returns path relative to the config's directory, as kraze reads it
func resolve(cfgPath, path string) string {
	if filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(filepath.Dir(cfgPath), path)
}

func parse(t *testing.T, paths ...string) *config.Config {
	t.Helper()
	cfg, err := config.ParseMultiple(paths)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

// A package holds everything the config needs, and its config points at the
// bundled copies: what `kraze up -f x.tar.gz` runs on another machine
func TestCreatePackageRoundTrip(t *testing.T) {
	isolateHelm(t)
	server := serveAssets(t)
	cfgPath := packProject(t, server)
	out := filepath.Join(t.TempDir(), "packed.tar.gz")

	if err := CreatePackage([]string{cfgPath}, parse(t, cfgPath), "1.2.3", out, true); err != nil {
		t.Fatalf("CreatePackage: %v", err)
	}
	if _, err := os.Stat(out + ".tmp"); !os.IsNotExist(err) {
		t.Error("the temp archive should be renamed into place")
	}

	paths, cleanup, err := MaybeExtract([]string{out})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if len(paths) != 1 || filepath.Base(paths[0]) != "kraze.yml" {
		t.Fatalf("extracted configs = %v", paths)
	}
	extracted := paths[0]
	cfg := parse(t, extracted)

	exists := func(what, path string) {
		t.Helper()
		if path == "" {
			t.Errorf("%s: no path", what)
			return
		}
		if config.IsHTTPURL(path) {
			t.Errorf("%s still points at %s", what, path)
			return
		}
		if _, err := os.Stat(resolve(extracted, path)); err != nil {
			t.Errorf("%s: %v", what, err)
		}
	}
	remote := cfg.Services["remote-chart"]
	if remote.Repo != "" || remote.Chart != "" {
		t.Errorf("the remote chart should be bundled, got repo %q chart %q", remote.Repo, remote.Chart)
	}
	exists("remote chart", remote.Path)
	if !strings.HasSuffix(remote.Path, ".krazepack/charts/remote-chart.tgz") {
		t.Errorf("remote chart path = %s", remote.Path)
	}
	exists("local chart", cfg.Services["local-chart"].Path)
	exists("local chart values", cfg.Services["local-chart"].Values.Files()[0])
	exists("local manifest", cfg.Services["local-manifest"].Path)
	exists("remote manifest", cfg.Services["remote-manifest"].Path)
	mixed := cfg.Services["mixed-manifests"].Paths
	if len(mixed) != 2 {
		t.Fatalf("mixed paths = %v", mixed)
	}
	for _, p := range mixed {
		exists("mixed manifest", p)
	}
	if got := cfg.Services["disabled-remote"]; got.IsEnabled() {
		t.Error("a disabled service stays disabled")
	}

	// The metadata names the config and the version that packed it
	meta, err := os.ReadFile(filepath.Join(filepath.Dir(extracted), MetadataFile))
	if err != nil || !strings.Contains(string(meta), `"kraze_version": "1.2.3"`) {
		t.Errorf("metadata = %s, %v", meta, err)
	}
}

// Configs in a subdirectory and the project root share the archive root
func TestCreatePackageMultipleConfigs(t *testing.T) {
	project := t.TempDir()
	base := filepath.Join(project, "kraze.yml")
	overlay := filepath.Join(project, "env", "dev.yml")
	writeFile(t, base, "project: web\ncluster:\n  name: packed\nservices:\n  app:\n    type: manifests\n    path: ./app.yaml\n")
	writeFile(t, overlay, "services:\n  extra:\n    type: manifests\n    path: ../app.yaml\n    namespace: dev\n")
	writeFile(t, filepath.Join(project, "app.yaml"), "apiVersion: v1\nkind: ConfigMap\nmetadata: {name: app}\n")
	out := filepath.Join(t.TempDir(), "multi.tar.gz")

	if err := CreatePackage([]string{base, overlay}, parse(t, base, overlay), "dev", out, false); err != nil {
		t.Fatal(err)
	}
	paths, cleanup, err := MaybeExtract([]string{out})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if len(paths) != 2 || !strings.HasSuffix(paths[1], filepath.Join("env", "dev.yml")) {
		t.Fatalf("extracted configs = %v", paths)
	}
	cfg := parse(t, paths...)
	if got := cfg.Services["extra"]; got.Namespace != "dev" {
		t.Errorf("the overlay's service = %+v", got)
	}
	if _, err := os.Stat(resolve(paths[1], cfg.Services["extra"].Path)); err != nil {
		t.Errorf("the overlay's manifest: %v", err)
	}
}

func TestCreatePackageFailures(t *testing.T) {
	isolateHelm(t)
	server := serveAssets(t)
	config := func(t *testing.T, services string) string {
		t.Helper()
		cfgPath := filepath.Join(t.TempDir(), "kraze.yml")
		writeFile(t, cfgPath, "project: web\ncluster:\n  name: packed\nservices:\n"+services)
		return cfgPath
	}

	tests := []struct {
		name     string
		services string
		setup    func(t *testing.T, cfgPath string)
		want     string
	}{
		{
			name:     "reserved directory",
			services: "  app:\n    type: manifests\n    path: ./app.yaml\n",
			setup: func(t *testing.T, cfgPath string) {
				writeFile(t, filepath.Join(filepath.Dir(cfgPath), "app.yaml"), "kind: ConfigMap\n")
				writeFile(t, filepath.Join(filepath.Dir(cfgPath), bundleDir, "x"), "")
			},
			want: "reserved by kraze pack",
		},
		{
			name:     "chart not in the repository",
			services: "  app:\n    type: helm\n    repo: " + server + "\n    chart: missing\n",
			want:     "pulling chart for service app",
		},
		{
			name:     "manifest download fails",
			services: "  app:\n    type: manifests\n    path: " + server + "/manifests/missing.yaml\n",
			want:     "HTTP 404",
		},
		{
			name:     "manifest in paths download fails",
			services: "  app:\n    type: manifests\n    paths:\n      - " + server + "/manifests/missing.yaml\n",
			want:     "downloading manifest paths[0] for service app",
		},
		{
			name:     "local asset missing",
			services: "  app:\n    type: manifests\n    path: ./missing.yaml\n",
			want:     "collecting local assets",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfgPath := config(t, tt.services)
			if tt.setup != nil {
				tt.setup(t, cfgPath)
			}
			out := filepath.Join(t.TempDir(), "out.tar.gz")
			err := CreatePackage([]string{cfgPath}, parse(t, cfgPath), "dev", out, false)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("err = %v, want %q", err, tt.want)
			}
			if _, statErr := os.Stat(out); !os.IsNotExist(statErr) {
				t.Error("a failed package must not leave an archive")
			}
			if _, statErr := os.Stat(out + ".tmp"); !os.IsNotExist(statErr) {
				t.Error("a failed package must not leave a temp archive")
			}
		})
	}

	// The output directory doesn't exist
	cfgPath := config(t, "  app:\n    type: manifests\n    path: ./app.yaml\n")
	writeFile(t, filepath.Join(filepath.Dir(cfgPath), "app.yaml"), "kind: ConfigMap\n")
	if err := CreatePackage([]string{cfgPath}, parse(t, cfgPath), "dev", filepath.Join(t.TempDir(), "no", "such", "out.tar.gz"), false); err == nil || !strings.Contains(err.Error(), "creating output file") {
		t.Errorf("err = %v", err)
	}
}

func TestMaybeExtractRejectsBadArchives(t *testing.T) {
	dir := t.TempDir()
	notGzip := filepath.Join(dir, "plain.tar.gz")
	writeFile(t, notGzip, "not gzip")
	if _, _, err := MaybeExtract([]string{notGzip}); err == nil || !strings.Contains(err.Error(), "not a valid gzip archive") {
		t.Errorf("err = %v", err)
	}
	if _, _, err := MaybeExtract([]string{filepath.Join(dir, "missing.tgz")}); err == nil {
		t.Error("want an error for a missing archive")
	}
	badMeta := filepath.Join(dir, "badmeta.tar.gz")
	writeTarGz(t, badMeta, tarEntry{name: MetadataFile, content: "{not json"}, tarEntry{name: "kraze.yml", content: "x"})
	if _, _, err := MaybeExtract([]string{badMeta}); err == nil || !strings.Contains(err.Error(), "parsing package metadata") {
		t.Errorf("err = %v", err)
	}
	emptyMeta := filepath.Join(dir, "emptymeta.tar.gz")
	writeTarGz(t, emptyMeta, tarEntry{name: MetadataFile, content: "{}"}, tarEntry{name: "kraze.yml", content: "x"})
	paths, cleanup, err := MaybeExtract([]string{emptyMeta})
	if err != nil || len(paths) != 1 || filepath.Base(paths[0]) != "kraze.yml" {
		t.Errorf("metadata without config files falls back to kraze.yml, got %v, %v", paths, err)
	}
	cleanup()
}

// Manifests outside the project, listed in paths: next to a URL, are bundled
// under .krazepack/external/
func TestCreatePackageExternalPaths(t *testing.T) {
	isolateHelm(t)
	server := serveAssets(t)
	outside := t.TempDir()
	writeFile(t, filepath.Join(outside, "shared.yaml"), "apiVersion: v1\nkind: ConfigMap\nmetadata: {name: shared}\n")
	project := t.TempDir()
	cfgPath := filepath.Join(project, "kraze.yml")
	writeFile(t, cfgPath, `project: web
cluster:
  name: packed
services:
  app:
    type: manifests
    paths:
      - `+server+`/manifests/a.yaml
      - `+filepath.Join(outside, "shared.yaml")+`
`)
	out := filepath.Join(t.TempDir(), "ext.tar.gz")
	var err error
	printed := captureOutput(t, func() { err = CreatePackage([]string{cfgPath}, parse(t, cfgPath), "dev", out, true) })
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(printed, "Bundling external asset") {
		t.Errorf("verbose output should list external assets:\n%s", printed)
	}
	paths, cleanup, err := MaybeExtract([]string{out})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	app := parse(t, paths...).Services["app"]
	if len(app.Paths) != 2 {
		t.Fatalf("paths = %v", app.Paths)
	}
	for _, p := range app.Paths {
		if config.IsHTTPURL(p) || !strings.Contains(p, bundleDir) {
			t.Errorf("path %q should point into the bundle", p)
		}
		if _, err := os.Stat(resolve(paths[0], p)); err != nil {
			t.Error(err)
		}
	}
}

func TestRewriteConfigLeavesOtherConfigsAlone(t *testing.T) {
	dir := t.TempDir()
	for name, content := range map[string]string{
		"no-services.yml":  "project: web\ncluster:\n  name: c\n",
		"null-service.yml": "project: web\ncluster:\n  name: c\nservices:\n  a:\n",
		"local-only.yml":   "project: web\ncluster:\n  name: c\nservices:\n  a:\n    type: manifests\n    path: ./a.yaml\n",
	} {
		path := filepath.Join(dir, name)
		writeFile(t, path, content)
		got, err := rewriteConfig(path, name, nil, nil, nil)
		if err != nil || string(got) != content {
			t.Errorf("%s: rewritten to %q, %v; want it unchanged", name, got, err)
		}
	}
}

func TestCommonAncestorShorterLaterPath(t *testing.T) {
	root := t.TempDir()
	got, err := CommonAncestor([]string{filepath.Join(root, "a", "b", "kraze.yml"), filepath.Join(root, "kraze.yml")})
	if err != nil || got != root {
		t.Errorf("ancestor = %q, %v; want %q", got, err, root)
	}
}

// captureOutput returns what fn writes to stdout
func captureOutput(t *testing.T, fn func()) string {
	t.Helper()
	orig := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	done := make(chan string)
	go func() {
		data, _ := io.ReadAll(r)
		done <- string(data)
	}()
	fn()
	_ = w.Close()
	os.Stdout = orig
	return <-done
}
