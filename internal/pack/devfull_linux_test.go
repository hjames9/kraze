//go:build linux

package pack

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A disk that fills up while writing the archive: the archive's temp file is
// a symlink to /dev/full, where every write fails with ENOSPC
func TestCreatePackageDiskFull(t *testing.T) {
	if _, err := os.Stat("/dev/full"); err != nil {
		t.Skip("no /dev/full")
	}
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "app.yaml"), "kind: ConfigMap\n")
	cfgPath := filepath.Join(dir, "kraze.yml")
	writeFile(t, cfgPath, "project: web\ncluster:\n  name: c\nservices:\n  app:\n    type: manifests\n    path: ./app.yaml\n")
	out := filepath.Join(t.TempDir(), "out.tar.gz")
	if err := os.Symlink("/dev/full", out+".tmp"); err != nil {
		t.Fatal(err)
	}

	err := CreatePackage([]string{cfgPath}, parse(t, cfgPath), "dev", out, false)
	if err == nil || !strings.Contains(err.Error(), "writing config") || !strings.Contains(err.Error(), "no space left on device") {
		t.Errorf("err = %v", err)
	}
	if _, err := os.Lstat(out); !os.IsNotExist(err) {
		t.Error("a failed package must not leave an archive")
	}
}
