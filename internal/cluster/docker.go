package cluster

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/docker/docker/client"
)

// getCommonDockerSockets returns a list of common Docker socket paths to try
// The list is ordered by likelihood/popularity:
// 1. DOCKER_HOST environment variable (if set)
// 2. Standard Docker socket (Linux/Docker Desktop)
// 3. Colima sockets (macOS)
// 4. Podman sockets (macOS/Linux)
func getCommonDockerSockets() []string {
	var sockets []string

	// First priority: DOCKER_HOST if explicitly set
	// We'll handle this separately via client.FromEnv

	// Standard Docker socket (Linux, Docker Desktop on macOS creates symlink here)
	sockets = append(sockets, "/var/run/docker.sock")

	// Colima sockets (macOS Docker alternative)
	if homeDir, err := os.UserHomeDir(); err == nil {
		sockets = append(sockets,
			filepath.Join(homeDir, ".colima/default/docker.sock"),
			filepath.Join(homeDir, ".colima/docker.sock"),
		)

		// Podman sockets (macOS/Linux Docker alternative)
		sockets = append(sockets,
			// Podman on macOS
			filepath.Join(homeDir, ".local/share/containers/podman/machine/podman.sock"),
			filepath.Join(homeDir, ".local/share/containers/podman/machine/qemu/podman.sock"),
		)
	}

	// Podman rootless socket on Linux (requires UID)
	// Format: /run/user/{uid}/podman/podman.sock
	if uid := os.Getuid(); uid > 0 {
		sockets = append(sockets, fmt.Sprintf("/run/user/%d/podman/podman.sock", uid))
	}

	return sockets
}

// tryDockerSocket attempts to create a Docker client using the specified socket
// Returns the client if successful, nil otherwise
func tryDockerSocket(ctx context.Context, socketPath string) (*client.Client, error) {
	// Check if socket exists first (optimization to avoid connection attempts to non-existent sockets)
	if _, err := os.Stat(socketPath); os.IsNotExist(err) {
		return nil, fmt.Errorf("socket does not exist: %s", socketPath)
	}

	// Try to create client with this socket
	cli, err := client.NewClientWithOpts(
		client.WithHost("unix://"+socketPath),
		client.WithAPIVersionNegotiation(),
	)
	if err != nil {
		return nil, err
	}

	// Try to ping the daemon
	_, err = cli.Ping(ctx)
	if err != nil {
		cli.Close()
		return nil, err
	}

	return cli, nil
}

// getDockerClientWithFallback tries to connect to Docker using multiple socket paths
// Returns the first working client, or an error if none work
func getDockerClientWithFallback(ctx context.Context) (*client.Client, error) {
	// First, try DOCKER_HOST if set (respects user's explicit configuration)
	if dockerHost := os.Getenv("DOCKER_HOST"); dockerHost != "" {
		cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
		if err == nil {
			// Verify it works
			if _, pingErr := cli.Ping(ctx); pingErr == nil {
				return cli, nil
			}
			cli.Close()
		}
	}

	// Try common socket paths
	var lastErr error
	triedPaths := []string{}

	for _, socketPath := range getCommonDockerSockets() {
		triedPaths = append(triedPaths, socketPath)
		cli, err := tryDockerSocket(ctx, socketPath)
		if err == nil {
			// Success! Found a working socket
			return cli, nil
		}
		lastErr = err
	}

	// None of the sockets worked, return helpful error
	return nil, fmt.Errorf("failed to connect to Docker daemon\n\n"+
		"Tried the following socket paths:\n"+
		"  DOCKER_HOST: %s\n"+
		"  Common paths: %v\n\n"+
		"Last error: %v\n\n"+
		"kraze requires Docker to be installed and running.\n"+
		"Supported alternatives: Docker, Docker Desktop, Colima, Podman\n\n"+
		"If using a non-standard socket path, set DOCKER_HOST:\n"+
		"  export DOCKER_HOST=unix:///path/to/docker.sock\n\n"+
		"Install Docker: https://docs.docker.com/get-docker/\n"+
		"Install Colima: https://github.com/abiosoft/colima",
		os.Getenv("DOCKER_HOST"),
		triedPaths,
		lastErr)
}

// CheckDockerAvailable checks if Docker is installed and running
// Returns an error with helpful message if Docker is not available
// Automatically detects and tries common Docker socket paths for:
// - Docker / Docker Desktop
// - Colima (macOS)
// - Podman (macOS/Linux)
func CheckDockerAvailable(ctx context.Context) error {
	// Try to connect to Docker daemon using multiple socket paths
	cli, err := getDockerClientWithFallback(ctx)
	if err != nil {
		return err
	}
	defer cli.Close()

	return nil
}

// GetDockerClient returns a Docker client
// Call CheckDockerAvailable first to ensure Docker is running
// Automatically detects and tries common Docker socket paths
func GetDockerClient(ctx context.Context) (*client.Client, error) {
	cli, err := getDockerClientWithFallback(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to create Docker client: %w", err)
	}

	return cli, nil
}
