package providers

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

const (
	// ManagedByLabel marks namespaces kraze created, so `kraze down` can tell
	// them apart from namespaces that existed before. Unlike the state flag,
	// the label survives a lost or reset state and is visible to every project.
	ManagedByLabel = "app.kubernetes.io/managed-by"

	// ManagedByValue is the ManagedByLabel value kraze sets
	ManagedByValue = "kraze"
)

// systemNamespaces are never created, labeled or deleted by kraze
var systemNamespaces = map[string]bool{
	"default":         true,
	"kube-system":     true,
	"kube-public":     true,
	"kube-node-lease": true,
}

// NamespaceInfo reports whether a namespace exists and whether kraze created it
func NamespaceInfo(ctx context.Context, clientset kubernetes.Interface, namespace string) (exists, managed bool, err error) {
	ns, err := clientset.CoreV1().Namespaces().Get(ctx, namespace, metav1.GetOptions{})
	if err != nil {
		if errors.IsNotFound(err) {
			return false, false, nil
		}
		return false, false, fmt.Errorf("failed to get namespace '%s': %w", namespace, err)
	}
	return true, ns.Labels[ManagedByLabel] == ManagedByValue, nil
}

// EnsureNamespace creates the namespace with the kraze managed-by label if it
// does not exist. Returns true only if this call created it.
func EnsureNamespace(ctx context.Context, clientset kubernetes.Interface, namespace string) (bool, error) {
	if namespace == "" || systemNamespaces[namespace] {
		return false, nil
	}

	_, err := clientset.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name:   namespace,
			Labels: map[string]string{ManagedByLabel: ManagedByValue},
		},
	}, metav1.CreateOptions{})
	if err != nil {
		if errors.IsAlreadyExists(err) {
			return false, nil
		}
		return false, fmt.Errorf("failed to create namespace '%s': %w", namespace, err)
	}
	return true, nil
}

// IsSystemNamespace reports whether kraze must never create, label or delete
// the namespace
func IsSystemNamespace(namespace string) bool {
	return systemNamespaces[namespace]
}

// NamespaceWorkloads describes what still runs in a namespace: pods that
// aren't terminating, and the Deployments, StatefulSets, DaemonSets and
// Services that would recreate or route to them. It returns nil when there
// are none. Leftover data (PVCs, ConfigMaps, Secrets) isn't counted.
func NamespaceWorkloads(ctx context.Context, clientset kubernetes.Interface, namespace string) ([]string, error) {
	var found []string
	add := func(kind string, count int) {
		if count > 0 {
			found = append(found, fmt.Sprintf("%d %s", count, kind))
		}
	}

	pods, err := clientset.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to list pods: %w", err)
	}
	running := 0
	for _, pod := range pods.Items {
		if pod.DeletionTimestamp == nil {
			running++
		}
	}
	add("pod(s)", running)

	deployments, err := clientset.AppsV1().Deployments(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to list Deployments: %w", err)
	}
	add("Deployment(s)", len(deployments.Items))

	statefulSets, err := clientset.AppsV1().StatefulSets(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to list StatefulSets: %w", err)
	}
	add("StatefulSet(s)", len(statefulSets.Items))

	daemonSets, err := clientset.AppsV1().DaemonSets(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to list DaemonSets: %w", err)
	}
	add("DaemonSet(s)", len(daemonSets.Items))

	services, err := clientset.CoreV1().Services(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to list Services: %w", err)
	}
	add("Service(s)", len(services.Items))

	return found, nil
}
