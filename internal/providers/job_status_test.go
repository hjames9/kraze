package providers

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/hjames9/kraze/internal/config"
	"github.com/hjames9/kraze/internal/state"
)

// A Job with ttlSecondsAfterFinished is deleted by Kubernetes once it has
// finished. `up` records that it saw the Job complete, so `status` can count
// the deleted Job as done rather than missing; a Job that never completed
// (as far as kraze knows) still counts as missing.

var jobGVR = schema.GroupVersionResource{Group: "batch", Version: "v1", Resource: "jobs"}

const jobManifests = `apiVersion: v1
kind: ConfigMap
metadata:
  name: settings
data:
  a: "1"
---
apiVersion: batch/v1
kind: Job
metadata:
  name: init
spec:
  ttlSecondsAfterFinished: 300
  template:
    spec:
      restartPolicy: OnFailure
      containers:
        - name: init
          image: busybox
---
apiVersion: batch/v1
kind: Job
metadata:
  name: migrate
spec:
  template:
    spec:
      restartPolicy: OnFailure
      containers:
        - name: migrate
          image: busybox
`

func jobService(t *testing.T) *config.ServiceConfig {
	t.Helper()
	path := filepath.Join(t.TempDir(), "app.yaml")
	if err := os.WriteFile(path, []byte(jobManifests), 0o644); err != nil {
		t.Fatal(err)
	}
	noCreate := false
	return &config.ServiceConfig{Name: "app", Type: "manifests", Namespace: "apps", Path: path, CreateNamespace: &noCreate}
}

func jobProvider(opts *ProviderOptions, objs ...runtime.Object) *ManifestsProvider {
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		configMapGVR: "ConfigMapList",
		jobGVR:       "JobList",
	}, objs...)
	mapper := meta.NewDefaultRESTMapper(nil)
	mapper.Add(schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"}, meta.RESTScopeNamespace)
	mapper.Add(schema.GroupVersionKind{Group: "batch", Version: "v1", Kind: "Job"}, meta.RESTScopeNamespace)
	return newManifestsProviderWithClients(opts, &kubeClients{dynamic: dyn, clientset: fake.NewSimpleClientset(), mapper: mapper})
}

func liveObject(apiVersion, kind, name string, status map[string]interface{}) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{Object: map[string]interface{}{"apiVersion": apiVersion, "kind": kind,
		"metadata": map[string]interface{}{"name": name, "namespace": "apps"}}}
	if status != nil {
		obj.Object["status"] = status
	}
	return obj
}

// What `up` waits for, it records: a Job seen succeeding is Completed
func TestUpRecordsCompletedJobs(t *testing.T) {
	interval := readinessPollInterval
	readinessPollInterval = 10 * time.Millisecond
	t.Cleanup(func() { readinessPollInterval = interval })

	svc := jobService(t)
	succeeded := map[string]interface{}{"succeeded": int64(1)}
	mp := jobProvider(&ProviderOptions{Project: "web", Quiet: true, Timeout: "5s"},
		liveObject("batch/v1", "Job", "init", succeeded), liveObject("batch/v1", "Job", "migrate", succeeded))

	objects, err := mp.parseServiceObjects(mustLoad(t, mp, svc), svc)
	if err != nil {
		t.Fatal(err)
	}
	if err := mp.waitForAppliedResources(context.Background(), objects); err != nil {
		t.Fatal(err)
	}
	refs, err := mp.ResourceRefs(svc)
	if err != nil {
		t.Fatal(err)
	}
	for _, ref := range refs {
		if want := ref.Kind == "Job"; ref.Completed != want {
			t.Errorf("%s/%s: Completed = %v, want %v", ref.Kind, ref.Name, ref.Completed, want)
		}
	}

	// Without waiting, nothing is known to have completed
	refs, err = jobProvider(&ProviderOptions{Project: "web", Quiet: true}).ResourceRefs(svc)
	if err != nil {
		t.Fatal(err)
	}
	for _, ref := range refs {
		if ref.Completed {
			t.Errorf("%s/%s recorded as completed without a wait", ref.Kind, ref.Name)
		}
	}
}

func mustLoad(t *testing.T, mp *ManifestsProvider, svc *config.ServiceConfig) []string {
	t.Helper()
	manifests, err := mp.loadManifests(svc)
	if err != nil {
		t.Fatal(err)
	}
	return manifests
}

func TestStatusCountsJobsDeletedAfterCompleting(t *testing.T) {
	completed := func(names ...string) []state.ResourceRef {
		var refs []state.ResourceRef
		for _, name := range names {
			refs = append(refs, state.ResourceRef{APIVersion: "batch/v1", Kind: "Job", Namespace: "apps", Name: name, Completed: true})
		}
		return refs
	}
	settings := liveObject("v1", "ConfigMap", "settings", nil)
	migrate := liveObject("batch/v1", "Job", "migrate", map[string]interface{}{"succeeded": int64(1)})
	init := liveObject("batch/v1", "Job", "init", map[string]interface{}{"succeeded": int64(1)})

	tests := []struct {
		name     string
		recorded []state.ResourceRef
		live     []runtime.Object
		ready    bool
		message  string
	}{
		{name: "TTL job deleted after up saw it complete",
			recorded: completed("init", "migrate"), live: []runtime.Object{settings, migrate},
			ready: true, message: "3/3 resources exist (1 job finished)"},
		{name: "TTL job missing, completion never recorded",
			live:  []runtime.Object{settings, migrate},
			ready: false, message: "2/3 resources exist"},
		{name: "job without a TTL is missing even if it completed",
			recorded: completed("init", "migrate"), live: []runtime.Object{settings, init},
			ready: false, message: "2/3 resources exist"},
		{name: "everything still there",
			recorded: completed("init", "migrate"), live: []runtime.Object{settings, init, migrate},
			ready: true, message: "3/3 resources exist"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mp := jobProvider(&ProviderOptions{Project: "web", Quiet: true, Recorded: tt.recorded}, tt.live...)
			status, err := mp.Status(context.Background(), jobService(t))
			if err != nil {
				t.Fatal(err)
			}
			if status.Ready != tt.ready || status.Message != tt.message {
				t.Errorf("ready = %v, message = %q; want %v, %q", status.Ready, status.Message, tt.ready, tt.message)
			}
		})
	}
}
