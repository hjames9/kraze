package providers

import (
	"bytes"
	"context"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/hjames9/kraze/internal/config"
	yaml "go.yaml.in/yaml/v3"
	"helm.sh/helm/v4/pkg/action"
	"helm.sh/helm/v4/pkg/chart/common"
	v2chart "helm.sh/helm/v4/pkg/chart/v2"
	kubefake "helm.sh/helm/v4/pkg/kube/fake"
	ri "helm.sh/helm/v4/pkg/release"
	"helm.sh/helm/v4/pkg/storage"
	"helm.sh/helm/v4/pkg/storage/driver"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// A config checksum must change when, and only when, a ConfigMap's or
// Secret's content changes: a missed change leaves pods on stale config, a
// spurious one restarts them for nothing.

const checksumBase = `apiVersion: v1
kind: ConfigMap
metadata: {name: cfg}
data: {mode: dev, level: info}
---
apiVersion: v1
kind: Secret
metadata: {name: creds}
stringData: {password: hunter2}
---
apiVersion: apps/v1
kind: Deployment
metadata: {name: web}
spec: {replicas: 1}
`

// checksums returns the Helm-side checksum of manifest and the
// manifests-side checksum of the same objects; they must agree
func checksums(t *testing.T, manifest string) (string, string) {
	t.Helper()
	fromManifest, err := calculateConfigChecksum(manifest)
	if err != nil {
		t.Fatal(err)
	}
	var objects []*unstructured.Unstructured
	for _, doc := range strings.Split(manifest, "\n---\n") {
		obj := &unstructured.Unstructured{}
		if err := yaml.Unmarshal([]byte(doc), &obj.Object); err != nil {
			t.Fatal(err)
		}
		objects = append(objects, obj)
	}
	fromObjects, err := calculateConfigChecksumFromObjects(objects)
	if err != nil {
		t.Fatal(err)
	}
	return fromManifest, fromObjects
}

func TestConfigChecksum(t *testing.T) {
	base, baseObjects := checksums(t, checksumBase)
	if base == "" || base != baseObjects {
		t.Fatalf("Helm and manifests checksums must agree and be set: %q vs %q", base, baseObjects)
	}

	tests := []struct {
		name     string
		manifest string
		changes  bool
	}{
		{name: "ConfigMap value", manifest: strings.Replace(checksumBase, "mode: dev", "mode: prod", 1), changes: true},
		{name: "ConfigMap key added", manifest: strings.Replace(checksumBase, "level: info", "level: info, extra: x", 1), changes: true},
		{name: "Secret stringData", manifest: strings.Replace(checksumBase, "hunter2", "hunter3", 1), changes: true},
		{name: "ConfigMap binaryData",
			manifest: strings.Replace(checksumBase, "data: {mode: dev, level: info}", "data: {mode: dev, level: info}\nbinaryData: {blob: AAEC}", 1),
			changes:  true},
		{name: "key order", manifest: strings.Replace(checksumBase, "{mode: dev, level: info}", "{level: info, mode: dev}", 1)},
		{name: "only the Deployment", manifest: strings.Replace(checksumBase, "replicas: 1", "replicas: 3", 1)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fromManifest, fromObjects := checksums(t, tt.manifest)
			if fromManifest != fromObjects {
				t.Errorf("Helm and manifests checksums disagree: %q vs %q", fromManifest, fromObjects)
			}
			if changed := fromManifest != base; changed != tt.changes {
				t.Errorf("checksum changed = %v, want %v", changed, tt.changes)
			}
		})
	}
}

// After a syntax error the YAML decoder returns the same error forever; the
// checksum used to loop on it
func TestConfigChecksumStopsOnInvalidYAML(t *testing.T) {
	for name, manifest := range map[string]string{
		"syntax error": "kind: ConfigMap\ndata: {a: 1\n---\nkind: ConfigMap\ndata: {b: 2}\n",
		"tab indent":   "kind: ConfigMap\ndata:\n\ta: 1\n",
	} {
		done := make(chan error, 1)
		go func() {
			_, err := calculateConfigChecksum(manifest)
			done <- err
		}()
		select {
		case err := <-done:
			if err == nil {
				t.Errorf("%s: want a parse error", name)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%s: calculateConfigChecksum hung", name)
		}
	}

	// A document that isn't a mapping is skipped, not an error
	sum, err := calculateConfigChecksum("- a\n---\nkind: ConfigMap\ndata: {b: \"2\"}\n")
	if err != nil || sum == "" {
		t.Errorf("non-mapping document: sum=%q err=%v", sum, err)
	}
}

func parseObjects(t *testing.T, manifest string) []*unstructured.Unstructured {
	t.Helper()
	var objects []*unstructured.Unstructured
	for _, doc := range strings.Split(manifest, "\n---\n") {
		obj := &unstructured.Unstructured{}
		if err := yaml.Unmarshal([]byte(doc), &obj.Object); err != nil {
			t.Fatal(err)
		}
		objects = append(objects, obj)
	}
	return objects
}

func templateAnnotations(obj *unstructured.Unstructured) map[string]string {
	annotations, _, _ := unstructured.NestedStringMap(obj.Object, "spec", "template", "metadata", "annotations")
	return annotations
}

func TestAnnotateConfigChecksum(t *testing.T) {
	manifest := checksumBase + `---
apiVersion: apps/v1
kind: StatefulSet
metadata: {name: db}
spec:
  template:
    metadata:
      annotations: {team: data}
---
apiVersion: v1
kind: Service
metadata: {name: web}
`
	objects := parseObjects(t, manifest)
	if err := annotateConfigChecksum(objects); err != nil {
		t.Fatal(err)
	}
	want, _ := calculateConfigChecksumFromObjects(objects)
	for _, obj := range objects {
		got := templateAnnotations(obj)[configChecksumAnnotation]
		switch obj.GetKind() {
		case "Deployment", "StatefulSet":
			if got != want {
				t.Errorf("%s: annotation = %q, want %q", obj.GetKind(), got, want)
			}
		default:
			if _, found, _ := unstructured.NestedFieldNoCopy(obj.Object, "spec", "template"); found {
				t.Errorf("%s should not get a pod template annotation", obj.GetKind())
			}
		}
	}
	if templateAnnotations(objects[3])["team"] != "data" {
		t.Error("existing pod template annotations must be kept")
	}

	// The same input yields the same pod template, so re-applying it doesn't
	// start a rollout
	again := parseObjects(t, manifest)
	if err := annotateConfigChecksum(again); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(again, objects) {
		t.Error("annotating the same objects twice gave different results")
	}

	// No ConfigMaps or Secrets: nothing to annotate
	workloadOnly := parseObjects(t, "apiVersion: apps/v1\nkind: Deployment\nmetadata: {name: web}\nspec: {}")
	if err := annotateConfigChecksum(workloadOnly); err != nil {
		t.Fatal(err)
	}
	if _, ok := templateAnnotations(workloadOnly[0])[configChecksumAnnotation]; ok {
		t.Error("a service without config should not be annotated")
	}
}

// postRender runs configChecksumPostRenderer over manifest
func postRender(t *testing.T, manifest string) string {
	t.Helper()
	out, err := configChecksumPostRenderer{}.Run(bytes.NewBufferString(manifest))
	if err != nil {
		t.Fatal(err)
	}
	return out.String()
}

func TestConfigChecksumPostRenderer(t *testing.T) {
	manifest := checksumBase + `---
apiVersion: apps/v1
kind: StatefulSet
metadata:
  name: db
  annotations: {helm.sh/source-file: templates/db.yaml}
spec:
  template:
    metadata:
      annotations: {team: data}
---
apiVersion: apps/v1
kind: DaemonSet
metadata: {name: agent}
spec:
  template:
    metadata:
---
apiVersion: v1
kind: Service
metadata: {name: web}
`
	objects := parseObjects(t, postRender(t, manifest))

	// Helm stores Secrets ahead of ConfigMaps, and the checksum patched on
	// after an install covered them in that order
	want, err := calculateConfigChecksum(strings.Join([]string{
		"kind: Secret\nstringData: {password: hunter2}",
		"kind: ConfigMap\ndata: {mode: dev, level: info}",
	}, "\n---\n"))
	if err != nil {
		t.Fatal(err)
	}
	for _, obj := range objects {
		got := templateAnnotations(obj)[configChecksumAnnotation]
		switch obj.GetKind() {
		case "Deployment", "StatefulSet", "DaemonSet":
			if got != want {
				t.Errorf("%s: annotation = %q, want %q", obj.GetKind(), got, want)
			}
		default:
			if _, found, _ := unstructured.NestedFieldNoCopy(obj.Object, "spec", "template"); found {
				t.Errorf("%s should not get a pod template annotation", obj.GetKind())
			}
		}
	}
	if templateAnnotations(objects[3])["team"] != "data" {
		t.Error("existing pod template annotations must be kept")
	}
	// Helm splits the post-rendered stream back into files by this annotation
	if objects[3].GetAnnotations()["helm.sh/source-file"] != "templates/db.yaml" {
		t.Error("object annotations must be kept")
	}

	// Rendering the same chart again leaves the pod templates alone
	if again := postRender(t, manifest); !reflect.DeepEqual(parseObjects(t, again), objects) {
		t.Error("post-rendering the same manifest twice gave different results")
	}

	// No ConfigMaps or Secrets: the manifest goes through untouched
	workloadOnly := "apiVersion: apps/v1\nkind: Deployment\nmetadata: {name: web}\nspec: {}\n"
	if got := postRender(t, workloadOnly); got != workloadOnly {
		t.Errorf("a service without config should not be changed, got:\n%s", got)
	}

	if _, err := (configChecksumPostRenderer{}).Run(bytes.NewBufferString("kind: ConfigMap\ndata: {a: 1\n")); err == nil {
		t.Error("want a parse error for invalid YAML")
	}
}

// checksumChart is a chart with config (a ConfigMap ahead of a Secret by
// file name), a Deployment, and a hook that must be left alone
func checksumChart() *v2chart.Chart {
	template := func(name, data string) *common.File {
		return &common.File{Name: "templates/" + name, ModTime: time.Now(), Data: []byte(data)}
	}
	return &v2chart.Chart{
		Metadata: &v2chart.Metadata{APIVersion: "v2", Name: "app", Version: "1.0.0"},
		Values:   map[string]interface{}{"mode": "dev"},
		Templates: []*common.File{
			template("a-config.yaml", "apiVersion: v1\nkind: ConfigMap\nmetadata: {name: cfg}\ndata: {mode: {{ .Values.mode }}}\n"),
			template("b-secret.yaml", "apiVersion: v1\nkind: Secret\nmetadata: {name: creds}\nstringData: {password: hunter2}\n"),
			template("deployment.yaml", "apiVersion: apps/v1\nkind: Deployment\nmetadata: {name: web}\nspec:\n  template:\n    spec:\n      hostNetwork: true\n"),
			template("hook.yaml", "apiVersion: batch/v1\nkind: Job\nmetadata:\n  name: migrate\n  annotations: {helm.sh/hook: pre-install}\nspec: {}\n---\n"+
				"apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: hook-cfg\n  annotations: {helm.sh/hook: pre-install}\ndata: {x: y}\n"),
		},
	}
}

// deploymentChecksum returns the config checksum on the web Deployment in
// rel's manifest, failing unless it matches the checksum of the manifest's
// config as it used to be patched on
func deploymentChecksum(t *testing.T, rel ri.Releaser) string {
	t.Helper()
	acc, err := ri.NewAccessor(rel)
	if err != nil {
		t.Fatal(err)
	}
	want, err := calculateConfigChecksum(acc.Manifest())
	if err != nil {
		t.Fatal(err)
	}
	var got string
	for _, obj := range parseObjects(t, strings.TrimPrefix(acc.Manifest(), "---\n")) {
		if obj.GetKind() == "Deployment" {
			got = templateAnnotations(obj)[configChecksumAnnotation]
		}
	}
	if got == "" || got != want {
		t.Errorf("Deployment config checksum = %q, want %q", got, want)
	}
	if len(acc.Hooks()) == 0 {
		t.Error("the chart's hooks are missing from the release")
	}
	for _, h := range acc.Hooks() {
		hook, err := ri.NewHookAccessor(h)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(hook.Manifest(), configChecksumAnnotation) {
			t.Errorf("hook %s must not be post-rendered", hook.Path())
		}
	}
	return got
}

// Helm installs and upgrades carry the config checksum in what they apply,
// so nothing patches it on afterwards and starts a second rollout
func TestHelmClientsRenderConfigChecksum(t *testing.T) {
	helm := &HelmProvider{opts: &ProviderOptions{}}
	service := &config.ServiceConfig{Name: "app", Namespace: "apps"}
	actionConfig := &action.Configuration{
		Releases:     storage.Init(driver.NewMemory()),
		KubeClient:   &kubefake.PrintingKubeClient{Out: io.Discard},
		Capabilities: common.DefaultCapabilities,
	}

	rel, err := helm.newInstallClient(actionConfig, service).RunWithContext(context.Background(), checksumChart(), nil)
	if err != nil {
		t.Fatal(err)
	}
	installed := deploymentChecksum(t, rel)

	upgrade := func(values map[string]interface{}) string {
		rel, err := helm.newUpgradeClient(actionConfig, service).RunWithContext(context.Background(), "app", checksumChart(), values)
		if err != nil {
			t.Fatal(err)
		}
		return deploymentChecksum(t, rel)
	}
	if same := upgrade(nil); same != installed {
		t.Errorf("an upgrade without config changes changed the checksum: %q -> %q", installed, same)
	}
	if changed := upgrade(map[string]interface{}{"mode": "prod"}); changed == installed {
		t.Error("a config change must change the checksum")
	}
}
