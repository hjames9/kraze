package cli

import (
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/hjames9/kraze/internal/history"
	"github.com/hjames9/kraze/internal/state"
	"github.com/spf13/cobra"
)

// commandRuns is every command that loads a config, with the arguments it
// needs to get that far
var commandRuns = []struct {
	name string
	run  func(*cobra.Command, []string) error
	args []string
}{
	{"up", runUp, nil},
	{"down", runDown, nil},
	{"status", runStatus, nil},
	{"plan", runPlan, nil},
	{"init", runInit, nil},
	{"destroy", runDestroy, nil},
	{"load-image", runLoadImage, []string{"app:dev"}},
	{"port-forward", runPortForward, []string{"a", "8080"}},
	{"history", runHistory, nil},
	{"list-images", runListImages, nil},
	{"validate", validateCmd.RunE, nil},
	{"pack", runPack, nil},
}

// Every command stops at a config it can't load, before touching anything
func TestCommandsRefuseUnloadableConfig(t *testing.T) {
	for name, setup := range map[string]func(t *testing.T) string{
		"invalid yaml": func(t *testing.T) string { return useConfig(t, "project: [\n") },
		"bad archive": func(t *testing.T) string {
			path := writeConfig(t, filepath.Join(t.TempDir(), "broken.tar.gz"), "not gzip")
			configFiles = []string{path}
			return path
		},
	} {
		for _, c := range commandRuns {
			t.Run(name+"/"+c.name, func(t *testing.T) {
				env := withFakeDeps(t)
				setup(t)
				if _, err := runCmd(t, c.run, c.args...); err == nil {
					t.Fatal("want an error")
				}
				if len(env.events) != 0 || len(env.skipTLS) != 0 || env.dockerChecks != 0 {
					t.Errorf("nothing should happen: events %v, clients %d, docker checks %d", env.events, len(env.skipTLS), env.dockerChecks)
				}
			})
		}
	}
}

// Every command on a kind cluster stops when Docker is unavailable, before
// touching the cluster
func TestCommandsNeedDockerForKind(t *testing.T) {
	for _, c := range commandRuns {
		if c.name == "validate" || c.name == "pack" {
			continue // offline commands
		}
		t.Run(c.name, func(t *testing.T) {
			env := withFakeDeps(t)
			useConfig(t, kindConfig)
			env.cluster.exists = true
			env.dockerErr = errBoom
			if _, err := runCmd(t, c.run, c.args...); !errors.Is(err, errBoom) {
				t.Fatalf("err = %v, want the Docker error", err)
			}
			if len(env.events) != 0 || len(env.skipTLS) != 0 {
				t.Errorf("nothing should happen: events %v, clients %d", env.events, len(env.skipTLS))
			}
		})
	}
}

// Commands report a kubeconfig or client failure for an existing cluster
func TestCommandsReportClusterAccessFailures(t *testing.T) {
	for _, c := range []struct {
		name string
		run  func(*cobra.Command, []string) error
		args []string
	}{{"up", runUp, nil}, {"down", runDown, nil}, {"status", runStatus, nil}, {"history", runHistory, nil}, {"port-forward", runPortForward, []string{"a", "80"}}} {
		for failure, setup := range map[string]func(env *fakeEnv){
			"cluster check": func(env *fakeEnv) { env.cluster.errs["ClusterExists"] = errBoom },
			"kubeconfig":    func(env *fakeEnv) { env.cluster.errs["GetKubeConfig"] = errBoom },
		} {
			t.Run(c.name+"/"+failure, func(t *testing.T) {
				env := withFakeDeps(t)
				useConfig(t, kindConfig)
				env.cluster.exists = true
				setup(env)
				if _, err := runCmd(t, c.run, c.args...); !errors.Is(err, errBoom) {
					t.Errorf("err = %v", err)
				}
			})
		}
	}

	// External: the kubeconfig can't be read
	for _, c := range []struct {
		name string
		run  func(*cobra.Command, []string) error
	}{{"up", runUp}, {"down", runDown}, {"status", runStatus}, {"init", runInit}, {"history", runHistory}} {
		t.Run(c.name+"/external kubeconfig", func(t *testing.T) {
			env := withFakeDeps(t)
			useConfig(t, externalConfig)
			env.cluster.errs["GetKubeconfigForExternalCluster"] = errBoom
			if _, err := runCmd(t, c.run); err == nil || !strings.Contains(err.Error(), "failed to get kubeconfig for external cluster") {
				t.Errorf("err = %v", err)
			}
		})
	}

	// A client that can't be created
	for _, c := range []struct {
		name string
		run  func(*cobra.Command, []string) error
	}{{"up", runUp}, {"down", runDown}, {"init", runInit}, {"history", runHistory}} {
		t.Run(c.name+"/client", func(t *testing.T) {
			env := withFakeDeps(t)
			useConfig(t, kindConfig)
			env.cluster.exists = true
			env.clientsetErr = errBoom
			if _, err := runCmd(t, c.run); err == nil || !strings.Contains(err.Error(), "failed to create Kubernetes client") {
				t.Errorf("err = %v", err)
			}
		})
	}
}

func TestCommandFlagValidation(t *testing.T) {
	tests := []struct {
		name  string
		run   func(*cobra.Command, []string) error
		args  []string
		flags func()
		want  string
	}{
		{"up names and labels", runUp, []string{"a"}, func() { upLabels = []string{"x=y"} }, "cannot specify both service names and labels"},
		{"up --no-deps with labels", runUp, nil, func() { upNoDeps = true; upLabels = []string{"x=y"} }, "--no-deps cannot be used with --label"},
		{"up --no-deps alone", runUp, nil, func() { upNoDeps = true }, "--no-deps requires specific service names"},
		{"up unknown service", runUp, []string{"nope"}, nil, "service 'nope' not found"},
		{"up --no-deps unknown service", runUp, []string{"nope"}, func() { upNoDeps = true }, "service 'nope' not found"},
		{"up unmatched label", runUp, nil, func() { upLabels = []string{"x=y"} }, "failed to filter services by labels"},
		{"down names and labels", runDown, []string{"a"}, func() { downLabels = []string{"x=y"} }, "cannot specify both service names and labels"},
		{"down unknown service", runDown, []string{"nope"}, nil, "service 'nope' not found"},
		{"down unmatched label", runDown, nil, func() { downLabels = []string{"x=y"} }, "failed to filter services by labels"},
		{"status names and labels", runStatus, []string{"a"}, func() { statusLabels = []string{"x=y"} }, "cannot specify both service names and labels"},
		{"status unknown service", runStatus, []string{"nope"}, nil, "service 'nope' not found"},
		{"status unmatched label", runStatus, nil, func() { statusLabels = []string{"x=y"} }, "failed to filter services by labels"},
		{"plan names and labels", runPlan, []string{"a"}, func() { planLabels = []string{"x=y"} }, "cannot specify both service names and labels"},
		{"plan unknown service", runPlan, []string{"nope"}, nil, "service 'nope' not found"},
		{"plan unmatched label", runPlan, nil, func() { planLabels = []string{"x=y"} }, "failed to filter services by labels"},
		{"history output", runHistory, nil, func() { historyOutput = "xml" }, "invalid output format 'xml'"},
		{"history limit", runHistory, nil, func() { historyLimit = -1 }, "--limit must be 0 (all) or a positive number"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := withFakeDeps(t)
			useConfig(t, kindConfig)
			env.cluster.exists = true
			if tt.flags != nil {
				tt.flags()
			}
			if _, err := runCmd(t, tt.run, tt.args...); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("err = %v, want %q", err, tt.want)
			}
		})
	}
}

const labeledKindConfig = `project: web
cluster:
  name: c
services:
  db:
    type: helm
    path: ./charts/db
    namespace: data
    labels: {tier: data}
    post_ready_delay: 0s
  api:
    type: helm
    chart: api
    repo: oci://registry.example/charts
    namespace: shop
    labels: {tier: backend}
    depends_on: [db]
    post_ready_delay: 0s
  extra:
    type: manifests
    path: ./extra.yaml
    namespace: shop
    labels: {tier: extra}
    post_ready_delay: 0s
`

func TestCommandsFilterByLabel(t *testing.T) {
	env := withFakeDeps(t)
	useConfig(t, labeledKindConfig)
	env.cluster.exists = true

	// up includes dependencies of what the label matches
	upLabels = []string{"tier=backend"}
	if _, err := runCmd(t, runUp); err != nil {
		t.Fatal(err)
	}
	if got := installOrder(env); !slices.Equal(got, []string{"db", "api"}) {
		t.Errorf("installed %v, want db then api", got)
	}

	// status and down take exactly what matches
	statusLabels = []string{"tier=backend"}
	out, err := runCmd(t, runStatus)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "api") || strings.Contains(out, "db  ") {
		t.Errorf("status:\n%s", out)
	}
	downLabels = []string{"tier=backend"}
	if _, err := runCmd(t, runDown); err != nil {
		t.Fatal(err)
	}
	if got := uninstallOrder(env); !slices.Equal(got, []string{"api"}) {
		t.Errorf("uninstalled %v, want only api", got)
	}

	// plan with a label or a name includes dependencies
	planLabels = []string{"tier=backend"}
	if out, err := runCmd(t, runPlan); err != nil || !strings.Contains(out, "db") {
		t.Errorf("plan by label: %v\n%s", err, out)
	}
	planLabels = nil
	if out, err := runCmd(t, runPlan, "api"); err != nil || !strings.Contains(out, "db") {
		t.Errorf("plan by name: %v\n%s", err, out)
	}
	statusLabels = nil
	if out, err := runCmd(t, runStatus, "extra"); err != nil || strings.Contains(out, "api") {
		t.Errorf("status by name: %v\n%s", err, out)
	}
}

func TestPlanOutputDetails(t *testing.T) {
	env := withFakeDeps(t)
	useConfig(t, `project: web
cluster:
  name: c
  version: 1.37.0
  network: dev-net
  ipv4_address: 172.30.0.10
services:
  local:
    type: helm
    path: ./charts/local
    namespace: shop
  pinned:
    type: helm
    chart: pinned
    repo: oci://registry.example/charts
    version: 2.0.0
    namespace: shop
  remote:
    type: manifests
    path: https://example.com/app.yaml
    namespace: shop
  taken:
    type: helm
    chart: taken
    repo: oci://registry.example/charts
    namespace: shop
  off:
    type: helm
    chart: off
    repo: oci://registry.example/charts
    enabled: false
`)
	env.cluster.exists = true
	other := state.New("c", false, false, 0, false, 0)
	other.SetProject("billing")
	other.MarkServiceInstalledWithNamespace("taken", "shop", true)
	env.saveState(t, other)

	out, err := runCmd(t, runPlan)
	if err != nil {
		t.Fatal(err)
	}
	assertContains(t, out,
		"Version: 1.37.0", "Network: dev-net (IP: 172.30.0.10)",
		"local chart", "helm chart pinned@2.0.0", "manifests from remote URL",
		"already installed in namespace 'shop' by project billing",
		"Disabled in configuration", "1 conflict", "1 skipped",
	)

	// An external cluster names its context
	withFakeDeps(t)
	useConfig(t, strings.Replace(externalConfig, "    enabled: true\n", "    enabled: true\n    context: prod\n", 1))
	if out, err := runCmd(t, runPlan); err != nil || !strings.Contains(out, "(context: prod)") {
		t.Errorf("external plan: %v\n%s", err, out)
	}
}

func TestValidateRun(t *testing.T) {
	withFakeDeps(t)
	useConfig(t, strings.Replace(kindConfig, "  name: c\n", "  name: c\n  version: 1.37.0\n  node_image: kindest/node:v1.37.0\n", 1)+
		"  off:\n    type: helm\n    chart: off\n    repo: oci://r/c\n    enabled: false\n")
	verbose = true
	out, err := runCmd(t, validateCmd.RunE)
	if err != nil {
		t.Fatal(err)
	}
	assertContains(t, out, "Configuration is valid", "Kubernetes version: 1.37.0", "Node image: kindest/node:v1.37.0",
		"Services: 5", "- d (helm)", "depends_on: [b c]")

	useConfig(t, "project: web\ncluster:\n  name: c\nservices:\n  a:\n    type: helm\n")
	if _, err := runCmd(t, validateCmd.RunE); err == nil || strings.Count(err.Error(), "validation failed") != 1 {
		t.Errorf("err = %v, want 'validation failed' once", err)
	}
}

func TestCompletion(t *testing.T) {
	withFakeDeps(t)
	useConfig(t, kindConfig)
	names, directive := getServiceNames(&cobra.Command{}, nil, "")
	slices.Sort(names)
	if !slices.Equal(names, []string{"a", "b", "c", "d"}) || directive != cobra.ShellCompDirectiveNoFileComp {
		t.Errorf("names = %v, directive %v", names, directive)
	}
	useConfig(t, "project: [\n")
	if names, _ := getServiceNames(&cobra.Command{}, nil, ""); names != nil {
		t.Errorf("a broken config completes nothing, got %v", names)
	}

	for _, shell := range []string{"bash", "zsh", "fish", "powershell"} {
		var err error
		out := captureStdout(t, func() { err = completionCmd.RunE(completionCmd, []string{shell}) })
		if err != nil || !strings.Contains(out, "kraze") {
			t.Errorf("%s completion: %v, %d bytes", shell, err, len(out))
		}
	}
	if err := completionCmd.Args(completionCmd, []string{"tcsh"}); err == nil {
		t.Error("tcsh isn't a supported shell")
	}
}

func TestRootAccessors(t *testing.T) {
	resetFlags(t)
	setVar(t, &version, "")
	setVar(t, &gitCommit, "")
	setVar(t, &buildDate, "")
	SetVersionInfo("1.2.3", "abc", "today")
	if version != "1.2.3" || gitCommit != "abc" || buildDate != "today" {
		t.Errorf("version info = %s %s %s", version, gitCommit, buildDate)
	}
	configFiles = []string{"a.yml"}
	verbose, dryRun = true, true
	if !slices.Equal(GetConfigFiles(), []string{"a.yml"}) || !IsVerbose() || !IsDryRun() {
		t.Error("the accessors should return the flags")
	}
}

func TestParsePortMappingErrors(t *testing.T) {
	for spec, want := range map[string]string{
		"x":        "invalid port number 'x'",
		"80:x":     "invalid remote port 'x'",
		"1:2:3":    "expected format [LOCAL_PORT:]REMOTE_PORT",
		"0:80":     "local port 0 out of range",
		"80:70000": "remote port 70000 out of range",
	} {
		if _, err := parsePortMapping(spec); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: err = %v, want %q", spec, err, want)
		}
	}
}

func TestColorResult(t *testing.T) {
	for _, r := range []history.Result{history.ResultSucceeded, history.ResultFailed, history.ResultRunning, history.ResultInterrupted} {
		if got := colorResult(r, "x"); !strings.Contains(got, "x") {
			t.Errorf("%s: %q", r, got)
		}
	}
}
