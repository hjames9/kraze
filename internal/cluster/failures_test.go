package cluster

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hjames9/kraze/internal/config"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	k8stesting "k8s.io/client-go/testing"
	"sigs.k8s.io/kind/pkg/cluster/images/layeraware"
)

var errFail = errors.New("forbidden")

func TestKindManagerFailures(test *testing.T) {
	ctx := context.Background()
	nvidia := &config.ClusterConfig{Name: "c", GPU: &config.GPUConfig{Nvidia: &config.GPUVendorConfig{Enabled: true}}}
	external := func(path string) *config.ClusterConfig {
		return &config.ClusterConfig{Name: "c", External: &config.ExternalClusterConfig{Enabled: true, Kubeconfig: path}}
	}
	tests := []struct {
		name  string
		setup func(tk *testKind)
		run   func(tk *testKind) error
		want  string
	}{
		{"create: cluster check", func(tk *testKind) { tk.provider.listErr = errFail },
			func(tk *testKind) error { return tk.CreateCluster(ctx, &config.ClusterConfig{Name: "c"}) }, "failed to check if cluster exists"},
		{"create: containerd can't be configured", nil,
			func(tk *testKind) error { return tk.CreateCluster(ctx, &config.ClusterConfig{Name: "c"}) }, "failed to configure containerd"},
		{"GODEBUG: no nodes listed", func(tk *testKind) { tk.provider.nodesErr = errFail },
			func(tk *testKind) error { return tk.configureContainerdGODEBUG("c") }, "failed to list cluster nodes"},
		{"GODEBUG: drop-in can't be written", nil,
			func(tk *testKind) error { return tk.configureContainerdGODEBUG("c") }, "failed to write GODEBUG config in node c-control-plane"},
		{"GODEBUG: systemd doesn't reload", func(tk *testKind) { tk.run.on("docker exec -i c-control-plane sh -c mkdir", "", nil) },
			func(tk *testKind) error { return tk.configureContainerdGODEBUG("c") }, "failed to reload systemd in node c-control-plane"},
		{"GODEBUG: containerd doesn't restart", func(tk *testKind) {
			tk.run.on("docker exec -i c-control-plane sh -c mkdir", "", nil)
			tk.run.on("docker exec -i c-control-plane sh -c systemctl daemon-reload", "", nil)
		}, func(tk *testKind) error { return tk.configureContainerdGODEBUG("c") }, "containerd did not restart in node c-control-plane"},
		{"delete: cluster check", func(tk *testKind) { tk.provider.listErr = errFail },
			func(tk *testKind) error { return tk.DeleteCluster("c") }, "failed to check if cluster exists"},
		{"delete: kind fails", func(tk *testKind) { tk.provider.clusters = []string{"c"}; tk.provider.deleteErr = errFail },
			func(tk *testKind) error { return tk.DeleteCluster("c") }, "failed to delete cluster"},
		{"kubeconfig: kind fails", func(tk *testKind) { tk.provider.kubeErr = errFail },
			func(tk *testKind) error { _, err := tk.GetKubeConfig("c", false); return err }, "forbidden"},
		{"update kubeconfig: kind fails", func(tk *testKind) { tk.provider.kubeErr = errFail },
			func(tk *testKind) error { return tk.UpdateKubeconfigFile("c") }, "failed to get kubeconfig"},
		{"update kubeconfig: can't write", func(tk *testKind) {
			tk.provider.kubeconfig = kindKubeconfig
			tk.KindManager.host.kubeconfigPath = filepath.Join(tk.host.home, "file", "config")
			_ = os.WriteFile(filepath.Join(tk.host.home, "file"), nil, 0o644)
		}, func(tk *testKind) error { return tk.UpdateKubeconfigFile("c") }, "failed to write kubeconfig"},
		{"load image: no nodes listed", func(tk *testKind) { tk.provider.nodesErr = errFail },
			func(tk *testKind) error { return tk.LoadImage(ctx, "c", "a:1") }, "failed to list cluster nodes"},
		{"load image: loading fails", func(tk *testKind) {
			tk.run.on("docker save", "", nil)
			tk.inspectErr, tk.loadErr = errFail, errFail
		}, func(tk *testKind) error { return tk.LoadImage(ctx, "c", "a:1") }, "failed to load image 'a:1'"},
		{"cgroup dirs: no nodes listed", func(tk *testKind) { tk.provider.nodesErr = errFail },
			func(tk *testKind) error { return tk.ensureKubeletCgroupDirectories("c") }, "failed to list cluster nodes"},
		{"CA certificates: no nodes listed", func(tk *testKind) { tk.provider.nodesErr = errFail },
			func(tk *testKind) error { return tk.updateCACertificates(ctx, "c", nil) }, "failed to list cluster nodes"},
		{"CA certificates: certificate can't be read", nil,
			func(tk *testKind) error { return tk.updateCACertificates(ctx, "c", []string{"/missing.crt"}) }, "failed to read CA certificate"},
		{"CA certificates: certificate can't be written", func(tk *testKind) { tk.host.files["/c.crt"] = "cert" },
			func(tk *testKind) error { return tk.updateCACertificates(ctx, "c", []string{"/c.crt"}) }, "failed to write CA certificate in node c-control-plane"},
		{"registries: no nodes listed", func(tk *testKind) { tk.provider.nodesErr = errFail },
			func(tk *testKind) error { return tk.configureInsecureRegistries("c", []string{"r"}) }, "failed to list cluster nodes"},
		{"registries: config can't be patched", func(tk *testKind) { tk.run.on("docker exec -i", "unsupported", errNotFaked) },
			func(tk *testKind) error { return tk.configureInsecureRegistries("c", []string{"r"}) }, "failed to patch containerd config"},
		{"registries: hosts.toml can't be written", func(tk *testKind) {
			tk.run.on("docker exec -i c-control-plane sh -c mkdir", "read-only", errNotFaked)
			tk.run.on("docker exec -i", "", nil)
		}, func(tk *testKind) error { return tk.configureInsecureRegistries("c", []string{"r"}) }, "failed to write hosts.toml for r"},
		{"proxy: no nodes listed", func(tk *testKind) { tk.provider.nodesErr = errFail },
			func(tk *testKind) error { return tk.configureProxy("c", "http://p:1", "", "") }, "failed to list cluster nodes"},
		{"proxy: systemd won't reload", func(tk *testKind) {
			tk.run.on("docker exec -i c-control-plane sh -c systemctl daemon-reload", "failed", errNotFaked)
			tk.run.on("docker exec -i", "", nil)
		}, func(tk *testKind) error { return tk.configureProxy("c", "http://p:1", "", "") }, "failed to reload systemd"},
		{"proxy: containerd won't restart", func(tk *testKind) {
			tk.run.on("docker exec -i c-control-plane sh -c systemctl restart", "failed", errNotFaked)
			tk.run.on("docker exec -i", "", nil)
		}, func(tk *testKind) error { return tk.configureProxy("c", "http://p:1", "", "") }, "containerd did not restart"},
		{"AMD: /dev/kfd unreadable", func(tk *testKind) { tk.host.statErr = map[string]error{"/dev/kfd": fs.ErrPermission} },
			func(tk *testKind) error { return tk.validateAMDGPUPrerequisites() }, "failed to stat /dev/kfd"},
		{"AMD: render node unreadable", func(tk *testKind) {
			tk.host.files["/dev/kfd"] = ""
			tk.host.statErr = map[string]error{"/dev/dri/renderD128": fs.ErrPermission}
		}, func(tk *testKind) error { return tk.validateAMDGPUPrerequisites() }, "failed to stat /dev/dri/renderD128"},
		{"runtime class: kind fails", func(tk *testKind) { tk.provider.kubeErr = errFail },
			func(tk *testKind) error { return tk.registerNvidiaRuntimeClass(ctx, "c") }, "failed to get kubeconfig"},
		{"runtime class: no dynamic client", func(tk *testKind) {
			tk.provider.kubeconfig = kindKubeconfig
			tk.clients.dynamic = func(*rest.Config) (dynamic.Interface, error) { return nil, errFail }
		}, func(tk *testKind) error { return tk.registerNvidiaRuntimeClass(ctx, "c") }, "failed to create dynamic client"},
		{"runtime class: can't be read", func(tk *testKind) {
			tk.provider.kubeconfig = kindKubeconfig
			tk.run.on("docker exec -i", "", nil) // containerd is configured
			tk.dynamic.PrependReactor("get", "runtimeclasses", func(k8stesting.Action) (bool, runtime.Object, error) { return true, nil, errFail })
		}, func(tk *testKind) error { return tk.registerNvidiaRuntimeClass(ctx, "c") }, "failed to check for existing RuntimeClass"},
		{"runtime class: can't be created", func(tk *testKind) {
			tk.provider.kubeconfig = kindKubeconfig
			tk.dynamic.PrependReactor("create", "runtimeclasses", func(k8stesting.Action) (bool, runtime.Object, error) { return true, nil, errFail })
		}, func(tk *testKind) error { return tk.registerNvidiaRuntimeClass(ctx, "c") }, "failed to create NVIDIA RuntimeClass"},
		{"runtime class: can't be updated", func(tk *testKind) {
			tk.provider.kubeconfig = kindKubeconfig
			_ = tk.registerNvidiaRuntimeClass(ctx, "c")
			tk.dynamic.PrependReactor("update", "runtimeclasses", func(k8stesting.Action) (bool, runtime.Object, error) { return true, nil, errFail })
		}, func(tk *testKind) error { return tk.registerNvidiaRuntimeClass(ctx, "c") }, "failed to update NVIDIA RuntimeClass"},
		{"default route: gateway unknown", nil,
			func(tk *testKind) error { return tk.fixDefaultRoutes("dev-net", []string{"c-control-plane"}, false) }, "inspect network dev-net"},
		{"external: no home for the default kubeconfig", func(tk *testKind) { tk.host.homeErr = errFail },
			func(tk *testKind) error { _, err := tk.GetKubeconfigForExternalCluster(external("")); return err }, "failed to get user home directory"},
		{"external: no home for ~", func(tk *testKind) { tk.host.homeErr = errFail },
			func(tk *testKind) error { _, err := tk.GetKubeconfigForExternalCluster(external("~/k")); return err }, "failed to get user home directory"},
		{"external: kubeconfig unreadable", func(tk *testKind) {
			tk.host.files["/k"] = ""
			tk.host.readErr = map[string]error{"/k": fs.ErrPermission}
		}, func(tk *testKind) error { _, err := tk.GetKubeconfigForExternalCluster(external("/k")); return err }, "failed to read kubeconfig file"},
		{"verify: no cluster in the kubeconfig", nil,
			func(tk *testKind) error {
				return tk.VerifyClusterAccess(ctx, "apiVersion: v1\nkind: Config\ncurrent-context: none\n")
			}, "failed to create REST config"},
		{"verify: no client", func(tk *testKind) {
			tk.clients.clientset = func(*rest.Config) (kubernetes.Interface, error) { return nil, errFail }
		}, func(tk *testKind) error { return tk.VerifyClusterAccess(ctx, kindKubeconfig) }, "failed to create Kubernetes client"},
		{"verify: unreachable", func(tk *testKind) {
			tk.clients.clientset = func(*rest.Config) (kubernetes.Interface, error) {
				cs := fake.NewSimpleClientset()
				cs.PrependReactor("get", "version", func(k8stesting.Action) (bool, runtime.Object, error) { return true, nil, errFail })
				return cs, nil
			}
		}, func(tk *testKind) error { return tk.VerifyClusterAccess(ctx, kindKubeconfig) }, "failed to connect to cluster"},
		{"create: NVIDIA runtime class can't be registered after creation", func(tk *testKind) {
			tk.host.bins["nvidia-container-runtime"] = "/usr/bin/nvidia-container-runtime"
			tk.host.bins["nvidia-ctk"] = "/usr/bin/nvidia-ctk"
			tk.provider.kubeErr = nil
			tk.provider.kubeconfig = kindKubeconfig
			tk.run.on("docker exec -i", "", nil) // containerd is configured
			tk.dynamic.PrependReactor("get", "runtimeclasses", func(k8stesting.Action) (bool, runtime.Object, error) { return true, nil, errFail })
		}, func(tk *testKind) error {
			var err error
			captureStdout(test, func() { err = tk.CreateCluster(ctx, nvidia) })
			return err
		}, "failed to register NVIDIA RuntimeClass"},
	}
	for _, tt := range tests {
		test.Run(tt.name, func(test *testing.T) {
			tk := newTestKind(test)
			if tt.setup != nil {
				tt.setup(tk)
			}
			var err error
			captureStdout(test, func() { err = tt.run(tk) })
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				test.Errorf("err = %v, want %q", err, tt.want)
			}
		})
	}
}

// Callers of GetKubeConfigQuiet add "failed to get kubeconfig"; it used to be
// added by both, so the message said it twice
func TestKubeconfigErrorNotRepeated(test *testing.T) {
	ctx := context.Background()
	for name, run := range map[string]func(tk *testKind) error{
		"update kubeconfig": func(tk *testKind) error { return tk.UpdateKubeconfigFile("c") },
		"runtime class":     func(tk *testKind) error { return tk.registerNvidiaRuntimeClass(ctx, "c") },
	} {
		tk := newTestKind(test)
		tk.provider.kubeErr = errFail
		var err error
		captureStdout(test, func() { err = run(tk) })
		if err == nil || strings.Count(err.Error(), "failed to get kubeconfig") != 1 {
			test.Errorf("%s: err = %v, want \"failed to get kubeconfig\" once", name, err)
		}
	}
}

func TestLoadImageNoTempDir(test *testing.T) {
	tk := newTestKind(test)
	test.Setenv("TMPDIR", filepath.Join(test.TempDir(), "missing"))
	if err := tk.LoadImage(context.Background(), "c", "a:1"); err == nil || !strings.Contains(err.Error(), "failed to create temp dir") {
		test.Errorf("err = %v", err)
	}
}

// A connected network whose route can't be fixed is only a warning
func TestConnectRouteWarning(test *testing.T) {
	tk := newTestKind(test)
	tk.run.on("docker network inspect dev-net --format", "", errNotFaked)
	tk.run.on("docker network inspect dev-net", "[]", nil)
	tk.run.on("docker network connect", "", nil)
	var err error
	out := captureStdout(test, func() { err = tk.connectToHostNetwork("c", "dev-net", "", "") })
	if err != nil || !strings.Contains(out, "Could not update default route via 'dev-net'") {
		test.Errorf("err = %v, output:\n%s", err, out)
	}
}

func TestImageExtractionFailures(test *testing.T) {
	ctx := context.Background()
	im, _ := newTestImages()
	im.verbose = true

	// No temp dir for a remote chart
	test.Setenv("TMPDIR", filepath.Join(test.TempDir(), "missing"))
	if _, err := im.renderedHelmImages(ctx, &config.ServiceConfig{Name: "a", Type: "helm", Repo: "oci://r", Chart: "c"}); err == nil || !strings.Contains(err.Error(), "failed to create temp dir") {
		test.Errorf("err = %v", err)
	}
	test.Setenv("TMPDIR", test.TempDir())

	dir := test.TempDir()
	write := func(name, content string, mode os.FileMode) string {
		path := filepath.Join(dir, name)
		_ = os.MkdirAll(filepath.Dir(path), 0o755)
		if err := os.WriteFile(path, []byte(content), mode); err != nil {
			test.Fatal(err)
		}
		return path
	}
	write("bad/Chart.yaml", "apiVersion: v2\nname: bad\nversion: 0.1.0\n", 0o644)
	write("bad/templates/x.yaml", "{{ .Values.missing.field }}\n", 0o644)
	if _, err := im.renderedHelmImages(ctx, &config.ServiceConfig{Name: "a", Type: "helm", Path: filepath.Join(dir, "bad")}); err == nil || !strings.Contains(err.Error(), "failed to render chart templates") {
		test.Errorf("err = %v", err)
	}
	svc := &config.ServiceConfig{Name: "a", Type: "helm", Path: filepath.Join(dir, "bad"), ValuesInline: "a: [\n"}
	if _, err := im.renderedHelmImages(ctx, svc); err == nil || !strings.Contains(err.Error(), "failed to parse inline values") {
		test.Errorf("err = %v", err)
	}
	// Values the chart's schema rejects
	write("schema/Chart.yaml", "apiVersion: v2\nname: schema\nversion: 0.1.0\n", 0o644)
	write("schema/values.schema.json", `{"type": "object", "properties": {"replicas": {"type": "integer"}}}`, 0o644)
	svc = &config.ServiceConfig{Name: "a", Type: "helm", Path: filepath.Join(dir, "schema"), ValuesInline: "replicas: many\n"}
	if _, err := im.renderedHelmImages(ctx, svc); err == nil || !strings.Contains(err.Error(), "failed to prepare values") {
		test.Errorf("err = %v", err)
	}

	// Default values that can't be read
	write("locked/Chart.yaml", "apiVersion: v2\nname: locked\nversion: 0.1.0\n", 0o644)
	write("locked/values.yaml", "image: x\n", 0o000)
	out := captureStdout(test, func() {
		_ = im.extractImagesFromLocalChart(&config.ServiceConfig{Name: "a", Type: "helm", Path: filepath.Join(dir, "locked")})
	})
	if !strings.Contains(out, "Failed to read default values") {
		test.Errorf("output:\n%s", out)
	}

	// Manifests that can't be found or read
	if _, err := im.ExtractImagesFromManifests([]string{filepath.Join(dir, "missing.yaml")}); err == nil || !strings.Contains(err.Error(), "failed to stat") {
		test.Errorf("err = %v", err)
	}
	lockedDir := filepath.Join(dir, "lockeddir")
	_ = os.MkdirAll(lockedDir, 0o000)
	test.Cleanup(func() { _ = os.Chmod(lockedDir, 0o755) })
	if _, err := im.ExtractImagesFromManifests([]string{lockedDir}); err == nil || !strings.Contains(err.Error(), "failed to read directory") {
		test.Errorf("err = %v", err)
	}
	lockedFile := write("locked.yaml", "image: x\n", 0o000)
	if _, err := im.ExtractImagesFromManifests([]string{lockedFile}); err == nil || !strings.Contains(err.Error(), "failed to read") {
		test.Errorf("err = %v", err)
	}
	if _, err := im.GetImagesForService(ctx, &config.ServiceConfig{Name: "a", Type: "manifests", Path: filepath.Join(dir, "missing.yaml")}, ""); err == nil || !strings.Contains(err.Error(), "failed to extract images from manifests") {
		test.Errorf("err = %v", err)
	}

	// A directory of manifests skips subdirectories and other files
	write("mixed/a.yaml", "image: app:1\n", 0o644)
	write("mixed/notes.txt", "image: no:1\n", 0o644)
	_ = os.MkdirAll(filepath.Join(dir, "mixed", "sub"), 0o755)
	got, err := im.ExtractImagesFromManifests([]string{filepath.Join(dir, "mixed")})
	if err != nil || len(got) != 1 || got[0] != "app:1" {
		test.Errorf("images = %v, %v", got, err)
	}

	// A rendered chart's images are reported in verbose mode
	write("good/Chart.yaml", "apiVersion: v2\nname: good\nversion: 0.1.0\n", 0o644)
	write("good/templates/d.yaml", "apiVersion: v1\nkind: Pod\nmetadata: {name: p}\nspec:\n  containers:\n  - {name: a, image: app:2}\n---\n# empty\n", 0o644)
	out = captureStdout(test, func() {
		_, _ = im.renderedHelmImages(ctx, &config.ServiceConfig{Name: "a", Type: "helm", Path: filepath.Join(dir, "good")})
	})
	if !strings.Contains(out, "Extracted 1 image(s) from rendered chart templates") {
		test.Errorf("output:\n%s", out)
	}
}

// The real wiring: building clients doesn't connect, a missing image archive
// fails to load, and the kubeconfig chain is read from $KUBECONFIG
func TestRealHostWiring(test *testing.T) {
	clients := realClusterClients()
	if _, err := clients.clientset(&rest.Config{Host: "https://127.0.0.1:1"}); err != nil {
		test.Error(err)
	}
	if _, err := clients.dynamic(&rest.Config{Host: "https://127.0.0.1:1"}); err != nil {
		test.Error(err)
	}
	loader, missing := realImageLoader(), filepath.Join(test.TempDir(), "missing.tar")
	if _, err := loader.inspect(missing); err == nil {
		test.Error("a missing archive can't be inspected")
	}
	if err := loader.full(missing, nil); err == nil {
		test.Error("a missing archive can't be loaded")
	}
	// Both loads go to the node through kind: the layer-aware one asks what
	// its content store holds, the full one which snapshotter to import with
	var ran []string
	node := unreachableNode{fakeNode{name: "c-control-plane"}, &ran}
	if err := loader.layerAware(missing, &layeraware.ImageMetadata{}, node); err == nil {
		test.Error("a layer-aware load onto an unreachable node can't succeed")
	}
	archive := filepath.Join(test.TempDir(), "image.tar")
	if err := os.WriteFile(archive, nil, 0o600); err != nil {
		test.Fatal(err)
	}
	if err := loader.full(archive, node); err == nil {
		test.Error("a full load onto an unreachable node can't succeed")
	}
	if len(ran) != 2 || !strings.HasPrefix(ran[0], "ctr --namespace=k8s.io content ls") ||
		!strings.HasPrefix(ran[1], "containerd config dump") {
		test.Errorf("ran %q on the node", ran)
	}
	test.Setenv("KUBECONFIG", filepath.Join(test.TempDir(), "none"))
	if cfg, err := realHost().loadKubeconfig(); err != nil || cfg == nil {
		test.Errorf("an absent kubeconfig starts empty, got %v, %v", cfg, err)
	}
}

func TestContainerNetworksWithoutHostname(test *testing.T) {
	tk := newTestKind(test)
	tk.host.hostErr = errFail
	if nets := tk.getCurrentContainerNetworks(); nets != nil {
		test.Errorf("networks = %v", nets)
	}
}

// A chart's NOTES.txt isn't a manifest, and a rendered document that isn't
// YAML is skipped
func TestRenderedChartSkipsNotesAndBrokenDocuments(test *testing.T) {
	dir := filepath.Join(test.TempDir(), "chart")
	for name, content := range map[string]string{
		"Chart.yaml":          "apiVersion: v2\nname: chart\nversion: 0.1.0\n",
		"templates/NOTES.txt": "image: from-notes:1\n",
		"templates/pod.yaml":  "apiVersion: v1\nkind: Pod\nmetadata: {name: p}\nspec:\n  containers:\n  - {name: a, image: app:1}\n---\nimage: [broken\n",
	} {
		path := filepath.Join(dir, name)
		_ = os.MkdirAll(filepath.Dir(path), 0o755)
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			test.Fatal(err)
		}
	}
	im, _ := newTestImages()
	got, err := im.renderedHelmImages(context.Background(), &config.ServiceConfig{Name: "a", Type: "helm", Path: dir})
	if err != nil || len(got) != 1 || got[0] != "app:1" {
		test.Errorf("images = %v, %v", got, err)
	}
}
