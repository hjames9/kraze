package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hjames9/kraze/internal/pack"
)

func TestPackCommand(t *testing.T) {
	withFakeDeps(t)
	project := t.TempDir()
	writeConfig(t, filepath.Join(project, "app.yaml"), "apiVersion: v1\nkind: ConfigMap\nmetadata: {name: app}\n")
	configFiles = []string{writeConfig(t, filepath.Join(project, "kraze.yml"), `project: web
cluster:
  name: packed
  config:
    - role: control-plane
      extraMounts:
        - hostPath: /data
          containerPath: /data
services:
  app:
    type: manifests
    path: ./app.yaml
`)}
	packOutput = filepath.Join(t.TempDir(), "out.tar.gz")

	out, err := runCmd(t, runPack)
	if err != nil {
		t.Fatalf("pack: %v\n%s", err, out)
	}
	assertContains(t, out, "extraMounts are not bundled", "Packaging to "+packOutput, "Created "+packOutput+" (")

	// The archive is a working config
	paths, cleanup, err := pack.MaybeExtract([]string{packOutput})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if _, err := os.Stat(filepath.Join(filepath.Dir(paths[0]), "app.yaml")); err != nil {
		t.Errorf("the manifest should be bundled: %v", err)
	}
}

func TestPackCommandDefaultOutputAndErrors(t *testing.T) {
	withFakeDeps(t)
	project := t.TempDir()
	configFiles = []string{writeConfig(t, filepath.Join(project, "kraze.yml"), "project: web\ncluster:\n  name: packed\nservices:\n  app:\n    type: manifests\n    path: ./missing.yaml\n")}
	t.Chdir(t.TempDir())

	_, err := runCmd(t, runPack)
	if err == nil || !strings.Contains(err.Error(), "packaging failed") {
		t.Errorf("err = %v", err)
	}
	if _, statErr := os.Stat("packed.tar.gz"); !os.IsNotExist(statErr) {
		t.Error("the default output is <cluster>.tar.gz, and a failed pack leaves none")
	}

	configFiles = []string{writeConfig(t, filepath.Join(project, "bad.yml"), "not: [valid")}
	if _, err := runCmd(t, runPack); err == nil || !strings.Contains(err.Error(), "failed to parse config") {
		t.Errorf("err = %v", err)
	}
}

func TestSummariseRemoteAssets(t *testing.T) {
	cfg := parseConfig(t, `project: web
cluster:
  name: c
  external:
    enabled: true
    kubeconfig: ~/.kube/prod
services:
  chart:
    type: helm
    repo: oci://registry.example/charts
    chart: app
  remote:
    type: manifests
    path: https://example.com/app.yaml
  off:
    type: helm
    enabled: false
    repo: oci://registry.example/charts
    chart: other
`)
	out := captureStdout(t, func() { summariseRemoteAssets(cfg) })
	assertContains(t, out, "Pulling remote Helm chart(s): chart\n", "Downloading HTTP manifest(s): remote", "external.kubeconfig is not bundled")
	if strings.Contains(out, "other") || strings.Contains(out, "off") {
		t.Errorf("disabled services aren't packed:\n%s", out)
	}
}

func TestHumanBytes(t *testing.T) {
	for n, want := range map[int64]string{0: "0 B", 1023: "1023 B", 1024: "1.0 KB", 1536: "1.5 KB", 1024 * 1024: "1.0 MB", 5 * 1024 * 1024: "5.0 MB"} {
		if got := humanBytes(n); got != want {
			t.Errorf("humanBytes(%d) = %q, want %q", n, got, want)
		}
	}
}
