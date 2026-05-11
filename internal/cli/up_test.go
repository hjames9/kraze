package cli

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/hjames9/kraze/internal/ui"
)

// noopProgress satisfies ui.ProgressManager for tests that don't need output.
type noopProgress struct{}

func (n *noopProgress) Start(total int, operation string) {}
func (n *noopProgress) UpdateService(index int, name string, status ui.ServiceStatus, message string) {
}
func (n *noopProgress) Finish(successCount int)                    {}
func (n *noopProgress) Stop()                                      {}
func (n *noopProgress) Verbose(format string, args ...interface{}) {}

func makePod(name, namespace string, containerStatuses []corev1.ContainerStatus, initStatuses []corev1.ContainerStatus) corev1.Pod {
	return corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Status: corev1.PodStatus{
			ContainerStatuses:     containerStatuses,
			InitContainerStatuses: initStatuses,
		},
	}
}

func waitingStatus(image, reason string) corev1.ContainerStatus {
	return corev1.ContainerStatus{
		Image: image,
		State: corev1.ContainerState{
			Waiting: &corev1.ContainerStateWaiting{Reason: reason},
		},
	}
}

func runningStatus(image string) corev1.ContainerStatus {
	return corev1.ContainerStatus{
		Image: image,
		State: corev1.ContainerState{
			Running: &corev1.ContainerStateRunning{},
		},
	}
}

func TestRestartImagePullBackOffPods(t *testing.T) {
	const ns = "kora"

	tests := []struct {
		name         string
		pods         []corev1.Pod
		loadedImages []string
		wantDeleted  []string // pod names that should be deleted
		wantKept     []string // pod names that should remain
	}{
		{
			name: "deletes pod stuck in ImagePullBackOff for loaded image",
			pods: []corev1.Pod{
				makePod("worker-abc", ns, []corev1.ContainerStatus{
					waitingStatus("hjames/ruya_worker:rocm7.2.1-torch2.9.1", "ImagePullBackOff"),
				}, nil),
			},
			loadedImages: []string{"hjames/ruya_worker:rocm7.2.1-torch2.9.1"},
			wantDeleted:  []string{"worker-abc"},
		},
		{
			name: "deletes pod stuck in ErrImagePull for loaded image",
			pods: []corev1.Pod{
				makePod("worker-abc", ns, []corev1.ContainerStatus{
					waitingStatus("hjames/ruya_worker:rocm7.2.1-torch2.9.1", "ErrImagePull"),
				}, nil),
			},
			loadedImages: []string{"hjames/ruya_worker:rocm7.2.1-torch2.9.1"},
			wantDeleted:  []string{"worker-abc"},
		},
		{
			name: "does not delete pod whose image was not loaded",
			pods: []corev1.Pod{
				makePod("other-pod", ns, []corev1.ContainerStatus{
					waitingStatus("hjames/other:latest", "ImagePullBackOff"),
				}, nil),
			},
			loadedImages: []string{"hjames/ruya_worker:rocm7.2.1-torch2.9.1"},
			wantKept:     []string{"other-pod"},
		},
		{
			name: "does not delete healthy pod for loaded image",
			pods: []corev1.Pod{
				makePod("worker-healthy", ns, []corev1.ContainerStatus{
					runningStatus("hjames/ruya_worker:rocm7.2.1-torch2.9.1"),
				}, nil),
			},
			loadedImages: []string{"hjames/ruya_worker:rocm7.2.1-torch2.9.1"},
			wantKept:     []string{"worker-healthy"},
		},
		{
			name: "deletes pod whose init container is stuck in ImagePullBackOff",
			pods: []corev1.Pod{
				makePod("worker-init", ns,
					[]corev1.ContainerStatus{runningStatus("hjames/ruya_worker:rocm7.2.1-torch2.9.1")},
					[]corev1.ContainerStatus{
						waitingStatus("hjames/ruya_models:yolo11s.pt-whisper-large-v3-pyannote", "ImagePullBackOff"),
					},
				),
			},
			loadedImages: []string{"hjames/ruya_models:yolo11s.pt-whisper-large-v3-pyannote"},
			wantDeleted:  []string{"worker-init"},
		},
		{
			name: "normalizes docker.io prefix when matching",
			pods: []corev1.Pod{
				// Pod spec uses bare image name (no docker.io/ prefix)
				makePod("worker-bare", ns, []corev1.ContainerStatus{
					waitingStatus("hjames/ruya_worker:rocm7.2.1-torch2.9.1", "ImagePullBackOff"),
				}, nil),
			},
			// Loaded image has explicit docker.io prefix
			loadedImages: []string{"docker.io/hjames/ruya_worker:rocm7.2.1-torch2.9.1"},
			wantDeleted:  []string{"worker-bare"},
		},
		{
			name: "no-op when loadedImages is empty",
			pods: []corev1.Pod{
				makePod("worker-abc", ns, []corev1.ContainerStatus{
					waitingStatus("hjames/ruya_worker:rocm7.2.1-torch2.9.1", "ImagePullBackOff"),
				}, nil),
			},
			loadedImages: []string{},
			wantKept:     []string{"worker-abc"},
		},
		{
			name: "deletes only matching pods, keeps unrelated ones",
			pods: []corev1.Pod{
				makePod("worker-stuck", ns, []corev1.ContainerStatus{
					waitingStatus("hjames/ruya_worker:rocm7.2.1-torch2.9.1", "ImagePullBackOff"),
				}, nil),
				makePod("api-running", ns, []corev1.ContainerStatus{
					runningStatus("hjames/ruya:latest"),
				}, nil),
				makePod("other-stuck", ns, []corev1.ContainerStatus{
					waitingStatus("hjames/other:v1", "ImagePullBackOff"),
				}, nil),
			},
			loadedImages: []string{"hjames/ruya_worker:rocm7.2.1-torch2.9.1"},
			wantDeleted:  []string{"worker-stuck"},
			wantKept:     []string{"api-running", "other-stuck"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cs := fake.NewClientset()
			ctx := context.Background()

			for i := range tt.pods {
				if _, err := cs.CoreV1().Pods(ns).Create(ctx, &tt.pods[i], metav1.CreateOptions{}); err != nil {
					t.Fatalf("failed to create pod: %v", err)
				}
			}

			restartImagePullBackOffPods(ctx, cs, ns, tt.loadedImages, &noopProgress{})

			for _, name := range tt.wantDeleted {
				_, err := cs.CoreV1().Pods(ns).Get(ctx, name, metav1.GetOptions{})
				if err == nil {
					t.Errorf("pod %q should have been deleted but still exists", name)
				}
			}
			for _, name := range tt.wantKept {
				_, err := cs.CoreV1().Pods(ns).Get(ctx, name, metav1.GetOptions{})
				if err != nil {
					t.Errorf("pod %q should still exist but got: %v", name, err)
				}
			}
		})
	}
}
