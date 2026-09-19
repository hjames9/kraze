package cli

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

const downTestConfig = `
cluster:
  name: down-test-cluster
  external:
    enabled: true

services:
  alpha:
    type: manifests
    path: ./manifests/alpha
    namespace: default
    enabled: true
    labels:
      tier: backend

  beta:
    type: manifests
    path: ./manifests/beta
    namespace: default
    enabled: false
    labels:
      tier: backend

  gamma:
    type: manifests
    path: ./manifests/gamma
    namespace: default
    enabled: true
    labels:
      tier: frontend

  delta:
    type: manifests
    path: ./manifests/delta
    namespace: default
    enabled: false
    depends_on:
      - alpha

  epsilon:
    type: manifests
    path: ./manifests/epsilon
    namespace: default
    enabled: false
    depends_on:
      - delta
`

// captureStdout runs fn while redirecting os.Stdout, returning everything fn wrote.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()

	orig := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("failed to create pipe: %v", err)
	}
	os.Stdout = w

	fn()

	closeErr := w.Close()
	os.Stdout = orig
	if closeErr != nil {
		t.Fatalf("failed to close pipe: %v", closeErr)
	}

	var buf bytes.Buffer
	if _, err := io.Copy(&buf, r); err != nil {
		t.Fatalf("failed to read captured stdout: %v", err)
	}
	return buf.String()
}

// disabledAnnotation is the suffix runDown appends to a service's dry-run
// line when that service is disabled in config.
const disabledAnnotation = " (disabled in config, removed only if still installed)"

// downDryRunOpts configures a runDownDryRunLinesInOrder call.
type downDryRunOpts struct {
	args         []string
	labels       []string
	disabledOnly bool
}

// runDownDryRunLinesInOrder writes downTestConfig to a temp file, runs
// `kraze down` in dry-run mode (which returns once the uninstall order is
// computed, before touching Docker/kind/Kubernetes) with the given options,
// and returns the raw "- name[ (annotation)]" lines it reports it would
// uninstall, in the order runDown printed them (i.e. uninstall order).
func runDownDryRunLinesInOrder(t *testing.T, opts downDryRunOpts) []string {
	t.Helper()

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "kraze.yml")
	if err := os.WriteFile(cfgPath, []byte(downTestConfig), 0644); err != nil {
		t.Fatalf("failed to write config: %v", err)
	}

	origConfigFiles, origDryRun, origLabels, origDisabledOnly := configFiles, dryRun, downLabels, downDisabledOnly
	defer func() {
		configFiles, dryRun, downLabels, downDisabledOnly = origConfigFiles, origDryRun, origLabels, origDisabledOnly
	}()

	configFiles = []string{cfgPath}
	dryRun = true
	downLabels = opts.labels
	downDisabledOnly = opts.disabledOnly

	var runErr error
	out := captureStdout(t, func() {
		runErr = runDown(downCmd, opts.args)
	})
	if runErr != nil {
		t.Fatalf("runDown returned error: %v", runErr)
	}

	var got []string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "- ") {
			got = append(got, strings.TrimPrefix(line, "- "))
		}
	}
	return got
}

// runDownDryRunLines is like runDownDryRunLinesInOrder but sorted, for tests
// that only care about which services were selected, not the order.
func runDownDryRunLines(t *testing.T, opts downDryRunOpts) []string {
	t.Helper()

	got := runDownDryRunLinesInOrder(t, opts)
	sort.Strings(got)
	return got
}

// runDownDryRun is like runDownDryRunLines but strips the disabled
// annotation, returning bare service names for tests that only care about
// which services were selected, not how they're annotated.
func runDownDryRun(t *testing.T, opts downDryRunOpts) []string {
	t.Helper()

	lines := runDownDryRunLines(t, opts)
	names := make([]string, len(lines))
	for i, line := range lines {
		names[i] = strings.TrimSuffix(line, disabledAnnotation)
	}
	return names
}

func assertServices(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("expected services %v, got %v", want, got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("expected services %v, got %v", want, got)
			break
		}
	}
}

func TestRunDownSkipsDisabledServicesWhenUnfiltered(t *testing.T) {
	// Plain `kraze down` (no args/labels/--disabled) leaves disabled services
	// alone entirely; they don't even appear in the preview.
	got := runDownDryRun(t, downDryRunOpts{})
	assertServices(t, got, []string{"alpha", "gamma"})
}

func TestRunDownUninstallsExplicitlyNamedDisabledService(t *testing.T) {
	got := runDownDryRun(t, downDryRunOpts{args: []string{"beta"}})
	want := []string{"beta"}

	if len(got) != 1 || got[0] != want[0] {
		t.Errorf("expected explicitly requested disabled service to be included, got %v", got)
	}
}

func TestRunDownUninstallsDisabledServiceMatchedByLabel(t *testing.T) {
	got := runDownDryRun(t, downDryRunOpts{labels: []string{"tier=backend"}})
	assertServices(t, got, []string{"alpha", "beta"})
}

func TestRunDownAllSkipsDisabledEvenWhenMixedWithEnabledNamed(t *testing.T) {
	// Sanity check: requesting only enabled services explicitly should not
	// pull in unrelated disabled services.
	got := runDownDryRun(t, downDryRunOpts{args: []string{"alpha", "gamma"}})
	assertServices(t, got, []string{"alpha", "gamma"})
}

func TestRunDownDisabledFlagTargetsOnlyDisabledServices(t *testing.T) {
	// --disabled must select every disabled service (beta, delta, epsilon)
	// and none of the enabled ones (alpha, gamma), even though delta depends
	// on the enabled service alpha (which is excluded from the result).
	got := runDownDryRun(t, downDryRunOpts{disabledOnly: true})
	assertServices(t, got, []string{"beta", "delta", "epsilon"})
}

func TestRunDownDisabledFlagAnnotatesEveryLine(t *testing.T) {
	lines := runDownDryRunLines(t, downDryRunOpts{disabledOnly: true})
	for _, line := range lines {
		if !strings.HasSuffix(line, disabledAnnotation) {
			t.Errorf("expected line %q to carry the disabled annotation", line)
		}
	}
}

func TestRunDownDisabledFlagOrdersByDependency(t *testing.T) {
	// epsilon (disabled) depends_on delta (disabled), which depends_on alpha
	// (enabled, excluded from the result). Uninstall order is reverse
	// dependency order, so epsilon must come before delta.
	lines := runDownDryRunLinesInOrder(t, downDryRunOpts{disabledOnly: true})

	epsilonIdx, deltaIdx := -1, -1
	for i, line := range lines {
		name := strings.TrimSuffix(line, disabledAnnotation)
		switch name {
		case "epsilon":
			epsilonIdx = i
		case "delta":
			deltaIdx = i
		}
	}

	if epsilonIdx == -1 || deltaIdx == -1 {
		t.Fatalf("expected both 'epsilon' and 'delta' in output, got %v", lines)
	}
	if epsilonIdx >= deltaIdx {
		t.Errorf("expected 'epsilon' (depends on delta) to be uninstalled before 'delta', got order %v", lines)
	}
}

func TestRunDownDisabledFlagRejectsServiceNames(t *testing.T) {
	origConfigFiles, origDryRun, origDisabledOnly := configFiles, dryRun, downDisabledOnly
	defer func() {
		configFiles, dryRun, downDisabledOnly = origConfigFiles, origDryRun, origDisabledOnly
	}()

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "kraze.yml")
	if err := os.WriteFile(cfgPath, []byte(downTestConfig), 0644); err != nil {
		t.Fatalf("failed to write config: %v", err)
	}
	configFiles = []string{cfgPath}
	dryRun = true
	downDisabledOnly = true

	err := runDown(downCmd, []string{"beta"})
	if err == nil {
		t.Fatal("expected an error when combining --disabled with explicit service names")
	}
}

func TestRunDownDisabledFlagWithNoDisabledServices(t *testing.T) {
	const cfgNoDisabled = `
cluster:
  name: down-test-cluster
  external:
    enabled: true

services:
  alpha:
    type: manifests
    path: ./manifests/alpha
    namespace: default
    enabled: true
`

	origConfigFiles, origDryRun, origDisabledOnly := configFiles, dryRun, downDisabledOnly
	defer func() {
		configFiles, dryRun, downDisabledOnly = origConfigFiles, origDryRun, origDisabledOnly
	}()

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "kraze.yml")
	if err := os.WriteFile(cfgPath, []byte(cfgNoDisabled), 0644); err != nil {
		t.Fatalf("failed to write config: %v", err)
	}
	configFiles = []string{cfgPath}
	dryRun = true
	downDisabledOnly = true

	var runErr error
	out := captureStdout(t, func() {
		runErr = runDown(downCmd, nil)
	})
	if runErr != nil {
		t.Fatalf("runDown returned error: %v", runErr)
	}
	if strings.Contains(out, "alpha") {
		t.Errorf("expected no mention of enabled service 'alpha', got output: %q", out)
	}
}
