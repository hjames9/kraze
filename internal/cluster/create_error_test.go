package cluster

import (
	"errors"
	"strings"
	"testing"

	kindexec "sigs.k8s.io/kind/pkg/exec"
)

func TestEnrichClusterCreateError(test *testing.T) {
	runErr := func(output string) error {
		return &kindexec.RunError{Command: []string{"docker", "run"}, Output: []byte(output), Inner: errors.New("exit status 125")}
	}

	tests := []struct {
		name string
		err  error
		want []string
	}{
		{
			name: "port in use",
			err:  runErr("docker: Error response from daemon: failed to bind host port for 0.0.0.0:8081:172.19.0.2:30801/tcp: address already in use.\n"),
			want: []string{"host port 8081/TCP is already in use", "ss -tlnup | grep 8081", "Docker output:"},
		},
		{
			name: "udp port in use",
			err:  runErr("failed to bind host port for 0.0.0.0:5353:172.19.0.2:30053/udp: address already in use"),
			want: []string{"host port 5353/UDP is already in use", "ss -ulnup | grep 5353"},
		},
		{
			name: "missing device",
			err:  runErr("error gathering device information while adding custom device \"/dev/kfd\": no such file or directory /dev/kfd"),
			want: []string{"required device /dev/kfd not found on this host"},
		},
		{
			name: "other docker failure",
			err:  runErr("  something else went wrong  \n"),
			want: []string{"exit status 125", "Docker output: something else went wrong"},
		},
	}
	for _, tt := range tests {
		test.Run(tt.name, func(test *testing.T) {
			got := enrichClusterCreateError(tt.err).Error()
			for _, want := range tt.want {
				if !strings.Contains(got, want) {
					test.Errorf("error should contain %q, got:\n%s", want, got)
				}
			}
		})
	}

	// Errors without Docker output are returned as they are
	plain := errors.New("plain failure")
	if got := enrichClusterCreateError(plain); got != plain {
		test.Errorf("a non-RunError should be returned unchanged, got %v", got)
	}
	empty := runErr("")
	if got := enrichClusterCreateError(empty); got != empty {
		test.Errorf("a RunError without output should be returned unchanged, got %v", got)
	}
}
