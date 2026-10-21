package main

import (
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"

	"k8s.io/klog/v2"
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

// TestMain lets TestGlobalFlagsBeforeSubcommand run kraze as a separate
// process: run's flag handling is process-wide and may exit
func TestMain(m *testing.M) {
	if args, ok := os.LookupEnv("KRAZE_TEST_RUN_MAIN"); ok {
		os.Args = append([]string{"kraze"}, strings.Fields(args)...)
		os.Exit(run())
	}
	os.Exit(m.Run())
}

func TestGlobalFlagsBeforeSubcommand(t *testing.T) {
	for _, args := range []string{"-v version", "--verbose version", "--plain version", "-v --plain version"} {
		t.Run(args, func(t *testing.T) {
			cmd := exec.Command(os.Args[0], "-test.run=^$")
			cmd.Env = append(os.Environ(), "KRAZE_TEST_RUN_MAIN="+args)
			out, err := cmd.CombinedOutput()
			if err != nil || !strings.Contains(string(out), "kraze version:") {
				t.Errorf("kraze %s: %v, output:\n%s", args, err, out)
			}
		})
	}
}

func TestQuietKlog(t *testing.T) {
	quietKlog()
	_, errOut := capture(t, func() {
		klog.Warning("client warning")
		klog.Error("client error")
		klog.Flush()
	})
	if errOut != "" {
		t.Errorf("klog wrote to stderr: %q", errOut)
	}
}
