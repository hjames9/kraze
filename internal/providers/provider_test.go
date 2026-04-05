package providers

import (
	"testing"
	"time"

	"github.com/hjames9/kraze/internal/config"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestNewProvider(test *testing.T) {
	tests := []struct {
		name        string
		service     *config.ServiceConfig
		expectError bool
	}{
		{
			name: "unsupported provider",
			service: &config.ServiceConfig{
				Name: "app",
				Type: "kustomize",
			},
			expectError: true,
		},
	}

	for _, tt := range tests {
		test.Run(tt.name, func(test *testing.T) {
			opts := &ProviderOptions{
				ClusterName: "test-cluster",
				KubeConfig:  "fake-kubeconfig",
				Verbose:     true,
			}

			_, err := NewProvider(tt.service, opts)

			if tt.expectError {
				if err == nil {
					test.Error("Expected error, got nil")
				}
				return
			}

			if err != nil {
				test.Errorf("Unexpected error: %v", err)
			}
		})
	}
}

func TestIsPodReady(test *testing.T) {
	tests := []struct {
		name       string
		phase      string
		conditions []map[string]interface{}
		wantReady  bool
	}{
		{
			name:      "Succeeded is ready (batch/job pod completed)",
			phase:     "Succeeded",
			wantReady: true,
		},
		{
			name:  "Running with Ready=True is ready (service pod)",
			phase: "Running",
			conditions: []map[string]interface{}{
				{"type": "Ready", "status": "True"},
			},
			wantReady: true,
		},
		{
			name:  "Running with Ready=False is not ready",
			phase: "Running",
			conditions: []map[string]interface{}{
				{"type": "Ready", "status": "False"},
			},
			wantReady: false,
		},
		{
			name:      "Running with no conditions is not ready",
			phase:     "Running",
			wantReady: false,
		},
		{
			name:      "Pending is not ready",
			phase:     "Pending",
			wantReady: false,
		},
		{
			name:      "Failed is not ready",
			phase:     "Failed",
			wantReady: false,
		},
		{
			name:      "Unknown is not ready",
			phase:     "Unknown",
			wantReady: false,
		},
	}

	for _, tt := range tests {
		test.Run(tt.name, func(test *testing.T) {
			obj := &unstructured.Unstructured{}
			status := map[string]interface{}{
				"phase": tt.phase,
			}
			if len(tt.conditions) > 0 {
				conds := make([]interface{}, len(tt.conditions))
				for i, c := range tt.conditions {
					conds[i] = c
				}
				status["conditions"] = conds
			}

			ready, err := isPodReady(obj, status)
			if err != nil {
				test.Fatalf("unexpected error: %v", err)
			}
			if ready != tt.wantReady {
				test.Errorf("isPodReady(%q) = %v, want %v", tt.phase, ready, tt.wantReady)
			}
		})
	}
}

// Note: Testing actual Helm and Manifests provider creation requires valid kubeconfig
// These are tested through integration tests or in actual cluster environments

func TestServiceStatus(test *testing.T) {
	status := &ServiceStatus{
		Name:      "test-service",
		Installed: true,
		Ready:     true,
		Message:   "Running",
	}

	if status.Name != "test-service" {
		test.Errorf("Expected name 'test-service', got '%s'", status.Name)
	}

	if !status.Installed {
		test.Error("Expected Installed to be true")
	}

	if !status.Ready {
		test.Error("Expected Ready to be true")
	}

	if status.Message != "Running" {
		test.Errorf("Expected message 'Running', got '%s'", status.Message)
	}
}

func TestIsImagePullFailure(t *testing.T) {
	tests := []struct {
		msg  string
		want bool
	}{
		{"Container 'app': ImagePullBackOff - Back-off pulling image", true},
		{"Container 'app': ErrImagePull - failed to pull image", true},
		{"Init container 'init': ImagePullBackOff - ...", true},
		{"Container 'app': CrashLoopBackOff - back-off restarting", false},
		{"Container 'app': CreateContainerError - failed to create", false},
		{"Container 'app': Exited with code 1 - Error", false},
		{"", false},
	}

	for _, tt := range tests {
		if got := isImagePullFailure(tt.msg); got != tt.want {
			t.Errorf("isImagePullFailure(%q) = %v, want %v", tt.msg, got, tt.want)
		}
	}
}

func TestWithinImagePullGracePeriod(t *testing.T) {
	t.Run("first observation records timestamp and returns true", func(t *testing.T) {
		firstSeen := make(map[string]time.Time)
		before := time.Now()

		result := withinImagePullGracePeriod("default/pod-abc", firstSeen)

		if !result {
			t.Error("expected true on first observation (within grace period)")
		}
		ts, ok := firstSeen["default/pod-abc"]
		if !ok {
			t.Fatal("expected pod key to be recorded in firstSeen map")
		}
		if ts.Before(before) {
			t.Error("recorded timestamp should not be before the call")
		}
	})

	t.Run("subsequent calls within grace period return true", func(t *testing.T) {
		firstSeen := map[string]time.Time{
			"default/pod-abc": time.Now().Add(-5 * time.Second),
		}

		if !withinImagePullGracePeriod("default/pod-abc", firstSeen) {
			t.Error("expected true when only 5s have elapsed (grace period is 30s)")
		}
	})

	t.Run("returns false after grace period has elapsed", func(t *testing.T) {
		firstSeen := map[string]time.Time{
			"default/pod-abc": time.Now().Add(-(imagePullGracePeriod + time.Second)),
		}

		if withinImagePullGracePeriod("default/pod-abc", firstSeen) {
			t.Error("expected false when grace period has elapsed")
		}
	})

	t.Run("different pod keys are tracked independently", func(t *testing.T) {
		firstSeen := map[string]time.Time{
			"default/pod-old": time.Now().Add(-(imagePullGracePeriod + time.Second)),
		}

		// pod-old has exceeded the grace period
		if withinImagePullGracePeriod("default/pod-old", firstSeen) {
			t.Error("expected false for pod-old whose grace period elapsed")
		}

		// pod-new has never been seen — should get a fresh timer
		if !withinImagePullGracePeriod("default/pod-new", firstSeen) {
			t.Error("expected true for pod-new on first observation")
		}
	})
}

func TestIsEventRecent(t *testing.T) {
	now := time.Now()
	cutoff := now.Add(-5 * time.Minute)

	makeEvent := func(lastTimestamp, eventTime time.Time) corev1.Event {
		e := corev1.Event{}
		if !lastTimestamp.IsZero() {
			e.LastTimestamp = metav1.Time{Time: lastTimestamp}
		}
		if !eventTime.IsZero() {
			e.EventTime = metav1.MicroTime{Time: eventTime}
		}
		return e
	}

	tests := []struct {
		name          string
		lastTimestamp time.Time
		eventTime     time.Time
		want          bool
	}{
		{
			name:          "recent LastTimestamp — included",
			lastTimestamp: now.Add(-1 * time.Minute),
			want:          true,
		},
		{
			name:          "old LastTimestamp — excluded",
			lastTimestamp: now.Add(-10 * time.Minute),
			want:          false,
		},
		{
			name:      "no LastTimestamp, recent EventTime — included",
			eventTime: now.Add(-1 * time.Minute),
			want:      true,
		},
		{
			name:      "no LastTimestamp, old EventTime — excluded",
			eventTime: now.Add(-10 * time.Minute),
			want:      false,
		},
		{
			name: "no timestamps at all — always included (never silently dropped)",
			want: true,
		},
		{
			name:          "LastTimestamp takes precedence over EventTime",
			lastTimestamp: now.Add(-1 * time.Minute),  // recent
			eventTime:     now.Add(-10 * time.Minute), // old
			want:          true,
		},
		{
			name:          "exactly at cutoff boundary — excluded (strictly before)",
			lastTimestamp: cutoff.Add(-time.Millisecond),
			want:          false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			event := makeEvent(tt.lastTimestamp, tt.eventTime)
			if got := isEventRecent(event, cutoff); got != tt.want {
				t.Errorf("isEventRecent() = %v, want %v", got, tt.want)
			}
		})
	}
}
