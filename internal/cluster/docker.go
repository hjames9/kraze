package cluster

import (
	"context"
	"fmt"

	"github.com/docker/docker/client"
)

// CheckDockerAvailable checks if Docker is installed and running
// Returns an error with helpful message if Docker is not available
func CheckDockerAvailable(ctx context.Context) error {
	// Try to connect to Docker daemon
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return fmt.Errorf("Docker is not available: %w\n\n"+
			"kraze requires Docker to be installed and running.\n"+
			"Please install Docker: https://docs.docker.com/get-docker/", err)
	}
	defer cli.Close()

	// Try to ping the Docker daemon
	_, err = cli.Ping(ctx)
	if err != nil {
		return fmt.Errorf("Docker daemon is not running: %w\n\n"+
			"Please start Docker and try again.", err)
	}

	return nil
}

// GetDockerClient returns a Docker client
// Call CheckDockerAvailable first to ensure Docker is running
func GetDockerClient(ctx context.Context) (*client.Client, error) {
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, fmt.Errorf("failed to create Docker client: %w", err)
	}

	return cli, nil
}
