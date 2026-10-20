package cluster

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/hjames9/kraze/internal/config"
)

func TestGetImageInfo(test *testing.T) {
	ctx := context.Background()
	im, run := newTestImages()
	run.on("docker inspect built:dev", `[{"Id":"sha256:built","Size":42,"RepoDigests":[]}]`, nil)
	run.on("docker inspect pulled:1", `[{"Id":"sha256:id","Size":7,"RepoDigests":["pulled@sha256:digest"]}]`, nil)
	run.on("docker inspect broken:1", `not json`, nil)

	built, err := im.GetImageInfo(ctx, "built:dev")
	if err != nil || !built.InLocalDaemon || !built.IsLocal || built.SHA256 != "sha256:built" || built.Size != 42 {
		test.Errorf("built image = %+v, %v", built, err)
	}
	pulled, err := im.GetImageInfo(ctx, "pulled:1")
	// The image ID, not the registry digest (see TestLocalAndClusterImageHashesMatch)
	if err != nil || !pulled.InLocalDaemon || pulled.IsLocal || pulled.SHA256 != "sha256:id" {
		test.Errorf("pulled image = %+v, %v", pulled, err)
	}
	missing, err := im.GetImageInfo(ctx, "missing:1")
	if err != nil || missing.InLocalDaemon || missing.SHA256 != "" {
		test.Errorf("missing image = %+v, %v", missing, err)
	}
	if _, err := im.GetImageInfo(ctx, "broken:1"); err == nil {
		test.Error("want an error for output that doesn't parse")
	}
}

// What `docker inspect` and `crictl inspecti` report for tonistiigi/binfmt
// loaded into a kind node: the registry digest hashes the manifest, and only
// the image ID is the same on both sides
const (
	binfmtImageID  = "sha256:15935d0512bf0a8e5afa1df990d971cf97a619ede08fa60f2d5966a62de6d6b7"
	binfmtRegistry = "sha256:400a4873b838d1b89194d982c45e5fb3cda4593fbfd7e08a02e76b03b21166f0"
)

// up loads an image when its local hash differs from the node's. For a
// pulled image the local hash was the registry digest, which never matched,
// so every up untagged and reloaded it
func TestLocalAndClusterImageHashesMatch(test *testing.T) {
	tests := []struct {
		name      string
		image     string
		inspect   string // docker inspect
		inspecti  string // crictl inspecti in the node
		wantLocal bool   // built locally rather than pulled
	}{
		{
			name:  "pulled from a registry",
			image: "tonistiigi/binfmt:latest",
			inspect: `[{"Id":"` + binfmtImageID + `","RepoTags":["tonistiigi/binfmt:latest"],` +
				`"RepoDigests":["tonistiigi/binfmt@` + binfmtRegistry + `"],"Size":71000000}]`,
			inspecti: `{"status":{"id":"` + binfmtImageID + `","repoTags":["docker.io/tonistiigi/binfmt:latest"],` +
				`"repoDigests":["docker.io/library/import-2026-10-19@sha256:82cd50c9173f31bc923d4e6cb30441e017dd28ea907e23f84bb5cd8aad2cc0ea",` +
				`"docker.io/tonistiigi/binfmt@` + binfmtRegistry + `"]}}`,
		},
		{
			name:      "built locally",
			image:     "marlin-local/webster:local",
			inspect:   `[{"Id":"sha256:aaaa1111","RepoTags":["marlin-local/webster:local"],"RepoDigests":[],"Size":1}]`,
			inspecti:  `{"status":{"id":"sha256:aaaa1111","repoTags":["docker.io/marlin-local/webster:local"],"repoDigests":[]}}`,
			wantLocal: true,
		},
	}
	for _, tt := range tests {
		test.Run(tt.name, func(test *testing.T) {
			ctx := context.Background()
			im, run := newTestImages()
			run.on("docker inspect "+tt.image, tt.inspect, nil)
			run.on("docker exec c-control-plane crictl inspecti ", tt.inspecti, nil)

			info, err := im.GetImageInfo(ctx, tt.image)
			if err != nil {
				test.Fatal(err)
			}
			clusterHash, err := im.GetClusterImageHash(ctx, "c", tt.image)
			if err != nil {
				test.Fatal(err)
			}
			if info.SHA256 != clusterHash {
				test.Errorf("local hash %s != cluster hash %s: up would reload an image the node already has", info.SHA256, clusterHash)
			}
			if info.IsLocal != tt.wantLocal || !info.InLocalDaemon {
				test.Errorf("IsLocal = %v, InLocalDaemon = %v; want %v, true", info.IsLocal, info.InLocalDaemon, tt.wantLocal)
			}
		})
	}
}

func TestGetClusterImageHash(test *testing.T) {
	ctx := context.Background()
	im, run := newTestImages()
	run.on("docker exec c-control-plane crictl inspecti docker.io/library/nginx:1", `{"status":{"id":"sha256:abc"}}`, nil)
	run.on("docker exec c-control-plane crictl inspecti ghcr.io/me/bad:1", `{`, nil)

	if got, err := im.GetClusterImageHash(ctx, "c", "nginx:1"); err != nil || got != "sha256:abc" {
		test.Errorf("got %q, %v", got, err)
	}
	if got, err := im.GetClusterImageHash(ctx, "c", "ghcr.io/me/app:1"); err != nil || got != "" {
		test.Errorf("an image not in the cluster: %q, %v", got, err)
	}
	if _, err := im.GetClusterImageHash(ctx, "c", "ghcr.io/me/bad:1"); err == nil {
		test.Error("want an error for output that doesn't parse")
	}
}

func TestListClusterImages(test *testing.T) {
	im, run := newTestImages()
	run.on("docker exec c-control-plane crictl images -o json", `{"images":[{"id":"sha256:1","repoTags":["app:1"],"size":"100"}]}`, nil)
	got, err := im.ListClusterImages(context.Background(), "c")
	if err != nil || len(got) != 1 || !slices.Equal(got[0].RepoTags, []string{"app:1"}) {
		test.Errorf("got %+v, %v", got, err)
	}
	im, _ = newTestImages()
	if _, err := im.ListClusterImages(context.Background(), "c"); err == nil {
		test.Error("want an error when crictl fails")
	}
}

// A pulled chart is rendered: the images it really runs, not guesses from values
func TestGetImagesForServicePulledChart(test *testing.T) {
	chartDir := filepath.Join(test.TempDir(), "app")
	if err := os.MkdirAll(filepath.Join(chartDir, "templates"), 0o755); err != nil {
		test.Fatal(err)
	}
	files := map[string]string{
		"Chart.yaml":                "apiVersion: v2\nname: app\nversion: 1.0.0\nappVersion: \"2.1\"\n",
		"values.yaml":               "image:\n  repository: registry.example/app\n",
		"templates/deployment.yaml": "apiVersion: apps/v1\nkind: Deployment\nmetadata: {name: app}\nspec:\n  template:\n    spec:\n      containers:\n      - name: app\n        image: \"{{ .Values.image.repository }}:{{ .Chart.AppVersion }}\"\n",
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(chartDir, name), []byte(content), 0o644); err != nil {
			test.Fatal(err)
		}
	}

	im, _ := newTestImages()
	var pulledInto string
	im.pullChart = func(svc *config.ServiceConfig, dir string) (string, error) {
		pulledInto = dir
		return chartDir, nil
	}
	svc := &config.ServiceConfig{Name: "app", Type: "helm", Repo: "https://charts.example.com", Chart: "app"}
	got, err := im.GetImagesForService(context.Background(), svc, "")
	if err != nil {
		test.Fatal(err)
	}
	if !slices.Contains(got, "registry.example/app:2.1") {
		test.Errorf("images = %v, want the appVersion tag the chart renders", got)
	}
	if pulledInto == "" || !strings.Contains(filepath.Base(pulledInto), "kraze-chart-") {
		test.Errorf("pulled into %q", pulledInto)
	}
	if _, err := os.Stat(pulledInto); !os.IsNotExist(err) {
		test.Error("the pull's temp dir should be removed")
	}
}

// execRunner runs real commands; sh is enough to check how it wires them
func TestExecRunner(test *testing.T) {
	ctx := context.Background()
	out, err := execRunner{}.Output(ctx, "sh", "-c", "echo out; echo err >&2")
	if err != nil || string(out) != "out\n" {
		test.Errorf("Output = %q, %v: want stdout only", out, err)
	}
	out, err = execRunner{}.CombinedOutput(ctx, "piped", "sh", "-c", "cat; echo err >&2")
	if err != nil || string(out) != "pipederr\n" {
		test.Errorf("CombinedOutput = %q, %v: want stdin echoed and stderr included", out, err)
	}
	if _, err := (execRunner{}).Output(ctx, "sh", "-c", "exit 3"); err == nil {
		test.Error("want the exit status as an error")
	}
}
