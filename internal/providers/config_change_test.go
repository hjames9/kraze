package providers

import (
	"context"
	"errors"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Renaming a manifests service keeps its objects' names, which still carry
// the old service's label. The new name can only be installed once the old
// one (now an orphan) is removed, which is why `up --remove-orphans` removes
// orphans before installing.
func TestRenamedManifestsServiceNeedsOrphanRemovedFirst(t *testing.T) {
	ctx := context.Background()
	mp := fakeManifestsProvider("web")
	old := ownershipService(t)
	if err := mp.Install(ctx, old); err != nil {
		t.Fatal(err)
	}
	refs, err := mp.ResourceRefs(old)
	if err != nil {
		t.Fatal(err)
	}

	renamed := *old
	renamed.Name = "backend"
	err = mp.Install(ctx, &renamed)
	if err == nil || !strings.Contains(err.Error(), "app") {
		t.Fatalf("installing the new name over the old one's objects should be refused, got %v", err)
	}
	// up doesn't record a refused install (nothing was applied)
	if !errors.Is(err, ErrOwnershipConflict) {
		t.Errorf("a refusal must be ErrOwnershipConflict, got %v", err)
	}

	if _, err := mp.DeleteResources(ctx, old.Name, refs); err != nil {
		t.Fatal(err)
	}
	if err := mp.Install(ctx, &renamed); err != nil {
		t.Fatalf("once the old name is removed, the new one installs: %v", err)
	}
	cm, err := mp.dynamicClient.Resource(configMapGVR).Namespace("apps").Get(ctx, "settings", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got := cm.GetLabels()[serviceLabel]; got != "backend" {
		t.Errorf("object labelled %q, want the new service name", got)
	}
}

// Renaming the project makes its own objects look like another project's, so
// install is refused until the old project's services are removed (with the
// old project name, e.g. `kraze down` before the rename)
func TestRenamedProjectIsRefused(t *testing.T) {
	ctx := context.Background()
	svc := ownershipService(t)
	mp := fakeManifestsProvider("web")
	if err := mp.Install(ctx, svc); err != nil {
		t.Fatal(err)
	}

	mp.opts = &ProviderOptions{Project: "storefront", Quiet: true}
	err := mp.Install(ctx, svc)
	if err == nil || !strings.Contains(err.Error(), "web") {
		t.Errorf("the renamed project should be refused, naming the old one; got %v", err)
	}
}
