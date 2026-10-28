package cluster

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hjames9/kraze/internal/config"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"sigs.k8s.io/kind/pkg/cluster/images/layeraware"
	"sigs.k8s.io/kind/pkg/cluster/nodes"
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

// Every address kind writes for the cluster points at the one server
func TestRewriteAPIServer(test *testing.T) {
	for in, want := range map[string]string{
		"server: https://c-control-plane:6443":          "server: https://10.0.0.9:6443",
		"server: https://c-external-load-balancer:6443": "server: https://10.0.0.9:6443",
		"server: https://127.0.0.1:41234":               "server: https://10.0.0.9:6443",
		"server: https://localhost:41234":               "server: https://10.0.0.9:6443",
		// Other clusters' addresses are left alone
		"server: https://other-control-plane:6443": "server: https://other-control-plane:6443",
		"server: https://c-control-plane2:6443":    "server: https://c-control-plane2:6443",
	} {
		if got := rewriteAPIServer(in, "c", "https://10.0.0.9:6443"); got != want {
			test.Errorf("%q: got %q, want %q", in, got, want)
		}
	}
}

// With several control-planes the API is reached through kind's load
// balancer, for both the internal kubeconfig (which names it) and the
// external one (127.0.0.1 and its published port)
func TestPatchKubeconfigWithLoadBalancer(test *testing.T) {
	for _, kubeconfig := range []string{"server: https://c-external-load-balancer:6443", "server: https://127.0.0.1:41234"} {
		tk := newTestKind(test)
		tk.provider.nodes = []string{"c-control-plane", "c-control-plane2", "c-control-plane3"}
		tk.provider.external = []string{"c-external-load-balancer"}
		tk.run.on("docker inspect c-external-load-balancer -f "+networkIPTemplate("dev-net"), "172.30.0.9", nil)
		tk.run.on("docker inspect c-control-plane -f "+networkIPTemplate("dev-net"), "172.30.0.2", nil)
		var got string
		var err error
		out := captureStdout(test, func() { got, err = tk.patchKubeconfigWithContainerIP("c", kubeconfig, "dev-net") })
		if err != nil || got != "server: https://172.30.0.9:6443" {
			test.Errorf("%q: got %q, %v", kubeconfig, got, err)
		}
		if !strings.Contains(out, "Using load balancer IP 172.30.0.9 from 'dev-net' network") {
			test.Errorf("output:\n%s", out)
		}
	}
}

// A cluster created before kraze connected the load balancer: the first
// control-plane is used, and kraze says why
func TestPatchKubeconfigLoadBalancerUnreachable(test *testing.T) {
	tk := newTestKind(test)
	tk.provider.nodes = []string{"c-control-plane", "c-control-plane2"}
	tk.provider.external = []string{"c-external-load-balancer"}
	tk.run.on("docker inspect c-external-load-balancer -f "+networkIPTemplate("dev-net"), "<no value>", nil)
	tk.run.on("docker inspect c-control-plane -f "+networkIPTemplate("dev-net"), "172.30.0.2", nil)
	var got string
	var err error
	out := captureStdout(test, func() {
		got, err = tk.patchKubeconfigWithContainerIP("c", "server: https://c-external-load-balancer:6443", "dev-net")
	})
	if err != nil || got != "server: https://172.30.0.2:6443" {
		test.Errorf("got %q, %v", got, err)
	}
	if !strings.Contains(out, "load balancer isn't on the 'dev-net' network, so the API is reached through 'c-control-plane' (172.30.0.2) only") {
		test.Errorf("output:\n%s", out)
	}

	// On no network: any IP, the load balancer's first
	tk = newTestKind(test)
	tk.provider.external = []string{"c-external-load-balancer"}
	tk.run.on("docker inspect c-external-load-balancer -f {{range .NetworkSettings.Networks}}", "172.18.0.9 ", nil)
	if got, err := tk.patchKubeconfigWithContainerIP("c", "server: https://127.0.0.1:1", "", true); err != nil || got != "server: https://172.18.0.9:6443" {
		test.Errorf("fallback got %q, %v", got, err)
	}
	tk = newTestKind(test)
	tk.provider.external = []string{"c-external-load-balancer"}
	tk.run.on("docker inspect c-control-plane -f {{range .NetworkSettings.Networks}}", "172.18.0.2 ", nil)
	if got, err := tk.patchKubeconfigWithContainerIP("c", "server: https://127.0.0.1:1", "", true); err != nil || got != "server: https://172.18.0.2:6443" {
		test.Errorf("fallback without a load balancer IP got %q, %v", got, err)
	}
	tk = newTestKind(test)
	tk.provider.external = []string{"c-external-load-balancer"}
	if _, err := tk.patchKubeconfigWithContainerIP("c", "x", "", true); err == nil || !strings.Contains(err.Error(), "c-external-load-balancer or c-control-plane") {
		test.Errorf("err = %v", err)
	}
}

// Listing the nodes failing means no load balancer is assumed
func TestLoadBalancerNameUnlisted(test *testing.T) {
	tk := newTestKind(test)
	tk.provider.external = []string{"c-external-load-balancer"}
	tk.provider.nodesErr = errors.New("docker ps failed")
	if got := tk.loadBalancerName("c"); got != "" {
		test.Errorf("got %q", got)
	}
	tk.provider.nodesErr = nil
	if got := tk.loadBalancerName("c"); got != "c-external-load-balancer" {
		test.Errorf("got %q", got)
	}
	// Another cluster's load balancer isn't this one's
	tk.provider.external = []string{"other-external-load-balancer"}
	if got := tk.loadBalancerName("c"); got != "" {
		test.Errorf("got %q", got)
	}
}

// kubectl's kubeconfig, written from kind's internal one, uses the load
// balancer too
func TestUpdateKubeconfigFileWithLoadBalancer(test *testing.T) {
	tk := newTestKind(test)
	tk.host.files["/.dockerenv"] = ""
	tk.provider.nodes = []string{"c-control-plane", "c-control-plane2"}
	tk.provider.external = []string{"c-external-load-balancer"}
	tk.provider.kubeconfig = strings.ReplaceAll(kindKubeconfig, "c-control-plane:6443", "c-external-load-balancer:6443")
	tk.run.on("docker inspect c-external-load-balancer -f "+networkIPTemplate("bridge"), "172.17.0.9", nil)
	if err := tk.UpdateKubeconfigFile("c"); err != nil {
		test.Fatal(err)
	}
	written, err := clientcmd.LoadFromFile(filepath.Join(tk.host.home, ".kube", "config"))
	if err != nil {
		test.Fatal(err)
	}
	if got := written.Clusters["kind-c"]; got == nil || got.Server != "https://172.17.0.9:6443" || !got.InsecureSkipTLSVerify {
		test.Errorf("kind cluster = %+v", got)
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
	// With several control-planes only the load balancer publishes the API
	tk = newTestKind(test)
	tk.run.on("docker port c-external-load-balancer 6443", "0.0.0.0:6002\n[::]:6002\n", nil)
	if got, err := tk.patchKubeconfigForNativeHost("c", "server: https://c-external-load-balancer:6443", true); err != nil || got != "server: https://127.0.0.1:6002" {
		test.Errorf("load balancer: %q, %v", got, err)
	}
	if len(tk.run.ranWith("docker port c-control-plane")) != 0 {
		test.Error("the control-plane publishes no port with several control-planes")
	}
	tk = newTestKind(test)
	if _, err := tk.patchKubeconfigForNativeHost("c", "https://c-external-load-balancer:6443", true); err == nil || !strings.Contains(err.Error(), "container c-external-load-balancer") {
		test.Errorf("err = %v", err)
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
	tk.layerErr, tk.loadErr = errors.New("no layers"), errors.New("disk full")
	if err := tk.LoadImage(ctx, "c", "app:dev"); err == nil || !strings.Contains(err.Error(), "onto node c-control-plane: disk full") {
		test.Errorf("err = %v", err)
	}

	tk = newTestKind(test)
	tk.provider.nodes = nil
	if err := tk.LoadImage(ctx, "c", "app:dev"); err == nil || !strings.Contains(err.Error(), "no nodes found") {
		test.Errorf("err = %v", err)
	}
}

// Each node loads layer-aware, falling back to the full archive when the
// archive can't be inspected or the layer-aware load fails on that node
func TestLoadImageFallback(test *testing.T) {
	ctx := context.Background()
	twoNodes := []string{"c-control-plane", "c-worker"}

	for _, tc := range []struct {
		name                          string
		inspectErr, layerErr, loadErr error
		want                          []string // in any order: nodes load concurrently
		wantErrs                      []string
	}{
		{name: "layer-aware",
			want: []string{"layer-aware c-control-plane", "layer-aware c-worker"}},
		{name: "archive can't be inspected", inspectErr: errors.New("unsupported format"),
			want: []string{"full c-control-plane", "full c-worker"}},
		{name: "layer-aware load fails", layerErr: errors.New("ctr failed"),
			want: []string{"full c-control-plane", "full c-worker", "layer-aware c-control-plane", "layer-aware c-worker"}},
		{name: "full load fails too", layerErr: errors.New("ctr failed"), loadErr: errors.New("disk full"),
			want: []string{"full c-control-plane", "full c-worker", "layer-aware c-control-plane", "layer-aware c-worker"},
			wantErrs: []string{
				"onto node c-control-plane: disk full (layer-aware load: ctr failed)",
				"onto node c-worker: disk full (layer-aware load: ctr failed)",
			}},
		{name: "inspect error is reported", inspectErr: errors.New("unsupported format"), loadErr: errors.New("disk full"),
			want:     []string{"full c-control-plane", "full c-worker"},
			wantErrs: []string{"disk full (layer-aware load: unsupported format)"}},
	} {
		test.Run(tc.name, func(t *testing.T) {
			tk := newTestKind(t)
			tk.provider.nodes = twoNodes
			tk.run.on("docker save", "", nil)
			tk.inspectErr, tk.layerErr, tk.loadErr = tc.inspectErr, tc.layerErr, tc.loadErr

			err := tk.LoadImage(ctx, "c", "app:dev")
			if len(tc.wantErrs) == 0 && err != nil {
				t.Fatal(err)
			}
			if len(tc.wantErrs) > 0 && err == nil {
				t.Fatalf("want errors %q", tc.wantErrs)
			}
			for _, want := range tc.wantErrs {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("err = %v, want %q", err, want)
				}
			}
			slices.Sort(tk.loads)
			if !slices.Equal(tk.loads, tc.want) {
				t.Errorf("loads = %v, want %v", tk.loads, tc.want)
			}
		})
	}
}

// Nodes load at the same time, as kind's own load does: every node's load
// starts before any finishes
func TestLoadImageNodesConcurrently(test *testing.T) {
	tk := newTestKind(test)
	tk.provider.nodes = []string{"c-control-plane", "c-worker", "c-worker2"}
	tk.run.on("docker save", "", nil)

	var started sync.WaitGroup
	started.Add(len(tk.provider.nodes))
	allStarted := make(chan struct{})
	go func() { started.Wait(); close(allStarted) }()
	tk.loadImages.layerAware = func(string, *layeraware.ImageMetadata, nodes.Node) error {
		started.Done()
		select {
		case <-allStarted:
			return nil
		case <-time.After(5 * time.Second):
			return errors.New("the other nodes' loads never started")
		}
	}
	// A timed-out load would otherwise be hidden by the fallback
	tk.loadImages.full = func(string, nodes.Node) error { return errors.New("fell back to a full load") }
	if err := tk.LoadImage(context.Background(), "c", "app:dev"); err != nil {
		test.Fatal(err)
	}
}

// A node that fails doesn't end the load early: the archive is removed once
// LoadImage returns, so it waits for the nodes still loading
func TestLoadImageWaitsForEveryNode(test *testing.T) {
	tk := newTestKind(test)
	tk.provider.nodes = []string{"c-control-plane", "c-worker"}
	tk.run.on("docker save", "", nil)

	failed := make(chan struct{})
	var slowDone atomic.Bool
	tk.loadImages.layerAware = func(string, *layeraware.ImageMetadata, nodes.Node) error { return errors.New("ctr failed") }
	tk.loadImages.full = func(_ string, n nodes.Node) error {
		if n.String() == "c-control-plane" {
			defer close(failed)
			return errors.New("disk full")
		}
		<-failed
		time.Sleep(20 * time.Millisecond)
		slowDone.Store(true)
		return nil
	}
	err := tk.LoadImage(context.Background(), "c", "app:dev")
	if err == nil || !strings.Contains(err.Error(), "onto node c-control-plane: disk full") {
		test.Errorf("err = %v", err)
	}
	if strings.Contains(fmt.Sprint(err), "c-worker") {
		test.Errorf("c-worker loaded, but err = %v", err)
	}
	if !slowDone.Load() {
		test.Error("LoadImage returned before c-worker finished loading")
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

// Every node joins the network the control-plane joined, and each node's
// default route then goes through it. The static IP is the control-plane's
func TestConnectToHostNetwork(test *testing.T) {
	tk := newTestKind(test)
	tk.provider.nodes = []string{"c-worker2", "c-control-plane", "c-worker"} // kind's order varies
	tk.run.on("docker network inspect dev-net --format", "172.30.0.1", nil)
	tk.run.on("docker network inspect dev-net", "[]", nil)
	tk.run.on("docker network connect", "", nil)
	tk.run.on("docker exec", "", nil)
	var err error
	out := captureStdout(test, func() { err = tk.connectToHostNetwork("c", "dev-net", "", "172.30.0.10") })
	if err != nil {
		test.Fatal(err)
	}
	assertArgv(test, "connects", tk.run.ranWith("docker network connect"), []string{
		"docker network connect --ip 172.30.0.10 dev-net c-control-plane",
		"docker network connect dev-net c-worker",
		"docker network connect dev-net c-worker2",
	})
	assertArgv(test, "routes", tk.run.ranWith("docker exec"), []string{
		"docker exec c-control-plane ip route replace default via 172.30.0.1",
		"docker exec c-worker ip route replace default via 172.30.0.1",
		"docker exec c-worker2 ip route replace default via 172.30.0.1",
	})
	if strings.Contains(out, "Warning") {
		test.Errorf("unexpected warning:\n%s", out)
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

// A node that can't join the network keeps kind's route: changing it to a
// gateway the node has no interface for fails ("Nexthop has invalid gateway")
// With several control-planes the load balancer joins too, without a route
// change: it only proxies to the control-planes
func TestConnectToHostNetworkLoadBalancer(test *testing.T) {
	tk := newTestKind(test)
	tk.provider.nodes = []string{"c-control-plane", "c-control-plane2", "c-worker"}
	tk.provider.external = []string{"c-external-load-balancer"}
	tk.run.on("docker network inspect dev-net --format", "172.30.0.1", nil)
	tk.run.on("docker network inspect dev-net", "[]", nil)
	tk.run.on("docker network connect", "", nil)
	tk.run.on("docker exec", "", nil)
	var err error
	out := captureStdout(test, func() { err = tk.connectToHostNetwork("c", "dev-net", "", "") })
	if err != nil {
		test.Fatal(err)
	}
	assertArgv(test, "connects", tk.run.ranWith("docker network connect"), []string{
		"docker network connect dev-net c-control-plane",
		"docker network connect dev-net c-control-plane2",
		"docker network connect dev-net c-worker",
		"docker network connect dev-net c-external-load-balancer",
	})
	assertArgv(test, "routes", tk.run.ranWith("docker exec"), []string{
		"docker exec c-control-plane ip route replace default via 172.30.0.1",
		"docker exec c-control-plane2 ip route replace default via 172.30.0.1",
		"docker exec c-worker ip route replace default via 172.30.0.1",
	})
	if !strings.Contains(out, "Connected load balancer 'c-external-load-balancer' to 'dev-net' network") {
		test.Errorf("output:\n%s", out)
	}

	// A load balancer that can't join is a warning; the nodes are still routed
	tk = newTestKind(test)
	tk.provider.nodes = []string{"c-control-plane", "c-control-plane2"}
	tk.provider.external = []string{"c-external-load-balancer"}
	tk.run.on("docker network inspect dev-net --format", "172.30.0.1", nil)
	tk.run.on("docker network inspect dev-net", "[]", nil)
	tk.run.on("docker network connect dev-net c-external-load-balancer", "no such container", errNotFaked)
	tk.run.on("docker network connect", "", nil)
	tk.run.on("docker exec", "", nil)
	out = captureStdout(test, func() { err = tk.connectToHostNetwork("c", "dev-net", "", "") })
	if err != nil || !strings.Contains(out, "Could not connect load balancer 'c-external-load-balancer' to 'dev-net' network") {
		test.Errorf("err = %v, output:\n%s", err, out)
	}
	if len(tk.run.ranWith("docker exec")) != 2 {
		test.Errorf("routes = %v", tk.run.ranWith("docker exec"))
	}
}

func TestConnectToHostNetworkNodeFails(test *testing.T) {
	tk := newTestKind(test)
	tk.provider.nodes = []string{"c-control-plane", "c-worker", "c-worker2"}
	tk.run.on("docker network inspect dev-net --format", "172.30.0.1", nil)
	tk.run.on("docker network inspect dev-net", "[]", nil)
	tk.run.on("docker network connect dev-net c-worker2", "Error response from daemon: no such container", errNotFaked)
	tk.run.on("docker network connect", "", nil)
	tk.run.on("docker exec", "", nil)
	var err error
	out := captureStdout(test, func() { err = tk.connectToHostNetwork("c", "dev-net", "", "") })
	if err != nil {
		test.Fatal(err)
	}
	if !strings.Contains(out, "Could not connect node 'c-worker2' to 'dev-net' network") || !strings.Contains(out, "no such container") {
		test.Errorf("output:\n%s", out)
	}
	assertArgv(test, "routes", tk.run.ranWith("docker exec"), []string{
		"docker exec c-control-plane ip route replace default via 172.30.0.1",
		"docker exec c-worker ip route replace default via 172.30.0.1",
	})
}

// Nodes join the network the control-plane ended up on, not the first one
// tried
func TestConnectToHostNetworkNodesFollowControlPlane(test *testing.T) {
	tk := newTestKind(test)
	tk.provider.nodes = []string{"c-control-plane", "c-worker"}
	tk.host.files["/.dockerenv"] = ""
	tk.run.on("docker inspect devbox", "dev-net\n", nil)
	tk.run.on("docker network inspect bridge --format", "172.17.0.1", nil)
	tk.run.on("docker network inspect", "[]", nil)
	tk.run.on("docker network connect dev-net c-control-plane", "already connected", errNotFaked)
	tk.run.on("docker network connect", "", nil)
	tk.run.on("docker exec", "", nil)
	if err := tk.connectToHostNetwork("c", "", "", ""); err != nil {
		test.Fatal(err)
	}
	assertArgv(test, "connects", tk.run.ranWith("docker network connect"), []string{
		"docker network connect dev-net c-control-plane",
		"docker network connect bridge c-control-plane",
		"docker network connect bridge c-worker",
	})
	assertArgv(test, "routes", tk.run.ranWith("docker exec"), []string{
		"docker exec c-control-plane ip route replace default via 172.17.0.1",
		"docker exec c-worker ip route replace default via 172.17.0.1",
	})
}

// If kind can't list the nodes, only the control-plane is connected, and it
// says so: the old kind CLI fallback skipped the workers silently
func TestConnectToHostNetworkNodesUnlisted(test *testing.T) {
	tk := newTestKind(test)
	tk.provider.nodesErr = errors.New("docker ps failed")
	tk.run.on("docker network inspect dev-net --format", "172.30.0.1", nil)
	tk.run.on("docker network inspect dev-net", "[]", nil)
	tk.run.on("docker network connect", "", nil)
	tk.run.on("docker exec", "", nil)
	var err error
	out := captureStdout(test, func() { err = tk.connectToHostNetwork("c", "dev-net", "", "") })
	if err != nil {
		test.Fatal(err)
	}
	if !strings.Contains(out, "Could not list the nodes of cluster 'c' (docker ps failed); only the control-plane is connected") {
		test.Errorf("output:\n%s", out)
	}
	assertArgv(test, "connects", tk.run.ranWith("docker network connect"), []string{"docker network connect dev-net c-control-plane"})
	assertArgv(test, "routes", tk.run.ranWith("docker exec"), []string{"docker exec c-control-plane ip route replace default via 172.30.0.1"})
}

func TestFixDefaultRoutes(test *testing.T) {
	tk := newTestKind(test)
	tk.run.on("docker network inspect bridge --format", "172.17.0.1\n", nil)
	tk.run.on("docker exec c-worker ", "Error: Nexthop has invalid gateway.", errNotFaked)
	tk.run.on("docker exec", "", nil)
	var err error
	out := captureStdout(test, func() { err = tk.fixDefaultRoutes("bridge", []string{"c-control-plane", "c-worker"}, false) })
	if err != nil {
		test.Fatal(err)
	}
	// A node whose route can't be changed is a warning; the others still are
	assertArgv(test, "routes", tk.run.ranWith("docker exec"), []string{
		"docker exec c-control-plane ip route replace default via 172.17.0.1",
		"docker exec c-worker ip route replace default via 172.17.0.1",
	})
	if !strings.Contains(out, "Could not update default route in node 'c-worker'") ||
		!strings.Contains(out, "Default route in 'c-control-plane' updated to use 'bridge' gateway (172.17.0.1)") {
		test.Errorf("output:\n%s", out)
	}

	tk = newTestKind(test)
	tk.run.on("docker network inspect", "\n", nil)
	if err := tk.fixDefaultRoutes("bridge", []string{"c-control-plane"}, false); err == nil {
		test.Error("want an error for a network without a gateway")
	}
}

// assertArgv checks the commands that ran, in order
func assertArgv(test *testing.T, what string, ran []runCall, want []string) {
	test.Helper()
	got := make([]string, len(ran))
	for iter, call := range ran {
		got[iter] = call.argv
	}
	if !slices.Equal(got, want) {
		test.Errorf("%s:\n got  %q\n want %q", what, got, want)
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

// repairKind is a cluster on dev-net (gateway 172.30.0.1) whose nodes' state
// the test sets: which containers are on the network, and each node's route
func repairKind(t *testing.T, onNetwork map[string]bool, routes map[string]string) *testKind {
	t.Helper()
	tk := newTestKind(t)
	tk.provider.nodes = []string{"c-control-plane", "c-control-plane2", "c-worker"}
	tk.provider.external = []string{"c-external-load-balancer"}
	tk.run.on("docker network inspect dev-net --format", "172.30.0.1\n", nil)
	for _, c := range []string{"c-control-plane", "c-control-plane2", "c-worker", "c-external-load-balancer"} {
		ip := "<no value>"
		if onNetwork[c] {
			ip = "172.30.0.9"
		}
		tk.run.on("docker inspect "+c+" -f "+networkIPTemplate("dev-net"), ip+"\n", nil)
	}
	for node, route := range routes {
		tk.run.on("docker exec "+node+" ip route show default", route+"\n", nil)
	}
	tk.run.on("docker network connect", "", nil)
	tk.run.on("docker exec", "", nil)
	return tk
}

var allOnNetwork = map[string]bool{"c-control-plane": true, "c-control-plane2": true, "c-worker": true, "c-external-load-balancer": true}

func viaGateway(nodes ...string) map[string]string {
	routes := map[string]string{}
	for _, n := range nodes {
		routes[n] = "default via 172.30.0.1 dev eth1"
	}
	return routes
}

// A cluster that needs nothing is left alone, quietly
func TestRepairClusterNetworkHealthy(test *testing.T) {
	tk := repairKind(test, allOnNetwork, viaGateway("c-control-plane", "c-control-plane2", "c-worker"))
	out := captureStdout(test, func() { tk.RepairClusterNetwork("c", "dev-net") })
	if out != "" {
		test.Errorf("output:\n%s", out)
	}
	if got := tk.run.ranWith("docker network connect"); len(got) != 0 {
		test.Errorf("connected %v", got)
	}
	if got := tk.run.ranWith("docker exec c-control-plane ip route replace"); len(got) != 0 {
		test.Errorf("routes %v", got)
	}
	for _, n := range []string{"c-control-plane2", "c-worker"} {
		if got := tk.run.ranWith("docker exec " + n + " ip route replace"); len(got) != 0 {
			test.Errorf("routes %v", got)
		}
	}
}

// Nodes restarted since the last up lost the route kraze set: it's restored
func TestRepairClusterNetworkRestoresRoutes(test *testing.T) {
	routes := viaGateway("c-control-plane2")
	routes["c-control-plane"] = "default via 172.18.0.1 dev eth0"
	routes["c-worker"] = "default via 172.18.0.1 dev eth0"
	tk := repairKind(test, allOnNetwork, routes)
	out := captureStdout(test, func() { tk.RepairClusterNetwork("c", "dev-net") })
	var replaced []string
	for _, call := range tk.run.ranWith("docker exec") {
		if strings.Contains(call.argv, "route replace") {
			replaced = append(replaced, call.argv)
		}
	}
	if !slices.Equal(replaced, []string{
		"docker exec c-control-plane ip route replace default via 172.30.0.1",
		"docker exec c-worker ip route replace default via 172.30.0.1",
	}) {
		test.Errorf("replaced %q", replaced)
	}
	if !strings.Contains(out, "Restored default route in 'c-control-plane' to use 'dev-net' gateway (172.30.0.1)") ||
		!strings.Contains(out, "Restored default route in 'c-worker'") || strings.Contains(out, "c-control-plane2") {
		test.Errorf("output:\n%s", out)
	}
	if got := tk.run.ranWith("docker network connect"); len(got) != 0 {
		test.Errorf("connected %v", got)
	}
}

// A cluster an older kraze created: only the control-plane is on the
// network. The rest joins, and the joined nodes are routed
func TestRepairClusterNetworkConnectsOlderCluster(test *testing.T) {
	tk := repairKind(test, map[string]bool{"c-control-plane": true},
		map[string]string{"c-control-plane": "default via 172.30.0.1 dev eth1"})
	out := captureStdout(test, func() { tk.RepairClusterNetwork("c", "dev-net") })
	assertArgv(test, "connects", tk.run.ranWith("docker network connect"), []string{
		"docker network connect dev-net c-control-plane2",
		"docker network connect dev-net c-worker",
		"docker network connect dev-net c-external-load-balancer",
	})
	for _, want := range []string{
		"Connected load balancer 'c-external-load-balancer' to 'dev-net' network",
		"Restored default route in 'c-control-plane2'", "Restored default route in 'c-worker'",
	} {
		if !strings.Contains(out, want) {
			test.Errorf("want %q in output:\n%s", want, out)
		}
	}
	if strings.Contains(out, "Restored default route in 'c-control-plane' ") {
		test.Errorf("the control-plane's route was right:\n%s", out)
	}
}

// The network is the configured one, or else the first detected one the
// control-plane is on; on none, nothing is touched
func TestRepairClusterNetworkFindsNetwork(test *testing.T) {
	tk := repairKind(test, allOnNetwork, viaGateway("c-control-plane", "c-control-plane2", "c-worker"))
	tk.host.files["/.dockerenv"] = ""
	tk.run.on("docker inspect devbox", "other-net dev-net\n", nil)
	tk.run.on("docker inspect c-control-plane -f "+networkIPTemplate("other-net"), "<no value>\n", nil)
	tk.RepairClusterNetwork("c", "")
	if got := tk.run.ranWith("docker network inspect dev-net --format"); len(got) != 1 {
		test.Errorf("want dev-net repaired, ran %v", tk.run.ran())
	}

	tk = newTestKind(test)
	tk.RepairClusterNetwork("c", "dev-net")
	if got := tk.run.ran(); len(got) != 1 || !strings.HasPrefix(got[0], "docker inspect c-control-plane") {
		test.Errorf("a control-plane on no network should only be looked up, ran %v", got)
	}
}

// The user's kubeconfig entry for the cluster gets the current address, and
// nothing else changes
func TestRefreshKubeconfigFile(test *testing.T) {
	stale := func() *clientcmdapi.Config {
		cfg := clientcmdapi.NewConfig()
		cfg.Clusters["kind-c"] = &clientcmdapi.Cluster{Server: "https://172.30.0.2:6443", InsecureSkipTLSVerify: true}
		cfg.Clusters["prod"] = &clientcmdapi.Cluster{Server: "https://prod"}
		cfg.AuthInfos["kind-c"] = &clientcmdapi.AuthInfo{Token: "keep"}
		cfg.Contexts["kind-c"] = &clientcmdapi.Context{Cluster: "kind-c", AuthInfo: "kind-c"}
		cfg.Contexts["prod"] = &clientcmdapi.Context{Cluster: "prod"}
		cfg.CurrentContext = "prod"
		return cfg
	}
	setup := func(t *testing.T) *testKind {
		tk := newTestKind(t)
		tk.host.files["/.dockerenv"] = ""
		tk.provider.kubeconfig = strings.ReplaceAll(kindKubeconfig, "c-control-plane:6443", "c-external-load-balancer:6443")
		tk.provider.nodes = []string{"c-control-plane", "c-control-plane2"}
		tk.provider.external = []string{"c-external-load-balancer"}
		tk.run.on("docker inspect c-external-load-balancer -f "+networkIPTemplate("bridge"), "172.30.0.9", nil)
		return tk
	}
	path := func(tk *testKind) string { return filepath.Join(tk.host.home, ".kube", "config") }

	tk := setup(test)
	tk.host.existing = stale()
	var err error
	out := captureStdout(test, func() { err = tk.RefreshKubeconfigFile("c") })
	if err != nil {
		test.Fatal(err)
	}
	if !strings.Contains(out, "Updated kubeconfig context 'kind-c': API server https://172.30.0.2:6443 -> https://172.30.0.9:6443") {
		test.Errorf("output:\n%s", out)
	}
	written, err := clientcmd.LoadFromFile(path(tk))
	if err != nil {
		test.Fatal(err)
	}
	if got := written.Clusters["kind-c"]; got == nil || got.Server != "https://172.30.0.9:6443" || !got.InsecureSkipTLSVerify {
		test.Errorf("kind-c = %+v", got)
	}
	if written.CurrentContext != "prod" || written.Clusters["prod"] == nil || written.AuthInfos["kind-c"].Token != "keep" {
		test.Errorf("only the address may change, got %+v", written)
	}

	// Up to date, no entry for the cluster, or no kubeconfig: nothing written
	upToDate := stale()
	upToDate.Clusters["kind-c"].Server = "https://172.30.0.9:6443"
	noEntry := stale()
	delete(noEntry.Clusters, "kind-c")
	for name, existing := range map[string]*clientcmdapi.Config{"up to date": upToDate, "no entry": noEntry, "no kubeconfig": nil} {
		tk := setup(test)
		tk.host.existing = existing
		out := captureStdout(test, func() { err = tk.RefreshKubeconfigFile("c") })
		if err != nil || out != "" {
			test.Errorf("%s: err = %v, output %q", name, err, out)
		}
		if _, statErr := os.Stat(path(tk)); !os.IsNotExist(statErr) {
			test.Errorf("%s: the kubeconfig was written", name)
		}
	}

	// Failures are returned
	tk = setup(test)
	tk.host.existing = stale()
	tk.provider.kubeErr = errors.New("no control-plane")
	if err := tk.RefreshKubeconfigFile("c"); err == nil || !strings.Contains(err.Error(), "no control-plane") {
		test.Errorf("err = %v", err)
	}
	tk = setup(test)
	tk.host.existing = stale()
	tk.KindManager.host.kubeconfigPath = filepath.Join(tk.host.home, "file", "config")
	_ = os.WriteFile(filepath.Join(tk.host.home, "file"), nil, 0o644)
	if err := tk.RefreshKubeconfigFile("c"); err == nil || !strings.Contains(err.Error(), "failed to write kubeconfig") {
		test.Errorf("err = %v", err)
	}
	tk = setup(test)
	tk.host.existing = stale()
	tk.provider.kubeconfig = "not: [yaml"
	if err := tk.RefreshKubeconfigFile("c"); err == nil {
		test.Error("want an error for a kubeconfig that doesn't parse")
	}
}
