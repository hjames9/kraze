package pack

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestCollectLocalAssetsMissing(t *testing.T) {
	for name, services := range map[string]string{
		"ca certificate": "",
		"chart":          "  a:\n    type: helm\n    path: ./charts/missing\n",
		"paths entry":    "  a:\n    type: manifests\n    paths: [./missing.yaml]\n",
		"values file":    "  a:\n    type: helm\n    path: ./chart\n    values: ./missing-values.yaml\n",
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			writeFile(t, filepath.Join(dir, "chart", "Chart.yaml"), "apiVersion: v2\nname: c\nversion: 0.1.0\n")
			cluster := "  name: c\n"
			if name == "ca certificate" {
				cluster += "  ca_certificates: [./missing.crt]\n"
			}
			cfgPath := filepath.Join(dir, "kraze.yml")
			writeFile(t, cfgPath, "project: web\ncluster:\n"+cluster+"services:\n"+services)
			if _, _, err := CollectLocalAssets(dir, dir, parse(t, cfgPath)); err == nil {
				t.Error("want an error for a missing asset")
			}
		})
	}

	// A chart directory with a subdirectory that can't be read
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "chart", "Chart.yaml"), "apiVersion: v2\nname: c\nversion: 0.1.0\n")
	locked := filepath.Join(dir, "chart", "templates")
	if err := os.MkdirAll(locked, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })
	cfgPath := filepath.Join(dir, "kraze.yml")
	writeFile(t, cfgPath, "project: web\ncluster:\n  name: c\nservices:\n  a:\n    type: helm\n    path: ./chart\n")
	if _, _, err := CollectLocalAssets(dir, dir, parse(t, cfgPath)); err == nil {
		t.Error("want an error for a chart directory that can't be walked")
	}
}

func TestCreatePackageFileFailures(t *testing.T) {
	project := func(t *testing.T) (string, string) {
		dir := t.TempDir()
		writeFile(t, filepath.Join(dir, "app.yaml"), "kind: ConfigMap\n")
		cfgPath := filepath.Join(dir, "kraze.yml")
		writeFile(t, cfgPath, "project: web\ncluster:\n  name: c\nservices:\n  app:\n    type: manifests\n    path: ./app.yaml\n")
		return dir, cfgPath
	}
	out := func(t *testing.T) string { return filepath.Join(t.TempDir(), "out.tar.gz") }

	t.Run("no staging dir", func(t *testing.T) {
		_, cfgPath := project(t)
		cfg := parse(t, cfgPath)
		t.Setenv("TMPDIR", filepath.Join(t.TempDir(), "missing"))
		if err := CreatePackage([]string{cfgPath}, cfg, "dev", out(t), false); err == nil || !strings.Contains(err.Error(), "creating staging dir") {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("config gone before rewriting", func(t *testing.T) {
		_, cfgPath := project(t)
		cfg := parse(t, cfgPath)
		if err := os.Remove(cfgPath); err != nil {
			t.Fatal(err)
		}
		if err := CreatePackage([]string{cfgPath}, cfg, "dev", out(t), false); err == nil || !strings.Contains(err.Error(), "rewriting") {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("config no longer YAML", func(t *testing.T) {
		// Only parsed when something in it is rewritten
		_, cfgPath := project(t)
		writeFile(t, cfgPath, "services: [\n")
		remote := map[string]remoteChartInfo{"app": {archPath: ".krazepack/charts/app.tgz"}}
		if _, err := rewriteConfig(cfgPath, "kraze.yml", remote, nil, nil); err == nil || !strings.Contains(err.Error(), "parsing YAML") {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("asset unreadable", func(t *testing.T) {
		dir, cfgPath := project(t)
		cfg := parse(t, cfgPath)
		if err := os.Chmod(filepath.Join(dir, "app.yaml"), 0o000); err != nil {
			t.Fatal(err)
		}
		if err := CreatePackage([]string{cfgPath}, cfg, "dev", out(t), false); err == nil || !strings.Contains(err.Error(), "writing asset") {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("manifest server unreachable", func(t *testing.T) {
		dir := t.TempDir()
		cfgPath := filepath.Join(dir, "kraze.yml")
		writeFile(t, cfgPath, "project: web\ncluster:\n  name: c\nservices:\n  app:\n    type: manifests\n    path: http://127.0.0.1:1/app.yaml\n")
		if err := CreatePackage([]string{cfgPath}, parse(t, cfgPath), "dev", out(t), false); err == nil || !strings.Contains(err.Error(), "downloading manifest") {
			t.Errorf("err = %v", err)
		}
	})
}

// rawTarGz writes a gzipped tar with entries given as headers and contents
func rawTarGz(t *testing.T, path string, entries ...func(tw *tar.Writer)) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	gw := gzip.NewWriter(f)
	tw := tar.NewWriter(gw)
	for _, e := range entries {
		e(tw)
	}
	_ = tw.Close()
	_ = gw.Close()
	_ = f.Close()
}

func dirEntry(name string) func(tw *tar.Writer) {
	return func(tw *tar.Writer) {
		_ = tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeDir, Mode: 0o755})
	}
}

func fileEntry(name, content string) func(tw *tar.Writer) {
	return func(tw *tar.Writer) {
		_ = tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(content))})
		_, _ = tw.Write([]byte(content))
	}
}

func TestExtractTarEntries(t *testing.T) {
	dir := t.TempDir()

	// Directory entries are created
	withDirs := filepath.Join(dir, "dirs.tar.gz")
	rawTarGz(t, withDirs, dirEntry("charts/"), fileEntry("kraze.yml", "x"))
	dest := t.TempDir()
	if err := extractTar(withDirs, dest); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(filepath.Join(dest, "charts")); err != nil || !info.IsDir() {
		t.Errorf("charts should be a directory: %v", err)
	}

	for name, entries := range map[string][]func(*tar.Writer){
		"a file where a directory is needed":  {fileEntry("a", "x"), fileEntry("a/b", "y")},
		"a directory where a file is needed":  {dirEntry("d/"), fileEntry("d", "x")},
		"a file where a directory entry goes": {fileEntry("e", "x"), dirEntry("e/f/")},
	} {
		archive := filepath.Join(dir, strings.ReplaceAll(name, " ", "-")+".tar.gz")
		rawTarGz(t, archive, entries...)
		if err := extractTar(archive, t.TempDir()); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}

	// Gzip around something that isn't a tar
	notTar := filepath.Join(dir, "nottar.tar.gz")
	f, err := os.Create(notTar)
	if err != nil {
		t.Fatal(err)
	}
	gw := gzip.NewWriter(f)
	_, _ = gw.Write([]byte(strings.Repeat("not a tar header ", 64)))
	_ = gw.Close()
	_ = f.Close()
	if err := extractTar(notTar, t.TempDir()); err == nil || !strings.Contains(err.Error(), "reading tar entry") {
		t.Errorf("err = %v", err)
	}

	// Metadata that can't be read (a directory under its name)
	badMeta := filepath.Join(dir, "badmeta.tar.gz")
	rawTarGz(t, badMeta, dirEntry(MetadataFile+"/"), fileEntry("kraze.yml", "x"))
	if _, _, err := MaybeExtract([]string{badMeta}); err == nil || !strings.Contains(err.Error(), "reading package metadata") {
		t.Errorf("err = %v", err)
	}

	// No temp dir for extracting
	t.Setenv("TMPDIR", filepath.Join(t.TempDir(), "missing"))
	if _, _, err := MaybeExtract([]string{withDirs}); err == nil || !strings.Contains(err.Error(), "creating temp dir") {
		t.Errorf("err = %v", err)
	}
}

// With something to rewrite, configs whose services don't match are left as
// they are
func TestRewriteConfigUnmatchedServices(t *testing.T) {
	remote := map[string]remoteChartInfo{"elsewhere": {archPath: ".krazepack/charts/elsewhere.tgz"}}
	dir := t.TempDir()
	for name, content := range map[string]string{
		"no-services.yml":  "project: web\ncluster:\n  name: c\n",
		"null-service.yml": "project: web\ncluster:\n  name: c\nservices:\n  a:\n",
		"unmatched.yml":    "project: web\ncluster:\n  name: c\nservices:\n  a:\n    type: manifests\n    path: ./a.yaml\n",
	} {
		path := filepath.Join(dir, name)
		writeFile(t, path, content)
		got, err := rewriteConfig(path, name, remote, nil, nil)
		if err != nil || string(got) != content {
			t.Errorf("%s: rewritten to %q, %v; want it unchanged", name, got, err)
		}
	}
}

// An entry shorter than its header says is an error, not a short file
func TestExtractTarTruncatedEntry(t *testing.T) {
	archive := filepath.Join(t.TempDir(), "truncated.tar.gz")
	rawTarGz(t, archive, func(tw *tar.Writer) {
		_ = tw.WriteHeader(&tar.Header{Name: "kraze.yml", Typeflag: tar.TypeReg, Mode: 0o644, Size: 100})
		_, _ = tw.Write([]byte("short"))
		// Close would pad the entry; flush only what was written
		_ = tw.Flush()
	})
	if err := extractTar(archive, t.TempDir()); err == nil {
		t.Error("want an error for a truncated entry")
	}
}

// A service name the filesystem can't use (YAML allows any string as a key)
// fails staging its chart
func TestCreatePackageUnusableServiceName(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "kraze.yml")
	writeFile(t, cfgPath, "project: web\ncluster:\n  name: c\nservices:\n  \"bad\\0name\":\n    type: helm\n    repo: https://charts.example\n    chart: c\n")
	err := CreatePackage([]string{cfgPath}, parse(t, cfgPath), "dev", filepath.Join(t.TempDir(), "out.tar.gz"), false)
	if err == nil || !strings.Contains(err.Error(), "creating chart staging dir") {
		t.Errorf("err = %v", err)
	}
}

// failAfter accepts n bytes, then fails every write
type failAfter struct{ n int }

func (w *failAfter) Write(p []byte) (int, error) {
	if len(p) > w.n {
		written := w.n
		w.n = 0
		return written, errors.New("disk full")
	}
	w.n -= len(p)
	return len(p), nil
}

// A package with one of everything writePackageTar writes
func testPackageContents(t *testing.T) packageContents {
	t.Helper()
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "kraze.yml")
	writeFile(t, cfgPath, "project: web\ncluster:\n  name: c\nservices:\n  a:\n    type: manifests\n    path: ./a.yaml\n")
	chart := filepath.Join(dir, "chart.tgz")
	writeFile(t, chart, "chart")
	asset := filepath.Join(dir, "a.yaml")
	writeFile(t, asset, "kind: ConfigMap\n")
	return packageContents{
		archiveRoot:  dir,
		configPaths:  []string{cfgPath},
		krazeVersion: "test",
		remoteCharts: map[string]remoteChartInfo{"db": {tgzPath: chart, archPath: ".krazepack/charts/db.tgz"}},
		remoteManifests: map[string]remoteManifestInfo{"web": {files: []remoteManifestFile{
			{origURL: "https://example.com/web.yaml", content: []byte("kind: Service\n"), archPath: ".krazepack/manifests/web-0.yaml"},
		}}},
		localAssets: []Asset{{AbsPath: asset, ArchPath: "a.yaml"}},
	}
}

// Every write of the package can fail; each failure names what was being written
func TestWritePackageTarFailures(t *testing.T) {
	pc := testPackageContents(t)
	var full countingWriter
	if err := writePackageTar(&full, pc); err != nil {
		t.Fatal(err)
	}

	// tar writes in 512-byte blocks, so failing at each block boundary reaches
	// every step
	var errs []string
	for n := 0; n < full.n; n += 512 {
		err := writePackageTar(&failAfter{n: n}, pc)
		if err == nil {
			t.Fatalf("failing after %d of %d bytes: want an error", n, full.n)
		}
		errs = append(errs, err.Error())
	}
	for _, want := range []string{"writing config", "writing bundled chart", "writing bundled manifest", "writing asset", "writing metadata", "closing tar"} {
		if !slices.ContainsFunc(errs, func(e string) bool { return strings.HasPrefix(e, want) }) {
			t.Errorf("no failure reported as %q; got %q", want, errs)
		}
	}
}

// gzip writes its header on the first write and buffers the rest, so a small
// package that fails after the header fails closing the gzip stream
func TestWritePackageGzipCloseFails(t *testing.T) {
	err := writePackage(&failAfter{n: 10}, testPackageContents(t))
	if err == nil || !strings.HasPrefix(err.Error(), "closing gzip") {
		t.Errorf("err = %v, want closing gzip", err)
	}
}

type countingWriter struct{ n int }

func (w *countingWriter) Write(p []byte) (int, error) { w.n += len(p); return len(p), nil }

// Entries and packages over the limits are rejected
func TestExtractTarLimits(t *testing.T) {
	archive := filepath.Join(t.TempDir(), "big.tar.gz")
	rawTarGz(t, archive, fileEntry("a", strings.Repeat("x", 10)), fileEntry("b", strings.Repeat("y", 10)))

	if err := extractTarLimited(archive, t.TempDir(), 100, 100); err != nil {
		t.Errorf("within the limits: %v", err)
	}
	if err := extractTarLimited(archive, t.TempDir(), 9, 100); err == nil || !strings.Contains(err.Error(), `"a" exceeds the 9 byte per-file limit`) {
		t.Errorf("entry over the limit: err = %v", err)
	}
	if err := extractTarLimited(archive, t.TempDir(), 100, 15); err == nil || !strings.Contains(err.Error(), "exceed the 15 byte limit") {
		t.Errorf("total over the limit: err = %v", err)
	}
}
