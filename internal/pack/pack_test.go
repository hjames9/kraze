package pack

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hjames9/kraze/internal/config"
)

// ---- helpers ---------------------------------------------------------------

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}

func assetArchPaths(assets []Asset) []string {
	paths := make([]string, len(assets))
	for i, a := range assets {
		paths[i] = a.ArchPath
	}
	return paths
}

func containsPath(paths []string, want string) bool {
	for _, p := range paths {
		if p == want {
			return true
		}
	}
	return false
}

// buildMinimalPackage creates a .tar.gz with the given config and extra files,
// plus a kraze-package.json pointing to kraze.yml.
func buildMinimalPackage(t *testing.T, configContent string, extraFiles map[string]string) string {
	t.Helper()
	outFile := filepath.Join(t.TempDir(), "test.tar.gz")
	f, err := os.Create(outFile)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	gw := gzip.NewWriter(f)
	tw := tar.NewWriter(gw)

	addEntry := func(name, content string) {
		hdr := &tar.Header{
			Name:     name,
			Mode:     0644,
			Size:     int64(len(content)),
			Typeflag: tar.TypeReg,
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}

	addEntry("kraze.yml", configContent)
	for name, content := range extraFiles {
		addEntry(name, content)
	}
	meta := PackageMetadata{Version: "1", ConfigFiles: []string{"kraze.yml"}}
	metaBytes, _ := json.Marshal(meta)
	addEntry(MetadataFile, string(metaBytes))

	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gw.Close(); err != nil {
		t.Fatal(err)
	}
	return outFile
}

// ---- CommonAncestor --------------------------------------------------------

func TestCommonAncestor(t *testing.T) {
	tests := []struct {
		name    string
		paths   []string
		want    string
		wantErr bool
	}{
		{
			name:  "single path",
			paths: []string{"/project/kraze.yml"},
			want:  "/project",
		},
		{
			name:  "same directory",
			paths: []string{"/project/kraze.yml", "/project/services.yml"},
			want:  "/project",
		},
		{
			name:  "parent and child directory",
			paths: []string{"/project/kraze.yml", "/project/services/db.yml"},
			want:  "/project",
		},
		{
			name:  "sibling directories",
			paths: []string{"/project/app/kraze.yml", "/project/infra/db.yml"},
			want:  "/project",
		},
		{
			name:  "deeply nested with common root",
			paths: []string{"/a/b/c/kraze.yml", "/a/b/d/other.yml"},
			want:  "/a/b",
		},
		{
			name:    "no common ancestor",
			paths:   []string{"/foo/kraze.yml", "/bar/other.yml"},
			wantErr: true,
		},
		{
			name:    "empty input",
			paths:   []string{},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := CommonAncestor(tt.paths)
			if (err != nil) != tt.wantErr {
				t.Fatalf("CommonAncestor() error = %v, wantErr %v", err, tt.wantErr)
			}
			if !tt.wantErr && got != tt.want {
				t.Errorf("CommonAncestor() = %q, want %q", got, tt.want)
			}
		})
	}
}

// ---- relArchivePath --------------------------------------------------------

func TestRelArchivePath(t *testing.T) {
	tests := []struct {
		root    string
		path    string
		want    string
		wantErr bool
	}{
		{"/project", "/project/charts/backend/Chart.yaml", "charts/backend/Chart.yaml", false},
		{"/project", "/project/kraze.yml", "kraze.yml", false},
		{"/project", "/other/file.yaml", "", true},
	}
	for _, tt := range tests {
		got, err := relArchivePath(tt.root, tt.path)
		if (err != nil) != tt.wantErr {
			t.Fatalf("relArchivePath(%q, %q) error = %v, wantErr %v", tt.root, tt.path, err, tt.wantErr)
		}
		if !tt.wantErr && got != tt.want {
			t.Errorf("relArchivePath() = %q, want %q", got, tt.want)
		}
	}
}

// ---- CollectLocalAssets ----------------------------------------------------

func TestCollectLocalAssets_LocalHelmChart(t *testing.T) {
	dir := t.TempDir()

	// Create project files
	writeFile(t, filepath.Join(dir, "chart", "Chart.yaml"), "apiVersion: v2\nname: myapp\nversion: 0.1.0\n")
	writeFile(t, filepath.Join(dir, "chart", "values.yaml"), "replicaCount: 1\n")
	writeFile(t, filepath.Join(dir, "chart", "templates", "deploy.yaml"), "kind: Deployment\n")
	writeFile(t, filepath.Join(dir, "values-override.yaml"), "replicaCount: 2\n")

	cfgContent := `
cluster:
  name: test
services:
  myapp:
    type: helm
    path: ./chart
    values:
      - ./values-override.yaml
`
	cfgPath := filepath.Join(dir, "kraze.yml")
	writeFile(t, cfgPath, cfgContent)

	cfg, err := config.Parse(cfgPath)
	if err != nil {
		t.Fatalf("config.Parse() error: %v", err)
	}

	assets, _, err := CollectLocalAssets(dir, dir, cfg)
	if err != nil {
		t.Fatalf("CollectLocalAssets() error: %v", err)
	}

	paths := assetArchPaths(assets)
	for _, want := range []string{
		"chart/Chart.yaml",
		"chart/values.yaml",
		"chart/templates/deploy.yaml",
		"values-override.yaml",
	} {
		if !containsPath(paths, want) {
			t.Errorf("expected %q in assets, got: %v", want, paths)
		}
	}
}

func TestCollectLocalAssets_ManifestFile(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "manifests", "app.yaml"), "kind: Deployment\n")

	cfgContent := `
cluster:
  name: test
services:
  app:
    type: manifests
    path: ./manifests/app.yaml
`
	cfgPath := filepath.Join(dir, "kraze.yml")
	writeFile(t, cfgPath, cfgContent)

	cfg, err := config.Parse(cfgPath)
	if err != nil {
		t.Fatalf("config.Parse() error: %v", err)
	}

	assets, _, err := CollectLocalAssets(dir, dir, cfg)
	if err != nil {
		t.Fatalf("CollectLocalAssets() error: %v", err)
	}

	if !containsPath(assetArchPaths(assets), "manifests/app.yaml") {
		t.Errorf("expected manifests/app.yaml, got: %v", assetArchPaths(assets))
	}
}

func TestCollectLocalAssets_ManifestDirectory(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "manifests", "app.yaml"), "kind: Deployment\n")
	writeFile(t, filepath.Join(dir, "manifests", "svc.yaml"), "kind: Service\n")

	cfgContent := `
cluster:
  name: test
services:
  app:
    type: manifests
    path: ./manifests
`
	cfgPath := filepath.Join(dir, "kraze.yml")
	writeFile(t, cfgPath, cfgContent)

	cfg, err := config.Parse(cfgPath)
	if err != nil {
		t.Fatalf("config.Parse() error: %v", err)
	}

	assets, _, err := CollectLocalAssets(dir, dir, cfg)
	if err != nil {
		t.Fatalf("CollectLocalAssets() error: %v", err)
	}

	paths := assetArchPaths(assets)
	for _, want := range []string{"manifests/app.yaml", "manifests/svc.yaml"} {
		if !containsPath(paths, want) {
			t.Errorf("expected %q in assets, got: %v", want, paths)
		}
	}
}

func TestCollectLocalAssets_PathsArray(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "k8s", "ns.yaml"), "kind: Namespace\n")
	writeFile(t, filepath.Join(dir, "k8s", "deploy.yaml"), "kind: Deployment\n")

	cfgContent := `
cluster:
  name: test
services:
  app:
    type: manifests
    paths:
      - ./k8s/ns.yaml
      - ./k8s/deploy.yaml
`
	cfgPath := filepath.Join(dir, "kraze.yml")
	writeFile(t, cfgPath, cfgContent)

	cfg, err := config.Parse(cfgPath)
	if err != nil {
		t.Fatalf("config.Parse() error: %v", err)
	}

	assets, _, err := CollectLocalAssets(dir, dir, cfg)
	if err != nil {
		t.Fatalf("CollectLocalAssets() error: %v", err)
	}

	paths := assetArchPaths(assets)
	for _, want := range []string{"k8s/ns.yaml", "k8s/deploy.yaml"} {
		if !containsPath(paths, want) {
			t.Errorf("expected %q in assets, got: %v", want, paths)
		}
	}
}

func TestCollectLocalAssets_CACerts(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "certs", "ca.crt"), "-----BEGIN CERTIFICATE-----\n")

	cfgContent := `
cluster:
  name: test
  ca_certificates:
    - ./certs/ca.crt
services: {}
`
	cfgPath := filepath.Join(dir, "kraze.yml")
	writeFile(t, cfgPath, cfgContent)

	// config.Parse does NOT resolve ca_certificates, so we pass them as-is.
	// CollectLocalAssets resolves relative paths using firstConfigDir.
	cfg, err := config.Parse(cfgPath)
	if err != nil {
		t.Fatalf("config.Parse() error: %v", err)
	}

	assets, _, err := CollectLocalAssets(dir, dir, cfg)
	if err != nil {
		t.Fatalf("CollectLocalAssets() error: %v", err)
	}

	paths := assetArchPaths(assets)
	if !containsPath(paths, "certs/ca.crt") {
		t.Errorf("expected certs/ca.crt in assets, got: %v", paths)
	}
}

func TestCollectLocalAssets_ExternalManifestFileBundled(t *testing.T) {
	dir := t.TempDir()
	outsideDir := t.TempDir()
	writeFile(t, filepath.Join(outsideDir, "external.yaml"), "kind: Deployment\n")

	extFile := filepath.Join(outsideDir, "external.yaml")
	cfg := &config.Config{
		Cluster: config.ClusterConfig{Name: "test"},
		Services: map[string]config.ServiceConfig{
			"app": {
				Name: "app",
				Type: "manifests",
				Path: extFile,
			},
		},
	}

	assets, extMap, err := CollectLocalAssets(dir, dir, cfg)
	if err != nil {
		t.Fatalf("CollectLocalAssets() unexpected error: %v", err)
	}
	if len(assets) != 1 {
		t.Fatalf("expected 1 asset, got %d: %v", len(assets), assetArchPaths(assets))
	}
	if !strings.HasPrefix(assets[0].ArchPath, ".krazepack/manifests/") {
		t.Errorf("expected external manifest asset under .krazepack/manifests/, got: %s", assets[0].ArchPath)
	}
	if _, ok := extMap[filepath.Clean(extFile)]; !ok {
		t.Errorf("expected externalPaths to contain the external asset abs path")
	}
}

func TestCollectLocalAssets_ExternalHelmChartDirBundled(t *testing.T) {
	dir := t.TempDir()
	outsideDir := t.TempDir()
	writeFile(t, filepath.Join(outsideDir, "charts", "myapp", "Chart.yaml"), "apiVersion: v2\nname: myapp\nversion: 0.1.0\n")
	writeFile(t, filepath.Join(outsideDir, "charts", "myapp", "values.yaml"), "replicaCount: 1\n")
	writeFile(t, filepath.Join(outsideDir, "charts", "myapp", "templates", "deploy.yaml"), "kind: Deployment\n")

	chartDir := filepath.Join(outsideDir, "charts", "myapp")
	cfg := &config.Config{
		Cluster: config.ClusterConfig{Name: "test"},
		Services: map[string]config.ServiceConfig{
			"myapp": {
				Name: "myapp",
				Type: "helm",
				Path: chartDir,
			},
		},
	}

	assets, extMap, err := CollectLocalAssets(dir, dir, cfg)
	if err != nil {
		t.Fatalf("CollectLocalAssets() unexpected error: %v", err)
	}
	if len(assets) != 3 {
		t.Fatalf("expected 3 assets (chart dir files), got %d: %v", len(assets), assetArchPaths(assets))
	}
	for _, a := range assets {
		if !strings.HasPrefix(a.ArchPath, ".krazepack/charts/myapp/") {
			t.Errorf("expected all files under .krazepack/charts/myapp/, got: %s", a.ArchPath)
		}
	}
	if archPath, ok := extMap[filepath.Clean(chartDir)]; !ok {
		t.Error("expected externalPaths to contain the chart dir")
	} else if archPath != ".krazepack/charts/myapp" {
		t.Errorf("expected archPath .krazepack/charts/myapp, got %s", archPath)
	}
}

func TestCollectLocalAssets_MissingFile(t *testing.T) {
	dir := t.TempDir()

	cfg := &config.Config{
		Cluster: config.ClusterConfig{Name: "test"},
		Services: map[string]config.ServiceConfig{
			"app": {
				Name: "app",
				Type: "manifests",
				Path: filepath.Join(dir, "does-not-exist.yaml"),
			},
		},
	}

	_, _, err := CollectLocalAssets(dir, dir, cfg)
	if err == nil {
		t.Fatal("expected error for missing file, got nil")
	}
}

func TestCollectLocalAssets_Deduplication(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "chart", "Chart.yaml"), "apiVersion: v2\nname: myapp\nversion: 0.1.0\n")
	writeFile(t, filepath.Join(dir, "values.yaml"), "replicaCount: 1\n")

	// Two services sharing the same chart dir and values file.
	cfg := &config.Config{
		Cluster: config.ClusterConfig{Name: "test"},
		Services: map[string]config.ServiceConfig{
			"svc1": {
				Name: "svc1",
				Type: "helm",
				Path: filepath.Join(dir, "chart"),
			},
			"svc2": {
				Name: "svc2",
				Type: "helm",
				Path: filepath.Join(dir, "chart"),
			},
		},
	}

	assets, _, err := CollectLocalAssets(dir, dir, cfg)
	if err != nil {
		t.Fatalf("CollectLocalAssets() error: %v", err)
	}

	seen := make(map[string]int)
	for _, a := range assets {
		seen[a.ArchPath]++
	}
	for p, count := range seen {
		if count > 1 {
			t.Errorf("duplicate asset %q appears %d times", p, count)
		}
	}
}

func TestCollectLocalAssets_SkipsRemoteChart(t *testing.T) {
	dir := t.TempDir()

	cfg := &config.Config{
		Cluster: config.ClusterConfig{Name: "test"},
		Services: map[string]config.ServiceConfig{
			"redis": {
				Name:  "redis",
				Type:  "helm",
				Repo:  "oci://registry-1.docker.io/bitnamicharts",
				Chart: "redis",
			},
		},
	}

	assets, _, err := CollectLocalAssets(dir, dir, cfg)
	if err != nil {
		t.Fatalf("CollectLocalAssets() error: %v", err)
	}
	if len(assets) != 0 {
		t.Errorf("expected no assets for remote chart, got: %v", assetArchPaths(assets))
	}
}

func TestCollectLocalAssets_SkipsHTTPManifest(t *testing.T) {
	dir := t.TempDir()

	cfg := &config.Config{
		Cluster: config.ClusterConfig{Name: "test"},
		Services: map[string]config.ServiceConfig{
			"app": {
				Name: "app",
				Type: "manifests",
				Path: "https://example.com/deploy.yaml",
			},
		},
	}

	assets, _, err := CollectLocalAssets(dir, dir, cfg)
	if err != nil {
		t.Fatalf("CollectLocalAssets() error: %v", err)
	}
	if len(assets) != 0 {
		t.Errorf("expected no assets for HTTP manifest, got: %v", assetArchPaths(assets))
	}
}

// ---- MaybeExtract ----------------------------------------------------------

func TestMaybeExtract_NonArchive(t *testing.T) {
	paths := []string{"/some/kraze.yml", "/other/services.yml"}
	got, cleanup, err := MaybeExtract(paths)
	if err != nil {
		t.Fatalf("MaybeExtract() error: %v", err)
	}
	defer cleanup()

	if len(got) != len(paths) || got[0] != paths[0] {
		t.Errorf("MaybeExtract() = %v, want %v", got, paths)
	}
}

func TestMaybeExtract_Archive(t *testing.T) {
	cfgContent := "cluster:\n  name: test\nservices: {}\n"
	pkg := buildMinimalPackage(t, cfgContent, nil)

	resolved, cleanup, err := MaybeExtract([]string{pkg})
	if err != nil {
		t.Fatalf("MaybeExtract() error: %v", err)
	}
	defer cleanup()

	if len(resolved) != 1 {
		t.Fatalf("expected 1 resolved path, got %d", len(resolved))
	}
	if !strings.HasSuffix(resolved[0], "kraze.yml") {
		t.Errorf("expected path ending in kraze.yml, got %s", resolved[0])
	}
	if _, err := os.Stat(resolved[0]); err != nil {
		t.Errorf("resolved config does not exist: %v", err)
	}
}

func TestMaybeExtract_TgzExtension(t *testing.T) {
	cfgContent := "cluster:\n  name: test\nservices: {}\n"
	pkg := buildMinimalPackage(t, cfgContent, nil)

	tgzPkg := strings.TrimSuffix(pkg, ".tar.gz") + ".tgz"
	if err := os.Rename(pkg, tgzPkg); err != nil {
		t.Fatal(err)
	}

	resolved, cleanup, err := MaybeExtract([]string{tgzPkg})
	if err != nil {
		t.Fatalf("MaybeExtract() for .tgz error: %v", err)
	}
	defer cleanup()

	if len(resolved) != 1 {
		t.Fatalf("expected 1 resolved path, got %d", len(resolved))
	}
}

func TestMaybeExtract_MultipleConfigFiles(t *testing.T) {
	outFile := filepath.Join(t.TempDir(), "multi.tar.gz")
	f, err := os.Create(outFile)
	if err != nil {
		t.Fatal(err)
	}
	gw := gzip.NewWriter(f)
	tw := tar.NewWriter(gw)

	addEntry := func(name, content string) {
		hdr := &tar.Header{Name: name, Mode: 0644, Size: int64(len(content)), Typeflag: tar.TypeReg}
		tw.WriteHeader(hdr)
		tw.Write([]byte(content))
	}
	addEntry("kraze.yml", "cluster:\n  name: test\n")
	addEntry("services/db.yml", "services:\n  postgres:\n    type: helm\n    path: ./.krazepack/charts/postgres.tgz\n")
	meta := PackageMetadata{Version: "1", ConfigFiles: []string{"kraze.yml", "services/db.yml"}}
	metaBytes, _ := json.Marshal(meta)
	addEntry(MetadataFile, string(metaBytes))
	tw.Close()
	gw.Close()
	f.Close()

	resolved, cleanup, err := MaybeExtract([]string{outFile})
	if err != nil {
		t.Fatalf("MaybeExtract() error: %v", err)
	}
	defer cleanup()

	if len(resolved) != 2 {
		t.Fatalf("expected 2 resolved paths, got %d: %v", len(resolved), resolved)
	}
	if !strings.HasSuffix(resolved[0], "kraze.yml") {
		t.Errorf("first config should be kraze.yml, got %s", resolved[0])
	}
	if !strings.HasSuffix(filepath.ToSlash(resolved[1]), "services/db.yml") {
		t.Errorf("second config should be services/db.yml, got %s", resolved[1])
	}
}

func TestMaybeExtract_MixedArchiveAndConfig(t *testing.T) {
	pkg := buildMinimalPackage(t, "cluster:\n  name: test\n", nil)

	_, _, err := MaybeExtract([]string{pkg, "/some/other.yml"})
	if err == nil {
		t.Fatal("expected error when mixing archive and regular config files")
	}
}

func TestMaybeExtract_ExtractedAssetsAccessible(t *testing.T) {
	assetContent := "kind: Deployment\napiVersion: apps/v1\n"
	pkg := buildMinimalPackage(t, "cluster:\n  name: test\nservices: {}\n", map[string]string{
		"manifests/app.yaml": assetContent,
	})

	resolved, cleanup, err := MaybeExtract([]string{pkg})
	if err != nil {
		t.Fatalf("MaybeExtract() error: %v", err)
	}
	defer cleanup()

	assetPath := filepath.Join(filepath.Dir(resolved[0]), "manifests", "app.yaml")
	data, err := os.ReadFile(assetPath)
	if err != nil {
		t.Fatalf("extracted asset not readable: %v", err)
	}
	if string(data) != assetContent {
		t.Errorf("asset content = %q, want %q", data, assetContent)
	}
}

func TestMaybeExtract_FallbackNoMetadata(t *testing.T) {
	// Archive without kraze-package.json — should fall back to kraze.yml.
	outFile := filepath.Join(t.TempDir(), "noMeta.tar.gz")
	f, _ := os.Create(outFile)
	gw := gzip.NewWriter(f)
	tw := tar.NewWriter(gw)

	cfgContent := "cluster:\n  name: test\nservices: {}\n"
	hdr := &tar.Header{Name: "kraze.yml", Mode: 0644, Size: int64(len(cfgContent)), Typeflag: tar.TypeReg}
	tw.WriteHeader(hdr)
	tw.Write([]byte(cfgContent))
	tw.Close()
	gw.Close()
	f.Close()

	resolved, cleanup, err := MaybeExtract([]string{outFile})
	if err != nil {
		t.Fatalf("MaybeExtract() error: %v", err)
	}
	defer cleanup()

	if len(resolved) != 1 || !strings.HasSuffix(resolved[0], "kraze.yml") {
		t.Errorf("expected fallback to kraze.yml, got: %v", resolved)
	}
}

// ---- rewriteConfig ---------------------------------------------------------

func TestRewriteConfig_RemoteHelmChart(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "kraze.yml")
	writeFile(t, cfgPath, `cluster:
  name: test
services:
  redis:
    type: helm
    repo: oci://registry-1.docker.io/bitnamicharts
    chart: redis
    version: "20.2.1"
    namespace: data
`)

	remoteCharts := map[string]remoteChartInfo{
		"redis": {tgzPath: "/tmp/redis-20.2.1.tgz", archPath: ".krazepack/charts/redis.tgz"},
	}
	result, err := rewriteConfig(cfgPath, "kraze.yml", remoteCharts, nil, nil)
	if err != nil {
		t.Fatalf("rewriteConfig() error: %v", err)
	}

	s := string(result)
	if !strings.Contains(s, ".krazepack/charts/redis.tgz") {
		t.Errorf("expected .krazepack/charts/redis.tgz in result:\n%s", s)
	}
	if strings.Contains(s, "repo:") {
		t.Errorf("expected repo: removed:\n%s", s)
	}
	if strings.Contains(s, "chart:") {
		t.Errorf("expected chart: removed:\n%s", s)
	}
	if strings.Contains(s, "version:") {
		t.Errorf("expected version: removed:\n%s", s)
	}
	if !strings.Contains(s, "namespace: data") {
		t.Errorf("expected namespace: data preserved:\n%s", s)
	}
}

func TestRewriteConfig_HTTPManifest(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "kraze.yml")
	writeFile(t, cfgPath, `cluster:
  name: test
services:
  metrics:
    type: manifests
    path: https://example.com/metrics.yaml
`)

	remoteManifests := map[string]remoteManifestInfo{
		"metrics": {files: []remoteManifestFile{{origURL: "https://example.com/metrics.yaml", content: []byte("kind: Deployment\n"), archPath: ".krazepack/manifests/metrics.yaml"}}},
	}
	result, err := rewriteConfig(cfgPath, "kraze.yml", nil, remoteManifests, nil)
	if err != nil {
		t.Fatalf("rewriteConfig() error: %v", err)
	}

	s := string(result)
	if !strings.Contains(s, ".krazepack/manifests/metrics.yaml") {
		t.Errorf("expected .krazepack/manifests/metrics.yaml:\n%s", s)
	}
	if strings.Contains(s, "https://") {
		t.Errorf("expected https:// URL replaced:\n%s", s)
	}
}

func TestRewriteConfig_NestedConfigFile(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "services", "db.yml")
	writeFile(t, cfgPath, `services:
  postgres:
    type: helm
    repo: oci://registry-1.docker.io/charts
    chart: postgres
    version: "1.0.0"
`)

	remoteCharts := map[string]remoteChartInfo{
		"postgres": {tgzPath: "/tmp/pg.tgz", archPath: ".krazepack/charts/postgres.tgz"},
	}
	// archConfigPath is "services/db.yml" (relative to archive root, one level deep)
	result, err := rewriteConfig(cfgPath, "services/db.yml", remoteCharts, nil, nil)
	if err != nil {
		t.Fatalf("rewriteConfig() error: %v", err)
	}

	// Since config is at "services/db.yml", path to .krazepack should go up one level.
	if !strings.Contains(string(result), "../.krazepack/charts/postgres.tgz") {
		t.Errorf("expected ../.krazepack/charts/postgres.tgz in result:\n%s", result)
	}
}

func TestRewriteConfig_NoRemoteAssets_Unchanged(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "kraze.yml")
	original := "cluster:\n  name: test\nservices:\n  myapp:\n    type: helm\n    path: ./myapp\n"
	writeFile(t, cfgPath, original)

	result, err := rewriteConfig(cfgPath, "kraze.yml", nil, nil, nil)
	if err != nil {
		t.Fatalf("rewriteConfig() error: %v", err)
	}
	if string(result) != original {
		t.Errorf("expected file unchanged when no remote assets, got:\n%s", result)
	}
}

// ---- extractTar ------------------------------------------------------------

func TestRewriteConfig_HTTPManifestPluralPaths(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "kraze.yml")
	writeFile(t, cfgPath, `cluster:
  name: test
services:
  crds:
    type: manifests
    paths:
      - https://example.com/crds.yaml
      - ./local-rbac.yaml
      - https://example.com/webhooks.yaml
`)

	// Simulate what CreatePackage would build: files for the two HTTP URLs (indices 0 and 2).
	remoteManifests := map[string]remoteManifestInfo{
		"crds": {files: []remoteManifestFile{
			{origURL: "https://example.com/crds.yaml", content: []byte("kind: CRD\n"), archPath: ".krazepack/manifests/crds-0.yaml"},
			{origURL: "https://example.com/webhooks.yaml", content: []byte("kind: MutatingWebhookConfiguration\n"), archPath: ".krazepack/manifests/crds-2.yaml"},
		}},
	}

	result, err := rewriteConfig(cfgPath, "kraze.yml", nil, remoteManifests, nil)
	if err != nil {
		t.Fatalf("rewriteConfig() error: %v", err)
	}

	s := string(result)

	if !strings.Contains(s, ".krazepack/manifests/crds-0.yaml") {
		t.Errorf("expected crds-0.yaml in result:\n%s", s)
	}
	if !strings.Contains(s, ".krazepack/manifests/crds-2.yaml") {
		t.Errorf("expected crds-2.yaml in result:\n%s", s)
	}
	// Local path entry must be preserved as-is.
	if !strings.Contains(s, "local-rbac.yaml") {
		t.Errorf("expected local-rbac.yaml preserved in result:\n%s", s)
	}
	if strings.Contains(s, "https://") {
		t.Errorf("expected all https:// URLs replaced:\n%s", s)
	}
}

func TestRewriteConfig_ExternalLocalHelmChart(t *testing.T) {
	dir := t.TempDir()
	outsideDir := t.TempDir()
	chartDir := filepath.Join(outsideDir, "charts", "ruya")
	writeFile(t, filepath.Join(chartDir, "Chart.yaml"), "apiVersion: v2\nname: ruya\nversion: 0.1.0\n")

	// The config references the chart with a relative path going outside the project.
	relPath, err := filepath.Rel(dir, chartDir)
	if err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(dir, "kraze.yml")
	writeFile(t, cfgPath, "cluster:\n  name: test\nservices:\n  ruya:\n    type: helm\n    path: "+relPath+"\n")

	// Simulate what CollectLocalAssets would produce for the external chart dir.
	externalPaths := map[string]string{
		filepath.Clean(chartDir): ".krazepack/charts/ruya",
	}

	result, err := rewriteConfig(cfgPath, "kraze.yml", nil, nil, externalPaths)
	if err != nil {
		t.Fatalf("rewriteConfig() error: %v", err)
	}

	s := string(result)
	if !strings.Contains(s, ".krazepack/charts/ruya") {
		t.Errorf("expected .krazepack/charts/ruya in result:\n%s", s)
	}
	// Original relative path should be gone.
	if strings.Contains(s, relPath) {
		t.Errorf("expected original path %q replaced in result:\n%s", relPath, s)
	}
}

func TestCollectLocalAssets_SkipsHTTPManifestInPaths(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "local.yaml"), "kind: Namespace\n")

	cfg := &config.Config{
		Cluster: config.ClusterConfig{Name: "test"},
		Services: map[string]config.ServiceConfig{
			"app": {
				Name:  "app",
				Type:  "manifests",
				Paths: []string{"https://example.com/remote.yaml", filepath.Join(dir, "local.yaml")},
			},
		},
	}

	assets, _, err := CollectLocalAssets(dir, dir, cfg)
	if err != nil {
		t.Fatalf("CollectLocalAssets() error: %v", err)
	}

	paths := assetArchPaths(assets)
	if len(assets) != 1 {
		t.Fatalf("expected 1 asset (local only), got %d: %v", len(assets), paths)
	}
	if !containsPath(paths, "local.yaml") {
		t.Errorf("expected local.yaml in assets, got: %v", paths)
	}
}

func TestExtractTar_PathTraversal(t *testing.T) {
	// Build a .tar.gz with a path traversal entry.
	outFile := filepath.Join(t.TempDir(), "evil.tar.gz")
	f, _ := os.Create(outFile)
	gw := gzip.NewWriter(f)
	tw := tar.NewWriter(gw)

	evil := "../../etc/passwd"
	hdr := &tar.Header{Name: evil, Mode: 0644, Size: 5, Typeflag: tar.TypeReg}
	tw.WriteHeader(hdr)
	tw.Write([]byte("hello"))
	tw.Close()
	gw.Close()
	f.Close()

	destDir := t.TempDir()
	err := extractTar(outFile, destDir)
	if err == nil {
		t.Fatal("expected error for path traversal, got nil")
	}
	if !strings.Contains(err.Error(), "path traversal") {
		t.Errorf("expected 'path traversal' in error, got: %v", err)
	}
}
