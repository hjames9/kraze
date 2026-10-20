package providers

import (
	"bytes"
	"context"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/kubernetes/fake"
)

// captureOutput returns what fn writes to stdout
func captureOutput(t *testing.T, fn func()) string {
	t.Helper()
	orig := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	done := make(chan string)
	go func() {
		var buf bytes.Buffer
		_, _ = io.Copy(&buf, r)
		done <- buf.String()
	}()
	fn()
	_ = w.Close()
	os.Stdout = orig
	return <-done
}

func waiting(name, reason string) corev1.ContainerStatus {
	return corev1.ContainerStatus{Name: name, State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: reason, Message: "details"}}}
}

func running(name string, ready bool, restarts int32, since time.Duration) corev1.ContainerStatus {
	return corev1.ContainerStatus{
		Name: name, Ready: ready, RestartCount: restarts,
		State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.NewTime(time.Now().Add(-since))}},
	}
}

func exited(name string, code int32) corev1.ContainerStatus {
	return corev1.ContainerStatus{Name: name, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: code, Reason: "Error"}}}
}

func TestCheckTypedPodFailureState(t *testing.T) {
	tests := []struct {
		name       string
		status     corev1.PodStatus
		wantFailed bool
		want       string
	}{
		{"healthy", corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{running("app", true, 0, time.Hour)}}, false, ""},
		{"failed phase", corev1.PodStatus{Phase: corev1.PodFailed, Reason: "Evicted", Message: "disk"}, true, "Pod failed: Evicted - disk"},
		{"image pull", corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{waiting("app", "ImagePullBackOff")}}, true, "Container 'app': ImagePullBackOff"},
		{"crash loop", corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{waiting("app", "CrashLoopBackOff")}}, true, "CrashLoopBackOff"},
		{"config error", corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{waiting("app", "CreateContainerConfigError")}}, true, "CreateContainerConfigError"},
		{"bad image name", corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{waiting("app", "InvalidImageName")}}, true, "InvalidImageName"},
		{"never pull", corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{waiting("app", "ErrImageNeverPull")}}, true, "ErrImageNeverPull"},
		{"still creating", corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{waiting("app", "ContainerCreating")}}, false, ""},
		{"exited", corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{exited("app", 2)}}, true, "Exited with code 2"},
		{"exited cleanly", corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{exited("app", 0)}}, false, ""},
		{"restart loop", corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{running("app", false, 5, time.Second)}}, true, "after 5 restart(s)"},
		{"failing probes", corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{running("app", false, 2, 2*time.Minute)}}, true, "likely failing probes"},
		{"slow startup", corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{running("app", false, 0, 4*time.Minute)}}, true, "likely failing startup probes"},
		{"starting up", corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{running("app", false, 1, time.Minute)}}, false, ""},
		{"init image pull", corev1.PodStatus{InitContainerStatuses: []corev1.ContainerStatus{waiting("init", "ErrImagePull")}}, true, "Init container 'init': ErrImagePull"},
		{"init crash", corev1.PodStatus{InitContainerStatuses: []corev1.ContainerStatus{waiting("init", "CrashLoopBackOff")}}, true, "Init container 'init': CrashLoopBackOff"},
		{"init config", corev1.PodStatus{InitContainerStatuses: []corev1.ContainerStatus{waiting("init", "CreateContainerError")}}, true, "CreateContainerError"},
		{"init exited", corev1.PodStatus{InitContainerStatuses: []corev1.ContainerStatus{exited("init", 1)}}, true, "Init container 'init': Exited with code 1"},
		{"init restarted", corev1.PodStatus{InitContainerStatuses: []corev1.ContainerStatus{running("init", false, 1, time.Second)}}, true, "after 1 restart(s)"},
		{"init stuck", corev1.PodStatus{InitContainerStatuses: []corev1.ContainerStatus{running("init", false, 0, time.Minute)}}, true, "Init container 'init': Running for"},
		{"init starting", corev1.PodStatus{InitContainerStatuses: []corev1.ContainerStatus{running("init", false, 0, time.Second)}}, false, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			failed, msg := checkTypedPodFailureState(&corev1.Pod{Status: tt.status})
			if failed != tt.wantFailed || !strings.Contains(msg, tt.want) {
				t.Errorf("got (%v, %q), want (%v, containing %q)", failed, msg, tt.wantFailed, tt.want)
			}
		})
	}
}

func TestCheckPodFailureState(t *testing.T) {
	pod := func(status map[string]interface{}) *unstructured.Unstructured {
		obj := map[string]interface{}{"apiVersion": "v1", "kind": "Pod", "metadata": map[string]interface{}{"name": "p"}}
		if status != nil {
			obj["status"] = status
		}
		return &unstructured.Unstructured{Object: obj}
	}
	container := func(state map[string]interface{}) []interface{} {
		return []interface{}{map[string]interface{}{"name": "app", "state": state}}
	}
	tests := []struct {
		name       string
		obj        *unstructured.Unstructured
		wantFailed bool
		want       string
	}{
		{"no status", pod(nil), false, ""},
		{"failed", pod(map[string]interface{}{"phase": "Failed", "reason": "OOMKilled", "message": "m"}), true, "Pod failed: OOMKilled - m"},
		{"crash loop", pod(map[string]interface{}{"containerStatuses": container(map[string]interface{}{
			"waiting": map[string]interface{}{"reason": "CrashLoopBackOff", "message": "back-off"}})}), true, "Container 'app': CrashLoopBackOff - back-off"},
		{"image pull", pod(map[string]interface{}{"containerStatuses": container(map[string]interface{}{
			"waiting": map[string]interface{}{"reason": "ErrImagePull"}})}), true, "ErrImagePull"},
		{"config", pod(map[string]interface{}{"containerStatuses": container(map[string]interface{}{
			"waiting": map[string]interface{}{"reason": "CreateContainerConfigError"}})}), true, "CreateContainerConfigError"},
		{"invalid image", pod(map[string]interface{}{"containerStatuses": container(map[string]interface{}{
			"waiting": map[string]interface{}{"reason": "InvalidImageName"}})}), true, "InvalidImageName"},
		{"never pull", pod(map[string]interface{}{"containerStatuses": container(map[string]interface{}{
			"waiting": map[string]interface{}{"reason": "ErrImageNeverPull"}})}), true, "ErrImageNeverPull"},
		{"exited", pod(map[string]interface{}{"containerStatuses": container(map[string]interface{}{
			"terminated": map[string]interface{}{"exitCode": int64(137), "reason": "Killed"}})}), true, "Exited with code 137 - Killed"},
		{"exited cleanly", pod(map[string]interface{}{"containerStatuses": container(map[string]interface{}{
			"terminated": map[string]interface{}{"exitCode": int64(0)}})}), false, ""},
		{"init crash", pod(map[string]interface{}{"initContainerStatuses": container(map[string]interface{}{
			"waiting": map[string]interface{}{"reason": "CrashLoopBackOff"}})}), true, "CrashLoopBackOff"},
		{"no state", pod(map[string]interface{}{"containerStatuses": []interface{}{map[string]interface{}{"name": "app"}, "junk"}}), false, ""},
		{"running", pod(map[string]interface{}{"phase": "Running"}), false, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			failed, msg := checkPodFailureState(tt.obj)
			if failed != tt.wantFailed || !strings.Contains(msg, tt.want) {
				t.Errorf("got (%v, %q), want (%v, containing %q)", failed, msg, tt.wantFailed, tt.want)
			}
		})
	}
}

func deploymentWithSelector() *unstructured.Unstructured {
	obj := workload("Deployment", 1, map[string]interface{}{
		"selector": map[string]interface{}{"matchLabels": map[string]interface{}{"app": "web"}},
	}, nil)
	obj.SetNamespace("apps")
	return obj
}

func failingPod(name string, deleting bool, statuses ...corev1.ContainerStatus) *corev1.Pod {
	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "apps", Labels: map[string]string{"app": "web"}},
		Status:     corev1.PodStatus{ContainerStatuses: statuses},
	}
	if deleting {
		now := metav1.Now()
		p.DeletionTimestamp = &now
	}
	return p
}

func TestCheckControlledPodsForFailures(t *testing.T) {
	ctx := context.Background()

	t.Run("crash loop fails with diagnostics", func(t *testing.T) {
		clientset := fake.NewSimpleClientset(
			failingPod("web-1", false, waiting("app", "CrashLoopBackOff")),
			&corev1.Event{
				ObjectMeta:     metav1.ObjectMeta{Name: "e1", Namespace: "apps"},
				InvolvedObject: corev1.ObjectReference{Kind: "Pod", Name: "web-1"},
				Reason:         "BackOff", Message: "Back-off restarting failed container",
				LastTimestamp: metav1.Now(),
			},
		)
		var err error
		out := captureOutput(t, func() {
			err = checkControlledPodsForFailures(ctx, clientset, deploymentWithSelector(), "Deployment", map[string]time.Time{})
		})
		if err == nil || !strings.Contains(err.Error(), "Deployment has failing pod web-1: Container 'app': CrashLoopBackOff") {
			t.Fatalf("err = %v", err)
		}
		for _, want := range []string{"Pod apps/web-1 is experiencing issues", "BackOff: Back-off restarting failed container",
			"log lines from container 'app'", "The container is crashing on startup"} {
			if !strings.Contains(out, want) {
				t.Errorf("diagnostics should contain %q, got:\n%s", want, out)
			}
		}
	})

	t.Run("image pull gets a grace period and a fresh pod", func(t *testing.T) {
		clientset := fake.NewSimpleClientset(failingPod("web-1", false, waiting("app", "ImagePullBackOff")))
		firstSeen := map[string]time.Time{}
		if err := checkControlledPodsForFailures(ctx, clientset, deploymentWithSelector(), "Deployment", firstSeen); err != nil {
			t.Errorf("an image pull failure is tolerated at first, got %v", err)
		}
		if _, err := clientset.CoreV1().Pods("apps").Get(ctx, "web-1", metav1.GetOptions{}); err == nil {
			t.Error("the pod should be deleted to reset its pull backoff")
		}

		// Still failing after the grace period
		clientset = fake.NewSimpleClientset(failingPod("web-1", false, waiting("app", "ImagePullBackOff")))
		firstSeen["apps/web-1"] = time.Now().Add(-time.Hour)
		var err error
		out := captureOutput(t, func() {
			err = checkControlledPodsForFailures(ctx, clientset, deploymentWithSelector(), "Deployment", firstSeen)
		})
		if err == nil || !strings.Contains(out, "kraze load-image") {
			t.Errorf("err = %v, output:\n%s", err, out)
		}
	})

	t.Run("recovered and terminating pods are fine", func(t *testing.T) {
		clientset := fake.NewSimpleClientset(
			failingPod("web-1", false, running("app", true, 0, time.Minute)),
			failingPod("web-old", true, waiting("app", "CrashLoopBackOff")),
		)
		firstSeen := map[string]time.Time{"apps/web-1": time.Now()}
		if err := checkControlledPodsForFailures(ctx, clientset, deploymentWithSelector(), "Deployment", firstSeen); err != nil {
			t.Error(err)
		}
		if _, ok := firstSeen["apps/web-1"]; ok {
			t.Error("a recovered pod's grace period should be cleared")
		}
	})

	t.Run("config error suggestion", func(t *testing.T) {
		clientset := fake.NewSimpleClientset(failingPod("web-1", false, waiting("app", "CreateContainerConfigError")))
		out := captureOutput(t, func() {
			_ = checkControlledPodsForFailures(ctx, clientset, deploymentWithSelector(), "Deployment", map[string]time.Time{})
		})
		if !strings.Contains(out, "Check ConfigMaps and Secrets are created") {
			t.Errorf("output:\n%s", out)
		}
	})

	t.Run("no selector", func(t *testing.T) {
		obj := workload("Deployment", 1, map[string]interface{}{}, nil)
		if err := checkControlledPodsForFailures(ctx, fake.NewSimpleClientset(), obj, "Deployment", nil); err != nil {
			t.Error(err)
		}
		noSpec := &unstructured.Unstructured{Object: map[string]interface{}{"kind": "Deployment"}}
		if err := checkControlledPodsForFailures(ctx, fake.NewSimpleClientset(), noSpec, "Deployment", nil); err != nil {
			t.Error(err)
		}
	})
}

func TestGetPodEventsSkipsOldEvents(t *testing.T) {
	event := func(name, reason string, age time.Duration) *corev1.Event {
		return &corev1.Event{
			ObjectMeta:     metav1.ObjectMeta{Name: name, Namespace: "apps"},
			InvolvedObject: corev1.ObjectReference{Kind: "Pod", Name: "p"},
			Reason:         reason,
			LastTimestamp:  metav1.NewTime(time.Now().Add(-age)),
		}
	}
	clientset := fake.NewSimpleClientset(
		event("a", "Old", time.Hour),
		event("b", "Pulled", time.Minute),
		event("c", "Started", time.Second),
		&corev1.Event{ObjectMeta: metav1.ObjectMeta{Name: "d", Namespace: "apps"}, Reason: "NoTime"},
	)
	got, err := getPodEvents(context.Background(), clientset, "apps", "p", 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || strings.Contains(strings.Join(got, ""), "Old") {
		t.Errorf("events = %v, want the 2 most recent, none older than 5 minutes", got)
	}
}

func TestExtractContainerName(t *testing.T) {
	for msg, want := range map[string]string{
		"Container 'app': CrashLoopBackOff - x":     "app",
		"Init container 'init': Exited with code 1": "", // logs are fetched for app containers
		"Container 'broken":                         "",
		"Pod failed":                                "",
	} {
		if got := extractContainerName(msg); got != want {
			t.Errorf("extractContainerName(%q) = %q, want %q", msg, got, want)
		}
	}
}

func TestHasReadyCondition(t *testing.T) {
	conditions := func(conds ...interface{}) map[string]interface{} {
		return map[string]interface{}{"conditions": conds}
	}
	for name, tc := range map[string]struct {
		status map[string]interface{}
		want   bool
	}{
		"ready":         {conditions(map[string]interface{}{"type": "Ready", "status": "True"}), true},
		"not ready":     {conditions(map[string]interface{}{"type": "Ready", "status": "False"}), false},
		"other only":    {conditions(map[string]interface{}{"type": "Synced", "status": "True"}), false},
		"no conditions": {map[string]interface{}{}, false},
		"junk":          {conditions("junk"), false},
	} {
		got, err := hasReadyCondition(tc.status)
		if err != nil || got != tc.want {
			t.Errorf("%s: got %v, %v; want %v", name, got, err, tc.want)
		}
	}
}
