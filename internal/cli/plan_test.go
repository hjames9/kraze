package cli

import (
	"testing"

	"github.com/hjames9/kraze/internal/config"
	"github.com/hjames9/kraze/internal/state"
)

func TestNamespaceAction(t *testing.T) {
	cluster := newClusterNamespaces("default", "kube-system", "data", "apps")
	unknown := clusterNamespaces{}
	createdByKraze := map[string]int{"apps": 1, "gone": 1}

	tests := []struct {
		name         string
		namespace    string
		shouldCreate bool
		namespaces   clusterNamespaces
		want         string
	}{
		// Regression: pre-existing namespaces were reported as "will be created"
		{name: "pre-existing namespace", namespace: "default", shouldCreate: true, namespaces: cluster, want: namespaceExisting},
		{name: "pre-existing namespace, create disabled", namespace: "data", shouldCreate: false, namespaces: cluster, want: namespaceExisting},
		// Regression: a namespace created for another service was not "managed"
		{name: "created by kraze for any service", namespace: "apps", shouldCreate: true, namespaces: cluster, want: namespaceManaged},
		{name: "missing, will be created", namespace: "new", shouldCreate: true, namespaces: cluster, want: namespaceWillCreate},
		// Regression: a missing namespace with create disabled was "existing"
		{name: "missing, create disabled", namespace: "new", shouldCreate: false, namespaces: cluster, want: namespaceMissing},
		{name: "created by kraze but since deleted", namespace: "gone", shouldCreate: true, namespaces: cluster, want: namespaceWillCreate},
		{name: "cluster unknown, create enabled", namespace: "new", shouldCreate: true, namespaces: unknown, want: namespaceCreateIfNeed},
		{name: "cluster unknown, create disabled", namespace: "new", shouldCreate: false, namespaces: unknown, want: namespaceMustExist},
		{name: "cluster unknown, recorded as created", namespace: "apps", shouldCreate: true, namespaces: unknown, want: namespaceManaged},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := namespaceAction(tt.namespace, tt.shouldCreate, createdByKraze, tt.namespaces)
			if got != tt.want {
				t.Errorf("namespaceAction(%q) = %q, want %q", tt.namespace, got, tt.want)
			}
		})
	}
}

func TestNamespaceActionForNewKindCluster(t *testing.T) {
	// A kind cluster that doesn't exist yet will still have the built-in namespaces
	cn := newClusterNamespaces(builtinNamespaces...)

	if got := namespaceAction("default", true, nil, cn); got != namespaceExisting {
		t.Errorf("default namespace: got %q, want %q", got, namespaceExisting)
	}
	if got := namespaceAction("apps", true, nil, cn); got != namespaceWillCreate {
		t.Errorf("new namespace: got %q, want %q", got, namespaceWillCreate)
	}
}

func TestAnalyzeServiceUsesClusterNamespaces(t *testing.T) {
	st := state.New("test", false, false, 0, false, 0)
	// frontend is installed into a namespace that kraze created for backend
	st.MarkServiceInstalledWithNamespace("backend", "apps", true)
	st.MarkServiceInstalledWithNamespace("frontend", "apps", false)

	svc := &config.ServiceConfig{Name: "frontend", Type: "manifests", Namespace: "apps", Path: "./manifests"}
	info := analyzeService(svc, st, newClusterNamespaces("default", "apps"))

	if info.Action != "no-change" {
		t.Errorf("Action = %q, want no-change", info.Action)
	}
	if info.NamespaceAction != namespaceManaged {
		t.Errorf("NamespaceAction = %q, want %q", info.NamespaceAction, namespaceManaged)
	}
}
