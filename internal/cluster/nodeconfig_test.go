package cluster

import (
	"os"
	osexec "os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"text/template"

	"github.com/BurntSushi/toml"
)

// The node configuration is checked by parsing it back with the parser the
// node uses (TOML for containerd, Go templates for docker inspect), not by
// comparing text

type hostsFile struct {
	Server string `toml:"server"`
	Host   map[string]struct {
		Capabilities []string `toml:"capabilities"`
		SkipVerify   bool     `toml:"skip_verify"`
	} `toml:"host"`
}

func TestRegistryHostsConfig(t *testing.T) {
	tests := []struct {
		entry string
		dir   string
		hosts []string // in the order containerd tries them
	}{
		{entry: "registry.corp.com", dir: "registry.corp.com",
			hosts: []string{"https://registry.corp.com", "http://registry.corp.com"}},
		// A port doesn't mean plain HTTP: a TLS registry on 8443 is reached
		// over HTTPS first (this used to be http:// only)
		{entry: "registry.corp.com:8443", dir: "registry.corp.com:8443",
			hosts: []string{"https://registry.corp.com:8443", "http://registry.corp.com:8443"}},
		{entry: "http://kind-registry:5000", dir: "kind-registry:5000", hosts: []string{"http://kind-registry:5000"}},
		// An explicit https:// used to give "https://https://..." and a
		// certs.d directory containing the scheme
		{entry: "https://ghcr.io", dir: "ghcr.io", hosts: []string{"https://ghcr.io"}},
		{entry: "HTTPS://ghcr.io/", dir: "ghcr.io", hosts: []string{"https://ghcr.io"}},
		// Docker Hub's API isn't served from docker.io
		{entry: "docker.io", dir: "docker.io", hosts: []string{"https://registry-1.docker.io", "http://registry-1.docker.io"}},
		{entry: "10.0.0.5:5000", dir: "10.0.0.5:5000", hosts: []string{"https://10.0.0.5:5000", "http://10.0.0.5:5000"}},
		{entry: "[fd00::1]:5000", dir: "[fd00::1]:5000", hosts: []string{"https://[fd00::1]:5000", "http://[fd00::1]:5000"}},
	}
	for _, tt := range tests {
		t.Run(tt.entry, func(t *testing.T) {
			dir, content, err := registryHostsConfig(tt.entry)
			if err != nil {
				t.Fatal(err)
			}
			if dir != containerdCertsDir+"/"+tt.dir {
				t.Errorf("dir = %q, want %q", dir, containerdCertsDir+"/"+tt.dir)
			}
			var parsed hostsFile
			meta, err := toml.Decode(content, &parsed)
			if err != nil {
				t.Fatalf("hosts.toml doesn't parse: %v\n%s", err, content)
			}
			var order []string
			for _, key := range meta.Keys() {
				if len(key) == 2 && key[0] == "host" {
					order = append(order, key[1])
				}
			}
			if !slices.Equal(order, tt.hosts) {
				t.Errorf("hosts = %v, want %v\n%s", order, tt.hosts, content)
			}
			if parsed.Server != tt.hosts[0] {
				t.Errorf("server = %q, want %q", parsed.Server, tt.hosts[0])
			}
			for url, host := range parsed.Host {
				if host.SkipVerify != strings.HasPrefix(url, "https://") {
					t.Errorf("%s: skip_verify = %v", url, host.SkipVerify)
				}
				if !slices.Equal(host.Capabilities, []string{"pull", "resolve"}) {
					t.Errorf("%s: capabilities = %v", url, host.Capabilities)
				}
			}
		})
	}

	// An entry becomes a directory name and a TOML key, so anything that
	// isn't a registry host is refused
	for _, entry := range []string{"", "reg; rm -rf /", "a b", "reg/path", "ftp://reg", "user:pw@reg", "reg:99999",
		"reg:", "reg:abc", `reg"x`, "reg\nx", "-reg", "https://", "reg?x=1", "../etc"} {
		if _, _, err := registryHostsConfig(entry); err == nil {
			t.Errorf("%q should be refused", entry)
		}
	}
}

// kindContainerdConfig is the shape of a kind node's config.toml: version 2,
// and (as here) possibly without a final newline
const kindContainerdConfig = `version = 2

[plugins."io.containerd.grpc.v1.cri"]
  sandbox_image = "registry.k8s.io/pause:3.10"
  restrict_oom_score_adj = false`

// The append script is run with sh, as in the node
func TestAppendRegistryConfigScript(t *testing.T) {
	run := func(t *testing.T, initial string) (string, error) {
		t.Helper()
		path := filepath.Join(t.TempDir(), "config.toml")
		if err := os.WriteFile(path, []byte(initial), 0o600); err != nil {
			t.Fatal(err)
		}
		cmd := osexec.Command("sh", "-c", appendRegistryConfigScript, "sh", path)
		cmd.Stdin = strings.NewReader(containerdRegistryConfigPatch)
		out, err := cmd.CombinedOutput()
		if err != nil {
			err = &scriptError{err: err, out: string(out)}
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			t.Fatal(readErr)
		}
		return string(data), err
	}

	// A kind config without a final newline: the result is valid TOML that
	// sets config_path (a missing newline used to merge the heredoc's
	// terminator into the file, and containerd didn't start again)
	patched, err := run(t, kindContainerdConfig)
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		Plugins map[string]struct {
			Registry struct {
				ConfigPath string `toml:"config_path"`
			} `toml:"registry"`
		} `toml:"plugins"`
	}
	if _, err := toml.Decode(patched, &cfg); err != nil {
		t.Fatalf("patched config isn't valid TOML: %v\n%s", err, patched)
	}
	if got := cfg.Plugins["io.containerd.grpc.v1.cri"].Registry.ConfigPath; got != containerdCertsDir {
		t.Errorf("config_path = %q", got)
	}

	// Running it again (or on a config that already sets config_path)
	// changes nothing: a second registry table is invalid TOML
	again, err := run(t, patched)
	if err != nil || again != patched {
		t.Errorf("second run changed the config (err %v):\n%s", err, again)
	}

	// Another config format is refused, unchanged
	v3 := "version = 3\n"
	if got, err := run(t, v3); err == nil || got != v3 {
		t.Errorf("version 3 config: err %v, config %q", err, got)
	}
}

type scriptError struct {
	err error
	out string
}

func (e *scriptError) Error() string { return e.err.Error() + ": " + e.out }

// Expected output is what systemd (in a kind node) resolves to the intended
// value: `systemctl show containerd -p Environment` printed each value intact
func TestContainerdProxyDropIn(t *testing.T) {
	got := containerdProxyDropIn(`http://user:p%40ss@proxy:8080`, `http://u:a"b\c d;e@proxy:8080`, "localhost,.svc")
	want := `[Service]
Environment="HTTP_PROXY=http://user:p%%40ss@proxy:8080"
Environment="HTTPS_PROXY=http://u:a\"b\\c d;e@proxy:8080"
Environment="NO_PROXY=localhost,.svc"
`
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestRedactProxyURL(t *testing.T) {
	for in, want := range map[string]string{
		"http://user:secret@proxy:8080": "http://user:xxxxx@proxy:8080",
		"http://proxy:8080":             "http://proxy:8080",
		"":                              "",
	} {
		if got := redactProxyURL(in); got != want {
			t.Errorf("redactProxyURL(%q) = %q, want %q", in, got, want)
		}
	}
}

// docker inspect evaluates the template against the container's JSON;
// a map of the same shape behaves the same (field access on a map is a key
// lookup)
func TestNetworkIPTemplate(t *testing.T) {
	for _, network := range []string{"kind", "my-net", "dev_default", "a.b", `odd"name`} {
		tmpl, err := template.New("").Parse(networkIPTemplate(network))
		if err != nil {
			t.Errorf("%q: %v", network, err)
			continue
		}
		data := map[string]any{"NetworkSettings": map[string]any{"Networks": map[string]any{
			network: map[string]any{"IPAddress": "172.18.0.2"},
			"other": map[string]any{"IPAddress": "10.0.0.9"},
		}}}
		var out strings.Builder
		if err := tmpl.Execute(&out, data); err != nil || out.String() != "172.18.0.2" {
			t.Errorf("%q: got %q, %v", network, out.String(), err)
		}
	}
}
