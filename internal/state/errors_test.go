package state

import (
	"context"
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func failingClientset(verb string) *fake.Clientset {
	cs := fake.NewSimpleClientset()
	cs.PrependReactor(verb, "configmaps", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("forbidden")
	})
	return cs
}

func TestLoadAndDeleteErrors(t *testing.T) {
	ctx := context.Background()
	if _, err := Load(ctx, failingClientset("get"), "c"); err == nil {
		t.Error("a ConfigMap that can't be read is an error")
	}
	empty := fake.NewSimpleClientset(&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: ConfigMapName, Namespace: ConfigMapNamespace}})
	if st, err := Load(ctx, empty, "c"); st != nil || err != nil {
		t.Errorf("a ConfigMap without data is no state, got %v, %v", st, err)
	}
	if err := Delete(ctx, fake.NewSimpleClientset()); err != nil {
		t.Errorf("deleting no state is fine, got %v", err)
	}
	if err := Delete(ctx, failingClientset("delete")); err == nil {
		t.Error("a delete that fails is an error")
	}
}

func TestAllServicesSortedByProjectThenName(t *testing.T) {
	st := New("c", false, false, 0, false, 0)
	for _, e := range []struct{ project, name string }{{"web", "b"}, {"billing", "z"}, {"web", "a"}, {"", "legacy"}} {
		st.SetProject(e.project)
		st.MarkServiceInstalledWithNamespace(e.name, "ns", false)
	}
	var got []string
	for _, svc := range st.AllServices() {
		got = append(got, svc.Project+"/"+svc.Name)
	}
	want := []string{"/legacy", "billing/z", "web/a", "web/b"}
	if len(got) != len(want) {
		t.Fatalf("services = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("services = %v, want %v", got, want)
			break
		}
	}
	if st.Project() != "" {
		t.Errorf("project = %q, want the last one set", st.Project())
	}
}

// A state written without services or projects loads with empty maps, so
// setting config paths or services doesn't hit a nil map
func TestDecodeFillsMissingMaps(t *testing.T) {
	st, err := decode(`{"version": 4, "cluster_name": "c"}`)
	if err != nil {
		t.Fatal(err)
	}
	if st.Services == nil || st.Projects == nil {
		t.Errorf("maps = %v, %v", st.Services, st.Projects)
	}
	st.SetProject("web")
	st.SetConfigPaths([]string{"/p/kraze.yml"})
	st.MarkServiceInstalledWithNamespace("a", "shop", true)

	bare := &ClusterState{}
	bare.SetConfigPaths([]string{"/p/kraze.yml"})
	if len(bare.Projects) != 1 {
		t.Errorf("projects = %v", bare.Projects)
	}
}

func TestSaveErrors(t *testing.T) {
	ctx := context.Background()
	st := New("c", false, false, 0, false, 0)
	st.SetProject("web")
	st.MarkServiceInstalledWithNamespace("a", "shop", true)
	if err := st.Save(ctx, failingClientset("get")); err == nil {
		t.Error("a state that can't be read can't be saved")
	}
	// Exists, but can't be updated
	cs := fake.NewSimpleClientset(&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: ConfigMapName, Namespace: ConfigMapNamespace}})
	cs.PrependReactor("update", "configmaps", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("forbidden")
	})
	if err := st.Save(ctx, cs); err == nil {
		t.Error("a state that can't be updated can't be saved")
	}
	// An existing ConfigMap without data is filled in
	empty := fake.NewSimpleClientset(&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: ConfigMapName, Namespace: ConfigMapNamespace}})
	if err := st.Save(ctx, empty); err != nil {
		t.Fatal(err)
	}
	if loaded, err := Load(ctx, empty, "c"); err != nil || loaded == nil {
		t.Errorf("loaded = %v, %v", loaded, err)
	}
}

func TestStateSmallCases(t *testing.T) {
	st := New("c", false, false, 0, false, 0)
	// The anonymous project has no orphans: nothing is known to be its own
	if orphans := st.Orphans(nil); orphans != nil {
		t.Errorf("orphans = %v", orphans)
	}
	if st.NamespaceCreatedByKraze("nowhere") {
		t.Error("no service records creating that namespace")
	}
	st.SetProject("web")
	if st.SetImageHashes("missing", map[string]string{"a": "b"}) {
		t.Error("hashes can't be set for a service that isn't installed")
	}
}

// Cluster-level fields: a loaded state's changes overwrite what's stored; a
// new state's only fill in what's missing
func TestSaveClusterMetadata(t *testing.T) {
	ctx := context.Background()
	cs := fake.NewSimpleClientset()
	first := New("c", false, false, 0, false, 0)
	first.SetProject("web")
	if err := first.Save(ctx, cs); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(ctx, cs, "c")
	if err != nil {
		t.Fatal(err)
	}
	loaded.SetClusterConfig("hash-2", "web")
	if err := loaded.Save(ctx, cs); err != nil {
		t.Fatal(err)
	}
	if again, _ := Load(ctx, cs, "c"); again.ClusterConfigHash != "hash-2" || again.CreatedBy != "web" {
		t.Errorf("stored = %q by %q", again.ClusterConfigHash, again.CreatedBy)
	}

	// Stored without a cluster name (an older kraze): a new state fills it in
	noName := fake.NewSimpleClientset(&corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: ConfigMapName, Namespace: ConfigMapNamespace},
		Data:       map[string]string{ConfigMapDataKey: `{"version": 4}`},
	})
	fresh := New("named", false, false, 0, false, 0)
	fresh.SetProject("web")
	if err := fresh.Save(ctx, noName); err != nil {
		t.Fatal(err)
	}
	if got, _ := Load(ctx, noName, "named"); got.ClusterName != "named" {
		t.Errorf("cluster name = %q", got.ClusterName)
	}
}
