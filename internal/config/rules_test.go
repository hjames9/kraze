package config

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"
)

// writeConfigs writes each config to its own file in one directory and
// returns their paths
func writeConfigs(t *testing.T, contents ...string) []string {
	t.Helper()
	dir := t.TempDir()
	var paths []string
	for i, content := range contents {
		path := filepath.Join(dir, "kraze"+strings.Repeat("x", i)+".yml")
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, path)
	}
	return paths
}

const mergeBase = "project: web\ncluster:\n  name: c\n"

// Cluster settings from several files merge, and settings that can't both
// hold are refused
func TestParseMultipleClusterRules(t *testing.T) {
	tests := []struct {
		name    string
		base    string // appended under cluster: in the first file
		other   string // appended under cluster: in the second file
		wantErr string
		check   func(t *testing.T, c ClusterConfig)
	}{
		{name: "version from the second file", other: "  version: 1.37.0\n",
			check: func(t *testing.T, c ClusterConfig) {
				if c.Version != "1.37.0" {
					t.Errorf("version = %q", c.Version)
				}
			}},
		{name: "node image from the second file", other: "  node_image: kindest/node:v1\n",
			check: func(t *testing.T, c ClusterConfig) {
				if c.NodeImage != "kindest/node:v1" {
					t.Errorf("node image = %q", c.NodeImage)
				}
			}},
		{name: "node image conflict", base: "  node_image: a\n", other: "  node_image: b\n", wantErr: "cluster.node_image conflict"},
		{name: "same node image", base: "  node_image: a\n", other: "  node_image: a\n"},
		{name: "preload images union", base: "  preload_images: [a, b]\n", other: "  preload_images: [b, c]\n",
			check: func(t *testing.T, c ClusterConfig) {
				if !slices.Equal(c.PreloadImages, []string{"a", "b", "c"}) {
					t.Errorf("preload = %v", c.PreloadImages)
				}
			}},
		{name: "insecure registries union", base: "  insecure_registries: [r1]\n", other: "  insecure_registries: [r1, r2]\n",
			check: func(t *testing.T, c ClusterConfig) {
				if !slices.Equal(c.InsecureRegistries, []string{"r1", "r2"}) {
					t.Errorf("registries = %v", c.InsecureRegistries)
				}
			}},
		{name: "networking from the second file", other: "  networking:\n    podSubnet: 10.0.0.0/16\n",
			check: func(t *testing.T, c ClusterConfig) {
				if c.Networking == nil || c.Networking.PodSubnet != "10.0.0.0/16" {
					t.Errorf("networking = %+v", c.Networking)
				}
			}},
		{name: "same networking", base: "  networking:\n    podSubnet: x\n", other: "  networking:\n    podSubnet: x\n"},
		{name: "networking conflict", base: "  networking:\n    podSubnet: x\n", other: "  networking:\n    podSubnet: y\n", wantErr: "cluster.networking conflict"},
		{name: "external from the second file", other: "  external:\n    enabled: true\n",
			check: func(t *testing.T, c ClusterConfig) {
				if !c.IsExternal() {
					t.Error("should be external")
				}
			}},
		{name: "same external", base: "  external:\n    enabled: true\n", other: "  external:\n    enabled: true\n"},
		{name: "external conflict", base: "  external:\n    enabled: true\n", other: "  external:\n    enabled: true\n    context: prod\n", wantErr: "cluster.external conflict"},
		{name: "network conflict", base: "  network: a\n", other: "  network: b\n", wantErr: "cluster.network conflict"},
		{name: "subnet conflict", base: "  subnet: 10.1.0.0/16\n", other: "  subnet: 10.2.0.0/16\n", wantErr: "cluster.subnet conflict"},
		{name: "ipv4 conflict", base: "  ipv4_address: 10.1.0.2\n", other: "  ipv4_address: 10.1.0.3\n", wantErr: "cluster.ipv4_address conflict"},
		{name: "network fields from the second file", other: "  network: dev\n  subnet: 10.1.0.0/16\n  ipv4_address: 10.1.0.2\n",
			check: func(t *testing.T, c ClusterConfig) {
				if c.Network != "dev" || c.Subnet != "10.1.0.0/16" || c.IPv4Address != "10.1.0.2" {
					t.Errorf("network = %q %q %q", c.Network, c.Subnet, c.IPv4Address)
				}
			}},
		{name: "gpu from the second file only", other: "  gpu:\n    amd:\n      enabled: true\n",
			check: func(t *testing.T, c ClusterConfig) {
				if !c.GPU.IsAMDEnabled() || c.GPU.IsNvidiaEnabled() {
					t.Errorf("gpu = %+v", c.GPU)
				}
			}},
		{name: "gpu in the first file only", base: "  gpu:\n    nvidia:\n      enabled: true\n",
			check: func(t *testing.T, c ClusterConfig) {
				if !c.GPU.IsNvidiaEnabled() {
					t.Errorf("gpu = %+v", c.GPU)
				}
			}},
		{name: "gpu vendors from each file", base: "  gpu:\n    nvidia:\n      enabled: true\n", other: "  gpu:\n    amd:\n      enabled: true\n",
			check: func(t *testing.T, c ClusterConfig) {
				if !c.GPU.IsAMDEnabled() || !c.GPU.IsNvidiaEnabled() {
					t.Errorf("gpu = %+v", c.GPU)
				}
			}},
		{name: "nvidia enabled by either", base: "  gpu:\n    nvidia:\n      enabled: true\n", other: "  gpu:\n    nvidia:\n      enabled: false\n",
			check: func(t *testing.T, c ClusterConfig) {
				if !c.GPU.IsNvidiaEnabled() {
					t.Errorf("gpu = %+v", c.GPU)
				}
			}},
		{name: "nvidia from the second file", base: "  gpu:\n    amd:\n      enabled: true\n", other: "  gpu:\n    nvidia:\n      enabled: true\n",
			check: func(t *testing.T, c ClusterConfig) {
				if !c.GPU.IsNvidiaEnabled() || !c.GPU.IsAMDEnabled() {
					t.Errorf("gpu = %+v", c.GPU)
				}
			}},
		{name: "amd enabled by either", base: "  gpu:\n    amd:\n      enabled: false\n", other: "  gpu:\n    amd:\n      enabled: true\n",
			check: func(t *testing.T, c ClusterConfig) {
				if !c.GPU.IsAMDEnabled() {
					t.Errorf("gpu = %+v", c.GPU)
				}
			}},
		{name: "proxy from the second file", other: "  proxy:\n    http_proxy: http://p:1\n",
			check: func(t *testing.T, c ClusterConfig) {
				if c.Proxy == nil || c.Proxy.HTTPProxy != "http://p:1" {
					t.Errorf("proxy = %+v", c.Proxy)
				}
			}},
		{name: "proxy from the first file", base: "  proxy:\n    http_proxy: http://p:1\n",
			check: func(t *testing.T, c ClusterConfig) {
				if c.Proxy == nil || c.Proxy.HTTPProxy != "http://p:1" {
					t.Errorf("proxy = %+v", c.Proxy)
				}
			}},
		{name: "proxy fields from each file", base: "  proxy:\n    enabled: true\n    http_proxy: http://p:1\n", other: "  proxy:\n    https_proxy: http://p:2\n",
			check: func(t *testing.T, c ClusterConfig) {
				p := c.Proxy
				if p == nil || p.HTTPProxy != "http://p:1" || p.HTTPSProxy != "http://p:2" || p.Enabled == nil || !*p.Enabled {
					t.Errorf("proxy = %+v", p)
				}
			}},
		{name: "proxy enabled from the second file", base: "  proxy:\n    http_proxy: http://p:1\n", other: "  proxy:\n    enabled: false\n",
			check: func(t *testing.T, c ClusterConfig) {
				if c.Proxy.Enabled == nil || *c.Proxy.Enabled {
					t.Errorf("proxy = %+v", c.Proxy)
				}
			}},
		{name: "proxy enabled conflict", base: "  proxy:\n    enabled: true\n", other: "  proxy:\n    enabled: false\n", wantErr: "cluster.proxy.enabled conflict"},
		{name: "https proxy conflict", base: "  proxy:\n    https_proxy: http://a\n", other: "  proxy:\n    https_proxy: http://b\n", wantErr: "cluster.proxy.https_proxy conflict"},
		{name: "kind node config from the second file", other: "  config:\n    - role: worker\n      replicas: 2\n",
			check: func(t *testing.T, c ClusterConfig) {
				if len(c.Config) != 1 || c.Config[0].Replicas != 2 {
					t.Errorf("nodes = %+v", c.Config)
				}
			}},
		{name: "replicas from the second file", base: "  config:\n    - role: worker\n", other: "  config:\n    - role: worker\n      replicas: 3\n",
			check: func(t *testing.T, c ClusterConfig) {
				if len(c.Config) != 1 || c.Config[0].Replicas != 3 {
					t.Errorf("nodes = %+v", c.Config)
				}
			}},
		{name: "same mount in both files", base: "  config:\n    - role: worker\n      extraMounts:\n        - {hostPath: /a, containerPath: /a}\n",
			other: "  config:\n    - role: worker\n      extraMounts:\n        - {hostPath: /a, containerPath: /a}\n",
			check: func(t *testing.T, c ClusterConfig) {
				if len(c.Config[0].ExtraMounts) != 1 {
					t.Errorf("mounts = %+v", c.Config[0].ExtraMounts)
				}
			}},
		{name: "no_proxy from the first file only", base: "  proxy:\n    no_proxy: localhost\n", other: "  proxy:\n    http_proxy: http://p:1\n",
			check: func(t *testing.T, c ClusterConfig) {
				if c.Proxy.NoProxy != "localhost" {
					t.Errorf("no_proxy = %q", c.Proxy.NoProxy)
				}
			}},
		{name: "replicas conflict", base: "  config:\n    - role: worker\n      replicas: 1\n", other: "  config:\n    - role: worker\n      replicas: 2\n", wantErr: "replicas"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			paths := writeConfigs(t, mergeBase+tt.base, "cluster:\n  name: c\n"+tt.other)
			cfg, err := ParseMultiple(paths)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if tt.check != nil {
				tt.check(t, cfg.Cluster)
			}
		})
	}
}

// Each file is checked on its own before the merge; references across files
// are checked after it
func TestParseMultipleFileErrors(t *testing.T) {
	svc := func(name, extra string) string {
		return "  " + name + ":\n    type: manifests\n    path: ./x.yaml\n" + extra
	}
	tests := []struct {
		name    string
		files   []string
		wantErr string
		invalid bool // errors.Is(err, ErrInvalid)
	}{
		{name: "bad project name", files: []string{mergeBase, "project: Not_A_Label\n"}, wantErr: "project"},
		{name: "gpu on an external cluster", files: []string{mergeBase, "cluster:\n  name: c\n  external:\n    enabled: true\n  gpu:\n    nvidia:\n      enabled: true\n"}, wantErr: "GPU support is only available for kind clusters"},
		{name: "invalid service", files: []string{mergeBase, "services:\n  bad:\n    type: kustomize\n"}, wantErr: "service 'bad'"},
		{name: "broken yaml", files: []string{mergeBase, "services: [\n"}, wantErr: "yaml"},
		{name: "missing dependency", files: []string{mergeBase + "services:\n" + svc("a", "    depends_on: [nope]\n"), "services:\n" + svc("b", "")},
			wantErr: "dependency 'nope' not found", invalid: true},
		{name: "depends on a disabled service", files: []string{mergeBase + "services:\n" + svc("a", "    depends_on: [b]\n"), "services:\n" + svc("b", "    enabled: false\n")},
			wantErr: "depends on disabled service 'b'", invalid: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseMultiple(writeConfigs(t, tt.files...))
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want %q", err, tt.wantErr)
			}
			if tt.invalid && !errors.Is(err, ErrInvalid) {
				t.Errorf("want ErrInvalid, got %v", err)
			}
		})
	}

	// A disabled service may depend on another disabled one
	files := []string{mergeBase + "services:\n" + svc("a", "    enabled: false\n    depends_on: [b]\n"), "services:\n" + svc("b", "    enabled: false\n")}
	if _, err := ParseMultiple(writeConfigs(t, files...)); err != nil {
		t.Errorf("disabled on disabled: %v", err)
	}
	if _, err := ParseMultiple([]string{filepath.Join(t.TempDir(), "missing.yml")}); err == nil {
		t.Error("want an error for a missing file")
	}
}

func labeledConfig() *Config {
	return &Config{Services: map[string]ServiceConfig{
		"db":     {Name: "db", Labels: map[string]string{"tier": "data"}},
		"cache":  {Name: "cache", Labels: map[string]string{"tier": "data", "env": "dev"}},
		"api":    {Name: "api", Labels: map[string]string{"tier": "backend"}, DependsOn: []string{"db", "cache"}},
		"web":    {Name: "web", Labels: map[string]string{"tier": "frontend"}, DependsOn: []string{"api"}},
		"loner":  {Name: "loner"},
		"broken": {Name: "broken", Labels: map[string]string{"tier": "broken"}, DependsOn: []string{"ghost"}},
	}}
}

func serviceNames(services map[string]ServiceConfig) []string {
	names := make([]string, 0, len(services))
	for name := range services {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

func TestFilterServicesWithDependencies(t *testing.T) {
	cfg := labeledConfig()
	got, err := cfg.FilterServicesWithDependencies([]string{"web"})
	if err != nil || !slices.Equal(serviceNames(got), []string{"api", "cache", "db", "web"}) {
		t.Errorf("web with dependencies = %v, %v", serviceNames(got), err)
	}
	// Shared dependencies are added once
	got, _ = cfg.FilterServicesWithDependencies([]string{"api", "db"})
	if !slices.Equal(serviceNames(got), []string{"api", "cache", "db"}) {
		t.Errorf("api and db = %v", serviceNames(got))
	}
	if all, _ := cfg.FilterServicesWithDependencies(nil); len(all) != len(cfg.Services) {
		t.Error("no names means every service")
	}
	if _, err := cfg.FilterServicesWithDependencies([]string{"nope"}); err == nil || !strings.Contains(err.Error(), "service 'nope' not found") {
		t.Errorf("err = %v", err)
	}
	if _, err := cfg.FilterServicesWithDependencies([]string{"broken"}); err == nil || !strings.Contains(err.Error(), "'ghost' not found") {
		t.Errorf("a missing dependency: %v", err)
	}
}

func TestFilterServicesByLabels(t *testing.T) {
	cfg := labeledConfig()
	tests := []struct {
		labels  []string
		want    []string
		withDep []string
		wantErr string
	}{
		{labels: []string{"tier=data"}, want: []string{"cache", "db"}, withDep: []string{"cache", "db"}},
		{labels: []string{"tier=data", "env=dev"}, want: []string{"cache"}, withDep: []string{"cache"}},
		{labels: []string{"tier=frontend"}, want: []string{"web"}, withDep: []string{"api", "cache", "db", "web"}},
		{labels: []string{"tier=nothing"}, wantErr: "no services found matching"},
		{labels: []string{"tier"}, wantErr: "must be in format 'key=value'"},
		{labels: []string{"tier=broken"}, want: []string{"broken"}, wantErr: "'ghost' not found"},
	}
	for _, tt := range tests {
		got, err := cfg.FilterServicesByLabels(tt.labels)
		if tt.want != nil {
			if err != nil || !slices.Equal(serviceNames(got), tt.want) {
				t.Errorf("%v: got %v, %v; want %v", tt.labels, serviceNames(got), err, tt.want)
			}
		} else if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
			t.Errorf("%v: err = %v, want %q", tt.labels, err, tt.wantErr)
		}

		withDeps, err := cfg.FilterServicesByLabelsWithDependencies(tt.labels)
		if tt.withDep != nil {
			if err != nil || !slices.Equal(serviceNames(withDeps), tt.withDep) {
				t.Errorf("%v with dependencies: got %v, %v; want %v", tt.labels, serviceNames(withDeps), err, tt.withDep)
			}
		} else if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
			t.Errorf("%v with dependencies: err = %v, want %q", tt.labels, err, tt.wantErr)
		}
	}
	if all, _ := cfg.FilterServicesByLabels(nil); len(all) != len(cfg.Services) {
		t.Error("no labels means every service")
	}
}

func TestGetService(t *testing.T) {
	cfg := labeledConfig()
	if svc, ok := cfg.GetService("api"); !ok || svc.Name != "api" {
		t.Errorf("api = %+v, %v", svc, ok)
	}
	if _, ok := cfg.GetService("nope"); ok {
		t.Error("nope shouldn't be found")
	}
	names := cfg.GetAllServiceNames()
	slices.Sort(names)
	if !slices.Equal(names, serviceNames(cfg.Services)) {
		t.Errorf("names = %v", names)
	}
}

func TestValuesFieldMarshalRoundTrip(t *testing.T) {
	for _, in := range []string{"values: one.yaml\n", "values:\n    - a.yaml\n    - b.yaml\n"} {
		var svc struct {
			Values ValuesField `yaml:"values,omitempty"`
		}
		if err := yaml.Unmarshal([]byte(in), &svc); err != nil {
			t.Fatal(err)
		}
		out, err := yaml.Marshal(svc)
		if err != nil || string(out) != in {
			t.Errorf("round trip of %q = %q, %v", in, out, err)
		}
	}
	if v, err := (ValuesField{}).MarshalYAML(); v != nil || err != nil {
		t.Errorf("empty values marshal to %v, %v; want nothing", v, err)
	}
	var bad ValuesField
	if err := yaml.Unmarshal([]byte("{a: b}"), &bad); err == nil || !strings.Contains(err.Error(), "string or array of strings") {
		t.Errorf("err = %v", err)
	}
}

func TestServiceDefaults(t *testing.T) {
	no := false
	if !(&ServiceConfig{}).ShouldCreateNamespace() || (&ServiceConfig{CreateNamespace: &no}).ShouldCreateNamespace() {
		t.Error("create_namespace defaults to true and can be turned off")
	}
	for delay, want := range map[string]time.Duration{"": 3 * time.Second, "0s": 0, "1m": time.Minute} {
		got, err := (&ServiceConfig{PostReadyDelay: delay}).GetPostReadyDelay()
		if err != nil || got != want {
			t.Errorf("post_ready_delay %q = %v, %v; want %v", delay, got, err, want)
		}
	}
	if _, err := (&ServiceConfig{PostReadyDelay: "soon"}).GetPostReadyDelay(); err == nil || !strings.Contains(err.Error(), "invalid post_ready_delay 'soon'") {
		t.Errorf("err = %v", err)
	}
}

func TestServiceValidate(t *testing.T) {
	values := ValuesField{}
	if err := yaml.Unmarshal([]byte("v.yaml"), &values); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		svc  ServiceConfig
		want string // empty: valid
	}{
		{ServiceConfig{Type: "helm", Path: "./c"}, "service name is required"},
		{ServiceConfig{Name: "x"}, "service type is required"},
		{ServiceConfig{Name: "x", Type: "kustomize"}, "type must be 'helm' or 'manifests'"},
		{ServiceConfig{Name: "x", Type: "helm", Path: "./c", Repo: "oci://r"}, "cannot specify both 'path' and 'repo'"},
		{ServiceConfig{Name: "x", Type: "helm"}, "must specify either 'path' or 'repo'"},
		{ServiceConfig{Name: "x", Type: "helm", Repo: "oci://r"}, "chart name is required"},
		{ServiceConfig{Name: "x", Type: "helm", Path: "./c", Values: values, ValuesInline: "a: 1"}, "cannot specify both 'values' and 'values_inline'"},
		{ServiceConfig{Name: "x", Type: "manifests"}, "must specify either 'path' or 'paths'"},
		{ServiceConfig{Name: "x", Type: "helm", Repo: "oci://r", Chart: "c"}, ""},
		{ServiceConfig{Name: "x", Type: "manifests", Paths: []string{"a.yaml"}}, ""},
	}
	for _, tt := range tests {
		err := tt.svc.Validate()
		if tt.want == "" {
			if err != nil {
				t.Errorf("%+v: %v", tt.svc, err)
			}
		} else if err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%+v: err = %v, want %q", tt.svc, err, tt.want)
		}
	}
	if got := (&ValidationError{Message: "just this"}).Error(); got != "just this" {
		t.Errorf("an error without a field = %q", got)
	}
}

func TestParseInsecureRegistry(t *testing.T) {
	valid := map[string]InsecureRegistry{
		"registry.corp":         {Host: "registry.corp"},
		"registry.corp:5000":    {Host: "registry.corp:5000"},
		"HTTP://registry.corp/": {Scheme: "http", Host: "registry.corp"},
		"https://10.0.0.1:8443": {Scheme: "https", Host: "10.0.0.1:8443"},
		"[::1]:5000":            {Host: "[::1]:5000"},
		"localhost":             {Host: "localhost"},
	}
	for entry, want := range valid {
		if got, err := ParseInsecureRegistry(entry); err != nil || got != want {
			t.Errorf("%q = %+v, %v; want %+v", entry, got, err, want)
		}
	}
	for _, entry := range []string{
		"ftp://registry.corp", "registry.corp:", "registry.corp:0", "registry.corp:70000", "registry.corp:port",
		"user@registry.corp", "registry.corp/path", "registry.corp?x=1", "registry.corp#frag", "-bad-.corp", "",
	} {
		if _, err := ParseInsecureRegistry(entry); err == nil {
			t.Errorf("%q should be invalid", entry)
		}
	}
}

func TestParseResolvesAbsoluteValuesFiles(t *testing.T) {
	abs := filepath.Join(t.TempDir(), "values.yaml")
	paths := writeConfigs(t, mergeBase+"services:\n  app:\n    type: helm\n    path: ./chart\n    values: ["+abs+", ./rel.yaml]\n")
	cfg, err := ParseMultiple(paths)
	if err != nil {
		t.Fatal(err)
	}
	files := cfg.Services["app"].Values.Files()
	if files[0] != abs || files[1] != filepath.Join(filepath.Dir(paths[0]), "rel.yaml") {
		t.Errorf("values files = %v", files)
	}
}

func TestSuggestProjectNameTruncates(t *testing.T) {
	long := strings.Repeat("a", 80)
	if got := suggestProjectName(filepath.Join("/tmp", long, "kraze.yml")); got != strings.Repeat("a", 63) {
		t.Errorf("suggestion = %q (%d chars), want 63", got, len(got))
	}
}

// values_inline that isn't YAML before expansion is expanded as text
func TestExpandEnvVarsInUnparsableYAMLText(t *testing.T) {
	t.Setenv("KRAZE_TEST_TAG", "v1")
	if got := expandEnvVarsInYAMLText("image: [${KRAZE_TEST_TAG}"); got != "image: [v1" {
		t.Errorf("got %q", got)
	}
}

func TestMoreConfigErrors(t *testing.T) {
	if _, err := ParseMultiple(writeConfigs(t, mergeBase+"  insecure_registries: [\"ftp://nope\"]\n")); err == nil || !strings.Contains(err.Error(), "insecure_registries") {
		t.Errorf("err = %v", err)
	}
	if _, err := ParseMultiple(writeConfigs(t, mergeBase+"  proxy:\n    http_proxy: http://a\n", "cluster:\n  name: c\n  proxy:\n    http_proxy: http://b\n")); err == nil || !strings.Contains(err.Error(), "cluster.proxy.http_proxy conflict") {
		t.Errorf("err = %v", err)
	}
	if _, err := ParseIdentity(filepath.Join(t.TempDir(), "missing.yml")); err == nil {
		t.Error("want an error for a missing file")
	}
	if _, err := ParseIdentity(writeConfigs(t, "project: [\n")[0]); err == nil {
		t.Error("want an error for a file that doesn't parse")
	}
}

// Errors that aren't yaml type errors (a custom unmarshaler's) pass through
// as they are, and ParseIdentity reports a project that isn't a string
func TestConfigErrorPassthrough(t *testing.T) {
	_, err := ParseMultiple(writeConfigs(t, mergeBase+"services:\n  a:\n    type: helm\n    path: ./c\n    values: {a: b}\n"))
	if err == nil || !strings.Contains(err.Error(), "values must be a string or array of strings") {
		t.Errorf("err = %v", err)
	}
	if _, err := ParseIdentity(writeConfigs(t, "project: {a: b}\n")[0]); err == nil || !strings.Contains(err.Error(), "failed to parse config") {
		t.Errorf("err = %v", err)
	}
}
