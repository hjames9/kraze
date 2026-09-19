package providers

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"helm.sh/helm/v4/pkg/cli"
	"helm.sh/helm/v4/pkg/helmpath"
	repov1 "helm.sh/helm/v4/pkg/repo/v1"
)

// indexYAML builds a minimal but valid Helm repository index containing the
// given versions of a chart named "mychart".
func indexYAML(repoURL string, versions ...string) string {
	index := "apiVersion: v1\nentries:\n  mychart:\n"
	for _, version := range versions {
		index += fmt.Sprintf(`  - apiVersion: v2
    name: mychart
    version: %s
    created: "2026-01-01T00:00:00Z"
    digest: %064d
    urls:
    - %s/mychart-%s.tgz
`, version, 0, repoURL, version)
	}
	index += "generated: \"2026-01-01T00:00:00Z\"\n"
	return index
}

// newIndexServer serves an index.yaml whose contents can be swapped between
// requests, and counts how many times the index was fetched.
func newIndexServer(test *testing.T) (server *httptest.Server, setIndex func(versions ...string), hits *atomic.Int32) {
	test.Helper()

	var current atomic.Value
	var counter atomic.Int32

	server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if filepath.Base(request.URL.Path) != "index.yaml" {
			http.NotFound(writer, request)
			return
		}
		counter.Add(1)
		_, _ = fmt.Fprint(writer, current.Load().(string))
	}))
	test.Cleanup(server.Close)

	setIndex = func(versions ...string) {
		current.Store(indexYAML(server.URL, versions...))
	}
	setIndex("1.0.0")

	return server, setIndex, &counter
}

// newTestHelmProvider returns a HelmProvider whose Helm config and cache live
// in an isolated temp directory, so tests never touch the developer's
// ~/.config/helm or ~/.cache/helm.
func newTestHelmProvider(test *testing.T) *HelmProvider {
	test.Helper()

	home := test.TempDir()
	test.Setenv("HELM_CONFIG_HOME", filepath.Join(home, "config"))
	test.Setenv("HELM_CACHE_HOME", filepath.Join(home, "cache"))
	test.Setenv("HELM_DATA_HOME", filepath.Join(home, "data"))

	return &HelmProvider{
		opts:     &ProviderOptions{Quiet: true},
		settings: cli.New(),
	}
}

// cachedIndex loads the repository index Helm's downloader would consult when
// resolving a chart reference.
func cachedIndex(test *testing.T, helm *HelmProvider, repoName string) *repov1.IndexFile {
	test.Helper()

	path := filepath.Join(helm.settings.RepositoryCache, helmpath.CacheIndexFile(repoName))
	index, err := repov1.LoadIndexFile(path)
	if err != nil {
		test.Fatalf("failed to load cached index %s: %v", path, err)
	}
	return index
}

// A chart version published after kraze first added the repository must be
// visible on the next `kraze up`, otherwise Helm resolves the chart from a
// stale cached index and fails with "not found in ... index".
func TestAddHTTPRepositoryForChart_RefreshesStaleIndex(test *testing.T) {
	server, setIndex, hits := newIndexServer(test)
	helm := newTestHelmProvider(test)

	repoName, err := helm.addHTTPRepositoryForChart(server.URL, "mychart", "")
	if err != nil {
		test.Fatalf("first addHTTPRepository failed: %v", err)
	}
	if got := hits.Load(); got != 1 {
		test.Fatalf("expected 1 index download on first add, got %d", got)
	}

	// A new chart version is published upstream.
	setIndex("1.0.0", "2.0.0")

	if _, err := helm.addHTTPRepositoryForChart(server.URL, "mychart", ""); err != nil {
		test.Fatalf("second addHTTPRepository failed: %v", err)
	}
	if got := hits.Load(); got != 2 {
		test.Errorf("expected the index to be re-downloaded, got %d total downloads", got)
	}

	if _, err := cachedIndex(test, helm, repoName).Get("mychart", "2.0.0"); err != nil {
		test.Errorf("newly published chart version missing from cached index: %v", err)
	}
}

// The repositories.yaml entry can outlive the cached index file (cleared cache,
// fresh machine sharing a config). kraze must re-download rather than let Helm
// fail with "no cached repo found".
func TestAddHTTPRepositoryForChart_RestoresMissingIndexCache(test *testing.T) {
	server, _, _ := newIndexServer(test)
	helm := newTestHelmProvider(test)

	repoName, err := helm.addHTTPRepositoryForChart(server.URL, "mychart", "")
	if err != nil {
		test.Fatalf("first addHTTPRepository failed: %v", err)
	}

	indexPath := filepath.Join(helm.settings.RepositoryCache, helmpath.CacheIndexFile(repoName))
	if err := os.Remove(indexPath); err != nil {
		test.Fatalf("failed to remove cached index: %v", err)
	}

	if _, err := helm.addHTTPRepositoryForChart(server.URL, "mychart", ""); err != nil {
		test.Fatalf("second addHTTPRepository failed: %v", err)
	}

	if _, err := os.Stat(indexPath); err != nil {
		test.Errorf("cached index was not restored: %v", err)
	}
}

// Helm's downloader reads the index from settings.RepositoryCache, but
// repov1.NewChartRepository defaults CachePath to helmpath.CachePath. When
// HELM_REPOSITORY_CACHE points elsewhere the index must still land where the
// downloader looks for it.
func TestAddHTTPRepositoryForChart_HonorsRepositoryCacheSetting(test *testing.T) {
	server, _, _ := newIndexServer(test)
	helm := newTestHelmProvider(test)

	cacheDir := test.TempDir()
	test.Setenv("HELM_REPOSITORY_CACHE", cacheDir)
	helm.settings = cli.New()

	repoName, err := helm.addHTTPRepositoryForChart(server.URL, "mychart", "")
	if err != nil {
		test.Fatalf("addHTTPRepository failed: %v", err)
	}

	indexPath := filepath.Join(cacheDir, helmpath.CacheIndexFile(repoName))
	if _, err := os.Stat(indexPath); err != nil {
		test.Errorf("index not written to HELM_REPOSITORY_CACHE (%s): %v", indexPath, err)
	}
}

// An exact version already present in the cached index needs no refresh, so
// large repository indexes are not re-downloaded on every `kraze up`.
func TestAddHTTPRepositoryForChart_SkipsRefreshWhenVersionCached(test *testing.T) {
	server, _, hits := newIndexServer(test)
	helm := newTestHelmProvider(test)

	if _, err := helm.addHTTPRepositoryForChart(server.URL, "mychart", "1.0.0"); err != nil {
		test.Fatalf("first call failed: %v", err)
	}
	if got := hits.Load(); got != 1 {
		test.Fatalf("expected 1 index download on first add, got %d", got)
	}

	if _, err := helm.addHTTPRepositoryForChart(server.URL, "mychart", "1.0.0"); err != nil {
		test.Fatalf("second call failed: %v", err)
	}
	if got := hits.Load(); got != 1 {
		test.Errorf("expected the cached index to be reused, got %d downloads", got)
	}
}

// A version the cached index cannot satisfy triggers a refresh, as does an
// unpinned version or a range, both of which mean "newest match".
func TestAddHTTPRepositoryForChart_RefreshesWhenVersionNotCached(test *testing.T) {
	tests := []struct {
		name    string
		version string
	}{
		{name: "newly published version", version: "2.0.0"},
		{name: "unpinned version", version: ""},
		{name: "version range", version: ">=1.0.0"},
	}

	for _, testCase := range tests {
		test.Run(testCase.name, func(test *testing.T) {
			server, setIndex, hits := newIndexServer(test)
			helm := newTestHelmProvider(test)

			if _, err := helm.addHTTPRepositoryForChart(server.URL, "mychart", "1.0.0"); err != nil {
				test.Fatalf("first call failed: %v", err)
			}
			setIndex("1.0.0", "2.0.0")

			repoName, err := helm.addHTTPRepositoryForChart(server.URL, "mychart", testCase.version)
			if err != nil {
				test.Fatalf("second call failed: %v", err)
			}
			if got := hits.Load(); got != 2 {
				test.Errorf("expected the index to be re-downloaded, got %d downloads", got)
			}
			if _, err := cachedIndex(test, helm, repoName).Get("mychart", "2.0.0"); err != nil {
				test.Errorf("newly published chart version missing from cached index: %v", err)
			}
		})
	}
}

// A failed refresh of a known repository must fall back to the cached index
// rather than break an otherwise-workable offline run.
func TestAddHTTPRepositoryForChart_FallsBackToCachedIndexWhenOffline(test *testing.T) {
	server, _, _ := newIndexServer(test)
	helm := newTestHelmProvider(test)

	repoName, err := helm.addHTTPRepositoryForChart(server.URL, "mychart", "1.0.0")
	if err != nil {
		test.Fatalf("first call failed: %v", err)
	}

	server.Close()

	if _, err := helm.addHTTPRepositoryForChart(server.URL, "mychart", "2.0.0"); err != nil {
		test.Fatalf("expected fallback to cached index, got error: %v", err)
	}
	if _, err := cachedIndex(test, helm, repoName).Get("mychart", "1.0.0"); err != nil {
		test.Errorf("cached index was lost: %v", err)
	}
}

// An unreachable repository that was never added has no cached index to fall
// back on, so it must still be a hard error.
func TestAddHTTPRepositoryForChart_FailsWhenUnreachableAndUncached(test *testing.T) {
	server, _, _ := newIndexServer(test)
	helm := newTestHelmProvider(test)
	server.Close()

	if _, err := helm.addHTTPRepositoryForChart(server.URL, "mychart", "1.0.0"); err == nil {
		test.Error("expected an error for an unreachable, never-added repository")
	}
}

// Refreshing a known repository must not clobber credentials or TLS settings
// recorded in repositories.yaml.
func TestAddHTTPRepositoryForChart_PreservesRepositoryEntry(test *testing.T) {
	server, setIndex, hits := newIndexServer(test)
	helm := newTestHelmProvider(test)

	repoName, err := helm.addHTTPRepositoryForChart(server.URL, "mychart", "1.0.0")
	if err != nil {
		test.Fatalf("first call failed: %v", err)
	}

	// Simulate a user (or `helm repo add`) attaching credentials to the entry.
	repoFile, err := repov1.LoadFile(helm.settings.RepositoryConfig)
	if err != nil {
		test.Fatalf("failed to load repository file: %v", err)
	}
	entry := repoFile.Get(repoName)
	entry.Username = "someone"
	entry.Password = "s3cret"
	entry.InsecureSkipTLSVerify = true
	if err := repoFile.WriteFile(helm.settings.RepositoryConfig, 0644); err != nil {
		test.Fatalf("failed to write repository file: %v", err)
	}

	setIndex("1.0.0", "2.0.0")
	if _, err := helm.addHTTPRepositoryForChart(server.URL, "mychart", "2.0.0"); err != nil {
		test.Fatalf("refresh failed: %v", err)
	}
	if got := hits.Load(); got != 2 {
		test.Fatalf("expected the index to be re-downloaded, got %d downloads", got)
	}

	repoFile, err = repov1.LoadFile(helm.settings.RepositoryConfig)
	if err != nil {
		test.Fatalf("failed to reload repository file: %v", err)
	}
	entry = repoFile.Get(repoName)
	if entry == nil {
		test.Fatalf("repository entry %q disappeared", repoName)
	}
	if entry.Username != "someone" || entry.Password != "s3cret" || !entry.InsecureSkipTLSVerify {
		test.Errorf("repository entry was clobbered by refresh: %+v", entry)
	}
}
