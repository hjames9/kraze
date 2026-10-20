package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateErrorsSayValidationFailedOnce(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    string
	}{
		{name: "invalid config", content: "cluster:\n  name: dev\nservices: {}\n", want: "validation failed: project: a project name is required"},
		{name: "YAML syntax error", content: "project: app\ncluster: [\n", want: "validation failed: "},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "kraze.yml")
			if err := os.WriteFile(path, []byte(tt.content), 0o644); err != nil {
				t.Fatal(err)
			}
			saved := configFiles
			configFiles = []string{path}
			t.Cleanup(func() { configFiles = saved })

			err := validateCmd.RunE(validateCmd, nil)
			if err == nil {
				t.Fatal("expected an error")
			}
			if !strings.HasPrefix(err.Error(), tt.want) {
				t.Errorf("error = %q, want prefix %q", err, tt.want)
			}
			if n := strings.Count(err.Error(), "validation failed"); n != 1 {
				t.Errorf("\"validation failed\" appears %d times in %q", n, err)
			}
		})
	}
}
