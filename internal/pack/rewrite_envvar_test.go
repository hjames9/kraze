package pack

import (
	"path/filepath"
	"strings"
	"testing"
)

// The packed config must point at the bundled copies. rewriteConfig reads
// the config's text, where a path may still be an environment variable
// reference, while the downloaded URLs and external paths come from the
// parsed config, where it is expanded.
func TestRewriteConfigMatchesExpandedPaths(t *testing.T) {
	t.Setenv("MANIFEST_BASE", "https://example.com/deploy")
	dir := t.TempDir()
	outside := t.TempDir()
	t.Setenv("SHARED_DIR", outside)
	cfgPath := filepath.Join(dir, "kraze.yml")
	writeFile(t, cfgPath, `project: p
cluster:
  name: test
services:
  metrics:
    type: manifests
    path: ${MANIFEST_BASE}/metrics.yaml
  extra:
    type: manifests
    paths:
      - ${MANIFEST_BASE}/a.yaml
      - ${SHARED_DIR}/b.yaml
`)
	remoteManifests := map[string]remoteManifestInfo{
		"metrics": {files: []remoteManifestFile{{origURL: "https://example.com/deploy/metrics.yaml", archPath: ".krazepack/manifests/metrics.yaml"}}},
		"extra":   {files: []remoteManifestFile{{origURL: "https://example.com/deploy/a.yaml", archPath: ".krazepack/manifests/extra-0.yaml"}}},
	}
	external := map[string]string{filepath.Join(outside, "b.yaml"): ".krazepack/external/b.yaml"}

	result, err := rewriteConfig(cfgPath, "kraze.yml", nil, remoteManifests, external)
	if err != nil {
		t.Fatal(err)
	}
	s := string(result)
	for _, want := range []string{".krazepack/manifests/metrics.yaml", ".krazepack/manifests/extra-0.yaml", ".krazepack/external/b.yaml"} {
		if !strings.Contains(s, want) {
			t.Errorf("packed config should reference %s:\n%s", want, s)
		}
	}
	if strings.Contains(s, "${") {
		t.Errorf("a bundled path still references the environment:\n%s", s)
	}
}
