//go:build linux

package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// inDeletedDir runs the rest of the test from a directory that no longer
// exists, where relative paths can't be made absolute
func inDeletedDir(t *testing.T) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "gone")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	t.Setenv("PWD", "") // os.Getwd would otherwise trust it
	if err := os.Remove(dir); err != nil {
		t.Fatal(err)
	}
}

func TestRelativePathsInDeletedDirectory(t *testing.T) {
	withFakeDeps(t)
	cfgPath := useConfig(t, kindConfig) // absolute: resolvable anywhere
	inDeletedDir(t)

	// A relative -f can't be resolved
	configFiles = []string{"kraze.yml"}
	if _, err := resolveConfigFiles(nil); err == nil || !strings.Contains(err.Error(), "failed to resolve config path 'kraze.yml'") {
		t.Errorf("err = %v", err)
	}

	// pack's default output is relative to the working directory
	configFiles = []string{cfgPath}
	if _, err := runCmd(t, runPack); err == nil || !strings.Contains(err.Error(), "resolving output path") {
		t.Errorf("err = %v", err)
	}
}
