package cli

import (
	"context"
	"strings"
	"testing"

	"github.com/hjames9/kraze/internal/config"
	"github.com/hjames9/kraze/internal/history"
	"github.com/hjames9/kraze/internal/state"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// projectServices builds a state with the given services, keyed by project.
// The empty project holds entries written before projects existed.
func projectServices(cluster string, external bool, services map[string][]string) *state.ClusterState {
	st := state.New(cluster, external, false, 0, false, 0)
	for project, names := range services {
		st.SetProject(project)
		for _, name := range names {
			st.MarkServiceInstalledWithNamespace(name, name, true)
		}
	}
	return st
}

func TestSharedClusterError(t *testing.T) {
	cfg := &config.Config{Project: "web", Cluster: config.ClusterConfig{Name: "shared"}}

	if err := sharedClusterError(nil, cfg, false); err != nil {
		t.Errorf("a cluster with no state should be deletable: %v", err)
	}

	// Our own services and ones from before projects existed don't block
	ours := projectServices("shared", false, map[string][]string{"web": {"frontend"}, "": {"legacy"}})
	if err := sharedClusterError(ours, cfg, false); err != nil {
		t.Errorf("only this project's (and unowned) services should not block: %v", err)
	}

	shared := projectServices("shared", false, map[string][]string{
		"web": {"frontend"}, "billing": {"api"}, "payments": {"ledger"},
	})
	err := sharedClusterError(shared, cfg, false)
	if err == nil || !strings.Contains(err.Error(), "billing, payments") || !strings.Contains(err.Error(), "--force") {
		t.Errorf("expected a refusal naming the other projects and --force, got %v", err)
	}
	if err := sharedClusterError(shared, cfg, true); err != nil {
		t.Errorf("--force should allow deleting a shared cluster: %v", err)
	}
}

func historyConfigMap() *corev1.ConfigMap {
	return &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: history.ConfigMapName, Namespace: history.ConfigMapNamespace}}
}

func historyExists(t *testing.T, clientset *fake.Clientset) bool {
	t.Helper()
	_, err := clientset.CoreV1().ConfigMaps(history.ConfigMapNamespace).Get(context.Background(), history.ConfigMapName, metav1.GetOptions{})
	if err != nil && !errors.IsNotFound(err) {
		t.Fatal(err)
	}
	return err == nil
}

func TestRemoveExternalClusterStateKeepsOtherProjects(t *testing.T) {
	ctx := context.Background()
	clientset := fake.NewSimpleClientset(historyConfigMap())
	st := projectServices("ext", true, map[string][]string{"web": {"frontend"}, "billing": {"api"}})
	if err := st.Save(ctx, clientset); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{Project: "web", Cluster: config.ClusterConfig{Name: "ext"}}
	out := captureStdout(t, func() { removeExternalClusterState(ctx, clientset, cfg) })
	if !strings.Contains(out, "kept state and history for project(s) billing") {
		t.Errorf("expected a note about the kept projects, got %q", out)
	}

	loaded, err := state.Load(ctx, clientset, "ext")
	if err != nil || loaded == nil {
		t.Fatalf("state should be kept for the other project: %v", err)
	}
	loaded.SetProject("web")
	if loaded.IsServiceInstalled("frontend") {
		t.Error("this project's services should be removed from state")
	}
	loaded.SetProject("billing")
	if !loaded.IsServiceInstalled("api") {
		t.Error("another project's services must stay in state")
	}
	if !historyExists(t, clientset) {
		t.Error("history must be kept while other projects use the cluster")
	}
}

func TestRemoveExternalClusterStateDeletesWhenAlone(t *testing.T) {
	ctx := context.Background()
	clientset := fake.NewSimpleClientset(historyConfigMap())
	// Unowned entries predate projects; they don't keep the state alive
	st := projectServices("ext", true, map[string][]string{"web": {"frontend"}, "": {"legacy"}})
	if err := st.Save(ctx, clientset); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{Project: "web", Cluster: config.ClusterConfig{Name: "ext"}}
	captureStdout(t, func() { removeExternalClusterState(ctx, clientset, cfg) })

	if loaded, err := state.Load(ctx, clientset, "ext"); err != nil || loaded != nil {
		t.Errorf("state should be deleted, got %v, %v", loaded, err)
	}
	if historyExists(t, clientset) {
		t.Error("history should be deleted with the state")
	}
}
