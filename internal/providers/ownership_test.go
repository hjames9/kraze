package providers

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hjames9/kraze/internal/config"
	"github.com/hjames9/kraze/internal/state"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8stesting "k8s.io/client-go/testing"
)

var (
	configMapGVR   = schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}
	clusterRoleGVR = schema.GroupVersionResource{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "clusterroles"}
)

const ownershipManifests = `apiVersion: v1
kind: ConfigMap
metadata:
  name: settings
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: reader
`

// fakeManifestsProvider returns a provider acting as project, backed by a
// fake dynamic client holding objs
func fakeManifestsProvider(project string, objs ...runtime.Object) *ManifestsProvider {
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		configMapGVR:   "ConfigMapList",
		clusterRoleGVR: "ClusterRoleList",
	}, objs...)
	mapper := meta.NewDefaultRESTMapper(nil)
	mapper.Add(schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"}, meta.RESTScopeNamespace)
	mapper.Add(schema.GroupVersionKind{Group: "rbac.authorization.k8s.io", Version: "v1", Kind: "ClusterRole"}, meta.RESTScopeRoot)
	return &ManifestsProvider{opts: &ProviderOptions{Project: project, Quiet: true}, dynamicClient: dyn, mapper: mapper}
}

// object builds an existing cluster object with the given labels and annotations
func object(apiVersion, kind, namespace, name string, labels, annotations map[string]string) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{}
	obj.SetAPIVersion(apiVersion)
	obj.SetKind(kind)
	obj.SetNamespace(namespace)
	obj.SetName(name)
	obj.SetLabels(labels)
	obj.SetAnnotations(annotations)
	return obj
}

func krazeLabels(service, project string) map[string]string {
	labels := map[string]string{managedByLabel: "kraze", serviceLabel: service}
	if project != "" {
		labels[projectLabel] = project
	}
	return labels
}

func ownershipService(t *testing.T) *config.ServiceConfig {
	t.Helper()
	path := filepath.Join(t.TempDir(), "app.yaml")
	if err := os.WriteFile(path, []byte(ownershipManifests), 0o644); err != nil {
		t.Fatal(err)
	}
	noCreate := false
	return &config.ServiceConfig{Name: "app", Type: "manifests", Namespace: "apps", Path: path, CreateNamespace: &noCreate}
}

func TestInstallOwnership(t *testing.T) {
	tests := []struct {
		name     string
		existing *unstructured.Unstructured
		wantErr  string // empty: install succeeds
	}{
		{name: "nothing exists"},
		{name: "unlabeled object is adopted",
			existing: object("v1", "ConfigMap", "apps", "settings", nil, nil)},
		{name: "our own object",
			existing: object("v1", "ConfigMap", "apps", "settings", krazeLabels("app", "web"), nil)},
		{name: "older kraze labeled it for this service",
			existing: object("v1", "ConfigMap", "apps", "settings", krazeLabels("app", ""), nil)},
		{name: "another service in this project",
			existing: object("v1", "ConfigMap", "apps", "settings", krazeLabels("other", "web"), nil),
			wantErr:  "ConfigMap/apps/settings is owned by kraze service 'other'\nif kraze service 'other' was renamed or removed from this config, `kraze up --remove-orphans` removes it first"},
		{name: "same service name in another project",
			existing: object("rbac.authorization.k8s.io/v1", "ClusterRole", "", "reader", krazeLabels("app", "billing"), nil),
			wantErr:  "ClusterRole/reader is owned by kraze service 'app' of project 'billing'"},
		{name: "a Helm release's object",
			existing: object("v1", "ConfigMap", "apps", "settings", map[string]string{managedByLabel: "Helm"}, map[string]string{helmReleaseAnnotation: "postgres"}),
			wantErr:  "owned by Helm release 'postgres'"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var objs []runtime.Object
			if tt.existing != nil {
				objs = append(objs, tt.existing)
			}
			mp := fakeManifestsProvider("web", objs...)
			err := mp.Install(context.Background(), ownershipService(t))

			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("Install: %v", err)
				}
				cm, err := mp.dynamicClient.Resource(configMapGVR).Namespace("apps").Get(context.Background(), "settings", metav1.GetOptions{})
				if err != nil {
					t.Fatal(err)
				}
				if cm.GetLabels()[projectLabel] != "web" || cm.GetLabels()[serviceLabel] != "app" {
					t.Errorf("applied object labels = %v", cm.GetLabels())
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Install error = %v, want it to contain %q", err, tt.wantErr)
			}
			// Nothing may be applied when one object conflicts
			if tt.existing.GetKind() == "ClusterRole" {
				if _, err := mp.dynamicClient.Resource(configMapGVR).Namespace("apps").Get(context.Background(), "settings", metav1.GetOptions{}); err == nil {
					t.Error("the ConfigMap was applied although the ClusterRole conflicts")
				}
			}
		})
	}
}

func TestUninstallSkipsForeignObjects(t *testing.T) {
	mp := fakeManifestsProvider("web",
		object("v1", "ConfigMap", "apps", "settings", krazeLabels("app", "web"), nil),
		object("rbac.authorization.k8s.io/v1", "ClusterRole", "", "reader", krazeLabels("app", "billing"), nil),
	)
	if err := mp.Uninstall(context.Background(), ownershipService(t)); err != nil {
		t.Fatal(err)
	}
	if _, err := mp.dynamicClient.Resource(configMapGVR).Namespace("apps").Get(context.Background(), "settings", metav1.GetOptions{}); err == nil {
		t.Error("our ConfigMap should be deleted")
	}
	if _, err := mp.dynamicClient.Resource(clusterRoleGVR).Get(context.Background(), "reader", metav1.GetOptions{}); err != nil {
		t.Error("billing's ClusterRole must survive web's uninstall")
	}
}

func TestResourceRefsAndDeleteResources(t *testing.T) {
	refs, err := fakeManifestsProvider("web").ResourceRefs(ownershipService(t))
	if err != nil {
		t.Fatal(err)
	}
	want := []state.ResourceRef{
		{APIVersion: "v1", Kind: "ConfigMap", Namespace: "apps", Name: "settings"},
		{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "ClusterRole", Name: "reader"},
	}
	if len(refs) != len(want) || refs[0] != want[0] || refs[1] != want[1] {
		t.Fatalf("ResourceRefs = %+v, want %+v", refs, want)
	}

	// The ClusterRole is unlabeled (replaced by hand), so it is kept
	mp := fakeManifestsProvider("web",
		object("v1", "ConfigMap", "apps", "settings", krazeLabels("app", "web"), nil),
		object("rbac.authorization.k8s.io/v1", "ClusterRole", "", "reader", nil, nil),
	)
	deleted, err := mp.DeleteResources(context.Background(), "app", refs)
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 1 {
		t.Errorf("deleted = %d, want 1", deleted)
	}
	if _, err := mp.dynamicClient.Resource(clusterRoleGVR).Get(context.Background(), "reader", metav1.GetOptions{}); err != nil {
		t.Error("an object not labeled as the service must not be deleted")
	}

	// Objects already gone are not an error
	if _, err := mp.DeleteResources(context.Background(), "app", refs); err != nil {
		t.Errorf("deleting again: %v", err)
	}
}

func TestCheckOwnershipIgnoresObjectsThatCantBeRead(t *testing.T) {
	// A service that ships a CRD with its custom resources: before the CRD is
	// installed, looking up a custom resource fails. That must not block the
	// install, nor hide a real conflict on another object.
	mp := fakeManifestsProvider("web",
		object("v1", "ConfigMap", "apps", "settings", krazeLabels("other", "web"), nil),
	)
	lookups := 0
	mp.dynamicClient.(*dynamicfake.FakeDynamicClient).PrependReactor("get", "widgets",
		func(k8stesting.Action) (bool, runtime.Object, error) {
			lookups++
			return true, nil, fmt.Errorf("the server could not find the requested resource")
		})

	widget := object("example.com/v1", "Widget", "apps", "w1", nil, nil)
	if err := mp.checkOwnership(context.Background(), []*unstructured.Unstructured{widget}, "app"); err != nil {
		t.Errorf("an unreadable object (CRD not installed yet) must not block the install: %v", err)
	}
	if lookups == 0 {
		t.Fatal("the Widget lookup never failed; the test isn't exercising the error path")
	}

	settings := object("v1", "ConfigMap", "apps", "settings", nil, nil)
	err := mp.checkOwnership(context.Background(), []*unstructured.Unstructured{widget, settings}, "app")
	if err == nil || !strings.Contains(err.Error(), "ConfigMap/apps/settings") || strings.Contains(err.Error(), "Widget") {
		t.Errorf("expected only the ConfigMap conflict, got %v", err)
	}
}

func TestDeleteResourcesSkipsSameServiceNameInAnotherProject(t *testing.T) {
	mp := fakeManifestsProvider("web",
		object("rbac.authorization.k8s.io/v1", "ClusterRole", "", "reader", krazeLabels("app", "billing"), nil),
	)
	refs := []state.ResourceRef{{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "ClusterRole", Name: "reader"}}

	deleted, err := mp.DeleteResources(context.Background(), "app", refs)
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 0 {
		t.Errorf("deleted = %d, want 0", deleted)
	}
	if _, err := mp.dynamicClient.Resource(clusterRoleGVR).Get(context.Background(), "reader", metav1.GetOptions{}); err != nil {
		t.Error("billing's 'app' ClusterRole must survive removal of web's 'app' orphan")
	}
}
