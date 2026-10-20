package cluster

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	"github.com/hjames9/kraze/internal/config"
)

// Node configuration that kraze writes into kind nodes after creation. The
// builders are pure so tests can parse their output back.

const (
	containerdConfigPath = "/etc/containerd/config.toml"
	containerdCertsDir   = "/etc/containerd/certs.d"
	containerdProxyConf  = "/etc/systemd/system/containerd.service.d/http-proxy.conf"
)

// containerdRegistryConfigPatch points containerd's CRI plugin at certs.d.
// It is appended to the config (version 2 format) and starts on a new line,
// as the file may not end with one.
const containerdRegistryConfigPatch = `
[plugins."io.containerd.grpc.v1.cri".registry]
  config_path = "` + containerdCertsDir + `"
`

// appendRegistryConfigScript appends the patch from stdin unless the config
// already sets config_path (a second table would make the TOML invalid and
// containerd would not start), and refuses a config format the patch doesn't
// fit. $1 is the config path.
const appendRegistryConfigScript = `grep -q 'config_path' "$1" && exit 0
grep -q '^version = 2' "$1" || { echo "unsupported containerd config version in $1" >&2; exit 1; }
cat >> "$1"`

// registryHostsConfig returns the certs.d directory and hosts.toml for an
// insecure_registries entry. Without a scheme, HTTPS without verification is
// tried first and plain HTTP second (containerd moves to the next host when
// one fails), as Docker does for insecure registries: a TLS registry on a
// port, e.g. registry.corp.com:8443, is reached over HTTPS.
func registryHostsConfig(entry string) (dir, hostsTOML string, err error) {
	reg, err := config.ParseInsecureRegistry(entry)
	if err != nil {
		return "", "", err
	}
	// Docker Hub's registry API is served from registry-1.docker.io, which
	// containerd uses for docker.io by default
	upstream := reg.Host
	if upstream == "docker.io" {
		upstream = "registry-1.docker.io"
	}
	schemes := []string{"https", "http"}
	if reg.Scheme != "" {
		schemes = []string{reg.Scheme}
	}

	var b strings.Builder
	fmt.Fprintf(&b, "server = %q\n", schemes[0]+"://"+upstream)
	for _, scheme := range schemes {
		fmt.Fprintf(&b, "\n[host.%q]\n  capabilities = [\"pull\", \"resolve\"]\n", scheme+"://"+upstream)
		if scheme == "https" {
			b.WriteString("  skip_verify = true\n")
		}
	}
	return containerdCertsDir + "/" + reg.Host, b.String(), nil
}

// containerdProxyDropIn returns a systemd drop-in setting containerd's proxy
// environment. Values are escaped for systemd: in Environment= a % starts a
// specifier (an unknown one, like %40 in a URL-encoded password, makes systemd
// ignore the whole line), and \ and " are quoting characters.
func containerdProxyDropIn(httpProxy, httpsProxy, noProxy string) string {
	escape := strings.NewReplacer(`%`, `%%`, `\`, `\\`, `"`, `\"`)
	var b strings.Builder
	b.WriteString("[Service]\n")
	for _, kv := range [][2]string{{"HTTP_PROXY", httpProxy}, {"HTTPS_PROXY", httpsProxy}, {"NO_PROXY", noProxy}} {
		fmt.Fprintf(&b, "Environment=\"%s=%s\"\n", kv[0], escape.Replace(kv[1]))
	}
	return b.String()
}

// redactProxyURL hides a proxy URL's password for printing
func redactProxyURL(proxy string) string {
	u, err := url.Parse(proxy)
	if err != nil || u.User == nil {
		return proxy
	}
	return u.Redacted()
}

// networkIPTemplate is a `docker inspect -f` template for a container's IP on
// network. Network names may contain characters a template field name can't
// (`.Networks.my-net` is a parse error), so the name is looked up with index.
func networkIPTemplate(network string) string {
	return fmt.Sprintf("{{(index .NetworkSettings.Networks %q).IPAddress}}", network)
}

// runInNode runs a shell script in a node container, with arg as $1 and
// stdin as its input. Content goes through stdin rather than into the script,
// so no value can end a heredoc or be read as shell.
func (kind *KindManager) runInNode(containerName, script, arg, stdin string) error {
	output, err := kind.run.CombinedOutput(context.Background(), stdin, "docker", "exec", "-i", containerName, "sh", "-c", script, "sh", arg)
	if err != nil {
		return fmt.Errorf("%w\nOutput: %s", err, string(output))
	}
	return nil
}

// writeNodeFile writes content to path in a node container, creating its
// directory
func (kind *KindManager) writeNodeFile(containerName, path, content string) error {
	return kind.runInNode(containerName, `mkdir -p "$(dirname "$1")" && cat > "$1"`, path, content)
}

// restartContainerd restarts containerd in a node and fails if it doesn't
// come back (e.g. a configuration it can't parse)
func (kind *KindManager) restartContainerd(containerName string) error {
	if err := kind.runInNode(containerName, "systemctl restart containerd", "", ""); err != nil {
		return fmt.Errorf("containerd did not restart in node %s: %w", containerName, err)
	}
	return nil
}
