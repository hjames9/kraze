package providers

import (
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	v2chart "helm.sh/helm/v4/pkg/chart/v2"
	chartutil "helm.sh/helm/v4/pkg/chart/v2/util"
	repov1 "helm.sh/helm/v4/pkg/repo/v1"

	"github.com/hjames9/kraze/internal/config"
)

// serveChart serves a chart repository holding mychart 1.0.0 and returns its
// URL (httptest URLs always have a port)
func serveChart(test *testing.T) string {
	test.Helper()
	dir := test.TempDir()
	chart := &v2chart.Chart{Metadata: &v2chart.Metadata{APIVersion: "v2", Name: "mychart", Version: "1.0.0"}}
	if _, err := chartutil.Save(chart, dir); err != nil {
		test.Fatal(err)
	}
	server := httptest.NewServer(http.FileServer(http.Dir(dir)))
	test.Cleanup(server.Close)
	index, err := repov1.IndexDirectory(dir, server.URL)
	if err != nil {
		test.Fatal(err)
	}
	if err := index.WriteFile(filepath.Join(dir, "index.yaml"), 0o644); err != nil {
		test.Fatal(err)
	}
	return server.URL
}

// A repository URL with a port (ChartMuseum on :8080, a registry on :8443)
// used to give a repository name with a ":", and Helm rejected the chart
// reference "<name>/<chart>" as an invalid URL. The tests above only
// downloaded the index, which doesn't use the name that way.
func TestPullChartFromRepositoryWithPort(test *testing.T) {
	helm := newTestHelmProvider(test)
	svc := &config.ServiceConfig{Name: "app", Type: "helm", Repo: serveChart(test), Chart: "mychart", Version: "1.0.0"}

	path, err := helm.PullChartToDir(svc, test.TempDir())
	if err != nil {
		test.Fatalf("pulling from %s: %v", svc.Repo, err)
	}
	if filepath.Base(path) != "mychart-1.0.0.tgz" {
		test.Errorf("pulled %s", path)
	}
}

// A repositories.yaml entry that already has this URL's generated name but
// another URL (the name isn't unique: http:// and https:// give the same one)
// must not be used: its repository would serve the chart instead
func TestAddHTTPRepositoryForChart_NameTakenByAnotherURL(test *testing.T) {
	helm := newTestHelmProvider(test)
	repoURL := serveChart(test)
	taken := generateRepoName(repoURL)

	file := repov1.NewFile()
	file.Update(&repov1.Entry{Name: taken, URL: "https://elsewhere.invalid/charts", Username: "keep"})
	if err := os.MkdirAll(filepath.Dir(helm.settings.RepositoryConfig), 0o755); err != nil {
		test.Fatal(err)
	}
	if err := file.WriteFile(helm.settings.RepositoryConfig, 0o644); err != nil {
		test.Fatal(err)
	}

	name, err := helm.addHTTPRepositoryForChart(repoURL, "mychart", "1.0.0")
	if err != nil {
		test.Fatal(err)
	}
	if name == taken {
		test.Fatalf("used the entry for another repository (%s)", taken)
	}
	if _, err := cachedIndex(test, helm, name).Get("mychart", "1.0.0"); err != nil {
		test.Errorf("index not cached under %s: %v", name, err)
	}
	file, err = repov1.LoadFile(helm.settings.RepositoryConfig)
	if err != nil {
		test.Fatal(err)
	}
	if other := file.Get(taken); other == nil || other.URL != "https://elsewhere.invalid/charts" || other.Username != "keep" {
		test.Errorf("the other repository's entry changed: %+v", other)
	}
	if mine := file.Get(name); mine == nil || mine.URL != repoURL {
		test.Errorf("entry for %s = %+v", repoURL, mine)
	}

	// The same URL resolves to the same name next time
	if again, err := helm.addHTTPRepositoryForChart(repoURL, "mychart", "1.0.0"); err != nil || again != name {
		test.Errorf("second call: %q, %v (want %q)", again, err, name)
	}
}

// Entries in repositories.yaml (and credentials on them) are found by name,
// so URLs that already gave a usable name keep it
func TestGenerateRepoNameKeepsUsableNames(test *testing.T) {
	for url, want := range map[string]string{
		"https://charts.bitnami.com/bitnami":                 "charts-bitnami-com-bitnami",
		"https://prometheus-community.github.io/helm-charts": "prometheus-community-github-io-helm-charts",
		"http://charts.example.com/":                         "charts-example-com",
	} {
		if got := generateRepoName(url); got != want {
			test.Errorf("generateRepoName(%q) = %q, want %q", url, got, want)
		}
	}
}

// Every name works as the first part of a chart reference
func TestGenerateRepoNameIsAlwaysUsable(test *testing.T) {
	valid := regexp.MustCompile(`^[A-Za-z0-9-]+$`)
	rng := rand.New(rand.NewSource(5))
	alphabet := "abcXYZ019.-_/:@?=&%+~"
	for run := 0; run < 2000; run++ {
		var b strings.Builder
		for i := rng.Intn(70) + 1; i > 0; i-- {
			b.WriteByte(alphabet[rng.Intn(len(alphabet))])
		}
		url := []string{"https://", "http://"}[rng.Intn(2)] + "h" + b.String()
		if name := generateRepoName(url); !valid.MatchString(name) {
			test.Fatalf("generateRepoName(%q) = %q", url, name)
		}
	}
}
