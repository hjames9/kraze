package cluster

import (
	"context"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeDockerSocket serves a Docker API that answers pings on a unix socket,
// and returns its path
func fakeDockerSocket(test *testing.T) string {
	test.Helper()
	// Unix socket paths are short: keep it out of the long test temp dir
	dir, err := os.MkdirTemp("", "kd")
	if err != nil {
		test.Fatal(err)
	}
	test.Cleanup(func() { _ = os.RemoveAll(dir) })
	path := filepath.Join(dir, "docker.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		test.Fatal(err)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Api-Version", "1.47")
		w.Header().Set("OSType", "linux")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("OK"))
	})}
	go func() { _ = server.Serve(listener) }()
	test.Cleanup(func() { _ = server.Close() })
	return path
}

func withSockets(test *testing.T, sockets ...string) {
	test.Helper()
	orig := dockerSockets
	dockerSockets = func() []string { return sockets }
	test.Cleanup(func() { dockerSockets = orig })
}

func TestDockerSocketDiscovery(test *testing.T) {
	ctx := context.Background()
	working := fakeDockerSocket(test)
	notASocket := filepath.Join(test.TempDir(), "file.sock")
	if err := os.WriteFile(notASocket, nil, 0o644); err != nil {
		test.Fatal(err)
	}
	missing := filepath.Join(test.TempDir(), "missing.sock")

	// The first socket that answers wins
	test.Setenv("DOCKER_HOST", "")
	withSockets(test, missing, notASocket, working)
	if err := CheckDockerAvailable(ctx); err != nil {
		test.Errorf("a working socket should be found: %v", err)
	}
	cli, err := GetDockerClient(ctx)
	if err != nil {
		test.Fatal(err)
	}
	_ = cli.Close()

	// None answers
	withSockets(test, missing, notASocket)
	err = CheckDockerAvailable(ctx)
	if err == nil || !strings.Contains(err.Error(), "failed to connect to Docker daemon") || !strings.Contains(err.Error(), missing) {
		test.Errorf("err = %v", err)
	}
	if _, err := GetDockerClient(ctx); err == nil || !strings.Contains(err.Error(), "failed to create Docker client") {
		test.Errorf("err = %v", err)
	}

	// DOCKER_HOST is tried first
	test.Setenv("DOCKER_HOST", "unix://"+working)
	if err := CheckDockerAvailable(ctx); err != nil {
		test.Errorf("DOCKER_HOST should be used: %v", err)
	}
	// and falls back to the common paths when it doesn't answer
	test.Setenv("DOCKER_HOST", "unix://"+notASocket)
	withSockets(test, working)
	if err := CheckDockerAvailable(ctx); err != nil {
		test.Errorf("should fall back to the common paths: %v", err)
	}
}

func TestCommonDockerSockets(test *testing.T) {
	test.Setenv("HOME", "/home/someone")
	sockets := getCommonDockerSockets()
	if sockets[0] != "/var/run/docker.sock" {
		test.Errorf("the standard socket comes first, got %v", sockets)
	}
	joined := strings.Join(sockets, " ")
	for _, want := range []string{".colima/default/docker.sock", "podman/machine/podman.sock"} {
		if !strings.Contains(joined, want) {
			test.Errorf("sockets should include %s: %v", want, sockets)
		}
	}
}
