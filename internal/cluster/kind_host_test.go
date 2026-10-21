package cluster

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hjames9/kraze/internal/config"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

func TestPatchKubeconfigWithContainerIP(test *testing.T) {
	tests := []struct {
		name       string
		kubeconfig string
		want       string
	}{
		{"localhost", "server: https://localhost:12345", "server: https://172.18.0.5:6443"},
		{"loopback", "server: https://127.0.0.1:54321", "server: https://172.18.0.5:6443"},
		{"container name", "server: https://c-control-plane:6443", "server: https://172.18.0.5:6443"},
	}
	for _, tt := range tests {
		test.Run(tt.name, func(test *testing.T) {
			tk := newTestKind(test)
			tk.run.on("docker inspect c-control-plane -f "+networkIPTemplate("bridge"), "172.18.0.5\n", nil)
			got, err := tk.patchKubeconfigWithContainerIP("c", tt.kubeconfig, "", true)
			if err != nil || got != tt.want {
				test.Errorf("got %q, %v; want %q", got, err, tt.want)
			}
		})
	}

	// The custom network is tried first; an unknown value falls through
	tk := newTestKind(test)
	tk.run.on("docker inspect c-control-plane -f "+networkIPTemplate("dev-net"), "<no value>", nil)
	tk.run.on("docker inspect c-control-plane -f "+networkIPTemplate("bridge"), "\"10.0.0.7\"", nil)
	got, err := tk.patchKubeconfigWithContainerIP("c", "server: https://127.0.0.1:1", "dev-net", true)
	if err != nil || got != "server: https://10.0.0.7:6443" {
		test.Errorf("got %q, %v", got, err)
	}
	if calls := tk.run.ran(); len(calls) != 2 || !strings.Contains(calls[0], "dev-net") {
		test.Errorf("ran %v, want the custom network tried first", calls)
	}

	// No network answers: any IP the container has
	tk = newTestKind(test)
	tk.run.on("docker inspect c-control-plane -f {{range .NetworkSettings.Networks}}", "10.1.1.1 10.2.2.2 ", nil)
	if got, err := tk.patchKubeconfigWithContainerIP("c", "server: https://localhost:9", "", true); err != nil || got != "server: https://10.1.1.1:6443" {
		test.Errorf("fallback got %q, %v", got, err)
	}
	tk = newTestKind(test)
	tk.run.on("docker inspect c-control-plane -f {{range .NetworkSettings.Networks}}", " ", nil)
	if _, err := tk.patchKubeconfigWithContainerIP("c", "x", "", true); err == nil {
		test.Error("want an error when the container has no IP")
	}
	tk = newTestKind(test)
	if _, err := tk.patchKubeconfigWithContainerIP("c", "x", "", true); err == nil {
		test.Error("want an error when docker inspect fails")
	}
}

func TestGetKubeConfigPatching(test *testing.T) {
	// Inside a container: the container's IP
	tk := newTestKind(test)
	tk.host.files["/.dockerenv"] = ""
	tk.run.on("docker inspect c-control-plane -f "+networkIPTemplate("bridge"), "172.18.0.5", nil)
	if got, err := tk.GetKubeConfig("c", false); err != nil || got != "server: https://172.18.0.5:6443\n" {
		test.Errorf("in a container: %q, %v", got, err)
	}

	// On the host: Docker's port forward
	tk = newTestKind(test)
	tk.run.on("docker port c-control-plane 6443", "0.0.0.0:53549\n", nil)
	if got, err := tk.GetKubeConfig("c", false); err != nil || got != "server: https://127.0.0.1:53549\n" {
		test.Errorf("on the host: %q, %v", got, err)
	}

	// A failed patch falls back to kind's kubeconfig
	tk = newTestKind(test)
	if got, err := tk.GetKubeConfig("c", false); err != nil || got != tk.provider.kubeconfig {
		test.Errorf("unpatched: %q, %v", got, err)
	}
}

func TestPatchKubeconfigForNativeHost(test *testing.T) {
	tk := newTestKind(test)
	// Already on localhost: nothing to do
	if got, err := tk.patchKubeconfigForNativeHost("c", "server: https://127.0.0.1:1", true); err != nil || got != "server: https://127.0.0.1:1" {
		test.Errorf("got %q, %v", got, err)
	}
	tk.run.on("docker port c-control-plane 6443", "[::]:6001", nil)
	if got, _ := tk.patchKubeconfigForNativeHost("c", "https://c-control-plane:6443", true); got != "https://127.0.0.1:6001" {
		test.Errorf("IPv6 mapping: %q", got)
	}
	for out, want := range map[string]string{"": "no port mapping", "garbage": "invalid port mapping"} {
		tk := newTestKind(test)
		tk.run.on("docker port", out, nil)
		if _, err := tk.patchKubeconfigForNativeHost("c", "https://c-control-plane:6443", true); err == nil || !strings.Contains(err.Error(), want) {
			test.Errorf("output %q: err = %v", out, err)
		}
	}
}

const kindKubeconfig = `apiVersion: v1
kind: Config
clusters:
- name: kind-c
  cluster:
    server: https://c-control-plane:6443
    certificate-authority-data: Y2E=
contexts:
- name: kind-c
  context: {cluster: kind-c, user: kind-c}
current-context: kind-c
users:
- name: kind-c
  user: {token: t}
`

func TestUpdateKubeconfigFileMerges(test *testing.T) {
	tk := newTestKind(test)
	tk.provider.kubeconfig = kindKubeconfig
	tk.run.on("docker port c-control-plane 6443", "127.0.0.1:7000", nil)
	tk.host.existing = clientcmdapi.NewConfig()
	tk.host.existing.Clusters["prod"] = &clientcmdapi.Cluster{Server: "https://prod"}
	tk.host.existing.Contexts["prod"] = &clientcmdapi.Context{Cluster: "prod"}
	tk.host.existing.CurrentContext = "prod"

	if err := tk.UpdateKubeconfigFile("c"); err != nil {
		test.Fatal(err)
	}
	written, err := clientcmd.LoadFromFile(filepath.Join(tk.host.home, ".kube", "config"))
	if err != nil {
		test.Fatal(err)
	}
	if written.CurrentContext != "kind-c" {
		test.Errorf("current context = %q", written.CurrentContext)
	}
	if written.Clusters["prod"] == nil || written.Contexts["prod"] == nil {
		test.Error("other clusters and contexts must be kept")
	}
	kindCluster := written.Clusters["kind-c"]
	if kindCluster == nil || !kindCluster.InsecureSkipTLSVerify || len(kindCluster.CertificateAuthorityData) != 0 || kindCluster.Server != "https://127.0.0.1:7000" {
		test.Errorf("kind cluster = %+v", kindCluster)
	}
}

func TestUpdateKubeconfigFileWithoutExisting(test *testing.T) {
	tk := newTestKind(test)
	tk.provider.kubeconfig = kindKubeconfig
	if err := tk.UpdateKubeconfigFile("c"); err != nil {
		test.Fatal(err)
	}
	written, err := clientcmd.LoadFromFile(filepath.Join(tk.host.home, ".kube", "config"))
	if err != nil || written.CurrentContext != "kind-c" {
		test.Errorf("written = %+v, %v", written, err)
	}

	tk.provider.kubeconfig = "not: [yaml"
	if err := tk.UpdateKubeconfigFile("c"); err == nil {
		test.Error("want an error for a kubeconfig that doesn't parse")
	}
}

func TestLoadImage(test *testing.T) {
	ctx := context.Background()

	tk := newTestKind(test)
	tk.run.on("docker save -o ", "", nil)
	if err := tk.LoadImage(ctx, "c", "app:dev"); err != nil {
		test.Fatal(err)
	}
	if len(tk.loaded) != 1 || filepath.Base(tk.loaded[0]) != "image.tar" {
		test.Errorf("loaded %v", tk.loaded)
	}
	if _, err := os.Stat(filepath.Dir(tk.loaded[0])); !os.IsNotExist(err) {
		test.Error("the image archive's temp dir should be removed")
	}

	// A digest reference is saved by its tag
	tk = newTestKind(test)
	tk.run.on("docker inspect app:1@sha256:", "[]", nil).on("docker tag", "", nil).on("docker save", "", nil)
	if err := tk.LoadImage(ctx, "c", "app:1@sha256:abc"); err != nil {
		test.Fatal(err)
	}
	saves := tk.run.ranWith("docker save")
	if len(saves) != 1 || !strings.HasSuffix(saves[0].argv, " app:1") {
		test.Errorf("saved %v, want the tag", saves)
	}

	tk = newTestKind(test)
	if err := tk.LoadImage(ctx, "c", "missing:dev"); err == nil || !strings.Contains(err.Error(), "make sure the image exists locally") {
		test.Errorf("err = %v", err)
	}

	tk = newTestKind(test)
	tk.run.on("docker save", "", nil)
	tk.loadErr = errors.New("disk full")
	if err := tk.LoadImage(ctx, "c", "app:dev"); err == nil || !strings.Contains(err.Error(), "onto node c-control-plane: disk full") {
		test.Errorf("err = %v", err)
	}

	tk = newTestKind(test)
	tk.provider.nodes = nil
	if err := tk.LoadImage(ctx, "c", "app:dev"); err == nil || !strings.Contains(err.Error(), "no nodes found") {
		test.Errorf("err = %v", err)
	}
}

func TestUntagImage(test *testing.T) {
	ctx := context.Background()
	for image, want := range map[string]string{
		"nginx:1":          "docker.io/library/nginx:1",
		"me/app:1":         "docker.io/me/app:1",
		"docker.io/me/app": "docker.io/me/app",
		"ghcr.io/me/app:1": "ghcr.io/me/app:1",
	} {
		tk := newTestKind(test)
		tk.run.on("docker exec c-control-plane ctr -n k8s.io images rm", "", nil)
		if err := tk.UntagImage(ctx, "c", image); err != nil {
			test.Fatal(err)
		}
		if calls := tk.run.ran(); len(calls) != 1 || !strings.HasSuffix(calls[0], " "+want) {
			test.Errorf("%s: ran %v, want it removed as %s", image, calls, want)
		}
	}

	tk := newTestKind(test)
	tk.run.on("docker exec", "ctr: image \"x\": not found", errors.New("exit status 1"))
	if err := tk.UntagImage(ctx, "c", "x:1"); err != nil {
		test.Errorf("an image that isn't there needs no untag, got %v", err)
	}
	tk = newTestKind(test)
	tk.run.on("docker exec", "permission denied", errors.New("exit status 1"))
	if err := tk.UntagImage(ctx, "c", "x:1"); err == nil {
		test.Error("want other failures reported")
	}
}

func TestEnsureKubeletCgroupDirectories(test *testing.T) {
	const cgroupPath = "/sys/fs/cgroup/systemd/kubelet.slice/kubelet-kubepods.slice"

	// cgroup v2: nothing to do
	tk := newTestKind(test)
	if err := tk.ensureKubeletCgroupDirectories("c"); err != nil {
		test.Fatal(err)
	}
	if len(tk.run.ranWith("docker exec c-control-plane mkdir")) != 0 {
		test.Error("cgroup v2 needs no directories")
	}

	// cgroup v1, directory missing
	tk = newTestKind(test)
	// First match wins: the longer path before its prefix
	tk.run.on("docker exec c-control-plane test -d "+cgroupPath, "", errNotFaked)
	tk.run.on("docker exec c-control-plane test -d /sys/fs/cgroup/systemd", "", nil)
	tk.run.on("docker exec c-control-plane mkdir -p "+cgroupPath, "", nil)
	if err := tk.ensureKubeletCgroupDirectories("c"); err != nil {
		test.Fatal(err)
	}
	if len(tk.run.ranWith("docker exec c-control-plane mkdir -p "+cgroupPath)) != 1 {
		test.Errorf("ran %v", tk.run.ran())
	}

	// cgroup v1, directory there
	tk = newTestKind(test)
	tk.run.on("docker exec c-control-plane test -d", "", nil)
	if err := tk.ensureKubeletCgroupDirectories("c"); err != nil || len(tk.run.ranWith("docker exec c-control-plane mkdir")) != 0 {
		test.Errorf("err = %v, ran %v", err, tk.run.ran())
	}

	// mkdir fails
	tk = newTestKind(test)
	// First match wins: the longer path before its prefix
	tk.run.on("docker exec c-control-plane test -d "+cgroupPath, "", errNotFaked)
	tk.run.on("docker exec c-control-plane test -d /sys/fs/cgroup/systemd", "", nil)
	tk.run.on("docker exec c-control-plane mkdir", "read-only file system", errNotFaked)
	if err := tk.ensureKubeletCgroupDirectories("c"); err == nil || !strings.Contains(err.Error(), "read-only file system") {
		test.Errorf("err = %v", err)
	}
}

func TestUpdateCACertificates(test *testing.T) {
	ctx := context.Background()
	tk := newTestKind(test)
	tk.provider.nodes = []string{"c-control-plane", "c-worker"}
	tk.host.files["/a.crt"], tk.host.files["/b.crt"] = "a", "b"
	tk.run.on("docker exec", "1 added", nil)
	if err := tk.updateCACertificates(ctx, "c", []string{"/a.crt", "/b.crt"}); err != nil {
		test.Fatal(err)
	}
	if got := len(tk.run.ranWith("docker exec")); got != 6 {
		test.Errorf("ran %d commands, want 6 (two certificates and an update per node)", got)
	}

	tk = newTestKind(test)
	tk.run.on("docker exec", "bad cert", errNotFaked)
	if err := tk.updateCACertificates(ctx, "c", nil); err == nil || !strings.Contains(err.Error(), "bad cert") {
		test.Errorf("err = %v", err)
	}

	// Hanging is cut off by the timeout
	orig := caUpdateTimeout
	caUpdateTimeout = 10 * time.Millisecond
	test.Cleanup(func() { caUpdateTimeout = orig })
	tk = newTestKind(test)
	tk.run.onRun("docker exec", func(ctx context.Context) ([]byte, error) {
		<-ctx.Done()
		return []byte("partial"), ctx.Err()
	})
	if err := tk.updateCACertificates(ctx, "c", nil); err == nil || !strings.Contains(err.Error(), "timed out after 10ms") {
		test.Errorf("err = %v", err)
	}
}

func TestConfigureInsecureRegistries(test *testing.T) {
	tk := newTestKind(test)
	tk.run.on("docker exec -i c-control-plane", "", nil)
	if err := tk.configureInsecureRegistries("c", []string{"registry.corp:5000"}); err != nil {
		test.Fatal(err)
	}
	calls := tk.run.ranWith("docker exec -i c-control-plane sh -c")
	if len(calls) != 3 {
		test.Fatalf("ran %v", tk.run.ran())
	}
	// The config patch comes first, from stdin; then hosts.toml; then the restart
	if !strings.HasSuffix(calls[0].argv, containerdConfigPath) || calls[0].stdin != containerdRegistryConfigPatch {
		test.Errorf("patch call = %+v", calls[0])
	}
	dir, hostsTOML, _ := registryHostsConfig("registry.corp:5000")
	if !strings.HasSuffix(calls[1].argv, dir+"/hosts.toml") || calls[1].stdin != hostsTOML {
		test.Errorf("hosts.toml call = %+v", calls[1])
	}
	if !strings.Contains(calls[2].argv, "systemctl restart containerd") {
		test.Errorf("restart call = %+v", calls[2])
	}

	tk = newTestKind(test)
	tk.run.on("docker exec -i c-control-plane sh -c systemctl restart containerd", "Job failed", errNotFaked)
	tk.run.on("docker exec -i", "", nil)
	if err := tk.configureInsecureRegistries("c", []string{"registry.corp"}); err == nil || !strings.Contains(err.Error(), "containerd did not restart") {
		test.Errorf("err = %v", err)
	}
	tk = newTestKind(test)
	tk.run.on("docker exec -i", "", nil)
	if err := tk.configureInsecureRegistries("c", []string{"ftp://nope"}); err == nil {
		test.Error("want an error for an invalid registry")
	}
}

func TestConfigureProxy(test *testing.T) {
	tk := newTestKind(test)
	tk.run.on("docker exec -i c-control-plane", "", nil)
	if err := tk.configureProxy("c", "http://user:p%40ss@proxy:3128", "", "localhost"); err != nil {
		test.Fatal(err)
	}
	calls := tk.run.ranWith("docker exec -i c-control-plane sh -c")
	if len(calls) != 3 {
		test.Fatalf("ran %v", tk.run.ran())
	}
	if !strings.HasSuffix(calls[0].argv, containerdProxyConf) || calls[0].stdin != containerdProxyDropIn("http://user:p%40ss@proxy:3128", "", "localhost") {
		test.Errorf("drop-in call = %+v", calls[0])
	}
	if !strings.Contains(calls[1].argv, "systemctl daemon-reload") || !strings.Contains(calls[2].argv, "systemctl restart containerd") {
		test.Errorf("calls = %+v", calls[1:])
	}

	tk = newTestKind(test)
	if err := tk.configureProxy("c", "http://proxy:3128", "", ""); err == nil {
		test.Error("want an error when the drop-in can't be written")
	}
}

func TestConnectToHostNetwork(test *testing.T) {
	tk := newTestKind(test)
	tk.run.on("docker network inspect dev-net --format", "172.30.0.1", nil)
	tk.run.on("docker network inspect dev-net", "[]", nil)
	tk.run.on("docker network connect", "", nil)
	tk.run.on("kind get nodes --name c", "c-control-plane\nc-worker\n", nil)
	tk.run.on("docker exec", "", nil)
	if err := tk.connectToHostNetwork("c", "dev-net", "", "172.30.0.10"); err != nil {
		test.Fatal(err)
	}
	if got := tk.run.ranWith("docker network connect"); len(got) != 1 || got[0].argv != "docker network connect --ip 172.30.0.10 dev-net c-control-plane" {
		test.Errorf("connect = %v", got)
	}
	routes := tk.run.ranWith("docker exec")
	if len(routes) != 2 || routes[1].argv != "docker exec c-worker ip route replace default via 172.30.0.1" {
		test.Errorf("routes = %v", routes)
	}

	// A missing network is created when a subnet is given
	tk = newTestKind(test)
	tk.run.on("docker network create --driver bridge --subnet 172.40.0.0/16 new-net", "", nil)
	if err := tk.connectToHostNetwork("c", "new-net", "172.40.0.0/16", ""); err == nil {
		test.Error("the created network still can't be connected to here (inspect is faked as failing)")
	}
	if len(tk.run.ranWith("docker network create")) != 1 {
		test.Errorf("ran %v", tk.run.ran())
	}

	tk = newTestKind(test)
	if err := tk.connectToHostNetwork("c", "new-net", "", ""); err == nil || !strings.Contains(err.Error(), "no subnet specified") {
		test.Errorf("err = %v", err)
	}
	tk = newTestKind(test)
	tk.run.on("docker network create", "pool overlaps", errNotFaked)
	if err := tk.connectToHostNetwork("c", "new-net", "10.0.0.0/8", ""); err == nil || !strings.Contains(err.Error(), "pool overlaps") {
		test.Errorf("err = %v", err)
	}
}

func TestFixDefaultRouteFallsBackToControlPlane(test *testing.T) {
	tk := newTestKind(test)
	tk.run.on("docker network inspect bridge --format", "172.17.0.1\n", nil)
	tk.run.on("docker exec", "", nil)
	if err := tk.fixDefaultRouteForCluster("c", "bridge"); err != nil {
		test.Fatal(err)
	}
	if got := tk.run.ranWith("docker exec"); len(got) != 1 || !strings.HasPrefix(got[0].argv, "docker exec c-control-plane ip route") {
		test.Errorf("routes = %v", got)
	}
	tk = newTestKind(test)
	tk.run.on("docker network inspect", "\n", nil)
	if err := tk.fixDefaultRouteForCluster("c", "bridge"); err == nil {
		test.Error("want an error for a network without a gateway")
	}
}

func TestDetectNetworks(test *testing.T) {
	// On the host
	tk := newTestKind(test)
	if got := tk.detectNetworks(); !slices.Equal(got, []string{"bridge"}) {
		test.Errorf("host networks = %v", got)
	}

	// In a container, its own networks first
	for name, setup := range map[string]func(h *testHost){
		"dockerenv": func(h *testHost) { h.files["/.dockerenv"] = "" },
		"cgroup":    func(h *testHost) { h.files["/proc/self/cgroup"] = "0::/docker/abc123\n" },
	} {
		tk := newTestKind(test)
		setup(tk.host)
		tk.run.on("docker inspect devbox", "bridge dev-net none host\n", nil)
		if got := tk.detectNetworks(); !slices.Equal(got, []string{"dev-net", "bridge"}) {
			test.Errorf("%s: container networks = %v", name, got)
		}
	}

	tk = newTestKind(test)
	tk.host.files["/.dockerenv"] = ""
	tk.run.on("docker inspect devbox", "bridge", nil)
	if got := tk.getCurrentContainerNetworks(); !slices.Equal(got, []string{"bridge"}) {
		test.Errorf("only bridge = %v", got)
	}
}

func TestGPUPrerequisites(test *testing.T) {
	tk := newTestKind(test)
	if err := tk.validateNvidiaGPUPrerequisites(); err == nil || !strings.Contains(err.Error(), "nvidia-container-runtime not found") {
		test.Errorf("err = %v", err)
	}
	tk.host.bins["nvidia-container-runtime"] = "/usr/bin/nvidia-container-runtime"
	if err := tk.validateNvidiaGPUPrerequisites(); err == nil || !strings.Contains(err.Error(), "nvidia-ctk not found") {
		test.Errorf("err = %v", err)
	}
	tk.host.bins["nvidia-ctk"] = "/usr/bin/nvidia-ctk"
	if err := tk.validateNvidiaGPUPrerequisites(); err != nil {
		test.Error(err)
	}

	if err := tk.validateAMDGPUPrerequisites(); err == nil || !strings.Contains(err.Error(), "/dev/kfd not found") {
		test.Errorf("err = %v", err)
	}
	tk.host.files["/dev/kfd"] = ""
	if err := tk.validateAMDGPUPrerequisites(); err == nil || !strings.Contains(err.Error(), "no AMD GPUs detected") {
		test.Errorf("err = %v", err)
	}
	tk.host.files["/dev/dri/renderD128"] = ""
	tk.host.files["/dev/dri/renderD129"] = ""
	if err := tk.validateAMDGPUPrerequisites(); err != nil {
		test.Error(err)
	}
	gpu := config.ClusterConfig{GPU: &config.GPUConfig{AMD: &config.GPUVendorConfig{Enabled: true}}}
	if mounts := tk.buildAMDGPUMounts(&gpu); len(mounts) != 3 {
		test.Errorf("mounts = %v, want kfd and two render nodes", mounts)
	}
}

func TestRegisterNvidiaRuntimeClass(test *testing.T) {
	tk := newTestKind(test)
	tk.provider.kubeconfig = kindKubeconfig
	ctx := context.Background()
	gvr := schema.GroupVersionResource{Group: "node.k8s.io", Version: "v1", Resource: "runtimeclasses"}

	if err := tk.registerNvidiaRuntimeClass(ctx, "c"); err != nil {
		test.Fatal(err)
	}
	obj, err := tk.dynamic.Resource(gvr).Get(ctx, "nvidia", metav1.GetOptions{})
	if err != nil || obj.Object["handler"] != "nvidia" {
		test.Errorf("runtime class = %v, %v", obj, err)
	}
	// Registering again updates it
	if err := tk.registerNvidiaRuntimeClass(ctx, "c"); err != nil {
		test.Fatal(err)
	}
}

func TestGetKubeconfigForExternalCluster(test *testing.T) {
	tk := newTestKind(test)
	tk.host.files[filepath.Join(tk.host.home, ".kube", "config")] = "default"
	tk.host.files[filepath.Join(tk.host.home, "work.yaml")] = "work"
	tk.host.files["/abs/kubeconfig"] = "abs"
	external := func(path string) *config.ClusterConfig {
		return &config.ClusterConfig{Name: "c", External: &config.ExternalClusterConfig{Enabled: true, Kubeconfig: path}}
	}
	for path, want := range map[string]string{"": "default", "~/work.yaml": "work", "/abs/kubeconfig": "abs"} {
		if got, err := tk.GetKubeconfigForExternalCluster(external(path)); err != nil || got != want {
			test.Errorf("%q: got %q, %v", path, got, err)
		}
	}
	if _, err := tk.GetKubeconfigForExternalCluster(external("/missing")); err == nil || !strings.Contains(err.Error(), "not found") {
		test.Errorf("err = %v", err)
	}
	if _, err := tk.GetKubeconfigForExternalCluster(&config.ClusterConfig{Name: "c"}); err == nil {
		test.Error("want an error for a kind cluster")
	}
}

func TestVerifyClusterAccess(test *testing.T) {
	tk := newTestKind(test)
	if err := tk.VerifyClusterAccess(context.Background(), kindKubeconfig); err != nil {
		test.Errorf("a reachable cluster: %v", err)
	}
	if err := tk.VerifyClusterAccess(context.Background(), "not: [yaml"); err == nil {
		test.Error("want an error for a broken kubeconfig")
	}
}

func TestDeleteAndListClusters(test *testing.T) {
	tk := newTestKind(test)
	if err := tk.DeleteCluster("c"); err == nil || !strings.Contains(err.Error(), "does not exist") {
		test.Errorf("err = %v", err)
	}
	tk.provider.clusters = []string{"c"}
	if err := tk.DeleteCluster("c"); err != nil || !slices.Equal(tk.provider.deleted, []string{"c"}) {
		test.Errorf("err = %v, deleted %v", err, tk.provider.deleted)
	}
	tk.provider.listErr = errors.New("docker down")
	if _, err := tk.ClusterExists("c"); err == nil || !strings.Contains(err.Error(), "docker down") {
		test.Errorf("err = %v", err)
	}
}

func TestCreateClusterSequence(test *testing.T) {
	tk := newTestKind(test)
	tk.run.on("docker inspect devbox", "", errNotFaked)
	tk.run.on("docker network inspect bridge --format", "172.17.0.1", nil)
	tk.run.on("docker network inspect bridge", "[]", nil)
	tk.run.on("docker network connect", "", nil)
	tk.run.on("docker exec", "", nil)
	cfg := &config.ClusterConfig{
		Name:               "c",
		CACertificates:     []string{"/certs/corp.crt"},
		InsecureRegistries: []string{"registry.corp"},
	}
	tk.host.files["/certs/corp.crt"] = "cert"

	if err := tk.CreateCluster(context.Background(), cfg); err != nil {
		test.Fatal(err)
	}
	if !slices.Equal(tk.provider.created, []string{"c"}) {
		test.Errorf("created %v", tk.provider.created)
	}
	if !slices.Equal(tk.host.sleeps, []time.Duration{10 * time.Second, 5 * time.Second, 3 * time.Second}) {
		test.Errorf("sleeps = %v", tk.host.sleeps)
	}
	if len(tk.run.ranWith("docker exec c-control-plane update-ca-certificates")) != 1 {
		test.Errorf("CA certificates not updated: %v", tk.run.ran())
	}
	if len(tk.run.ranWith("docker exec -i c-control-plane sh -c grep -q 'config_path'")) != 1 {
		test.Errorf("insecure registry not configured: %v", tk.run.ran())
	}
	if len(tk.run.ranWith("docker exec -i c-control-plane sh -c systemctl restart containerd")) != 2 {
		test.Errorf("containerd not restarted for GODEBUG and the registry: %v", tk.run.ran())
	}

	// Exists already
	if err := tk.CreateCluster(context.Background(), cfg); err == nil || !strings.Contains(err.Error(), "already exists") {
		test.Errorf("err = %v", err)
	}
}

func TestCreateClusterFailures(test *testing.T) {
	// kind fails
	tk := newTestKind(test)
	tk.provider.createErr = errors.New("port in use")
	if err := tk.CreateCluster(context.Background(), &config.ClusterConfig{Name: "c"}); err == nil || !strings.Contains(err.Error(), "port in use") {
		test.Errorf("err = %v", err)
	}

	// A CA update failure is fatal: images wouldn't pull
	tk = newTestKind(test)
	tk.run.on("docker network inspect", "[]", nil).on("docker network connect", "", nil)
	tk.host.files["/c.crt"] = "cert"
	tk.run.on("docker exec -i c-control-plane sh -c mkdir", "", nil)
	tk.run.on("docker exec c-control-plane update-ca-certificates", "boom", errNotFaked)
	err := tk.CreateCluster(context.Background(), &config.ClusterConfig{Name: "c", CACertificates: []string{"/c.crt"}})
	if err == nil || !strings.Contains(err.Error(), "failed to update CA certificates") {
		test.Errorf("err = %v", err)
	}

	// Registry and proxy failures are only warnings
	tk = newTestKind(test)
	tk.run.on("docker network inspect", "[]", nil).on("docker network connect", "", nil)
	tk.run.on("docker exec -i c-control-plane sh -c mkdir -p \"$(dirname \"$1\")\" && cat > \"$1\" sh "+containerdGODEBUGConf, "", nil)
	tk.run.on("docker exec -i c-control-plane sh -c systemctl", "", nil)
	cfg := &config.ClusterConfig{Name: "c", InsecureRegistries: []string{"registry.corp"},
		Proxy: &config.ProxyConfig{HTTPProxy: "http://proxy:3128"}}
	if err := tk.CreateCluster(context.Background(), cfg); err != nil {
		test.Errorf("err = %v", err)
	}

	// GPU prerequisites are checked first
	tk = newTestKind(test)
	nvidia := &config.ClusterConfig{Name: "c", GPU: &config.GPUConfig{Nvidia: &config.GPUVendorConfig{Enabled: true}}}
	if err := tk.CreateCluster(context.Background(), nvidia); err == nil || !strings.Contains(err.Error(), "NVIDIA GPU prerequisite") {
		test.Errorf("err = %v", err)
	}
	if len(tk.provider.created) != 0 {
		test.Error("nothing should be created")
	}
}
