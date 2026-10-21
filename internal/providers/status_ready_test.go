package providers

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/hjames9/kraze/internal/config"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8stesting "k8s.io/client-go/testing"
)

// webDeployment is the chart's Deployment "web" in "apps", rolled out or not
func webDeployment(ready bool) *unstructured.Unstructured {
	status := map[string]interface{}{"observedGeneration": int64(1), "replicas": int64(1), "updatedReplicas": int64(1)}
	if ready {
		status["availableReplicas"] = int64(1)
		status["readyReplicas"] = int64(1)
	}
	deployment := workload("Deployment", 1, map[string]interface{}{"replicas": int64(1)}, status)
	deployment.SetName("web")
	deployment.SetNamespace("apps")
	return deployment
}

// `kraze status` reports a service Ready only when its workloads are, as
// `up` waits for them: a Helm release is "deployed" as soon as its objects
// are applied, so a Deployment whose pods can't pull their image was Ready
func TestHelmStatusChecksWorkloads(t *testing.T) {
	helm := newMemoryHelm(t, &ProviderOptions{Quiet: true})
	svc := localChart(t)
	ctx := context.Background()
	if err := helm.Install(ctx, svc); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name      string
		objs      []runtime.Object
		clientErr error
		wantReady bool
		wantMsg   string
	}{
		{name: "rolled out", objs: []runtime.Object{webDeployment(true)}, wantReady: true, wantMsg: "deployed"},
		{name: "pods not available", objs: []runtime.Object{webDeployment(false)}, wantMsg: "not ready: Deployment/web"},
		{name: "workload deleted", wantMsg: "not ready: Deployment/web (missing)"},
		{name: "cluster unreachable", clientErr: errors.New("connection refused"), wantMsg: "readiness unknown: connection refused"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			helm.waitClients = func() (*kubeClients, error) {
				if tt.clientErr != nil {
					return nil, tt.clientErr
				}
				return waitTestClients(tt.objs...), nil
			}
			status, err := helm.Status(ctx, svc)
			if err != nil {
				t.Fatal(err)
			}
			if !status.Installed || status.Ready != tt.wantReady || status.Message != tt.wantMsg {
				t.Errorf("status = %+v, want ready=%v message %q", status, tt.wantReady, tt.wantMsg)
			}
		})
	}
}

func TestManifestsStatusChecksWorkloads(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.yaml")
	manifest := "apiVersion: v1\nkind: ConfigMap\nmetadata: {name: cfg}\n---\napiVersion: apps/v1\nkind: Deployment\nmetadata: {name: web}\nspec: {replicas: 1}\n"
	if err := os.WriteFile(path, []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	svc := &config.ServiceConfig{Name: "app", Type: "manifests", Namespace: "apps", Path: path}
	cfg := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]interface{}{"name": "cfg", "namespace": "apps"},
	}}

	for _, ready := range []bool{true, false} {
		mp := newManifestsProviderWithClients(&ProviderOptions{Project: "web", Quiet: true}, waitTestClients(cfg, webDeployment(ready)))
		status, err := mp.Status(context.Background(), svc)
		if err != nil {
			t.Fatal(err)
		}
		wantMsg := "2/2 resources exist"
		if !ready {
			wantMsg = "not ready: Deployment/web"
		}
		if !status.Installed || status.Ready != ready || status.Message != wantMsg {
			t.Errorf("deployment ready=%v: status = %+v, want message %q", ready, status, wantMsg)
		}
	}

	// A missing workload is reported once, by the existence count
	mp := newManifestsProviderWithClients(&ProviderOptions{Project: "web", Quiet: true}, waitTestClients(cfg))
	status, _ := mp.Status(context.Background(), svc)
	if status.Ready || status.Message != "1/2 resources exist" || strings.Contains(status.Message, "missing") {
		t.Errorf("status = %+v", status)
	}
}

func TestUnreadyWorkloadsEdges(t *testing.T) {
	ctx := context.Background()
	parse := func(manifest string) []*unstructured.Unstructured {
		objs, err := parseManifestsYAML(manifest)
		if err != nil {
			t.Fatal(err)
		}
		return objs
	}
	clients := waitTestClients()

	// A kind the cluster doesn't know
	if _, err := unreadyWorkloads(ctx, clients.dynamic, clients.mapper, parse("apiVersion: batch/v1\nkind: Job\nmetadata: {name: j}\n"), "apps", true); err == nil {
		t.Error("want an error for an unmapped kind")
	}

	clients.mapper.(*meta.DefaultRESTMapper).Add(schema.GroupVersionKind{Group: "batch", Version: "v1", Kind: "Job"}, meta.RESTScopeNamespace)
	// A Job with a TTL that's gone was cleaned up after finishing; one without isn't
	jobs := parse("apiVersion: batch/v1\nkind: Job\nmetadata: {name: ttl}\nspec: {ttlSecondsAfterFinished: 60}\n---\napiVersion: batch/v1\nkind: Job\nmetadata: {name: once}\n")
	unready, err := unreadyWorkloads(ctx, clients.dynamic, clients.mapper, jobs, "apps", true)
	if err != nil || !slices.Equal(unready, []string{"Job/once (missing)"}) {
		t.Errorf("unready = %v, %v", unready, err)
	}

	// The API fails
	clients.dynamic.(*dynamicfake.FakeDynamicClient).PrependReactor("get", "*", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("forbidden")
	})
	if _, err := unreadyWorkloads(ctx, clients.dynamic, clients.mapper, jobs, "apps", true); err == nil || !strings.Contains(err.Error(), "forbidden") {
		t.Errorf("err = %v", err)
	}

	// A release manifest that doesn't split into documents
	helm := newMemoryHelm(t, &ProviderOptions{Quiet: true})
	helm.waitClients = func() (*kubeClients, error) { return waitTestClients(), nil }
	if _, err := helm.unreadyWorkloads(ctx, "--- foo\n", "apps"); err == nil {
		t.Error("want an error for a manifest that doesn't parse")
	}
}
