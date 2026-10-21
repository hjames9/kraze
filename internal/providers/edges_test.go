package providers

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hjames9/kraze/internal/config"
	"github.com/hjames9/kraze/internal/state"
	"go.yaml.in/yaml/v3"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func TestManifestsStatusEdges(t *testing.T) {
	ctx := context.Background()
	mp, _ := applyProvider()

	status, err := mp.Status(ctx, &config.ServiceConfig{Name: "x", Type: "manifests", Path: filepath.Join(t.TempDir(), "missing.yaml")})
	if err != nil || status.Installed || !strings.HasPrefix(status.Message, "Failed to load manifests") {
		t.Errorf("missing manifests: %+v, %v", status, err)
	}
	empty := filepath.Join(t.TempDir(), "empty.yaml")
	if err := os.WriteFile(empty, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	status, err = mp.Status(ctx, &config.ServiceConfig{Name: "x", Type: "manifests", Path: empty})
	if err != nil || status.Message != "No manifests found" {
		t.Errorf("empty manifests: %+v, %v", status, err)
	}

	// Two TTL Jobs that finished and were deleted
	jobs := filepath.Join(t.TempDir(), "jobs.yaml")
	job := func(name string) string {
		return "apiVersion: batch/v1\nkind: Job\nmetadata:\n  name: " + name + "\nspec:\n  ttlSecondsAfterFinished: 60\n"
	}
	if err := os.WriteFile(jobs, []byte(job("one")+"---\n"+job("two")), 0o644); err != nil {
		t.Fatal(err)
	}
	mp.opts.Recorded = []state.ResourceRef{
		{APIVersion: "batch/v1", Kind: "Job", Namespace: "apps", Name: "one", Completed: true},
		{APIVersion: "batch/v1", Kind: "Job", Namespace: "apps", Name: "two", Completed: true},
	}
	status, err = mp.Status(ctx, &config.ServiceConfig{Name: "x", Type: "manifests", Namespace: "apps", Path: jobs})
	if err != nil || !status.Ready || status.Message != "2/2 resources exist (2 jobs finished)" {
		t.Errorf("finished jobs: %+v, %v", status, err)
	}
}

func TestManifestsVerboseOutput(t *testing.T) {
	ctx := context.Background()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("apiVersion: v1\nkind: ConfigMap\nmetadata: {name: a}\n"))
	}))
	defer server.Close()
	mp, _ := applyProvider()
	out := captureOutput(t, func() {
		_, _ = mp.loadManifests(&config.ServiceConfig{Name: "x", Type: "manifests", Path: server.URL + "/a.yaml"})
		_ = mp.ensureNamespace(ctx, "fresh")
	})
	for _, want := range []string{"Downloading manifest from " + server.URL, "Creating namespace 'fresh'"} {
		if !strings.Contains(out, want) {
			t.Errorf("output should contain %q:\n%s", want, out)
		}
	}
}

func TestWaitForCRDsEstablishedEdges(t *testing.T) {
	ctx := context.Background()
	crd := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "apiextensions.k8s.io/v1", "kind": "CustomResourceDefinition",
		"metadata": map[string]interface{}{"name": "gadgets.example.com"},
		"status": map[string]interface{}{"conditions": []interface{}{
			"junk", map[string]interface{}{"type": "Established", "status": "True"},
		}},
	}}
	mp, _, dyn := crdProvider(t, false)
	mp.opts.Verbose = true
	if err := dyn.Tracker().Add(crd); err != nil {
		t.Fatal(err)
	}
	out := captureOutput(t, func() {
		if err := mp.waitForCRDsEstablished(ctx, []*unstructured.Unstructured{crd}); err != nil {
			t.Error(err)
		}
	})
	if !strings.Contains(out, "CRD gadgets.example.com established") {
		t.Errorf("output:\n%s", out)
	}

	// The CRD can't be read: the last read error is reported
	orig := crdEstablishTimeout
	crdEstablishTimeout = 10 * time.Millisecond
	t.Cleanup(func() { crdEstablishTimeout = orig })
	mp, _, dyn = crdProvider(t, false)
	dyn.PrependReactor("get", "customresourcedefinitions", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("forbidden")
	})
	if err := mp.waitForCRDsEstablished(ctx, []*unstructured.Unstructured{crd}); err == nil || !strings.Contains(err.Error(), "forbidden") {
		t.Errorf("err = %v, want the read error", err)
	}
}

func TestDeleteResourcesEdges(t *testing.T) {
	ctx := context.Background()
	ref := func(name string) state.ResourceRef {
		return state.ResourceRef{APIVersion: "v1", Kind: "ConfigMap", Namespace: "apps", Name: name}
	}
	relabeled := object("v1", "ConfigMap", "apps", "relabeled", krazeLabels("other", "web"), nil)
	stuck := object("v1", "ConfigMap", "apps", "stuck", krazeLabels("app", "web"), nil)
	mp, dyn := applyProvider(relabeled, stuck)
	mp.opts.Quiet = false
	dyn.PrependReactor("get", "configmaps", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if a.(k8stesting.GetAction).GetName() == "unreadable" {
			return true, nil, errors.New("forbidden")
		}
		return false, nil, nil
	})
	dyn.PrependReactor("delete", "configmaps", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("finalizer stuck")
	})

	var n int
	var err error
	out := captureOutput(t, func() {
		n, err = mp.DeleteResources(ctx, "app", []state.ResourceRef{ref("relabeled"), ref("unreadable"), ref("stuck"), ref("gone")})
	})
	if n != 0 || err == nil || !strings.Contains(err.Error(), "failed to delete 2 object(s)") ||
		!strings.Contains(err.Error(), "forbidden") || !strings.Contains(err.Error(), "finalizer stuck") {
		t.Errorf("deleted %d, err = %v", n, err)
	}
	if !strings.Contains(out, "Skipping ConfigMap/apps/relabeled: no longer labeled as service 'app'") {
		t.Errorf("output:\n%s", out)
	}
}

func TestExistingOwnerFromOlderKraze(t *testing.T) {
	old := object("v1", "ConfigMap", "apps", "settings", krazeLabels("other", ""), nil)
	mp, _ := applyProvider(old)
	owner, same := mp.existingOwner(context.Background(), old, "app")
	if owner != "kraze service 'other'" || same {
		t.Errorf("owner = %q, same project = %v", owner, same)
	}
}

func TestReadinessEdges(t *testing.T) {
	obj := func(kind string, spec, status map[string]interface{}) *unstructured.Unstructured {
		o := workload(kind, 1, spec, status)
		o.SetAPIVersion("example.com/v1")
		return o
	}
	tests := []struct {
		name string
		obj  *unstructured.Unstructured
		want bool
	}{
		{"custom kind ready", obj("Gadget", nil, map[string]interface{}{"conditions": []interface{}{map[string]interface{}{"type": "Ready", "status": "True"}}}), true},
		{"custom kind not ready", obj("Gadget", nil, map[string]interface{}{"conditions": []interface{}{}}), false},
		{"deployment with junk conditions", obj("Deployment", map[string]interface{}{"replicas": int64(1)},
			map[string]interface{}{"observedGeneration": int64(1), "updatedReplicas": int64(1), "replicas": int64(1), "availableReplicas": int64(1), "conditions": []interface{}{"junk"}}), true},
		{"statefulset on delete", obj("StatefulSet", map[string]interface{}{"replicas": int64(1), "updateStrategy": map[string]interface{}{"type": "OnDelete"}},
			map[string]interface{}{"observedGeneration": int64(1), "readyReplicas": int64(1)}), true},
		{"job with junk conditions", obj("Job", nil, map[string]interface{}{"conditions": []interface{}{"junk", map[string]interface{}{"type": "Suspended", "status": "True"}}}), false},
		{"pod with junk conditions", obj("Pod", nil, map[string]interface{}{"phase": "Running", "conditions": []interface{}{"junk"}}), false},
	}
	for _, tt := range tests {
		got, err := isResourceReady(tt.obj, tt.obj.GetKind())
		if err != nil || got != tt.want {
			t.Errorf("%s: ready = %v, %v; want %v", tt.name, got, err, tt.want)
		}
	}

	if obj, err := parseYAMLToUnstructured(""); obj != nil || err != nil {
		t.Errorf("an empty document is no object: %v, %v", obj, err)
	}
}

// A cluster-scoped object whose status can't be read is retried until the
// wait times out
func TestWaitForResourceReadyUnreadableStatus(t *testing.T) {
	setVarDuration(t, &readinessPollInterval, time.Millisecond)
	role := object("rbac.authorization.k8s.io/v1", "ClusterRole", "", "reader", nil, nil)
	role.Object["status"] = "not a map"
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	var err error
	out := captureOutput(t, func() {
		err = waitForResourceReady(ctx, widerDynamic(role), fake.NewSimpleClientset(), widerMapper(), role, true)
	})
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(out, "failed to check readiness") {
		t.Errorf("err = %v, output:\n%s", err, out)
	}

	// A controller without matchLabels has no pods to check
	noLabels := workload("Deployment", 1, map[string]interface{}{"selector": map[string]interface{}{}}, nil)
	if err := checkControlledPodsForFailures(context.Background(), fake.NewSimpleClientset(), noLabels, "Deployment", nil); err != nil {
		t.Error(err)
	}
}

func TestHelmProviderEdges(t *testing.T) {
	ctx := context.Background()

	// Verbose Helm logs, and the clients built from the kubeconfig
	helm, err := NewHelmProvider(&ProviderOptions{KubeConfig: unreachableKubeconfig, Verbose: true, Wait: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := helm.actionConfig("apps"); err != nil {
		t.Errorf("verbose action config: %v", err)
	}
	memory := newMemoryHelm(t, &ProviderOptions{KubeConfig: unreachableKubeconfig, Quiet: true, Wait: true, Timeout: "50ms"})
	memory.waitClients = nil // build them from the kubeconfig
	if err := memory.Install(ctx, localChart(t)); err == nil || !strings.Contains(err.Error(), "failed waiting for resources") {
		t.Errorf("waiting on an unreachable cluster: %v", err)
	}
	helm.dynamicClient = nil
	if err := helm.deleteCRDs(ctx, []string{"gadgets.example.com"}); err == nil {
		t.Error("deleting CRDs on an unreachable cluster should fail")
	}

	// A remote chart from neither OCI nor HTTP is used as it is
	path, cleanup, err := helm.getChartPath(ctx, &config.ServiceConfig{Name: "x", Type: "helm", Repo: "file:///charts", Chart: "c", Path: "/charts/c"})
	cleanup()
	if err != nil || path != "/charts/c" {
		t.Errorf("path = %q, %v", path, err)
	}
}

func TestPullChartToDir(t *testing.T) {
	isolated := newTestHelmProvider(t)
	helm := NewHelmProviderForPacking(true)
	helm.settings = isolated.settings
	svc := &config.ServiceConfig{Name: "app", Type: "helm", Repo: serveChart(t), Chart: "mychart", Version: "1.0.0"}
	var path string
	var err error
	out := captureOutput(t, func() { path, err = helm.PullChartToDir(svc, t.TempDir()) })
	if err != nil || filepath.Base(path) != "mychart-1.0.0.tgz" || !strings.Contains(out, "Pulling chart") {
		t.Errorf("path = %q, err = %v, output:\n%s", path, err, out)
	}

	for name, svc := range map[string]*config.ServiceConfig{
		"local chart":     {Name: "app", Type: "helm", Path: "./chart"},
		"unsupported URL": {Name: "app", Type: "helm", Repo: "ftp://charts", Chart: "c"},
		"unreachable OCI": {Name: "app", Type: "helm", Repo: "oci://127.0.0.1:1/charts", Chart: "c"},
	} {
		if _, err := helm.PullChartToDir(svc, t.TempDir()); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
}

func TestYAMLMappingHelpers(t *testing.T) {
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte("spec:\n  template: [a]\n  name: x\nkind: Deployment\n"), &doc); err != nil {
		t.Fatal(err)
	}
	root := doc.Content[0]
	if mappingAt(root, "kind", "x") != nil {
		t.Error("a path through a scalar has no mapping")
	}
	if mappingAt(root, "spec", "template") != nil {
		t.Error("a sequence isn't a mapping")
	}
	spec := mappingAt(root, "spec")
	setMappingValue(spec, "name", "y")
	setMappingValue(spec, "added", "123")
	out, err := yaml.Marshal(root)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `name: "y"`) || !strings.Contains(string(out), `added: "123"`) {
		t.Errorf("yaml:\n%s", out)
	}
}
