package providers

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hjames9/kraze/internal/config"
	"helm.sh/helm/v4/pkg/action"
	kubefake "helm.sh/helm/v4/pkg/kube/fake"
	"helm.sh/helm/v4/pkg/release"
	"helm.sh/helm/v4/pkg/storage"
	"helm.sh/helm/v4/pkg/storage/driver"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/dynamic"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	k8stesting "k8s.io/client-go/testing"
)

var errFail = errors.New("forbidden")

func failOn(verb, resource string) k8stesting.ReactionFunc {
	return func(a k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errFail
	}
}

// writeManifest writes content to a manifest file and returns a service
// installing it into apps
func writeManifest(t *testing.T, content string) *config.ServiceConfig {
	t.Helper()
	path := filepath.Join(t.TempDir(), "app.yaml")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	no := false
	return &config.ServiceConfig{Name: "app", Type: "manifests", Namespace: "apps", Path: path, CreateNamespace: &no}
}

const configMapYAML = "apiVersion: v1\nkind: ConfigMap\nmetadata: {name: cfg}\n"

func TestManifestsInstallFailures(t *testing.T) {
	ctx := context.Background()
	setVarDuration(t, &readinessPollInterval, time.Millisecond)
	tests := []struct {
		name     string
		manifest string
		setup    func(mp *ManifestsProvider, dyn *dynamicfake.FakeDynamicClient, svc *config.ServiceConfig)
		want     string
	}{
		{name: "namespace can't be checked", manifest: configMapYAML,
			setup: func(mp *ManifestsProvider, dyn *dynamicfake.FakeDynamicClient, svc *config.ServiceConfig) {
				yes := true
				svc.CreateNamespace = &yes
				dyn.PrependReactor("get", "namespaces", failOn("", ""))
			}, want: "failed to ensure namespace"},
		{name: "namespace can't be created", manifest: configMapYAML,
			setup: func(mp *ManifestsProvider, dyn *dynamicfake.FakeDynamicClient, svc *config.ServiceConfig) {
				yes := true
				svc.CreateNamespace = &yes
				dyn.PrependReactor("create", "namespaces", failOn("", ""))
			}, want: "failed to create namespace"},
		{name: "manifests can't be loaded", manifest: configMapYAML,
			setup: func(mp *ManifestsProvider, dyn *dynamicfake.FakeDynamicClient, svc *config.ServiceConfig) {
				svc.Path = filepath.Join(svc.Path, "missing")
			},
			want: "failed to load manifests"},
		{name: "no manifests", manifest: "", want: "no manifests found"},
		{name: "a manifest doesn't parse", manifest: "kind: [\n", want: "failed to parse manifest 1"},
		{name: "apply fails", manifest: configMapYAML,
			setup: func(mp *ManifestsProvider, dyn *dynamicfake.FakeDynamicClient, svc *config.ServiceConfig) {
				dyn.PrependReactor("create", "configmaps", failOn("", ""))
			}, want: "failed to apply ConfigMap/apps/cfg"},
		{name: "a waited-for pod fails", manifest: "apiVersion: v1\nkind: Pod\nmetadata: {name: web}\n",
			setup: func(mp *ManifestsProvider, dyn *dynamicfake.FakeDynamicClient, svc *config.ServiceConfig) {
				mp.opts.Wait, mp.opts.Timeout = true, "5s"
				// Once created, the pod has failed
				dyn.PrependReactor("get", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
					if _, err := dyn.Tracker().Get(podGVR, "apps", "web"); err != nil {
						return false, nil, nil
					}
					return true, podObject("web", map[string]interface{}{"phase": "Failed", "reason": "Evicted"}), nil
				})
			}, want: "error waiting for Pod/web"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mp, dyn := applyProvider()
			svc := writeManifest(t, tt.manifest)
			if tt.setup != nil {
				tt.setup(mp, dyn, svc)
			}
			var err error
			captureOutput(t, func() { err = mp.Install(ctx, svc) })
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("err = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestManifestsInstallCRDFailures(t *testing.T) {
	ctx := context.Background()
	crd := "apiVersion: apiextensions.k8s.io/v1\nkind: CustomResourceDefinition\nmetadata:\n  name: gadgets.example.com\n"

	mp, _, dyn := crdProvider(t, false)
	dyn.PrependReactor("create", "customresourcedefinitions", failOn("", ""))
	if err := mp.Install(ctx, writeManifest(t, crd)); err == nil || !strings.Contains(err.Error(), "failed to apply") {
		t.Errorf("err = %v", err)
	}

	orig := crdEstablishTimeout
	crdEstablishTimeout = 10 * time.Millisecond
	t.Cleanup(func() { crdEstablishTimeout = orig })
	mp, _, _ = crdProvider(t, false)
	if err := mp.Install(ctx, writeManifest(t, crd)); err == nil || !strings.Contains(err.Error(), "was not established") {
		t.Errorf("err = %v", err)
	}
}

// A workload whose pod template isn't a mapping can't get the config
// checksum: a warning, and the apply goes on
func TestManifestsChecksumWarning(t *testing.T) {
	mp, _ := applyProvider()
	mp.opts.Quiet, mp.opts.Verbose = false, true
	svc := writeManifest(t, configMapYAML+"data: {a: b}\n---\napiVersion: apps/v1\nkind: Deployment\nmetadata: {name: web}\nspec:\n  template: not-a-map\n")
	var err error
	out := captureOutput(t, func() { err = mp.Install(context.Background(), svc) })
	if err != nil || !strings.Contains(out, "failed to calculate config checksum") {
		t.Errorf("err = %v, output:\n%s", err, out)
	}
}

func TestLoadManifestsFailures(t *testing.T) {
	mp, _ := applyProvider()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "a.yaml"), []byte(configMapYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Subdirectories and other files in a directory are skipped
	docs, err := mp.loadManifests(&config.ServiceConfig{Name: "x", Path: dir})
	if err != nil || len(docs) != 1 {
		t.Errorf("docs = %d, %v", len(docs), err)
	}

	unreadable := filepath.Join(t.TempDir(), "locked")
	if err := os.MkdirAll(unreadable, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(unreadable, 0o755) })
	if _, err := mp.loadManifests(&config.ServiceConfig{Name: "x", Path: unreadable}); err == nil || !strings.Contains(err.Error(), "failed to read directory") {
		t.Errorf("err = %v", err)
	}
	lockedFile := filepath.Join(t.TempDir(), "locked.yaml")
	if err := os.WriteFile(lockedFile, []byte(configMapYAML), 0o000); err != nil {
		t.Fatal(err)
	}
	if _, err := mp.loadManifests(&config.ServiceConfig{Name: "x", Paths: []string{lockedFile}}); err == nil || !strings.Contains(err.Error(), "failed to read file") {
		t.Errorf("err = %v", err)
	}
	if _, err := mp.loadManifests(&config.ServiceConfig{Name: "x"}); err == nil || !strings.Contains(err.Error(), "no manifest paths specified") {
		t.Errorf("err = %v", err)
	}

	// A response that ends early
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "100")
		_, _ = w.Write([]byte("short"))
	}))
	defer server.Close()
	if _, err := mp.downloadManifest(server.URL); err == nil || !strings.Contains(err.Error(), "failed to read response body") {
		t.Errorf("err = %v", err)
	}

	if _, err := mp.ResourceRefs(&config.ServiceConfig{Name: "x", Path: filepath.Join(dir, "missing.yaml")}); err == nil {
		t.Error("want an error for manifests that can't be loaded")
	}
	if _, err := mp.ResourceRefs(writeManifest(t, "kind: [\n")); err == nil {
		t.Error("want an error for a manifest that doesn't parse")
	}
	if obj, err := mp.parseManifest("  \n"); obj != nil || err != nil {
		t.Errorf("a blank manifest is no object: %v, %v", obj, err)
	}
}

// Documents without objects and ones that don't parse are skipped where
// they can be
func TestManifestDocumentsSkipped(t *testing.T) {
	ctx := context.Background()
	mp, _ := applyProvider(object("v1", "ConfigMap", "apps", "cfg", krazeLabels("app", "web"), nil))
	svc := writeManifest(t, "# just a comment\n---\n"+configMapYAML+"---\nkind: [\n")
	status, err := mp.Status(ctx, svc)
	if err != nil || status.Message != "1/2 resources exist" {
		t.Errorf("status = %+v, %v", status, err)
	}
	captureOutput(t, func() { err = mp.Uninstall(ctx, svc) })
	if err != nil {
		t.Error(err)
	}

	objs, err := parseManifestsYAML("---\n\n---\n" + configMapYAML + "---\nkind: [\n")
	if err != nil || len(objs) != 1 {
		t.Errorf("objects = %d, %v", len(objs), err)
	}
	if _, err := parseYAMLToUnstructured("kind: ["); err == nil {
		t.Error("want an error for a document that doesn't parse")
	}
}

func TestNamespaceWorkloadsListFailures(t *testing.T) {
	for _, resource := range []string{"pods", "deployments", "statefulsets", "daemonsets", "services"} {
		clientset := fake.NewSimpleClientset()
		clientset.PrependReactor("list", resource, failOn("", ""))
		if _, err := NamespaceWorkloads(context.Background(), clientset, "apps"); err == nil {
			t.Errorf("%s: want an error", resource)
		}
	}
}

func TestPodDiagnosticsFailures(t *testing.T) {
	ctx := context.Background()
	clientset := fake.NewSimpleClientset()
	clientset.PrependReactor("list", "events", failOn("", ""))
	if _, err := getPodEvents(ctx, clientset, "apps", "p", 5); err == nil {
		t.Error("want an error when events can't be listed")
	}
	pods := fake.NewSimpleClientset()
	pods.PrependReactor("list", "pods", failOn("", ""))
	deploy := deploymentWithSelector()
	if err := checkControlledPodsForFailures(ctx, pods, deploy, "Deployment", nil); err != nil {
		t.Errorf("pods that can't be listed aren't a failure: %v", err)
	}
}

func TestKubeconfigClientFailures(t *testing.T) {
	noCluster := "apiVersion: v1\nkind: Config\ncurrent-context: missing\n"
	if _, err := getRESTConfigFromKubeconfig(noCluster); err == nil || !strings.Contains(err.Error(), "failed to create REST config") {
		t.Errorf("err = %v", err)
	}
	badHost := strings.Replace(unreachableKubeconfig, "https://127.0.0.1:1", "https://bad host", 1)
	if _, err := clientsetFromKubeconfig(badHost); err == nil {
		t.Error("want an error for a server URL with a space")
	}
	if _, err := newKubeClients(badHost); err == nil {
		t.Error("want an error for a server URL with a space")
	}
	if err := DeleteNamespace(context.Background(), noCluster, "apps"); err == nil {
		t.Error("want an error for a kubeconfig without a cluster")
	}
	if _, err := GetClientsetFromKubeconfigContent(noCluster, true); err == nil {
		t.Error("want an error for a kubeconfig without a cluster")
	}
	if _, err := GetClientsetFromKubeconfigContent(badHost, true); err == nil {
		t.Error("want an error for a server URL with a space")
	}
	if _, err := GetClientsetFromKubeconfigContent("not: [yaml", true); err == nil {
		t.Error("want an error for a kubeconfig that doesn't parse")
	}

	getter := &restClientGetter{config: &rest.Config{Host: "https://bad host"}}
	if _, err := getter.ToDiscoveryClient(); err == nil {
		t.Error("want an error for a bad host")
	}
	if _, err := getter.ToRESTMapper(); err == nil {
		t.Error("want an error for a bad host")
	}
}

func TestHelmFailures(t *testing.T) {
	ctx := context.Background()
	svc := localChart(t)

	// No action config
	helm := newMemoryHelm(t, &ProviderOptions{Quiet: true})
	helm.actionConfigFor = func(string) (*action.Configuration, error) { return nil, errFail }
	if err := helm.Install(ctx, svc); !errors.Is(err, errFail) {
		t.Errorf("install: %v", err)
	}
	if err := helm.Uninstall(ctx, svc); !errors.Is(err, errFail) {
		t.Errorf("uninstall: %v", err)
	}
	if _, err := helm.Status(ctx, svc); !errors.Is(err, errFail) {
		t.Errorf("status: %v", err)
	}
	if _, err := helm.IsInstalled(ctx, svc); !errors.Is(err, errFail) {
		t.Errorf("is installed: %v", err)
	}

	// The cluster refuses the install, then the upgrade
	helm = newMemoryHelm(t, &ProviderOptions{Quiet: true})
	cfg, _ := helm.actionConfig("apps")
	cfg.KubeClient = &kubefake.FailingKubeClient{PrintingKubeClient: kubefake.PrintingKubeClient{Out: io.Discard}, CreateError: errFail}
	if err := helm.Install(ctx, svc); err == nil || !strings.Contains(err.Error(), "failed to install chart") {
		t.Errorf("err = %v", err)
	}
	helm = newMemoryHelm(t, &ProviderOptions{Quiet: true})
	if err := helm.Install(ctx, svc); err != nil {
		t.Fatal(err)
	}
	cfg, _ = helm.actionConfig("apps")
	cfg.KubeClient = &kubefake.FailingKubeClient{PrintingKubeClient: kubefake.PrintingKubeClient{Out: io.Discard}, UpdateError: errFail, CreateError: errFail}
	if err := helm.Install(ctx, svc); err == nil || !strings.Contains(err.Error(), "failed to upgrade chart") {
		t.Errorf("err = %v", err)
	}

	// A local path that isn't a chart
	notChart := *svc
	notChart.Path = t.TempDir()
	if err := newMemoryHelm(t, &ProviderOptions{Quiet: true}).Install(ctx, &notChart); err == nil || !strings.Contains(err.Error(), "failed to load chart") {
		t.Errorf("err = %v", err)
	}

	// A values file that doesn't parse
	bad := filepath.Join(t.TempDir(), "bad.yaml")
	if err := os.WriteFile(bad, []byte("a: [\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadHelmValues(&config.ServiceConfig{Values: valuesFieldOf(t, []string{bad})}); err == nil || !strings.Contains(err.Error(), "failed to parse values file") {
		t.Errorf("err = %v", err)
	}

	// No dynamic client for deleting CRDs
	helm = newMemoryHelm(t, &ProviderOptions{Quiet: true})
	helm.dynamicClient = func() (dynamic.Interface, error) { return nil, errFail }
	if err := helm.deleteCRDs(ctx, []string{"x"}); err == nil || !strings.Contains(err.Error(), "failed to create dynamic client") {
		t.Errorf("err = %v", err)
	}
}

func TestHelmPullFailures(t *testing.T) {
	isolated := newTestHelmProvider(t)
	helm := newMemoryHelm(t, &ProviderOptions{Quiet: true})
	helm.settings = isolated.settings
	server := serveChart(t)
	svc := &config.ServiceConfig{Name: "app", Type: "helm", Repo: server, Chart: "mychart", Version: "1.0.0"}

	// No temp dir can be made
	t.Setenv("TMPDIR", filepath.Join(t.TempDir(), "missing"))
	if _, _, err := helm.pullHTTPChart(svc); err == nil || !strings.Contains(err.Error(), "failed to create temp directory") {
		t.Errorf("err = %v", err)
	}
	t.Setenv("TMPDIR", t.TempDir())

	// Helm's repository file is garbage
	if err := os.MkdirAll(filepath.Dir(helm.settings.RepositoryConfig), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(helm.settings.RepositoryConfig, []byte("repositories: [\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := helm.pullHTTPChart(svc); err == nil || !strings.Contains(err.Error(), "failed to add repository") {
		t.Errorf("err = %v", err)
	}
	if _, err := helm.PullChartToDir(svc, t.TempDir()); err == nil || !strings.Contains(err.Error(), "failed to add repository") {
		t.Errorf("err = %v", err)
	}
	if err := os.Remove(helm.settings.RepositoryConfig); err != nil {
		t.Fatal(err)
	}

	// Its directory can't be created (a file is in the way)
	blocked := newTestHelmProvider(t)
	if err := os.WriteFile(filepath.Dir(blocked.settings.RepositoryConfig), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := blocked.addHTTPRepositoryForChart(server, "mychart", "1.0.0"); err == nil || !strings.Contains(err.Error(), "failed to create repository directory") {
		t.Errorf("err = %v", err)
	}

	// It can't be written
	readOnly := newTestHelmProvider(t)
	dir := filepath.Dir(readOnly.settings.RepositoryConfig)
	if err := os.MkdirAll(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	if _, err := readOnly.addHTTPRepositoryForChart(server, "mychart", "1.0.0"); err == nil || !strings.Contains(err.Error(), "failed to write repository file") {
		t.Errorf("err = %v", err)
	}
}

// The checksum post-renderer refuses a workload whose pod template isn't a
// mapping, rather than dropping the annotation silently
func TestChecksumPostRendererBadTemplate(t *testing.T) {
	rendered := "apiVersion: v1\nkind: ConfigMap\nmetadata: {name: cfg}\ndata: {a: b}\n---\napiVersion: apps/v1\nkind: Deployment\nmetadata: {name: web}\nspec:\n  template: [x]\n---\njust a string\n"
	_, err := configChecksumPostRenderer{}.Run(bytes.NewBufferString(rendered))
	if err == nil || !strings.Contains(err.Error(), "pod template isn't a mapping") {
		t.Errorf("err = %v", err)
	}
}

func TestHelmStorageFailures(t *testing.T) {
	// An unknown storage driver
	t.Setenv("HELM_DRIVER", "carrier-pigeon")
	helm, err := NewHelmProvider(&ProviderOptions{KubeConfig: offlineKubeconfig})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := helm.getActionConfig("apps"); err == nil || !strings.Contains(err.Error(), "failed to initialize Helm action config") {
		t.Errorf("err = %v", err)
	}

	// Releases that can't be listed
	memory := newMemoryHelm(t, &ProviderOptions{Quiet: true})
	cfg, _ := memory.actionConfig("apps")
	cfg.Releases = storage.Init(failingListDriver{driver.NewMemory()})
	if _, err := memory.IsInstalled(context.Background(), localChart(t)); err == nil || !strings.Contains(err.Error(), "failed to list releases") {
		t.Errorf("err = %v", err)
	}
}

// failingListDriver is a release store that can't list
type failingListDriver struct{ *driver.Memory }

func (failingListDriver) List(func(release.Releaser) bool) ([]release.Releaser, error) {
	return nil, errFail
}

// Helm's registry client can't load its credentials file
func TestHelmRegistryClientFailures(t *testing.T) {
	isolated := newTestHelmProvider(t)
	isolated.settings.RegistryConfig = t.TempDir() // a directory, not a file

	helm, err := NewHelmProvider(&ProviderOptions{KubeConfig: offlineKubeconfig})
	if err != nil {
		t.Fatal(err)
	}
	helm.settings = isolated.settings
	if _, err := helm.getActionConfig("apps"); err == nil || !strings.Contains(err.Error(), "failed to create registry client") {
		t.Errorf("err = %v", err)
	}

	server := serveChart(t)
	svc := &config.ServiceConfig{Name: "app", Type: "helm", Repo: server, Chart: "mychart", Version: "1.0.0"}
	if _, err := isolated.pullHTTPChartInto(svc, t.TempDir()); err == nil || !strings.Contains(err.Error(), "failed to create registry client") {
		t.Errorf("err = %v", err)
	}
	if _, err := isolated.PullChartToDir(svc, t.TempDir()); err == nil || !strings.Contains(err.Error(), "failed to create registry client") {
		t.Errorf("err = %v", err)
	}
}

func TestHelmRepositoryEdges(t *testing.T) {
	helm := newTestHelmProvider(t)
	if _, err := helm.addHTTPRepositoryForChart("http://[::1", "c", ""); err == nil || !strings.Contains(err.Error(), "failed to create chart repository") {
		t.Errorf("err = %v", err)
	}
	// A remote chart from neither OCI nor HTTP, without a path, has none
	path, cleanup, err := helm.getChartPath(context.Background(), &config.ServiceConfig{Name: "x", Type: "helm", Repo: "file:///charts", Chart: "c"})
	cleanup()
	if err != nil || path != "" {
		t.Errorf("path = %q, %v", path, err)
	}
}

// A document that's only a comment (a license header) is no object
func TestCommentOnlyManifest(t *testing.T) {
	mp, _ := applyProvider()
	if obj, err := mp.parseManifest("# Copyright 2026\n# Licensed under ..."); obj != nil || err != nil {
		t.Errorf("a comment is no object: %v, %v", obj, err)
	}
}

func TestGetPodEventsReachesOldEvents(t *testing.T) {
	old := &corev1.Event{
		ObjectMeta:     metav1.ObjectMeta{Name: "old", Namespace: "apps"},
		InvolvedObject: corev1.ObjectReference{Kind: "Pod", Name: "p"},
		Reason:         "Old",
		LastTimestamp:  metav1.NewTime(time.Now().Add(-time.Hour)),
	}
	events, err := getPodEvents(context.Background(), fake.NewSimpleClientset(old), "apps", "p", 10)
	if err != nil || len(events) != 0 {
		t.Errorf("events = %v, %v; want the old one skipped", events, err)
	}
}

// Release records Helm can't read: status reports it, the installed check
// skips them
func TestHelmUnreadableReleases(t *testing.T) {
	ctx := context.Background()
	helm := newMemoryHelm(t, &ProviderOptions{Quiet: true})
	svc := localChart(t)
	if err := helm.Install(ctx, svc); err != nil {
		t.Fatal(err)
	}
	orig := release.NewAccessor
	release.NewAccessor = func(release.Releaser) (release.Accessor, error) { return nil, errFail }
	t.Cleanup(func() { release.NewAccessor = orig })

	if _, err := helm.Status(ctx, svc); err == nil || !strings.Contains(err.Error(), "failed to read release status") {
		t.Errorf("err = %v", err)
	}
	if installed, err := helm.IsInstalled(ctx, svc); err != nil || installed {
		t.Errorf("an unreadable release isn't counted: %v, %v", installed, err)
	}
}

// Logs that can't be fetched, or with a line too long to read, show no logs
func TestContainerLogFailures(t *testing.T) {
	ctx := context.Background()
	failing := fake.NewSimpleClientset()
	failing.PrependReactor("get", "pods", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if a.GetSubresource() == "log" {
			return true, nil, errFail
		}
		return false, nil, nil
	})
	if _, err := getContainerLogs(ctx, failing, "apps", "p", "app", 10); err == nil {
		t.Error("want an error when logs can't be fetched")
	}

	long := fake.NewSimpleClientset()
	long.PrependReactor("get", "pods", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if a.GetSubresource() == "log" {
			return true, &runtime.Unknown{Raw: []byte(strings.Repeat("x", 70*1024) + "\n")}, nil
		}
		return false, nil, nil
	})
	if _, err := getContainerLogs(ctx, long, "apps", "p", "app", 10); err == nil {
		t.Error("want an error for a log line longer than the scanner reads")
	}
}

// A "---" followed by anything but a comment isn't a separator kubectl
// accepts: every reader of manifests reports it
func TestBadDocumentSeparator(t *testing.T) {
	const bad = "apiVersion: v1\nkind: ConfigMap\nmetadata: {name: a}\n--- not a comment\nkind: ConfigMap\n"
	ctx := context.Background()
	mp, _ := applyProvider()

	if _, err := mp.loadManifests(writeManifest(t, bad)); err == nil || !strings.Contains(err.Error(), "failed to split") {
		t.Errorf("file: err = %v", err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(bad)) }))
	defer server.Close()
	if _, err := mp.loadManifests(&config.ServiceConfig{Name: "x", Path: server.URL + "/a.yaml"}); err == nil || !strings.Contains(err.Error(), "failed to split manifest from") {
		t.Errorf("URL: err = %v", err)
	}
	if _, err := parseManifestsYAML(bad); err == nil || !strings.Contains(err.Error(), "invalid Yaml document separator") {
		t.Errorf("parse: err = %v", err)
	}
	if err := waitForManifests(ctx, waitTestClients(), bad, "apps", &ProviderOptions{Wait: true, Quiet: true}); err == nil || !strings.Contains(err.Error(), "failed to parse manifests") {
		t.Errorf("wait: err = %v", err)
	}

	// A release manifest whose CRDs can't be found deletes none
	helm := &HelmProvider{opts: &ProviderOptions{Verbose: true}}
	var crds []string
	out := captureOutput(t, func() { crds = helm.extractCRDsFromManifest(bad) })
	if crds != nil || !strings.Contains(out, "Failed to parse the release manifest for CRDs") {
		t.Errorf("CRDs = %v, output:\n%s", crds, out)
	}
}

// What Helm leaves after a pull: the untarred chart's directory, or a .tgz
func TestFindPulledChart(t *testing.T) {
	dir := func(names ...string) string {
		d := t.TempDir()
		for _, name := range names {
			var err error
			if strings.HasSuffix(name, "/") {
				err = os.Mkdir(filepath.Join(d, name), 0o755)
			} else {
				err = os.WriteFile(filepath.Join(d, name), nil, 0o644)
			}
			if err != nil {
				t.Fatal(err)
			}
		}
		return d
	}
	for _, tt := range []struct {
		name, want, wantErr string
		dir                 string
	}{
		{name: "untarred", dir: dir("README", "mychart/"), want: "mychart"},
		{name: "tgz", dir: dir("README", "mychart-1.0.0.tgz"), want: "mychart-1.0.0.tgz"},
		{name: "empty", dir: dir(), wantErr: "no chart found"},
		{name: "no chart", dir: dir("README"), wantErr: "could not find chart"},
		{name: "missing", dir: filepath.Join(t.TempDir(), "missing"), wantErr: "failed to read temp directory"},
	} {
		got, err := findPulledChart(tt.dir)
		if tt.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("%s: err = %v, want %q", tt.name, err, tt.wantErr)
			}
		} else if err != nil || got != filepath.Join(tt.dir, tt.want) {
			t.Errorf("%s: got %q, %v; want %q", tt.name, got, err, tt.want)
		}
	}

	if _, err := findPulledTgz(dir("mychart/"), "repo/mychart"); err == nil || !strings.Contains(err.Error(), `no .tgz found`) {
		t.Errorf("no .tgz: err = %v", err)
	}
	if _, err := findPulledTgz(filepath.Join(t.TempDir(), "missing"), "repo/mychart"); err == nil || !strings.Contains(err.Error(), "failed to read") {
		t.Errorf("missing dir: err = %v", err)
	}
}
