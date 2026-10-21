package providers

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hjames9/kraze/internal/config"
	"helm.sh/helm/v4/pkg/action"
	"helm.sh/helm/v4/pkg/chart/common"
	v2chart "helm.sh/helm/v4/pkg/chart/v2"
	chartutil "helm.sh/helm/v4/pkg/chart/v2/util"
	"helm.sh/helm/v4/pkg/cli"
	kubefake "helm.sh/helm/v4/pkg/kube/fake"
	"helm.sh/helm/v4/pkg/storage"
	"helm.sh/helm/v4/pkg/storage/driver"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

// memoryHelm is a HelmProvider whose releases live in memory, one store per
// namespace as Helm keeps them in the cluster, and whose CRD deletions go to
// a fake dynamic client
type memoryHelm struct {
	*HelmProvider
	dynamic *dynamicfake.FakeDynamicClient
}

func newMemoryHelm(t *testing.T, opts *ProviderOptions) *memoryHelm {
	t.Helper()
	configs := map[string]*action.Configuration{}
	dyn := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme())
	helm := &HelmProvider{
		opts:     opts,
		settings: cli.New(),
		actionConfigFor: func(namespace string) (*action.Configuration, error) {
			if cfg, ok := configs[namespace]; ok {
				return cfg, nil
			}
			store := driver.NewMemory()
			store.SetNamespace(namespace)
			cfg := &action.Configuration{
				Releases:     storage.Init(store),
				KubeClient:   &kubefake.PrintingKubeClient{Out: io.Discard},
				Capabilities: common.DefaultCapabilities,
			}
			configs[namespace] = cfg
			return cfg, nil
		},
		dynamicClient: func() (dynamic.Interface, error) { return dyn, nil },
	}
	return &memoryHelm{HelmProvider: helm, dynamic: dyn}
}

// localChart saves a chart with a ConfigMap, a Deployment and a CRD to a
// directory, and returns a service installing it
func localChart(t *testing.T) *config.ServiceConfig {
	t.Helper()
	template := func(name, data string) *common.File {
		return &common.File{Name: "templates/" + name, ModTime: time.Now(), Data: []byte(data)}
	}
	chart := &v2chart.Chart{
		Metadata: &v2chart.Metadata{APIVersion: "v2", Name: "app", Version: "1.0.0"},
		Values:   map[string]interface{}{"mode": "dev"},
		Templates: []*common.File{
			template("config.yaml", "apiVersion: v1\nkind: ConfigMap\nmetadata: {name: cfg}\ndata: {mode: {{ .Values.mode }}}\n"),
			template("deployment.yaml", "apiVersion: apps/v1\nkind: Deployment\nmetadata: {name: web}\nspec:\n  replicas: 1\n"),
			template("crd.yaml", "apiVersion: apiextensions.k8s.io/v1\nkind: CustomResourceDefinition\nmetadata: {name: gadgets.example.com}\n"),
		},
	}
	dir := t.TempDir()
	if err := chartutil.SaveDir(chart, dir); err != nil {
		t.Fatal(err)
	}
	return &config.ServiceConfig{Name: "app", Type: "helm", Namespace: "apps", Path: filepath.Join(dir, "app")}
}

// revision returns the release's latest revision, 0 if there is none
func (m *memoryHelm) revision(t *testing.T, svc *config.ServiceConfig) int {
	t.Helper()
	cfg, _ := m.actionConfig(svc.GetNamespace())
	history := action.NewHistory(cfg)
	releases, err := history.Run(svc.Name)
	if err != nil {
		return 0
	}
	return len(releases)
}

// deletedCRDs returns the CRDs deleted through the dynamic client
func (m *memoryHelm) deletedCRDs() []string {
	var names []string
	for _, a := range m.dynamic.Actions() {
		if del, ok := a.(k8stesting.DeleteAction); ok && del.GetResource() == crdGVR {
			names = append(names, del.GetName())
		}
	}
	return names
}

func TestHelmInstallThenUpgrade(t *testing.T) {
	helm := newMemoryHelm(t, &ProviderOptions{Quiet: true})
	svc := localChart(t)
	ctx := context.Background()

	if installed, err := helm.IsInstalled(ctx, svc); err != nil || installed {
		t.Fatalf("before install: installed=%v err=%v", installed, err)
	}
	status, err := helm.Status(ctx, svc)
	if err != nil || status.Installed || !strings.HasPrefix(status.Message, "Not installed") {
		t.Errorf("status before install = %+v, %v", status, err)
	}

	if err := helm.Install(ctx, svc); err != nil {
		t.Fatalf("install: %v", err)
	}
	if installed, err := helm.IsInstalled(ctx, svc); err != nil || !installed {
		t.Errorf("after install: installed=%v err=%v", installed, err)
	}
	helm.waitClients = func() (*kubeClients, error) { return waitTestClients(webDeployment(true)), nil }
	status, err = helm.Status(ctx, svc)
	if err != nil || !status.Installed || !status.Ready || status.Message != "deployed" {
		t.Errorf("status after install = %+v, %v", status, err)
	}
	if got := helm.revision(t, svc); got != 1 {
		t.Errorf("revision = %d, want 1", got)
	}

	// Installing again upgrades the release
	if err := helm.Install(ctx, svc); err != nil {
		t.Fatalf("second install: %v", err)
	}
	if got := helm.revision(t, svc); got != 2 {
		t.Errorf("revision after second install = %d, want an upgrade to 2", got)
	}

	// Another release name in the namespace isn't this one
	other := *svc
	other.Name = "ap"
	if installed, _ := helm.IsInstalled(ctx, &other); installed {
		t.Error("IsInstalled must match the release name exactly, not as a filter prefix")
	}
}

func TestHelmUninstallDeletesCRDs(t *testing.T) {
	keep := true
	for name, tc := range map[string]struct {
		flag    bool  // --keep-crds
		service *bool // keep_crds
		deleted []string
	}{
		"default deletes":   {deleted: []string{"gadgets.example.com"}},
		"--keep-crds keeps": {flag: true},
		"keep_crds keeps":   {service: &keep},
		"--keep-crds wins":  {flag: true, service: new(bool)},
	} {
		t.Run(name, func(t *testing.T) {
			helm := newMemoryHelm(t, &ProviderOptions{Quiet: true, KeepCRDs: tc.flag})
			svc := localChart(t)
			svc.KeepCRDs = tc.service
			ctx := context.Background()
			if err := helm.Install(ctx, svc); err != nil {
				t.Fatal(err)
			}

			if err := helm.Uninstall(ctx, svc); err != nil {
				t.Fatalf("uninstall: %v", err)
			}
			if installed, _ := helm.IsInstalled(ctx, svc); installed {
				t.Error("the release should be gone")
			}
			if got := helm.deletedCRDs(); !slices.Equal(got, tc.deleted) {
				t.Errorf("deleted CRDs = %v, want %v", got, tc.deleted)
			}
		})
	}
}

func TestHelmUninstallMissingRelease(t *testing.T) {
	helm := newMemoryHelm(t, &ProviderOptions{Quiet: true})
	err := helm.Uninstall(context.Background(), localChart(t))
	if !IsReleaseNotFound(err) {
		t.Errorf("err = %v, want a release-not-found error", err)
	}
}

func TestHelmDeleteCRDsErrors(t *testing.T) {
	helm := newMemoryHelm(t, &ProviderOptions{Quiet: true})
	helm.dynamic.PrependReactor("delete", "customresourcedefinitions", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if a.(k8stesting.DeleteAction).GetName() == "broken.example.com" {
			return true, nil, errors.New("forbidden")
		}
		return false, nil, nil // the tracker reports the others as not found
	})

	err := helm.deleteCRDs(context.Background(), []string{"gone.example.com", "broken.example.com"})
	if err == nil || !strings.Contains(err.Error(), "broken.example.com: forbidden") || strings.Contains(err.Error(), "gone.example.com") {
		t.Errorf("err = %v, want only the CRD that failed to delete (missing ones are fine)", err)
	}
	if err := helm.deleteCRDs(context.Background(), nil); err != nil {
		t.Errorf("no CRDs: %v", err)
	}
}

func TestHelmInstallWaitsForWorkloads(t *testing.T) {
	setVarDuration(t, &readinessPollInterval, time.Millisecond)
	ready := workload("Deployment", 1, map[string]interface{}{"replicas": int64(1)}, map[string]interface{}{
		"observedGeneration": int64(1), "replicas": int64(1), "updatedReplicas": int64(1), "availableReplicas": int64(1), "readyReplicas": int64(1),
	})
	ready.SetName("web")
	ready.SetNamespace("apps")

	helm := newMemoryHelm(t, &ProviderOptions{Quiet: true, Wait: true, Timeout: "5s"})
	var asked []string
	helm.waitClients = func() (*kubeClients, error) {
		clients := waitTestClients(ready)
		clients.dynamic.(*dynamicfake.FakeDynamicClient).PrependReactor("get", "*", func(a k8stesting.Action) (bool, runtime.Object, error) {
			get := a.(k8stesting.GetAction)
			asked = append(asked, get.GetResource().Resource+"/"+get.GetNamespace()+"/"+get.GetName())
			return false, nil, nil
		})
		return clients, nil
	}

	if err := helm.Install(context.Background(), localChart(t)); err != nil {
		t.Fatalf("install: %v", err)
	}
	// Only the Deployment is waited for, in the release's namespace
	if !slices.Equal(asked, []string{"deployments/apps/web"}) {
		t.Errorf("waited for %v", asked)
	}

	helm.waitClients = func() (*kubeClients, error) { return nil, errors.New("unreachable") }
	if err := helm.Install(context.Background(), localChart(t)); err == nil || !strings.Contains(err.Error(), "failed waiting for resources: unreachable") {
		t.Errorf("err = %v", err)
	}
}

// waitTestClients are fake clients holding objs, with a mapper for the kinds
// the tests use
func waitTestClients(objs ...runtime.Object) *kubeClients {
	mapper := meta.NewDefaultRESTMapper(nil)
	mapper.Add(schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "Deployment"}, meta.RESTScopeNamespace)
	mapper.Add(schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"}, meta.RESTScopeNamespace)
	mapper.Add(schema.GroupVersionKind{Group: "apiextensions.k8s.io", Version: "v1", Kind: "CustomResourceDefinition"}, meta.RESTScopeRoot)
	return &kubeClients{
		dynamic:   dynamicfake.NewSimpleDynamicClient(runtime.NewScheme(), objs...),
		clientset: fake.NewSimpleClientset(),
		mapper:    mapper,
	}
}

func TestWaitForManifests(t *testing.T) {
	setVarDuration(t, &readinessPollInterval, time.Millisecond)
	notReady := workload("Deployment", 2, map[string]interface{}{"replicas": int64(1)}, map[string]interface{}{"observedGeneration": int64(1)})
	notReady.SetName("web")
	notReady.SetNamespace("apps")
	manifest := "apiVersion: v1\nkind: ConfigMap\nmetadata: {name: cfg}\n---\napiVersion: apps/v1\nkind: Deployment\nmetadata: {name: web}\n"
	ctx := context.Background()

	if err := waitForManifests(ctx, waitTestClients(), manifest, "apps", &ProviderOptions{Wait: false}); err != nil {
		t.Errorf("no wait: %v", err)
	}
	if err := waitForManifests(ctx, waitTestClients(), "", "apps", &ProviderOptions{Wait: true, Quiet: true}); err != nil {
		t.Errorf("nothing to wait for: %v", err)
	}
	err := waitForManifests(ctx, waitTestClients(notReady), manifest, "apps", &ProviderOptions{Wait: true, Quiet: true, Timeout: "50ms"})
	if err == nil || !strings.Contains(err.Error(), "timeout waiting for Deployment/web to be ready") {
		t.Errorf("err = %v, want a timeout naming the Deployment", err)
	}
}

func TestWaitForManifestsInNamespaceNeedsKubeconfig(t *testing.T) {
	if err := WaitForManifestsInNamespace(context.Background(), "", "x", "", &ProviderOptions{Wait: true}); err == nil {
		t.Error("want an error for an empty kubeconfig")
	}
	if err := WaitForManifestsInNamespace(context.Background(), "", "x", "", &ProviderOptions{}); err != nil {
		t.Errorf("without wait nothing is needed, got %v", err)
	}
}

func TestExtractCRDsFromManifest(t *testing.T) {
	helm := &HelmProvider{opts: &ProviderOptions{}}
	manifest := `---
# Source: app/templates/crds.yaml
apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  annotations:
    controller-gen.kubebuilder.io/version: v0.14.0
  labels:
    name: not-the-name
  name: certificates.cert-manager.io
---
apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata: {name: "issuers.cert-manager.io"}
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: mentions-crds
data:
  note: "kind: CustomResourceDefinition"
---
apiVersion: example.com/v1
kind: CustomResourceDefinition
metadata:
  name: not-a-real-crd
`
	got := helm.extractCRDsFromManifest(manifest)
	want := []string{"certificates.cert-manager.io", "issuers.cert-manager.io"}
	if !slices.Equal(got, want) {
		t.Errorf("CRDs = %v, want %v", got, want)
	}
	if got := helm.extractCRDsFromManifest(""); len(got) != 0 {
		t.Errorf("empty manifest: %v", got)
	}
}

// setVarDuration sets *p to v for the rest of the test
func setVarDuration(t *testing.T, p *time.Duration, v time.Duration) {
	t.Helper()
	orig := *p
	*p = v
	t.Cleanup(func() { *p = orig })
}

// A chart from an HTTP repository is pulled, then installed with its values
func TestHelmInstallFromHTTPRepository(t *testing.T) {
	isolated := newTestHelmProvider(t) // Helm's config and cache in a temp dir
	helm := newMemoryHelm(t, &ProviderOptions{Quiet: true, Verbose: true})
	helm.settings = isolated.settings

	valuesFile := filepath.Join(t.TempDir(), "values.yaml")
	if err := os.WriteFile(valuesFile, []byte("replicas: 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	svc := &config.ServiceConfig{Name: "app", Type: "helm", Namespace: "apps", Repo: serveChart(t), Chart: "mychart", Version: "1.0.0",
		Values: valuesFieldOf(t, []string{valuesFile})}

	if err := helm.Install(context.Background(), svc); err != nil {
		t.Fatalf("install: %v", err)
	}
	cfg, _ := helm.actionConfig("apps")
	values, err := action.NewGetValues(cfg).Run("app")
	if err != nil || fmt.Sprint(values["replicas"]) != "2" {
		t.Errorf("release values = %v, %v", values, err)
	}

	missing := *svc
	missing.Chart = "nochart"
	if err := helm.Install(context.Background(), &missing); err == nil || !strings.Contains(err.Error(), "failed to get chart") {
		t.Errorf("err = %v", err)
	}
	badValues := *svc
	badValues.Values = valuesFieldOf(t, []string{filepath.Join(t.TempDir(), "missing.yaml")})
	if err := helm.Install(context.Background(), &badValues); err == nil || !strings.Contains(err.Error(), "failed to load values") {
		t.Errorf("err = %v", err)
	}
}

// Each pull from an HTTP repository went into a temp dir that was never removed
func TestHelmHTTPChartTempDirRemoved(t *testing.T) {
	isolated := newTestHelmProvider(t)
	helm := newMemoryHelm(t, &ProviderOptions{Quiet: true})
	helm.settings = isolated.settings
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	leftovers := func() []string {
		matches, _ := filepath.Glob(filepath.Join(tmp, "kraze-helm-*"))
		return matches
	}
	svc := &config.ServiceConfig{Name: "app", Type: "helm", Namespace: "apps", Repo: serveChart(t), Chart: "mychart", Version: "1.0.0"}

	if err := helm.Install(context.Background(), svc); err != nil {
		t.Fatal(err)
	}
	if got := leftovers(); len(got) != 0 {
		t.Errorf("after install, left %v", got)
	}

	missing := *svc
	missing.Chart = "nochart"
	if err := helm.Install(context.Background(), &missing); err == nil {
		t.Fatal("want an error for a chart the repository doesn't have")
	}
	if got := leftovers(); len(got) != 0 {
		t.Errorf("after a failed pull, left %v", got)
	}
}

func TestGetChartPathNeedsAChart(t *testing.T) {
	helm := newMemoryHelm(t, &ProviderOptions{Quiet: true})
	if _, _, err := helm.getChartPath(context.Background(), &config.ServiceConfig{Name: "x", Type: "helm"}); err == nil {
		t.Error("want an error for a service with neither path nor repo")
	}
}
