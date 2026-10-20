package providers

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

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

const crdManifests = `apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: gadgets.example.com
spec:
  group: example.com
  scope: Cluster
---
apiVersion: example.com/v1
kind: Gadget
metadata:
  name: g1
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: settings
`

var (
	crdGVR    = schema.GroupVersionResource{Group: "apiextensions.k8s.io", Version: "v1", Resource: "customresourcedefinitions"}
	gadgetGVR = schema.GroupVersionResource{Group: "example.com", Version: "v1", Resource: "gadgets"}
	gadgetGVK = schema.GroupVersionKind{Group: "example.com", Version: "v1", Kind: "Gadget"}
)

// discoveringMapper stands in for kraze's discovery-backed mapper: it learns
// the cluster-scoped Gadget kind only when Reset, as if its CRD had just been
// installed
type discoveringMapper struct {
	*meta.DefaultRESTMapper
	resets int
}

func (m *discoveringMapper) Reset() {
	m.resets++
	m.Add(gadgetGVK, meta.RESTScopeRoot)
}

// crdProvider returns a provider whose fake cluster marks CRDs Established as
// soon as they are created, unless establish is false
func crdProvider(t *testing.T, establish bool) (*ManifestsProvider, *discoveringMapper, *dynamicfake.FakeDynamicClient) {
	t.Helper()
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		configMapGVR: "ConfigMapList",
		crdGVR:       "CustomResourceDefinitionList",
		gadgetGVR:    "GadgetList",
	})
	if establish {
		dyn.PrependReactor("create", "customresourcedefinitions", func(action k8stesting.Action) (bool, runtime.Object, error) {
			obj := action.(k8stesting.CreateAction).GetObject().(*unstructured.Unstructured)
			_ = unstructured.SetNestedSlice(obj.Object, []interface{}{
				map[string]interface{}{"type": "Established", "status": "True"},
			}, "status", "conditions")
			return false, nil, nil // let the tracker store the established CRD
		})
	}

	mapper := &discoveringMapper{DefaultRESTMapper: meta.NewDefaultRESTMapper(nil)}
	mapper.Add(schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"}, meta.RESTScopeNamespace)
	mapper.Add(schema.GroupVersionKind{Group: "apiextensions.k8s.io", Version: "v1", Kind: "CustomResourceDefinition"}, meta.RESTScopeRoot)

	return &ManifestsProvider{opts: &ProviderOptions{Project: "web", Quiet: true}, dynamicClient: dyn, mapper: mapper}, mapper, dyn
}

func crdService(t *testing.T) *config.ServiceConfig {
	t.Helper()
	path := filepath.Join(t.TempDir(), "gadgets.yaml")
	if err := os.WriteFile(path, []byte(crdManifests), 0o644); err != nil {
		t.Fatal(err)
	}
	noCreate := false
	return &config.ServiceConfig{Name: "gadgets", Type: "manifests", Namespace: "apps", Path: path, CreateNamespace: &noCreate}
}

// actionsOn lists "verb resource/name" for the fake client's actions of verb
func actionsOn(dyn *dynamicfake.FakeDynamicClient, verb string) []string {
	var out []string
	for _, a := range dyn.Actions() {
		if a.GetVerb() != verb {
			continue
		}
		name := ""
		switch act := a.(type) {
		case k8stesting.CreateAction:
			name = act.GetObject().(*unstructured.Unstructured).GetName()
		case k8stesting.DeleteAction:
			name = act.GetName()
		}
		out = append(out, a.GetResource().Resource+"/"+name)
	}
	return out
}

func TestInstallAppliesCRDsFirstAndWaits(t *testing.T) {
	mp, mapper, dyn := crdProvider(t, true)
	if err := mp.Install(context.Background(), crdService(t)); err != nil {
		t.Fatalf("Install: %v", err)
	}

	want := []string{"customresourcedefinitions/gadgets.example.com", "gadgets/g1", "configmaps/settings"}
	if got := actionsOn(dyn, "create"); !slices.Equal(got, want) {
		t.Errorf("create order = %v, want %v", got, want)
	}
	if mapper.resets != 1 {
		t.Errorf("mapper reset %d times, want 1 (after the CRD is established)", mapper.resets)
	}

	// Gadget is cluster-scoped: once the mapper knows that, it must not get
	// the service's namespace
	if _, err := dyn.Resource(gadgetGVR).Get(context.Background(), "g1", metav1.GetOptions{}); err != nil {
		t.Errorf("cluster-scoped Gadget should exist without a namespace: %v", err)
	}
}

func TestInstallWithoutCRDsDoesNotWait(t *testing.T) {
	mp, mapper, _ := crdProvider(t, false)
	svc := ownershipService(t) // ConfigMap and ClusterRole, no CRDs
	mp.mapper.(*discoveringMapper).Add(schema.GroupVersionKind{Group: "rbac.authorization.k8s.io", Version: "v1", Kind: "ClusterRole"}, meta.RESTScopeRoot)
	if err := mp.Install(context.Background(), svc); err != nil {
		t.Fatalf("Install: %v", err)
	}
	if mapper.resets != 0 {
		t.Error("a service without CRDs shouldn't reset the mapper")
	}
}

func TestInstallFailsWhenCRDNeverEstablished(t *testing.T) {
	saved := crdEstablishTimeout
	crdEstablishTimeout = 300 * time.Millisecond
	t.Cleanup(func() { crdEstablishTimeout = saved })

	mp, _, dyn := crdProvider(t, false)
	err := mp.Install(context.Background(), crdService(t))
	if err == nil || !strings.Contains(err.Error(), "gadgets.example.com was not established") {
		t.Fatalf("expected a not-established error, got %v", err)
	}
	if got := actionsOn(dyn, "create"); !slices.Equal(got, []string{"customresourcedefinitions/gadgets.example.com"}) {
		t.Errorf("nothing but the CRD should be applied, got %v", got)
	}
}

func TestUninstallAndDeleteResourcesRemoveCRDsLast(t *testing.T) {
	mp, _, dyn := crdProvider(t, true)
	svc := crdService(t)
	if err := mp.Install(context.Background(), svc); err != nil {
		t.Fatal(err)
	}
	refs, err := mp.ResourceRefs(svc)
	if err != nil {
		t.Fatal(err)
	}

	dyn.ClearActions()
	if err := mp.Uninstall(context.Background(), svc); err != nil {
		t.Fatal(err)
	}
	want := []string{"gadgets/g1", "configmaps/settings", "customresourcedefinitions/gadgets.example.com"}
	if got := actionsOn(dyn, "delete"); !slices.Equal(got, want) {
		t.Errorf("Uninstall delete order = %v, want %v", got, want)
	}

	// The same order when removing it as an orphan from recorded refs
	if err := mp.Install(context.Background(), svc); err != nil {
		t.Fatal(err)
	}
	dyn.ClearActions()
	if _, err := mp.DeleteResources(context.Background(), "gadgets", refs); err != nil {
		t.Fatal(err)
	}
	if got := actionsOn(dyn, "delete"); !slices.Equal(got, want) {
		t.Errorf("DeleteResources delete order = %v, want %v", got, want)
	}
	if !slices.ContainsFunc(refs, func(r state.ResourceRef) bool { return r.Kind == "Gadget" && r.Namespace == "" }) {
		t.Errorf("recorded refs should hold the cluster-scoped Gadget without a namespace: %+v", refs)
	}
}
