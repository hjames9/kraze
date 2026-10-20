package providers

import (
	"testing"
	"time"

	"github.com/hjames9/kraze/internal/config"
	"helm.sh/helm/v4/pkg/action"
)

func TestCalculateConfigChecksum(test *testing.T) {
	tests := []struct {
		name        string
		manifest    string
		wantEmpty   bool
		expectError bool
	}{
		{
			name:      "empty manifest",
			manifest:  "",
			wantEmpty: true,
		},
		{
			name: "manifest with no ConfigMaps or Secrets",
			manifest: `
apiVersion: apps/v1
kind: Deployment
metadata:
  name: myapp
spec:
  replicas: 1
`,
			wantEmpty: true,
		},
		{
			name: "manifest with a ConfigMap",
			manifest: `
apiVersion: v1
kind: ConfigMap
metadata:
  name: myconfig
data:
  key1: value1
  key2: value2
`,
			wantEmpty: false,
		},
		{
			name: "manifest with a Secret",
			manifest: `
apiVersion: v1
kind: Secret
metadata:
  name: mysecret
data:
  password: c2VjcmV0
`,
			wantEmpty: false,
		},
		{
			name: "manifest with a Secret using stringData",
			manifest: `
apiVersion: v1
kind: Secret
metadata:
  name: mysecret
stringData:
  password: plaintext
`,
			wantEmpty: false,
		},
		{
			name: "manifest with multiple ConfigMaps",
			manifest: `
apiVersion: v1
kind: ConfigMap
metadata:
  name: config1
data:
  a: 1
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: config2
data:
  b: 2
`,
			wantEmpty: false,
		},
		{
			name: "mixed manifest — only ConfigMap contributes to hash",
			manifest: `
apiVersion: apps/v1
kind: Deployment
metadata:
  name: myapp
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: myconfig
data:
  key: value
`,
			wantEmpty: false,
		},
	}

	for _, tt := range tests {
		test.Run(tt.name, func(test *testing.T) {
			checksum, err := calculateConfigChecksum(tt.manifest)

			if tt.expectError {
				if err == nil {
					test.Error("Expected error but got none")
				}
				return
			}

			if err != nil {
				test.Fatalf("Unexpected error: %v", err)
			}

			if tt.wantEmpty && checksum != "" {
				test.Errorf("Expected empty checksum, got %q", checksum)
			}
			if !tt.wantEmpty && checksum == "" {
				test.Error("Expected non-empty checksum, got empty string")
			}
		})
	}
}

func TestCalculateConfigChecksum_Deterministic(test *testing.T) {
	manifest := `
apiVersion: v1
kind: ConfigMap
metadata:
  name: myconfig
data:
  key1: value1
  key2: value2
`
	first, err := calculateConfigChecksum(manifest)
	if err != nil {
		test.Fatalf("First call error: %v", err)
	}

	second, err := calculateConfigChecksum(manifest)
	if err != nil {
		test.Fatalf("Second call error: %v", err)
	}

	if first != second {
		test.Errorf("Checksums differ across calls: %q vs %q", first, second)
	}
}

func TestCalculateConfigChecksum_ChangeSensitive(test *testing.T) {
	base := `
apiVersion: v1
kind: ConfigMap
metadata:
  name: myconfig
data:
  key: original
`
	changed := `
apiVersion: v1
kind: ConfigMap
metadata:
  name: myconfig
data:
  key: modified
`
	baseHash, err := calculateConfigChecksum(base)
	if err != nil {
		test.Fatalf("base: %v", err)
	}

	changedHash, err := calculateConfigChecksum(changed)
	if err != nil {
		test.Fatalf("changed: %v", err)
	}

	if baseHash == changedHash {
		test.Error("Expected different checksums for different ConfigMap data, got the same")
	}
}

func TestHelmClientsForceConflicts(test *testing.T) {
	helm := &HelmProvider{opts: &ProviderOptions{Timeout: "5m"}}
	service := &config.ServiceConfig{Name: "app", Namespace: "apps", Version: "1.2.3"}
	actionConfig := &action.Configuration{}

	upgrade := helm.newUpgradeClient(actionConfig, service)
	if !upgrade.ForceConflicts {
		test.Error("upgrades must force conflicts: fields earlier client-side upgrades left to manager \"kraze\" otherwise block every later change")
	}
	// Helm rejects ForceConflicts on a client-side upgrade, which its default
	// ("auto") picks for releases that were installed client-side
	if upgrade.ServerSideApply != "true" {
		test.Errorf("upgrades must always apply server-side, got ServerSideApply %q", upgrade.ServerSideApply)
	}
	if upgrade.Namespace != "apps" || upgrade.Version != "1.2.3" || upgrade.Timeout != 5*time.Minute {
		test.Errorf("upgrade client = namespace %q, version %q, timeout %v", upgrade.Namespace, upgrade.Version, upgrade.Timeout)
	}

	install := helm.newInstallClient(actionConfig, service)
	if !install.ForceConflicts {
		test.Error("installs must force conflicts too: they can adopt existing objects")
	}
	if !install.ServerSideApply {
		test.Error("installs must apply server-side (Helm's default), or forcing conflicts is rejected")
	}
	if install.ReleaseName != "app" || install.Namespace != "apps" || install.Version != "1.2.3" || install.Timeout != 5*time.Minute {
		test.Errorf("install client = release %q, namespace %q, version %q, timeout %v", install.ReleaseName, install.Namespace, install.Version, install.Timeout)
	}
}
