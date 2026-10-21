package providers

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hjames9/kraze/internal/config"
	"helm.sh/helm/v4/pkg/action"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

var (
	podGVR        = schema.GroupVersionResource{Version: "v1", Resource: "pods"}
	deploymentGVR = schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}
	pvcGVR        = schema.GroupVersionResource{Version: "v1", Resource: "persistentvolumeclaims"}
)

// widerMapper knows the kinds these tests apply and wait for
func widerMapper() *meta.DefaultRESTMapper {
	mapper := meta.NewDefaultRESTMapper(nil)
	for _, gvk := range []schema.GroupVersionKind{
		{Version: "v1", Kind: "ConfigMap"}, {Version: "v1", Kind: "Pod"}, {Version: "v1", Kind: "PersistentVolumeClaim"},
		{Group: "apps", Version: "v1", Kind: "Deployment"}, {Group: "batch", Version: "v1", Kind: "Job"},
	} {
		mapper.Add(gvk, meta.RESTScopeNamespace)
	}
	mapper.Add(schema.GroupVersionKind{Group: "rbac.authorization.k8s.io", Version: "v1", Kind: "ClusterRole"}, meta.RESTScopeRoot)
	return mapper
}

func widerDynamic(objs ...runtime.Object) *dynamicfake.FakeDynamicClient {
	return dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		configMapGVR: "ConfigMapList", clusterRoleGVR: "ClusterRoleList", podGVR: "PodList",
		deploymentGVR: "DeploymentList", pvcGVR: "PersistentVolumeClaimList", jobGVR: "JobList",
	}, objs...)
}

func podObject(name string, status map[string]interface{}) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "v1", "kind": "Pod",
		"metadata": map[string]interface{}{"name": name, "namespace": "apps"},
	}}
	if status != nil {
		obj.Object["status"] = status
	}
	return obj
}

func readyPodStatus() map[string]interface{} {
	return map[string]interface{}{"phase": "Running", "conditions": []interface{}{map[string]interface{}{"type": "Ready", "status": "True"}}}
}

func TestWaitForResourceReadyBranches(t *testing.T) {
	setVarDuration(t, &readinessPollInterval, time.Millisecond)
	ctx := context.Background()
	wait := func(dyn *dynamicfake.FakeDynamicClient, obj *unstructured.Unstructured, timeout time.Duration) error {
		ctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		var err error
		captureOutput(t, func() { err = waitForResourceReady(ctx, dyn, fake.NewSimpleClientset(), widerMapper(), obj, true) })
		return err
	}

	t.Run("created while waiting", func(t *testing.T) {
		dyn := widerDynamic()
		var gets atomic.Int32
		dyn.PrependReactor("get", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
			if gets.Add(1) == 3 {
				_ = dyn.Tracker().Add(podObject("web", readyPodStatus()))
			}
			return false, nil, nil
		})
		if err := wait(dyn, podObject("web", nil), time.Second); err != nil {
			t.Errorf("err = %v", err)
		}
	})

	t.Run("deleted while waiting", func(t *testing.T) {
		dyn := widerDynamic(podObject("web", map[string]interface{}{"phase": "Pending"}))
		var gets atomic.Int32
		dyn.PrependReactor("get", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
			if gets.Add(1) == 2 {
				_ = dyn.Tracker().Delete(podGVR, "apps", "web")
			}
			return false, nil, nil
		})
		if err := wait(dyn, podObject("web", nil), time.Second); err == nil || err.Error() != "resource was deleted" {
			t.Errorf("err = %v", err)
		}
	})

	t.Run("transient errors are retried", func(t *testing.T) {
		dyn := widerDynamic(podObject("web", readyPodStatus()))
		var gets atomic.Int32
		dyn.PrependReactor("get", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
			if gets.Add(1) <= 2 {
				return true, nil, errors.New("connection reset")
			}
			return false, nil, nil
		})
		if err := wait(dyn, podObject("web", nil), time.Second); err != nil {
			t.Errorf("err = %v", err)
		}
	})

	t.Run("a failing pod ends the wait", func(t *testing.T) {
		crashing := podObject("web", map[string]interface{}{"containerStatuses": []interface{}{map[string]interface{}{
			"name": "app", "state": map[string]interface{}{"waiting": map[string]interface{}{"reason": "CrashLoopBackOff"}},
		}}})
		if err := wait(widerDynamic(crashing), podObject("web", nil), time.Second); err == nil || !strings.Contains(err.Error(), "pod failed") {
			t.Errorf("err = %v", err)
		}
	})

	t.Run("a terminating pod isn't judged", func(t *testing.T) {
		terminating := podObject("web", map[string]interface{}{"phase": "Failed"})
		terminating.SetDeletionTimestamp(&metav1.Time{Time: time.Now()})
		if err := wait(widerDynamic(terminating), podObject("web", nil), 30*time.Millisecond); !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("err = %v, want the timeout", err)
		}
	})

	t.Run("a deployment's failing pod ends the wait", func(t *testing.T) {
		deploy := workload("Deployment", 1, map[string]interface{}{
			"replicas": int64(1), "selector": map[string]interface{}{"matchLabels": map[string]interface{}{"app": "web"}},
		}, map[string]interface{}{"observedGeneration": int64(1)})
		deploy.SetName("web")
		deploy.SetNamespace("apps")
		clientset := fake.NewSimpleClientset(&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "web-1", Namespace: "apps", Labels: map[string]string{"app": "web"}},
			Status:     corev1.PodStatus{Phase: corev1.PodFailed, Reason: "OOMKilled"},
		})
		var err error
		captureOutput(t, func() {
			err = waitForResourceReady(ctx, widerDynamic(deploy), clientset, widerMapper(), deploy, false)
		})
		if err == nil || !strings.Contains(err.Error(), "Deployment has failing pod web-1") {
			t.Errorf("err = %v", err)
		}
	})

	t.Run("unknown kind", func(t *testing.T) {
		gadget := &unstructured.Unstructured{Object: map[string]interface{}{"apiVersion": "example.com/v1", "kind": "Gadget", "metadata": map[string]interface{}{"name": "g"}}}
		if err := wait(widerDynamic(), gadget, time.Second); err == nil {
			t.Error("want an error for a kind the cluster doesn't know")
		}
	})
}

func applyProvider(objs ...runtime.Object) (*ManifestsProvider, *dynamicfake.FakeDynamicClient) {
	dyn := widerDynamic(objs...)
	return newManifestsProviderWithClients(&ProviderOptions{Project: "web", Quiet: true, Verbose: true},
		&kubeClients{dynamic: dyn, clientset: fake.NewSimpleClientset(), mapper: widerMapper()}), dyn
}

func configMap(data string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "v1", "kind": "ConfigMap",
		"metadata": map[string]interface{}{"name": "settings", "namespace": "apps"},
		"data":     map[string]interface{}{"v": data},
	}}
}

func TestApplyResource(t *testing.T) {
	ctx := context.Background()
	apply := func(mp *ManifestsProvider, obj *unstructured.Unstructured) error {
		var err error
		captureOutput(t, func() { err = mp.applyResource(ctx, obj) })
		return err
	}
	get := func(dyn *dynamicfake.FakeDynamicClient) string {
		obj, err := dyn.Resource(configMapGVR).Namespace("apps").Get(ctx, "settings", metav1.GetOptions{})
		if err != nil {
			return "<" + err.Error() + ">"
		}
		v, _, _ := unstructured.NestedString(obj.Object, "data", "v")
		return v
	}

	mp, dyn := applyProvider()
	if err := apply(mp, configMap("1")); err != nil || get(dyn) != "1" {
		t.Errorf("create: %v, %s", err, get(dyn))
	}
	if err := apply(mp, configMap("2")); err != nil || get(dyn) != "2" {
		t.Errorf("update: %v, %s", err, get(dyn))
	}

	// A conflict is retried with the latest version
	var updates atomic.Int32
	dyn.PrependReactor("update", "configmaps", func(k8stesting.Action) (bool, runtime.Object, error) {
		if updates.Add(1) == 1 {
			return true, nil, apierrors.NewConflict(schema.GroupResource{Resource: "configmaps"}, "settings", errors.New("changed"))
		}
		return false, nil, nil
	})
	if err := apply(mp, configMap("3")); err != nil || get(dyn) != "3" || updates.Load() != 2 {
		t.Errorf("after a conflict: %v, %s, %d updates", err, get(dyn), updates.Load())
	}

	// Conflicts every time
	mp, dyn = applyProvider(configMap("1"))
	dyn.PrependReactor("update", "configmaps", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewConflict(schema.GroupResource{Resource: "configmaps"}, "settings", errors.New("changed"))
	})
	if err := apply(mp, configMap("2")); err == nil || !strings.Contains(err.Error(), "after 5 attempts") {
		t.Errorf("err = %v", err)
	}

	// Other update, get, create and re-fetch errors
	for name, setup := range map[string]func(dyn *dynamicfake.FakeDynamicClient){
		"update": func(dyn *dynamicfake.FakeDynamicClient) {
			dyn.PrependReactor("update", "configmaps", func(k8stesting.Action) (bool, runtime.Object, error) { return true, nil, errors.New("forbidden") })
		},
		"get": func(dyn *dynamicfake.FakeDynamicClient) {
			dyn.PrependReactor("get", "configmaps", func(k8stesting.Action) (bool, runtime.Object, error) { return true, nil, errors.New("forbidden") })
		},
		"re-fetch": func(dyn *dynamicfake.FakeDynamicClient) {
			var gets atomic.Int32
			dyn.PrependReactor("get", "configmaps", func(k8stesting.Action) (bool, runtime.Object, error) {
				if gets.Add(1) > 1 {
					return true, nil, errors.New("gone away")
				}
				return false, nil, nil
			})
			dyn.PrependReactor("update", "configmaps", func(k8stesting.Action) (bool, runtime.Object, error) {
				return true, nil, apierrors.NewConflict(schema.GroupResource{Resource: "configmaps"}, "settings", errors.New("changed"))
			})
		},
	} {
		mp, dyn := applyProvider(configMap("1"))
		setup(dyn)
		if err := apply(mp, configMap("2")); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
	mp, dyn = applyProvider()
	dyn.PrependReactor("create", "configmaps", func(k8stesting.Action) (bool, runtime.Object, error) { return true, nil, errors.New("quota") })
	if err := apply(mp, configMap("1")); err == nil || !strings.Contains(err.Error(), "failed to create resource: quota") {
		t.Errorf("err = %v", err)
	}

	// A bound PVC is immutable: never updated
	pvc := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "v1", "kind": "PersistentVolumeClaim",
		"metadata": map[string]interface{}{"name": "data", "namespace": "apps"},
	}}
	mp, dyn = applyProvider(pvc.DeepCopy())
	if err := apply(mp, pvc); err != nil {
		t.Fatal(err)
	}
	for _, a := range dyn.Actions() {
		if a.GetVerb() == "update" {
			t.Error("a PVC must not be updated")
		}
	}

	// A kind discovery doesn't know (yet) is applied under its guessed plural
	gadget := &unstructured.Unstructured{Object: map[string]interface{}{"apiVersion": "example.com/v1", "kind": "Gadget", "metadata": map[string]interface{}{"name": "g"}}}
	mp, dyn = applyProvider()
	if err := apply(mp, gadget); err != nil {
		t.Fatal(err)
	}
	created := false
	for _, a := range dyn.Actions() {
		if a.GetVerb() == "create" && a.GetResource() == (schema.GroupVersionResource{Group: "example.com", Version: "v1", Resource: "gadgets"}) {
			created = true
		}
	}
	if !created {
		t.Errorf("want a create of example.com/v1 gadgets, got %v", dyn.Actions())
	}
}

func TestManifestsUninstallBranches(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "app.yaml")
	if err := os.WriteFile(path, []byte(ownershipManifests+"---\nkind: [\n---\napiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: theirs\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	svc := &config.ServiceConfig{Name: "app", Type: "manifests", Namespace: "apps", Path: path}
	theirs := object("v1", "ConfigMap", "apps", "theirs", krazeLabels("other", "web"), nil)
	mine := object("v1", "ConfigMap", "apps", "settings", krazeLabels("app", "web"), nil)
	role := object("rbac.authorization.k8s.io/v1", "ClusterRole", "", "reader", krazeLabels("app", "web"), nil)
	mp, dyn := applyProvider(theirs, mine, role)
	mp.opts.Quiet = false
	dyn.PrependReactor("delete", "clusterroles", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("forbidden")
	})

	var err error
	out := captureOutput(t, func() { err = mp.Uninstall(ctx, svc) })
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Warning: failed to parse manifest", "Skipping ConfigMap/apps/theirs: owned by", "Warning: failed to delete ClusterRole/reader: forbidden", "Deleted 1 resource(s) for 'app'"} {
		if !strings.Contains(out, want) {
			t.Errorf("output should contain %q:\n%s", want, out)
		}
	}
	if _, err := dyn.Resource(configMapGVR).Namespace("apps").Get(ctx, "theirs", metav1.GetOptions{}); err != nil {
		t.Error("another service's object must be kept")
	}

	if err := mp.Uninstall(ctx, &config.ServiceConfig{Name: "app", Type: "manifests", Path: filepath.Join(t.TempDir(), "missing.yaml")}); err == nil {
		t.Error("want an error for manifests that can't be loaded")
	}
}

// Install waits for what it applied: Jobs that succeed are recorded
func TestManifestsInstallWaits(t *testing.T) {
	setVarDuration(t, &readinessPollInterval, time.Millisecond)
	path := filepath.Join(t.TempDir(), "job.yaml")
	if err := os.WriteFile(path, []byte("apiVersion: batch/v1\nkind: Job\nmetadata:\n  name: migrate\n---\napiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: cfg\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	no := false
	svc := &config.ServiceConfig{Name: "app", Type: "manifests", Namespace: "apps", Path: path, CreateNamespace: &no}

	mp, dyn := applyProvider()
	mp.opts.Wait, mp.opts.Quiet, mp.opts.Timeout = true, false, "5s"
	dyn.PrependReactor("get", "jobs", func(a k8stesting.Action) (bool, runtime.Object, error) {
		job, err := dyn.Tracker().Get(jobGVR, "apps", "migrate")
		if err != nil {
			return false, nil, nil
		}
		done := job.DeepCopyObject().(*unstructured.Unstructured)
		_ = unstructured.SetNestedField(done.Object, int64(1), "status", "succeeded")
		return true, done, nil
	})
	var err error
	out := captureOutput(t, func() { err = mp.Install(context.Background(), svc) })
	if err != nil {
		t.Fatalf("install: %v\n%s", err, out)
	}
	if !mp.completedJobs[jobKey("apps", "migrate")] {
		t.Error("the succeeded Job should be recorded")
	}
	for _, want := range []string{"timeout: 5s", "Skipping wait for ConfigMap/cfg", "Job/migrate is ready", "All resources are ready"} {
		if !strings.Contains(out, want) {
			t.Errorf("output should contain %q:\n%s", want, out)
		}
	}

	// Never succeeds
	mp, _ = applyProvider()
	mp.opts.Wait, mp.opts.Timeout = true, "30ms"
	captureOutput(t, func() { err = mp.Install(context.Background(), svc) })
	if err == nil || !strings.Contains(err.Error(), "timeout waiting for Job/migrate") {
		t.Errorf("err = %v", err)
	}
}

// A chart in an OCI registry is located through Helm's registry client
func TestGetChartPathOCI(t *testing.T) {
	isolated := newTestHelmProvider(t)
	helm := newMemoryHelm(t, &ProviderOptions{Quiet: true})
	helm.settings = isolated.settings
	svc := &config.ServiceConfig{Name: "app", Type: "helm", Repo: "oci://127.0.0.1:1/charts", Chart: "app", Version: "1.0.0"}
	if _, _, err := helm.getChartPath(context.Background(), svc); err == nil {
		t.Error("an unreachable registry should fail")
	}
	helm.actionConfigFor = func(string) (*action.Configuration, error) { return nil, errors.New("no config") }
	if _, _, err := helm.getChartPath(context.Background(), svc); err == nil || !strings.Contains(err.Error(), "failed to get action config for OCI chart") {
		t.Errorf("err = %v", err)
	}
}

func TestLoadValuesVerbose(t *testing.T) {
	helm := &HelmProvider{opts: &ProviderOptions{Verbose: true}}
	dir := t.TempDir()
	a, b := filepath.Join(dir, "a.yaml"), filepath.Join(dir, "b.yaml")
	for _, f := range []string{a, b} {
		if err := os.WriteFile(f, []byte("k: "+filepath.Base(f)+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for name, svc := range map[string]*config.ServiceConfig{
		"inline":   {ValuesInline: "k: inline\n"},
		"one file": {Values: valuesFieldOf(t, []string{a})},
		"merged":   {Values: valuesFieldOf(t, []string{a, b})},
	} {
		var values map[string]interface{}
		var err error
		out := captureOutput(t, func() { values, err = helm.loadValues(svc) })
		if err != nil || values["k"] == nil || !strings.Contains(out, "Loading") {
			t.Errorf("%s: %v, %v\n%s", name, values, err, out)
		}
	}
	if _, err := helm.loadValues(&config.ServiceConfig{ValuesInline: "k: ["}); err == nil {
		t.Error("want an error for inline values that don't parse")
	}
}
