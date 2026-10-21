package cluster

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hjames9/kraze/internal/config"
	"go.yaml.in/yaml/v3"
)

// captureStdout returns what fn writes to stdout
func captureStdout(test *testing.T, fn func()) string {
	test.Helper()
	orig := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		test.Fatal(err)
	}
	os.Stdout = w
	done := make(chan string)
	go func() {
		var b strings.Builder
		buf := make([]byte, 4096)
		for {
			n, err := r.Read(buf)
			b.Write(buf[:n])
			if err != nil {
				break
			}
		}
		done <- b.String()
	}()
	fn()
	_ = w.Close()
	os.Stdout = orig
	return <-done
}

func TestCreateClusterBranches(test *testing.T) {
	// GPUs, a custom network, and the steps that only warn
	tk := newTestKind(test)
	tk.host.bins["nvidia-container-runtime"] = "/usr/bin/nvidia-container-runtime"
	tk.host.bins["nvidia-ctk"] = "/usr/bin/nvidia-ctk"
	tk.host.files["/dev/kfd"] = ""
	tk.host.files["/dev/dri/renderD128"] = ""
	tk.provider.kubeconfig = kindKubeconfig
	// cgroup v1 whose directory can't be created; the network can't be joined
	tk.run.on("docker exec c-control-plane test -d /sys/fs/cgroup/systemd/", "", errNotFaked)
	tk.run.on("docker exec c-control-plane test -d /sys/fs/cgroup/systemd", "", nil)
	tk.run.on("docker exec c-control-plane mkdir", "read-only", errNotFaked)
	tk.run.on("docker network inspect dev-net", "", nil)
	tk.run.on("docker network connect", "no such network", errNotFaked)
	tk.run.on("docker exec -i", "", nil) // containerd is configured
	cfg := &config.ClusterConfig{
		Name: "c", Network: "dev-net",
		GPU: &config.GPUConfig{Nvidia: &config.GPUVendorConfig{Enabled: true}, AMD: &config.GPUVendorConfig{Enabled: true}},
	}

	var err error
	out := captureStdout(test, func() { err = tk.CreateCluster(context.Background(), cfg) })
	if err != nil {
		test.Fatalf("%v\n%s", err, out)
	}
	for _, want := range []string{
		"Note: Could not create kubelet cgroup directories",
		"Warning: Could not connect to host network",
		"Registering NVIDIA RuntimeClass",
		"AMD GPU mounts configured",
	} {
		if !strings.Contains(out, want) {
			test.Errorf("output should contain %q:\n%s", want, out)
		}
	}
	if tk.customNetwork != "dev-net" {
		test.Errorf("the network should be remembered for the kubeconfig, got %q", tk.customNetwork)
	}

	// AMD prerequisites are checked before anything else
	tk = newTestKind(test)
	amd := &config.ClusterConfig{Name: "c", GPU: &config.GPUConfig{AMD: &config.GPUVendorConfig{Enabled: true}}}
	if err := tk.CreateCluster(context.Background(), amd); err == nil || !strings.Contains(err.Error(), "AMD GPU prerequisite") {
		test.Errorf("err = %v", err)
	}

	// The RuntimeClass can't be registered
	tk = newTestKind(test)
	tk.host.bins["nvidia-container-runtime"] = "/usr/bin/nvidia-container-runtime"
	tk.host.bins["nvidia-ctk"] = "/usr/bin/nvidia-ctk"
	tk.provider.kubeconfig = "not: [yaml"
	tk.run.on("docker exec -i", "", nil)
	nvidia := &config.ClusterConfig{Name: "c", GPU: &config.GPUConfig{Nvidia: &config.GPUVendorConfig{Enabled: true}}}
	captureStdout(test, func() { err = tk.CreateCluster(context.Background(), nvidia) })
	if err == nil || !strings.Contains(err.Error(), "failed to register NVIDIA RuntimeClass") {
		test.Errorf("err = %v", err)
	}
}

func TestImagesForServiceWarnings(test *testing.T) {
	dir := test.TempDir()
	write := func(name, content string) string {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			test.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			test.Fatal(err)
		}
		return path
	}
	write("chart/Chart.yaml", "apiVersion: v2\nname: chart\nversion: 0.1.0\n")
	write("chart/values.yaml", "image: [unclosed\n")
	broken := write("broken.yaml", "image: [unclosed\n")
	missing := filepath.Join(dir, "missing.yaml")
	values := config.ValuesField{}
	if err := yaml.Unmarshal([]byte("["+broken+", "+missing+"]"), &values); err != nil {
		test.Fatal(err)
	}

	im, _ := newTestImages()
	im.verbose = true
	svc := &config.ServiceConfig{Name: "app", Type: "helm", Path: filepath.Join(dir, "chart"), Values: values, ValuesInline: "image: [unclosed\n"}
	out := captureStdout(test, func() { _, _ = im.GetImagesForService(context.Background(), svc, "") })
	for _, want := range []string{
		"Failed to render chart for 'app'",
		"Failed to extract images from inline values",
		"Failed to extract images from values file",
		"Failed to parse default values",
		"Failed to read values file " + missing,
		"Failed to parse values file " + broken,
	} {
		if !strings.Contains(out, want) {
			test.Errorf("output should contain %q:\n%s", want, out)
		}
	}

	// A chart archive that isn't one
	archive := write("bad.tgz", "not a chart")
	out = captureStdout(test, func() {
		_ = im.extractImagesFromLocalChart(&config.ServiceConfig{Name: "app", Type: "helm", Path: archive})
	})
	if !strings.Contains(out, "Failed to load chart archive") {
		test.Errorf("output:\n%s", out)
	}
}

func TestKindConfigEdges(test *testing.T) {
	tk := newTestKind(test)

	// A node image applies to every node, the default one and configured ones
	cfg := &config.ClusterConfig{Name: "c", NodeImage: "kindest/node:v1.36.0"}
	built := tk.buildKindConfig(cfg)
	if len(built.Nodes) != 1 || built.Nodes[0].Image != "kindest/node:v1.36.0" {
		test.Errorf("default node = %+v", built.Nodes)
	}
	cfg.Config = []config.KindNode{{Role: "control-plane"}, {Role: "worker"}, {Role: "something-else"}}
	built = tk.buildKindConfig(cfg)
	if len(built.Nodes) != 3 {
		test.Fatalf("nodes = %+v", built.Nodes)
	}
	for _, n := range built.Nodes {
		if n.Image != "kindest/node:v1.36.0" {
			test.Errorf("node image = %q", n.Image)
		}
	}
	if built.Nodes[2].Role != "control-plane" {
		test.Errorf("an unknown role defaults to control-plane, got %q", built.Nodes[2].Role)
	}

	if got := tk.parseK8sVersion(&config.ClusterConfig{NodeImage: "my-node-image"}); got != "my-node-image" {
		test.Errorf("an image without a tag = %q", got)
	}

	// A proxy section that enables nothing and no proxy in the environment
	for _, key := range []string{"HTTP_PROXY", "http_proxy", "HTTPS_PROXY", "https_proxy", "NO_PROXY", "no_proxy"} {
		test.Setenv(key, "")
	}
	off := false
	for _, proxy := range []*config.ProxyConfig{{}, {Enabled: &off, HTTPProxy: "http://ignored"}} {
		if h, hs, n := tk.getEffectiveProxyConfig(&config.ClusterConfig{Proxy: proxy}); h != "" || hs != "" || n != "" {
			test.Errorf("proxy %+v = %q %q %q, want none", proxy, h, hs, n)
		}
	}
}

func TestKubeconfigFallbacks(test *testing.T) {
	// In a container, but the container IP can't be found: kind's kubeconfig
	tk := newTestKind(test)
	tk.host.files["/.dockerenv"] = ""
	tk.run.on("docker inspect devbox", "", nil) // no networks of its own
	if got, err := tk.GetKubeConfig("c", false); err != nil || got != tk.provider.kubeconfig {
		test.Errorf("got %q, %v", got, err)
	}
	if nets := tk.getCurrentContainerNetworks(); nets != nil {
		test.Errorf("no networks = %v", nets)
	}
}
