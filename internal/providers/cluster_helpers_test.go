package providers

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hjames9/kraze/internal/config"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func TestDeleteNamespace(t *testing.T) {
	ctx := context.Background()
	clientset := fake.NewSimpleClientset(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "apps"}})

	if err := deleteNamespace(ctx, clientset, "apps"); err != nil {
		t.Fatal(err)
	}
	if _, err := clientset.CoreV1().Namespaces().Get(ctx, "apps", metav1.GetOptions{}); err == nil {
		t.Error("apps should be deleted")
	}
	if err := deleteNamespace(ctx, clientset, "apps"); err != nil {
		t.Errorf("an already deleted namespace is fine, got %v", err)
	}
	for _, ns := range []string{"default", "kube-system", "kube-public", "kube-node-lease"} {
		if err := deleteNamespace(ctx, clientset, ns); err == nil || !strings.Contains(err.Error(), "refusing") {
			t.Errorf("%s: err = %v, want a refusal", ns, err)
		}
		if err := DeleteNamespace(ctx, "", ns); err == nil || !strings.Contains(err.Error(), "refusing") {
			t.Errorf("DeleteNamespace(%s) must refuse before connecting, got %v", ns, err)
		}
	}

	clientset.PrependReactor("delete", "namespaces", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("forbidden")
	})
	if err := deleteNamespace(ctx, clientset, "other"); err == nil || !strings.Contains(err.Error(), "forbidden") {
		t.Errorf("err = %v", err)
	}
}

func TestWaitForNamespaceDeletion(t *testing.T) {
	setVarDuration(t, &namespaceDeletionPollInterval, time.Millisecond)
	ctx := context.Background()

	if err := waitForNamespaceDeletion(ctx, fake.NewSimpleClientset(), "gone", time.Second); err != nil {
		t.Errorf("a deleted namespace: %v", err)
	}

	// Still terminating: times out
	terminating := fake.NewSimpleClientset(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "stuck"}})
	err := waitForNamespaceDeletion(ctx, terminating, "stuck", 20*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "timeout waiting for namespace 'stuck'") {
		t.Errorf("err = %v", err)
	}

	// Deleted while waiting
	going := fake.NewSimpleClientset(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "going"}})
	gets := 0
	going.PrependReactor("get", "namespaces", func(k8stesting.Action) (bool, runtime.Object, error) {
		gets++
		if gets == 3 {
			_ = going.Tracker().Delete(schema.GroupVersionResource{Version: "v1", Resource: "namespaces"}, "", "going")
		}
		return false, nil, nil
	})
	if err := waitForNamespaceDeletion(ctx, going, "going", time.Second); err != nil {
		t.Errorf("err = %v", err)
	}

	if err := WaitForNamespaceDeletion(ctx, "", "x", time.Second); err == nil {
		t.Error("want an error for an empty kubeconfig")
	}
}

func pod(name, namespace string, labels map[string]string, phase corev1.PodPhase, deleting bool) *corev1.Pod {
	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, Labels: labels},
		Status:     corev1.PodStatus{Phase: phase},
	}
	if deleting {
		now := metav1.NewTime(time.Now())
		p.DeletionTimestamp = &now
	}
	return p
}

func TestPodsForService(t *testing.T) {
	ctx := context.Background()
	helmLabels := map[string]string{"app.kubernetes.io/instance": "api"}
	clientset := fake.NewSimpleClientset(
		pod("api-running", "apps", helmLabels, corev1.PodRunning, false),
		pod("api-pending", "apps", helmLabels, corev1.PodPending, false),
		pod("api-done", "apps", helmLabels, corev1.PodSucceeded, false),
		pod("api-deleting", "apps", helmLabels, corev1.PodRunning, true),
		pod("api-elsewhere", "other", helmLabels, corev1.PodRunning, false),
		pod("worker", "apps", map[string]string{"app": "worker"}, corev1.PodRunning, false),
		pod("labeled", "apps", map[string]string{"tier": "jobs"}, corev1.PodRunning, false),
	)

	got, err := podsForService(ctx, clientset, &config.ServiceConfig{Name: "api", Type: "helm", Namespace: "apps"})
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(got)
	if want := []string{"api-pending", "api-running"}; !slices.Equal(got, want) {
		t.Errorf("helm pods = %v, want %v", got, want)
	}

	if got, err := podsForService(ctx, clientset, &config.ServiceConfig{Name: "worker", Type: "manifests", Namespace: "apps"}); err != nil || !slices.Equal(got, []string{"worker"}) {
		t.Errorf("manifests pods by app label = %v, %v", got, err)
	}
	labeled := &config.ServiceConfig{Name: "jobs", Type: "manifests", Namespace: "apps", Labels: map[string]string{"tier": "jobs"}}
	if got, err := podsForService(ctx, clientset, labeled); err != nil || !slices.Equal(got, []string{"labeled"}) {
		t.Errorf("manifests pods by service labels = %v, %v", got, err)
	}

	if _, err := podsForService(ctx, clientset, &config.ServiceConfig{Name: "none", Type: "helm", Namespace: "apps"}); err == nil || !strings.Contains(err.Error(), "no running pods found for service 'none'") {
		t.Errorf("err = %v", err)
	}
	if _, err := podsForService(ctx, clientset, &config.ServiceConfig{Name: "x", Type: "kustomize"}); err == nil || !strings.Contains(err.Error(), "unsupported service type") {
		t.Errorf("err = %v", err)
	}
	if _, err := GetPodsForService(ctx, "", &config.ServiceConfig{Name: "x", Type: "helm"}); err == nil {
		t.Error("want an error for an empty kubeconfig")
	}
}

// A minimal kubeconfig: building clients from it doesn't connect
const offlineKubeconfig = `apiVersion: v1
kind: Config
clusters:
- name: c
  cluster:
    server: https://127.0.0.1:1
    certificate-authority-data: ZmFrZQ==
contexts:
- name: c
  context: {cluster: c, user: u}
current-context: c
users:
- name: u
  user: {token: t}
`

func TestConstructorsDoNotConnect(t *testing.T) {
	opts := &ProviderOptions{Project: "web", KubeConfig: offlineKubeconfig}
	for _, typ := range []string{"helm", "manifests"} {
		p, err := NewProvider(&config.ServiceConfig{Name: "x", Type: typ}, opts)
		if err != nil || p == nil {
			t.Errorf("%s: %v", typ, err)
		}
	}
	if _, err := NewProvider(&config.ServiceConfig{Name: "x", Type: "kustomize"}, opts); err == nil {
		t.Error("want an error for an unknown type")
	}
	if _, err := NewHelmProvider(&ProviderOptions{KubeConfig: "not: [yaml"}); err == nil {
		t.Error("want an error for a broken kubeconfig")
	}
	if _, err := newKubeClients(""); err == nil {
		t.Error("want an error for an empty kubeconfig")
	}

	// Skipping TLS verification drops the CA, which isn't a real one here
	if cs, err := GetClientsetFromKubeconfigContent(offlineKubeconfig, true); err != nil || cs == nil {
		t.Errorf("skipTLS: %v", err)
	}
	if _, err := GetClientsetFromKubeconfigContent(offlineKubeconfig, false); err == nil {
		t.Error("with TLS verification the CA must be used, and this one doesn't parse")
	}
	withoutCA := strings.Replace(offlineKubeconfig, "    certificate-authority-data: ZmFrZQ==\n", "", 1)
	if cs, err := GetClientsetFromKubeconfigContent(withoutCA, false); err != nil || cs == nil {
		t.Errorf("verified TLS: %v", err)
	}
	if _, err := GetClientsetFromKubeconfigContent("", false); err == nil {
		t.Error("want an error for an empty kubeconfig")
	}
	if _, err := clientsetFromKubeconfig(offlineKubeconfig); err != nil {
		t.Error(err)
	}
}

func TestGetRESTConfigSkipsCAVerification(t *testing.T) {
	cfg, err := getRESTConfigFromKubeconfig(offlineKubeconfig)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Insecure || cfg.CAData != nil || cfg.CAFile != "" {
		t.Errorf("CA verification should be off for the patched container IP, got insecure=%v ca=%q", cfg.Insecure, cfg.CAData)
	}
}

func TestManifestsFromURL(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/app.yaml" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte("apiVersion: v1\nkind: ConfigMap\nmetadata: {name: a}\n---\napiVersion: v1\nkind: ConfigMap\nmetadata: {name: b}\n"))
	}))
	defer server.Close()

	mp := &ManifestsProvider{opts: &ProviderOptions{Project: "web", Quiet: true}}
	docs, err := mp.loadManifests(&config.ServiceConfig{Name: "app", Type: "manifests", Path: server.URL + "/app.yaml"})
	if err != nil || len(docs) != 2 {
		t.Errorf("docs = %d, err = %v", len(docs), err)
	}
	_, err = mp.loadManifests(&config.ServiceConfig{Name: "app", Type: "manifests", Path: server.URL + "/missing.yaml"})
	if err == nil || !strings.Contains(err.Error(), "HTTP 404") {
		t.Errorf("err = %v", err)
	}
	_, err = mp.loadManifests(&config.ServiceConfig{Name: "app", Type: "manifests", Path: "http://127.0.0.1:1/x.yaml"})
	if err == nil || !strings.Contains(err.Error(), "failed to fetch URL") {
		t.Errorf("err = %v", err)
	}
}

func TestManifestsInstallCreatesNamespace(t *testing.T) {
	namespacesGVR := schema.GroupVersionResource{Version: "v1", Resource: "namespaces"}
	mp := fakeManifestsProvider("web")
	dyn := mp.dynamicClient.(*dynamicfake.FakeDynamicClient)
	svc := ownershipService(t)
	create := true
	svc.CreateNamespace = &create
	ctx := context.Background()

	if err := mp.Install(ctx, svc); err != nil {
		t.Fatal(err)
	}
	if _, err := dyn.Resource(namespacesGVR).Get(ctx, "apps", metav1.GetOptions{}); err != nil {
		t.Errorf("namespace apps should be created: %v", err)
	}
	if installed, err := mp.IsInstalled(ctx, svc); err != nil || !installed {
		t.Errorf("installed = %v, %v", installed, err)
	}

	// Existing and system namespaces are left alone
	creates := func() int {
		n := 0
		for _, a := range dyn.Actions() {
			if a.GetVerb() == "create" && a.GetResource() == namespacesGVR {
				n++
			}
		}
		return n
	}
	before := creates()
	if err := mp.Install(ctx, svc); err != nil {
		t.Fatal(err)
	}
	svc.Namespace = "kube-system"
	if err := mp.ensureNamespace(ctx, "kube-system"); err != nil {
		t.Fatal(err)
	}
	if creates() != before {
		t.Errorf("no namespace should be created again, got %d creates after %d", creates(), before)
	}

	dyn.PrependReactor("get", "namespaces", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("unreachable")
	})
	if err := mp.ensureNamespace(ctx, "fresh"); err == nil || !strings.Contains(err.Error(), "failed to check namespace") {
		t.Errorf("err = %v", err)
	}
}

// Helm's action configuration builds its clients lazily: nothing connects
func TestGetActionConfigDoesNotConnect(t *testing.T) {
	helm, err := NewHelmProvider(&ProviderOptions{KubeConfig: offlineKubeconfig})
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := helm.actionConfig("apps")
	if err != nil || cfg.Releases == nil || cfg.RegistryClient == nil {
		t.Fatalf("action config = %+v, %v", cfg, err)
	}

	getter := &restClientGetter{config: helm.restConfig, namespace: "apps"}
	if rc, err := getter.ToRESTConfig(); err != nil || rc != helm.restConfig {
		t.Errorf("ToRESTConfig = %v, %v", rc, err)
	}
	dc, err := getter.ToDiscoveryClient()
	if err != nil {
		t.Fatal(err)
	}
	if again, _ := getter.ToDiscoveryClient(); again != dc {
		t.Error("the discovery client should be cached")
	}
	mapper, err := getter.ToRESTMapper()
	if err != nil {
		t.Fatal(err)
	}
	if again, _ := getter.ToRESTMapper(); again != mapper {
		t.Error("the REST mapper should be cached")
	}
	loader := getter.ToRawKubeConfigLoader()
	if ns, _, err := loader.Namespace(); err != nil || ns != "apps" {
		t.Errorf("namespace = %q, %v", ns, err)
	}
	if _, err := loader.RawConfig(); err == nil {
		t.Error("RawConfig isn't implemented")
	}
	if _, err := loader.ClientConfig(); err == nil {
		t.Error("ClientConfig isn't implemented")
	}
	if loader.ConfigAccess() != nil {
		t.Error("ConfigAccess isn't implemented")
	}
}
