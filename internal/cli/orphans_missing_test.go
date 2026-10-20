package cli

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/hjames9/kraze/internal/config"
	"github.com/hjames9/kraze/internal/providers"
	"github.com/hjames9/kraze/internal/state"
	"helm.sh/helm/v4/pkg/storage/driver"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
)

// releaseGone is the error HelmProvider.Uninstall returns for a release whose
// records are gone, e.g. because its namespace was deleted
func releaseGone(name string) error {
	return fmt.Errorf("failed to uninstall chart: %w",
		fmt.Errorf("uninstall: Release not loaded: %s: %w", name, driver.ErrReleaseNotFound))
}

// stubNamespaceDeletion records the namespaces cleanup deletes
func stubNamespaceDeletion(t *testing.T) func() []string {
	t.Helper()
	var mu sync.Mutex
	var deleted []string
	deleteNamespace = func(_ context.Context, _, ns string) error {
		mu.Lock()
		defer mu.Unlock()
		deleted = append(deleted, ns)
		return nil
	}
	timeout := downNamespaceDeletionTimeout
	downNamespaceDeletionTimeout = 0
	t.Cleanup(func() {
		deleteNamespace = providers.DeleteNamespace
		downNamespaceDeletionTimeout = timeout
	})
	return func() []string {
		mu.Lock()
		defer mu.Unlock()
		out := slices.Clone(deleted)
		slices.Sort(out)
		return out
	}
}

// reload reads the saved state back as project
func reload(t *testing.T, clientset kubernetes.Interface, project string) *state.ClusterState {
	t.Helper()
	st, err := state.Load(context.Background(), clientset, "c")
	if err != nil || st == nil {
		t.Fatalf("loading saved state: %v (state %v)", err, st)
	}
	st.SetProject(project)
	return st
}

func newOrphanBackend() *fakeOrphanBackend {
	return &fakeOrphanBackend{
		helmReleases: map[string]bool{},
		uninstallErr: map[string]error{},
		deleted:      map[string][]state.ResourceRef{},
		keepCRDs:     map[string]bool{},
	}
}

// A renamed Helm service whose old release was already removed, by deleting
// its namespace: removing the orphan succeeds, forgets it, and cleanup goes on.
// The legacy orphan's release exists when checked but is gone by the time it's
// uninstalled.
func TestRemoveOrphansReleaseAlreadyGone(t *testing.T) {
	deletedNamespaces := stubNamespaceDeletion(t)

	st := state.New("c", false, false, 0, false, 0)
	st.SetProject("marlin-local")
	st.MarkServiceInstalledWithNamespace("gateway", "gateway", true)
	st.SetServiceSource("gateway", "helm", nil)
	st.MarkServiceInstalledWithNamespace("legacy", "legacy", true) // no recorded type
	// The gateway namespace is gone with its release; legacy's is still there
	clientset := fake.NewSimpleClientset(namespace("legacy", true))

	backend := newOrphanBackend()
	backend.uninstallErr["gateway"] = releaseGone("gateway")
	backend.helmReleases["legacy"] = true
	backend.uninstallErr["legacy"] = releaseGone("legacy")

	cfg := &config.Config{Services: map[string]config.ServiceConfig{
		"marlin-gateway": {Name: "marlin-gateway", Type: "helm", Namespace: "marlin"},
	}}
	run := func(orphans []state.ServiceMetadata) error {
		var err error
		captureStdout(t, func() {
			err = removeOrphans(context.Background(), st, clientset, "", orphans, nil, backend, true, nil, func(string, ...interface{}) {})
		})
		return err
	}

	orphans := st.Orphans(configuredServices(cfg))
	if got := orphanNames(orphans); !slices.Equal(got, []string{"gateway", "legacy"}) {
		t.Fatalf("orphans = %v", got)
	}
	if err := run(orphans); err != nil {
		t.Fatalf("an already removed release should count as removed, got %v", err)
	}
	for _, name := range []string{"gateway", "legacy"} {
		if st.IsServiceInstalled(name) {
			t.Errorf("%s should be gone from state", name)
		}
		if reload(t, clientset, "marlin-local").IsServiceInstalled(name) {
			t.Errorf("%s should be gone from the saved state", name)
		}
	}
	if got := deletedNamespaces(); !slices.Equal(got, []string{"legacy"}) {
		t.Errorf("deleted namespaces = %v, want [legacy]", got)
	}

	// Again: nothing is left to remove
	if left := st.Orphans(configuredServices(cfg)); len(left) != 0 {
		t.Errorf("orphans after cleanup = %v, want none", orphanNames(left))
	}
	if err := run(st.Orphans(configuredServices(cfg))); err != nil {
		t.Errorf("repeated cleanup failed: %v", err)
	}
	// Even acting on the earlier, now stale list succeeds
	if err := run(orphans); err != nil {
		t.Errorf("repeated cleanup of the same orphans failed: %v", err)
	}
}

// Failing to remove a release, other than finding it already gone, keeps the
// orphan's entry and its namespace so a later run can retry
func TestRemoveOrphansGenuineFailuresKeepEntry(t *testing.T) {
	forbidden := apierrors.NewForbidden(schema.GroupResource{Resource: "secrets"}, "", errors.New("RBAC: access denied"))
	for name, uninstallErr := range map[string]error{
		"forbidden": fmt.Errorf("failed to uninstall chart: %w",
			fmt.Errorf("uninstall: Release not loaded: gateway: %w",
				fmt.Errorf("query: failed to query with labels: %w", forbidden))),
		"unreachable": fmt.Errorf("failed to uninstall chart: %w",
			errors.New("dial tcp 172.18.0.2:6443: connect: connection refused")),
		"uninstall": errors.New("failed to uninstall chart: uninstallation completed with 1 error(s): timed out waiting for the condition"),
	} {
		t.Run(name, func(t *testing.T) {
			deletedNamespaces := stubNamespaceDeletion(t)
			st := state.New("c", false, false, 0, false, 0)
			st.SetProject("marlin-local")
			st.MarkServiceInstalledWithNamespace("gateway", "gateway", true)
			st.SetServiceSource("gateway", "helm", nil)
			clientset := fake.NewSimpleClientset(namespace("gateway", true))
			backend := newOrphanBackend()
			backend.uninstallErr["gateway"] = uninstallErr

			var err error
			captureStdout(t, func() {
				err = removeOrphans(context.Background(), st, clientset, "", st.Orphans(nil), nil, backend, true, nil, func(string, ...interface{}) {})
			})
			if err == nil || !strings.Contains(err.Error(), "gateway") {
				t.Errorf("expected an error naming gateway, got %v", err)
			}
			if !st.IsServiceInstalled("gateway") {
				t.Error("a failed removal must keep the orphan's entry")
			}
			if got := deletedNamespaces(); len(got) != 0 {
				t.Errorf("deleted namespaces = %v, want none: the release may still be there", got)
			}
		})
	}
}

// Another project on the same cluster with a service of the same name keeps
// its entry, and the namespace it shares with the removed orphan
func TestRemoveOrphansReleaseAlreadyGoneSharedCluster(t *testing.T) {
	deletedNamespaces := stubNamespaceDeletion(t)
	ctx := context.Background()
	clientset := fake.NewSimpleClientset(namespace("shared", true), namespace("billing", true))

	billing := state.New("c", false, false, 0, false, 0)
	billing.SetProject("billing")
	billing.MarkServiceInstalledWithNamespace("gateway", "billing", true)
	billing.SetServiceSource("gateway", "helm", nil)
	billing.MarkServiceInstalledWithNamespace("worker", "shared", true)
	billing.SetServiceSource("worker", "helm", nil)
	if err := billing.Save(ctx, clientset); err != nil {
		t.Fatal(err)
	}

	marlin := reload(t, clientset, "marlin-local")
	marlin.MarkServiceInstalledWithNamespace("gateway", "shared", true)
	marlin.SetServiceSource("gateway", "helm", nil)
	if err := marlin.Save(ctx, clientset); err != nil {
		t.Fatal(err)
	}

	st := reload(t, clientset, "marlin-local")
	backend := newOrphanBackend()
	backend.uninstallErr["gateway"] = releaseGone("gateway")
	var calls []string
	recording := &namespaceRecordingBackend{fakeOrphanBackend: backend, calls: &calls}
	captureStdout(t, func() {
		if err := removeOrphans(ctx, st, clientset, "", st.Orphans(nil), nil, recording, true, nil, func(string, ...interface{}) {}); err != nil {
			t.Error(err)
		}
	})

	if !slices.Equal(calls, []string{"gateway in shared"}) {
		t.Errorf("uninstalled %v, want only marlin-local's gateway in shared", calls)
	}
	if reload(t, clientset, "marlin-local").IsServiceInstalled("gateway") {
		t.Error("marlin-local's gateway should be gone from the saved state")
	}
	after := reload(t, clientset, "billing")
	for _, name := range []string{"gateway", "worker"} {
		if meta, ok := after.GetService(name); !ok || !meta.Installed {
			t.Errorf("billing's %s must stay in state, got %+v", name, meta)
		}
	}
	if got := deletedNamespaces(); len(got) != 0 {
		t.Errorf("deleted namespaces = %v, want none: billing's worker uses shared", got)
	}
}

// namespaceRecordingBackend records where Helm releases are uninstalled
type namespaceRecordingBackend struct {
	*fakeOrphanBackend
	calls *[]string
}

func (b *namespaceRecordingBackend) uninstallHelm(ctx context.Context, svc *config.ServiceConfig) error {
	*b.calls = append(*b.calls, svc.Name+" in "+svc.Namespace)
	return b.fakeOrphanBackend.uninstallHelm(ctx, svc)
}
