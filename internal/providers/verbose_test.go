package providers

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/hjames9/kraze/internal/config"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

// unreachableKubeconfig points at a closed loopback port: clients build, and
// calls fail at once without reaching a real cluster
const unreachableKubeconfig = `apiVersion: v1
kind: Config
clusters:
- name: c
  cluster: {server: "https://127.0.0.1:1", insecure-skip-tls-verify: true}
contexts:
- name: c
  context: {cluster: c, user: u}
current-context: c
users:
- name: u
  user: {token: t}
`

func TestNamespaceInfoAndEnsure(t *testing.T) {
	ctx := context.Background()
	clientset := fake.NewSimpleClientset(
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "theirs"}},
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "ours", Labels: map[string]string{ManagedByLabel: ManagedByValue}}},
	)
	for ns, want := range map[string][2]bool{"theirs": {true, false}, "ours": {true, true}, "missing": {false, false}} {
		exists, managed, err := NamespaceInfo(ctx, clientset, ns)
		if err != nil || exists != want[0] || managed != want[1] {
			t.Errorf("%s: exists=%v managed=%v err=%v, want %v", ns, exists, managed, err, want)
		}
	}

	created, err := EnsureNamespace(ctx, clientset, "fresh")
	if err != nil || !created {
		t.Fatalf("created=%v err=%v", created, err)
	}
	if _, managed, _ := NamespaceInfo(ctx, clientset, "fresh"); !managed {
		t.Error("a namespace kraze creates carries the managed-by label")
	}
	for _, ns := range []string{"fresh", "", "kube-system", "default"} {
		if created, err := EnsureNamespace(ctx, clientset, ns); err != nil || created {
			t.Errorf("%q: created=%v err=%v, want neither", ns, created, err)
		}
	}

	failing := fake.NewSimpleClientset()
	failing.PrependReactor("*", "namespaces", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("forbidden")
	})
	if _, _, err := NamespaceInfo(ctx, failing, "x"); err == nil {
		t.Error("want an error when namespaces can't be read")
	}
	if _, err := EnsureNamespace(ctx, failing, "x"); err == nil {
		t.Error("want an error when a namespace can't be created")
	}
}

// The kubeconfig wrappers build a client and pass the call on
func TestKubeconfigWrappersReachTheCluster(t *testing.T) {
	ctx := context.Background()
	if err := DeleteNamespace(ctx, unreachableKubeconfig, "apps"); err == nil {
		t.Error("an unreachable cluster can't delete")
	}
	// The wait polls until the timeout, as an unreachable cluster never says gone
	setVarDuration(t, &namespaceDeletionPollInterval, time.Millisecond)
	if err := WaitForNamespaceDeletion(ctx, unreachableKubeconfig, "apps", 20*time.Millisecond); err == nil || !strings.Contains(err.Error(), "timeout") {
		t.Errorf("err = %v", err)
	}
	if _, err := GetPodsForService(ctx, unreachableKubeconfig, &config.ServiceConfig{Name: "a", Type: "helm"}); err == nil {
		t.Error("an unreachable cluster has no pods to list")
	}
	if err := WaitForManifestsInNamespace(ctx, unreachableKubeconfig, "", "apps", &ProviderOptions{Wait: true, Quiet: true}); err != nil {
		t.Errorf("an empty manifest needs no waiting, got %v", err)
	}
}

// The same flows with output on: what users see without --plain
func TestHelmFlowsWithOutput(t *testing.T) {
	ctx := context.Background()
	helm := newMemoryHelm(t, &ProviderOptions{Verbose: true, Timeout: "1m"})
	svc := localChart(t)
	var err error
	out := captureOutput(t, func() {
		if err = helm.Install(ctx, svc); err != nil {
			return
		}
		if err = helm.Install(ctx, svc); err != nil {
			return
		}
		err = helm.Uninstall(ctx, svc)
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"Installing Helm chart 'app'", "installed successfully",
		"Upgrading Helm chart 'app'", "upgraded successfully",
		"Uninstalling Helm release 'app'", "CRDs will be deleted", "Deleting 1 CRD(s)", "Deleting CRD: gadgets.example.com",
		"CRD gadgets.example.com already deleted", "uninstalled successfully",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output should contain %q:\n%s", want, out)
		}
	}

	// Keeping CRDs, and a CRD that can't be deleted
	helm = newMemoryHelm(t, &ProviderOptions{Verbose: true, KeepCRDs: true})
	captureOutput(t, func() { _ = helm.Install(ctx, svc) })
	out = captureOutput(t, func() { err = helm.Uninstall(ctx, svc) })
	if err != nil || !strings.Contains(out, "CRDs will be preserved") {
		t.Errorf("err = %v:\n%s", err, out)
	}
	helm = newMemoryHelm(t, &ProviderOptions{})
	helm.dynamic.PrependReactor("delete", "customresourcedefinitions", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("forbidden")
	})
	captureOutput(t, func() { _ = helm.Install(ctx, svc) })
	out = captureOutput(t, func() { err = helm.Uninstall(ctx, svc) })
	if err != nil || !strings.Contains(out, "Failed to delete some CRDs") {
		t.Errorf("a CRD that can't be deleted is a warning: %v:\n%s", err, out)
	}
}

func TestWaitForManifestsWithOutput(t *testing.T) {
	setVarDuration(t, &readinessPollInterval, time.Millisecond)
	ready := workload("Deployment", 1, map[string]interface{}{"replicas": int64(1)}, map[string]interface{}{
		"observedGeneration": int64(1), "replicas": int64(1), "updatedReplicas": int64(1), "availableReplicas": int64(1),
	})
	ready.SetName("web")
	ready.SetNamespace("apps")
	manifest := "apiVersion: v1\nkind: ConfigMap\nmetadata: {name: cfg}\n---\napiVersion: apps/v1\nkind: Deployment\nmetadata: {name: web}\n"

	var err error
	out := captureOutput(t, func() {
		err = waitForManifests(context.Background(), waitTestClients(ready), manifest, "apps", &ProviderOptions{Wait: true, Verbose: true})
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Waiting for resources to be ready (timeout: 10m0s)", "Skipping wait for ConfigMap/cfg", "Waiting for Deployment/web", "Deployment/web is ready", "All resources are ready"} {
		if !strings.Contains(out, want) {
			t.Errorf("output should contain %q:\n%s", want, out)
		}
	}
}
