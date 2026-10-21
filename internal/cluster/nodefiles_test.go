package cluster

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/hjames9/kraze/internal/config"
)

// The files kraze gives kind nodes (the GODEBUG drop-in for containerd and
// the CA certificates) are written into the nodes with docker exec, not
// bind-mounted. A bind mount names a path on the Docker daemon's host: from
// a dev container using the host's daemon (or with a remote daemon) the path
// kraze wrote doesn't exist there, so Docker mounted an empty directory in
// its place and containerd started without GODEBUG=x509negativeserial=1.
func TestNodeFilesAreWrittenNotMounted(test *testing.T) {
	tk := newTestKind(test)
	tk.host.files["/certs/corp.crt"] = "-----BEGIN CERTIFICATE-----\ncorp\n-----END CERTIFICATE-----\n"
	tk.run.on("docker network inspect", "[]", nil).on("docker network connect", "", nil)
	tk.run.on("docker exec", "", nil)
	cfg := &config.ClusterConfig{
		Name:           "c",
		CACertificates: []string{"/certs/corp.crt"},
		Config:         []config.KindNode{{Role: "control-plane"}, {Role: "worker"}},
	}

	for _, node := range tk.buildKindConfig(cfg).Nodes {
		for _, mount := range node.ExtraMounts {
			test.Errorf("node %s bind-mounts %s from the daemon's host", node.Role, mount.HostPath)
		}
	}

	tk.provider.nodes = []string{"c-control-plane", "c-worker"}
	if err := tk.CreateCluster(context.Background(), cfg); err != nil {
		test.Fatal(err)
	}
	for _, node := range tk.provider.nodes {
		var steps []string
		for _, call := range tk.run.ranWith("docker exec") {
			if !strings.Contains(call.argv, " "+node+" ") {
				continue
			}
			switch {
			case strings.HasSuffix(call.argv, " "+containerdGODEBUGConf):
				if call.stdin != containerdGODEBUGDropIn {
					test.Errorf("%s: GODEBUG drop-in = %q", node, call.stdin)
				}
				steps = append(steps, "godebug")
			case strings.HasSuffix(call.argv, " /usr/local/share/ca-certificates/kraze-ca-0.crt"):
				if call.stdin != tk.host.files["/certs/corp.crt"] {
					test.Errorf("%s: CA certificate = %q", node, call.stdin)
				}
				steps = append(steps, "ca")
			case strings.HasSuffix(call.argv, "update-ca-certificates"):
				steps = append(steps, "update-ca")
			case strings.Contains(call.argv, "systemctl daemon-reload"):
				steps = append(steps, "reload")
			case strings.Contains(call.argv, "systemctl restart containerd"):
				steps = append(steps, "restart")
			}
		}
		// containerd restarts last, so it starts with GODEBUG and loads the
		// updated trust store (Go reads system roots once per process)
		if want := []string{"ca", "update-ca", "godebug", "reload", "restart"}; !slices.Equal(steps, want) {
			test.Errorf("%s: steps = %v, want %v", node, steps, want)
		}
	}

	// Nothing is written on kraze's side for nodes to mount
	if _, err := os.Stat(filepath.Join(tk.host.home, ".kraze")); !os.IsNotExist(err) {
		test.Errorf("~/.kraze was created: %v", err)
	}
}
