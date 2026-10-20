package cluster

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
	chartcommon "helm.sh/helm/v4/pkg/chart/common"
	chartcommonutil "helm.sh/helm/v4/pkg/chart/common/util"
	v2loader "helm.sh/helm/v4/pkg/chart/v2/loader"
	chartutil "helm.sh/helm/v4/pkg/chart/v2/util"
	"helm.sh/helm/v4/pkg/engine"
	repo "helm.sh/helm/v4/pkg/repo/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	k8syaml "k8s.io/apimachinery/pkg/util/yaml"

	"github.com/hjames9/kraze/internal/config"
)

// The images kraze preloads for a Helm service must include every image the
// chart actually runs with the service's values. Helm is the oracle: the
// chart is rendered with Helm's own values handling, and the images are read
// from the rendered pod specs.

const oracleChartYAML = `apiVersion: v2
name: app
version: 0.1.0
appVersion: "1.2.3"
`

const oracleValuesYAML = `global:
  imageRegistry: ""
image:
  repository: myapp
  tag: ""
worker:
  image:
    registry: localhost:5000
    repository: team/worker
    tag: "2.0"
sidecar:
  enabled: false
  image: sidecar:1.0
init:
  repository: init-tool
  tag: "0.9"
  digest: ""
db:
  registry: docker.io
  repository: bitnami/postgresql
  tag: "16"
`

// Common image patterns: tag defaulting to appVersion, registry/repository/tag,
// a conditional sidecar, digests, global.imageRegistry (Bitnami), a CronJob
const oracleTemplate = `apiVersion: apps/v1
kind: Deployment
metadata:
  name: {{ .Release.Name }}
spec:
  template:
    spec:
      initContainers:
        - name: init
          image: {{ if .Values.init.digest }}{{ .Values.init.repository }}@{{ .Values.init.digest }}{{ else }}{{ .Values.init.repository }}:{{ .Values.init.tag }}{{ end }}
      containers:
        - name: app
          image: "{{ .Values.image.repository }}:{{ .Values.image.tag | default .Chart.AppVersion }}"
        - name: worker
          image: {{ printf "%s/%s:%s" .Values.worker.image.registry .Values.worker.image.repository .Values.worker.image.tag }}
        {{- if .Values.sidecar.enabled }}
        - name: sidecar
          image: {{ .Values.sidecar.image }}
        {{- end }}
        - name: db
          image: {{ .Values.global.imageRegistry | default .Values.db.registry }}/{{ .Values.db.repository }}:{{ .Values.db.tag }}
---
apiVersion: batch/v1
kind: CronJob
metadata:
  name: {{ .Release.Name }}-backup
spec:
  schedule: "@daily"
  jobTemplate:
    spec:
      template:
        spec:
          containers:
            - name: backup
              image: "{{ .Values.image.repository }}:{{ .Values.image.tag | default .Chart.AppVersion }}"
`

// writeOracleChart writes the chart directory and returns its path
func writeOracleChart(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "app")
	for name, content := range map[string]string{
		"Chart.yaml": oracleChartYAML, "values.yaml": oracleValuesYAML, "templates/deployment.yaml": oracleTemplate,
	} {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// renderedImages renders the chart as `helm install -f <files>` would and
// returns the images of every container in the result
func renderedImages(t *testing.T, chartDir string, valueFiles []string) []string {
	t.Helper()
	chrt, err := v2loader.Load(chartDir)
	if err != nil {
		t.Fatal(err)
	}
	user := map[string]any{}
	for _, file := range valueFiles {
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		vals, err := v2loader.LoadValues(bytes.NewReader(data))
		if err != nil {
			t.Fatal(err)
		}
		user = v2loader.MergeMaps(user, vals)
	}
	renderVals, err := chartcommonutil.ToRenderValues(chrt, user, chartcommon.ReleaseOptions{Name: "rel", Namespace: "ns", IsInstall: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	rendered, err := engine.Render(chrt, renderVals)
	if err != nil {
		t.Fatal(err)
	}
	var images []string
	for _, manifest := range rendered {
		images = append(images, renderedManifestImages(t, manifest)...)
	}
	return images
}

// renderedManifestImages parses a multi-document manifest and returns the
// images of its containers
func renderedManifestImages(t *testing.T, manifest string) []string {
	t.Helper()
	var images []string
	decoder := k8syaml.NewYAMLOrJSONDecoder(strings.NewReader(manifest), 4096)
	for {
		obj := &unstructured.Unstructured{}
		if err := decoder.Decode(&obj.Object); err != nil {
			break
		}
		images = append(images, containerImages(obj.Object)...)
	}
	return images
}

// containerImages returns the image of every container, init container and
// ephemeral container found anywhere in obj
func containerImages(node any) []string {
	var images []string
	switch v := node.(type) {
	case map[string]any:
		for key, val := range v {
			if key == "containers" || key == "initContainers" || key == "ephemeralContainers" {
				if list, ok := val.([]any); ok {
					for _, c := range list {
						if container, ok := c.(map[string]any); ok {
							if image, ok := container["image"].(string); ok {
								images = append(images, image)
							}
						}
					}
				}
			}
			images = append(images, containerImages(val)...)
		}
	case []any:
		for _, item := range v {
			images = append(images, containerImages(item)...)
		}
	}
	return images
}

// serveChartRepo packages the chart into an HTTP chart repository and
// returns its URL
func serveChartRepo(t *testing.T, chartDir string) string {
	t.Helper()
	chrt, err := v2loader.Load(chartDir)
	if err != nil {
		t.Fatal(err)
	}
	repoDir := t.TempDir()
	if _, err := chartutil.Save(chrt, repoDir); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.FileServer(http.Dir(repoDir)))
	t.Cleanup(server.Close)
	index, err := repo.IndexDirectory(repoDir, server.URL)
	if err != nil {
		t.Fatal(err)
	}
	if err := index.WriteFile(filepath.Join(repoDir, "index.yaml"), 0o644); err != nil {
		t.Fatal(err)
	}
	return server.URL
}

// isolateHelm points Helm's config and caches at a temporary directory
func isolateHelm(t *testing.T) {
	home := t.TempDir()
	for _, env := range []string{"HELM_CONFIG_HOME", "HELM_CACHE_HOME", "HELM_DATA_HOME"} {
		t.Setenv(env, filepath.Join(home, env))
	}
	t.Setenv("HELM_REPOSITORY_CONFIG", filepath.Join(home, "repositories.yaml"))
	t.Setenv("HELM_REPOSITORY_CACHE", filepath.Join(home, "repository"))
	t.Setenv("HELM_REGISTRY_CONFIG", filepath.Join(home, "registry.json"))
}

func TestHelmImagesMatchRenderedChart(t *testing.T) {
	isolateHelm(t)
	chartDir := writeOracleChart(t)
	archiveDir := t.TempDir()
	chrt, err := v2loader.Load(chartDir)
	if err != nil {
		t.Fatal(err)
	}
	archive, err := chartutil.Save(chrt, archiveDir)
	if err != nil {
		t.Fatal(err)
	}
	repoURL := serveChartRepo(t, chartDir)

	scenarios := []struct {
		name   string
		files  []string // values files, in order
		inline string
	}{
		{name: "chart defaults"},
		{name: "values file overrides only the tag", files: []string{"image:\n  tag: dev\n"}},
		{name: "values_inline overrides only the tag", inline: "image:\n  tag: dev\n"},
		{name: "values file overrides only the repository", files: []string{"image:\n  repository: myapp-local\n"}},
		{name: "two values files merge", files: []string{"worker:\n  image:\n    tag: \"3.0\"\n", "image:\n  tag: b\n"}},
		{name: "later file wins", files: []string{"image:\n  tag: one\n", "image:\n  tag: two\n"}},
		{name: "values enable a sidecar", files: []string{"sidecar:\n  enabled: true\n"}},
		{name: "global.imageRegistry", files: []string{"global:\n  imageRegistry: mirror.local\n"}},
		{name: "digest", files: []string{"init:\n  digest: sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef\n"}},
	}
	sources := map[string]func(*config.ServiceConfig){
		"chart directory": func(svc *config.ServiceConfig) { svc.Path = chartDir },
		"chart archive":   func(svc *config.ServiceConfig) { svc.Path = archive },
		"remote chart":    func(svc *config.ServiceConfig) { svc.Repo, svc.Chart, svc.Version = repoURL, "app", "0.1.0" },
	}
	sourceNames := []string{"chart directory", "chart archive", "remote chart"}

	for _, sc := range scenarios {
		var files []string
		for i, content := range sc.files {
			path := filepath.Join(t.TempDir(), "values"+string(rune('a'+i))+".yaml")
			if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
			files = append(files, path)
		}
		oracleFiles := files
		if sc.inline != "" {
			path := filepath.Join(t.TempDir(), "inline.yaml")
			if err := os.WriteFile(path, []byte(sc.inline), 0o644); err != nil {
				t.Fatal(err)
			}
			oracleFiles = []string{path}
		}
		want := renderedImages(t, chartDir, oracleFiles)

		for _, source := range sourceNames {
			t.Run(sc.name+"/"+source, func(t *testing.T) {
				svc := &config.ServiceConfig{Name: "rel", Type: "helm", Namespace: "ns", ValuesInline: sc.inline}
				if len(files) > 0 {
					svc.Values = valuesFieldOf(t, files)
				}
				sources[source](svc)
				got, err := NewImageManager(os.Getenv("ORACLE_VERBOSE") != "").GetImagesForService(context.Background(), svc, "")
				if err != nil {
					t.Fatal(err)
				}
				var missing []string
				for _, image := range want {
					if !slices.Contains(got, image) && !slices.Contains(missing, image) {
						missing = append(missing, image)
					}
				}
				sort.Strings(missing)
				if len(missing) > 0 {
					t.Errorf("Helm runs %v, kraze doesn't preload it (kraze found %v)", missing, got)
				}
			})
		}
	}
}

func valuesFieldOf(t *testing.T, files []string) config.ValuesField {
	t.Helper()
	data, err := yaml.Marshal(files)
	if err != nil {
		t.Fatal(err)
	}
	var field config.ValuesField
	if err := yaml.Unmarshal(data, &field); err != nil {
		t.Fatal(err)
	}
	return field
}
