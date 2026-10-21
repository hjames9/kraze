package main

import (
	"io"
	"os"
	"strings"
	"testing"
)

// capture runs fn with os.Stdout and os.Stderr redirected, returning both
func capture(t *testing.T, fn func()) (stdout, stderr string) {
	t.Helper()
	pipe := func(f **os.File) (func() string, func()) {
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		orig := *f
		*f = w
		done := make(chan string)
		go func() {
			data, _ := io.ReadAll(r)
			done <- string(data)
		}()
		return func() string { return <-done }, func() { _ = w.Close(); *f = orig }
	}
	readOut, restoreOut := pipe(&os.Stdout)
	readErr, restoreErr := pipe(&os.Stderr)
	fn()
	restoreOut()
	restoreErr()
	return readOut(), readErr()
}

func TestRun(t *testing.T) {
	origArgs := os.Args
	t.Cleanup(func() { os.Args = origArgs })
	Version = "9.9.9"

	os.Args = []string{"kraze", "version"}
	var code int
	out, _ := capture(t, func() { code = run() })
	if code != 0 || !strings.Contains(out, "kraze version: 9.9.9") {
		t.Errorf("version: exit %d, output:\n%s", code, out)
	}

	os.Args = []string{"kraze", "no-such-command"}
	_, errOut := capture(t, func() { code = run() })
	if code != 1 || !strings.Contains(errOut, "Error: unknown command") {
		t.Errorf("unknown command: exit %d, stderr:\n%s", code, errOut)
	}
}
