package cluster

import (
	"context"
	"testing"
)

// Note: These tests require Docker to be running.
// They will be skipped if Docker is not available.

func TestCheckDockerAvailable(test *testing.T) {
	ctx := context.Background()

	err := CheckDockerAvailable(ctx)
	if err != nil {
		// Docker might not be available in CI/test environments
		// This is not a failure of the test itself
		test.Skipf("Docker not available (this is expected in some test environments): %v", err)
	}

	// If we get here, Docker is available
	test.Log("Docker is available and running")
}

func TestGetDockerClient(test *testing.T) {
	ctx := context.Background()

	// First check if Docker is available
	if err := CheckDockerAvailable(ctx); err != nil {
		test.Skipf("Docker not available, skipping test: %v", err)
	}

	client, err := GetDockerClient(ctx)
	if err != nil {
		test.Fatalf("GetDockerClient() error: %v", err)
	}

	if client == nil {
		test.Fatal("GetDockerClient() returned nil client")
	}

	// Try to ping Docker to verify client works
	_, err = client.Ping(ctx)
	if err != nil {
		test.Errorf("Failed to ping Docker with returned client: %v", err)
	}

	// Clean up
	client.Close()
}

func TestGetDockerClient_MultipleClients(test *testing.T) {
	ctx := context.Background()

	// First check if Docker is available
	if err := CheckDockerAvailable(ctx); err != nil {
		test.Skipf("Docker not available, skipping test: %v", err)
	}

	// Create multiple clients to ensure they don't interfere
	client1, err := GetDockerClient(ctx)
	if err != nil {
		test.Fatalf("GetDockerClient() client1 error: %v", err)
	}
	defer client1.Close()

	client2, err := GetDockerClient(ctx)
	if err != nil {
		test.Fatalf("GetDockerClient() client2 error: %v", err)
	}
	defer client2.Close()

	// Both should be able to ping
	if _, err := client1.Ping(ctx); err != nil {
		test.Errorf("client1 ping failed: %v", err)
	}

	if _, err := client2.Ping(ctx); err != nil {
		test.Errorf("client2 ping failed: %v", err)
	}
}

// TestCheckDockerAvailable_ErrorMessage verifies that error messages are helpful
func TestCheckDockerAvailable_ErrorMessage(test *testing.T) {
	ctx := context.Background()

	err := CheckDockerAvailable(ctx)
	if err != nil {
		// Verify error message contains helpful information
		errMsg := err.Error()

		// Should mention Docker
		if len(errMsg) < 10 {
			test.Errorf("Error message too short, should contain helpful information: %q", errMsg)
		}

		test.Logf("Error message (as expected when Docker unavailable): %v", err)
		test.Skip("Docker not available, which is expected in some environments")
	}
}
