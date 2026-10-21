package cli

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/hjames9/kraze/internal/config"
	"github.com/hjames9/kraze/internal/providers"
	"github.com/hjames9/kraze/internal/state"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

// unreachableKubeconfig points at a closed port: building clients works,
// calls fail at once without reaching any real cluster
const unreachableKubeconfig = `apiVersion: v1
kind: Config
clusters:
- name: c
  cluster: {server: "https://127.0.0.1:1", insecure-skip-tls-verify: true}
contexts:
- name: c
  context: {cluster: c, user: u}
current-context: c
users:
- name: u
  user: {token: t}
`

// The real orphan backend passes calls to the providers
func TestProviderOrphanBackend(t *testing.T) {
	ctx := context.Background()
	svc := &config.ServiceConfig{Name: "old", Type: "helm", Namespace: "shop"}

	backend := realDeps().orphanBackend(&providers.ProviderOptions{Project: "web", KubeConfig: unreachableKubeconfig, Quiet: true})
	if _, err := backend.helmReleaseExists(ctx, svc); err == nil {
		t.Error("an unreachable cluster can't be asked about releases")
	}
	if err := backend.uninstallHelm(ctx, svc); err == nil {
		t.Error("an unreachable cluster can't uninstall")
	}
	if n, err := backend.deleteManifests(ctx, "old", nil); err != nil || n != 0 {
		t.Errorf("nothing recorded, nothing deleted: %d, %v", n, err)
	}

	// Providers that can't be created
	broken := realDeps().orphanBackend(&providers.ProviderOptions{Project: "web"})
	if _, err := broken.helmReleaseExists(ctx, svc); err == nil {
		t.Error("want an error for an empty kubeconfig")
	}
	if err := broken.uninstallHelm(ctx, svc); err == nil {
		t.Error("want an error for an empty kubeconfig")
	}
	if _, err := broken.deleteManifests(ctx, "old", nil); err == nil {
		t.Error("want an error for an empty kubeconfig")
	}
}

func TestRemoveExternalClusterStateFailures(t *testing.T) {
	ctx := context.Background()
	cfg := &config.Config{Project: "web", Cluster: config.ClusterConfig{Name: "c"}}
	failing := func(verb, resource string) *fake.Clientset {
		cs := fake.NewSimpleClientset()
		cs.PrependReactor(verb, resource, func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, errors.New("forbidden")
		})
		return cs
	}

	out := captureStdout(t, func() { removeExternalClusterState(ctx, failing("get", "configmaps"), cfg) })
	assertContains(t, out, "Warning: failed to load cluster state")

	// Others remain, but this project's entries can't be saved
	shared := fake.NewSimpleClientset()
	st := state.New("c", true, false, 0, false, 0)
	st.SetProject("billing")
	st.MarkServiceInstalledWithNamespace("ledger", "billing", true)
	st.SetProject("web")
	st.MarkServiceInstalledWithNamespace("api", "shop", true)
	if err := st.Save(ctx, shared); err != nil {
		t.Fatal(err)
	}
	shared.PrependReactor("update", "configmaps", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("forbidden")
	})
	out = captureStdout(t, func() { removeExternalClusterState(ctx, shared, cfg) })
	assertContains(t, out, "Warning: failed to remove project state")

	// Alone, but the ConfigMaps can't be deleted
	alone := failing("delete", "configmaps")
	mine := state.New("c", true, false, 0, false, 0)
	mine.SetProject("web")
	if err := mine.Save(ctx, alone); err != nil {
		t.Fatal(err)
	}
	out = captureStdout(t, func() { removeExternalClusterState(ctx, alone, cfg) })
	if !strings.Contains(out, "failed to delete cluster state") || !strings.Contains(out, "failed to delete cluster history") {
		t.Errorf("output:\n%s", out)
	}
}

func TestDestroyExternalClusterWarnings(t *testing.T) {
	for name, setup := range map[string]func(env *fakeEnv){
		"kubeconfig": func(env *fakeEnv) { env.cluster.errs["GetKubeconfigForExternalCluster"] = errBoom },
		"client":     func(env *fakeEnv) { env.clientsetErr = errBoom },
	} {
		t.Run(name, func(t *testing.T) {
			env := withFakeDeps(t)
			useConfig(t, externalConfig)
			setup(env)
			out, err := runCmd(t, runDestroy)
			if err != nil {
				t.Fatalf("destroy of an external cluster only warns, got %v", err)
			}
			assertContains(t, out, "Warning: failed to")
		})
	}
}
