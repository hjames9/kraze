package providers

import (
	"testing"
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
