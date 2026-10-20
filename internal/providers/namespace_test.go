package providers

import (
	"context"
	"slices"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestNamespaceWorkloads(t *testing.T) {
	now := metav1.Now()
	clientset := fake.NewSimpleClientset(
		// "quiet": only things that don't count
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "leaving", Namespace: "quiet", DeletionTimestamp: &now, Finalizers: []string{"x"}}},
		&corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "data", Namespace: "quiet"}},
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "cfg", Namespace: "quiet"}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "s", Namespace: "quiet"}},
		// "busy": one of each workload kind
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "busy"}},
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "d", Namespace: "busy"}},
		&appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: "ss", Namespace: "busy"}},
		&appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Name: "ds", Namespace: "busy"}},
		&corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "svc", Namespace: "busy"}},
	)

	got, err := NamespaceWorkloads(context.Background(), clientset, "quiet")
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Errorf("terminating pods and leftover data aren't workloads, got %v", got)
	}

	got, err = NamespaceWorkloads(context.Background(), clientset, "busy")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"1 pod(s)", "1 Deployment(s)", "1 StatefulSet(s)", "1 DaemonSet(s)", "1 Service(s)"}
	if !slices.Equal(got, want) {
		t.Errorf("NamespaceWorkloads = %v, want %v", got, want)
	}
}

func TestIsSystemNamespace(t *testing.T) {
	for _, ns := range []string{"default", "kube-system", "kube-public", "kube-node-lease"} {
		if !IsSystemNamespace(ns) {
			t.Errorf("%s should be a system namespace", ns)
		}
	}
	if IsSystemNamespace("apps") {
		t.Error("apps is not a system namespace")
	}
}
