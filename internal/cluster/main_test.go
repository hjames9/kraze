package cluster

import (
	"fmt"
	"os"
	"testing"
)

// TestMain runs the package's tests with HOME in a temporary directory:
// building a kind config writes ~/.kraze/clusters/<name>/, which must not land
// in the developer's real home
func TestMain(m *testing.M) {
	home, err := os.MkdirTemp("", "kraze-cluster-test-home-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err := os.Setenv("HOME", home); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	code := m.Run()
	_ = os.RemoveAll(home)
	os.Exit(code)
}
