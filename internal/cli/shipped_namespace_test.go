package cli

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/hjames9/kraze/internal/state"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// shippedNamespaces has manifests service c (namespace jobs) ship two
// Namespaces: shop, where a, b and d live, and extra, which no service uses
var shippedNamespaces = []state.ResourceRef{
	{APIVersion: "v1", Kind: "Namespace", Name: "shop"},
	{APIVersion: "v1", Kind: "Namespace", Name: "extra"},
	{APIVersion: "v1", Kind: "ConfigMap", Namespace: "jobs", Name: "settings"},
}

// installShipping installs kindConfig with c shipping shippedNamespaces. The
// provider applies them labelled as kraze's, like every object it applies.
func installShipping(t *testing.T) *fakeEnv {
	t.Helper()
	env := withFakeDeps(t)
	env.provider("c").refs = shippedNamespaces
	if _, err := env.clientset.CoreV1().Namespaces().Create(context.Background(), namespace("extra", true), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	installAll(t, env, kindConfig)
	return env
}

// A Namespace a manifests service ships is deleted by `down`'s namespace
// check, like the namespaces services are installed in: only once no other
// installed service uses it. The provider used to delete it with the
// service's objects, taking every other service in it along.
func TestDownDeletesShippedNamespaceOnlyWhenUnused(t *testing.T) {
	env := installShipping(t)

	// a, b and d still use shop
	if out, err := runCmd(t, runDown, "c"); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if slices.Contains(env.deletedNamespaces, "shop") {
		t.Errorf("shop deleted while other services use it: %v", env.deletedNamespaces)
	}
	if !slices.Contains(env.deletedNamespaces, "extra") || !slices.Contains(env.deletedNamespaces, "jobs") {
		t.Errorf("unused namespaces not deleted: %v", env.deletedNamespaces)
	}

	// Nothing uses it any more
	env.deletedNamespaces = nil
	if out, err := runCmd(t, runDown); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if !slices.Contains(env.deletedNamespaces, "shop") {
		t.Errorf("deleted %v, want shop", env.deletedNamespaces)
	}
}

func TestDownKeepsShippedNamespaceAnotherProjectUses(t *testing.T) {
	env := installShipping(t)
	other := env.state(t, "billing")
	other.MarkServiceInstalledWithNamespace("api", "extra", false)
	env.saveState(t, other)

	if out, err := runCmd(t, runDown); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if slices.Contains(env.deletedNamespaces, "extra") {
		t.Errorf("extra deleted while project billing uses it: %v", env.deletedNamespaces)
	}
	if !slices.Contains(env.deletedNamespaces, "shop") {
		t.Errorf("shop not deleted: %v", env.deletedNamespaces)
	}
}

// Removing an orphan applies the same check to the Namespaces it shipped
func TestRemoveOrphansDeletesShippedNamespaceOnlyWhenUnused(t *testing.T) {
	env := installShipping(t)
	// c leaves the config; a, b and d stay in shop
	withoutC := kindConfig[:strings.Index(kindConfig, "  c:\n")] + kindConfig[strings.Index(kindConfig, "  d:\n"):]
	useConfig(t, strings.Replace(withoutC, "depends_on: [b, c]", "depends_on: [b]", 1))
	setVar(t, &upRemoveOrphans, true)
	if out, err := runCmd(t, runUp); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if !slices.Contains(env.deletedNamespaces, "extra") || slices.Contains(env.deletedNamespaces, "shop") {
		t.Errorf("deleted %v, want extra and not shop", env.deletedNamespaces)
	}
}

// A service whose uninstall fails keeps the Namespaces it shipped, as it
// keeps its own: what's left of it may be in them
func TestDownFailureKeepsShippedNamespaces(t *testing.T) {
	env := installShipping(t)
	env.provider("c").uninstallErr = errors.New("forbidden")
	out, _ := runCmd(t, runDown, "c")
	assertContains(t, out, "forbidden")
	if len(env.deletedNamespaces) != 0 {
		t.Errorf("deleted %v", env.deletedNamespaces)
	}

	// The same for an orphan
	env = installShipping(t)
	env.orphans.deleteErr = errors.New("forbidden")
	withoutC := kindConfig[:strings.Index(kindConfig, "  c:\n")] + kindConfig[strings.Index(kindConfig, "  d:\n"):]
	useConfig(t, strings.Replace(withoutC, "depends_on: [b, c]", "depends_on: [b]", 1))
	setVar(t, &upRemoveOrphans, true)
	if _, err := runCmd(t, runUp); err == nil {
		t.Fatal("want the orphan removal failure")
	}
	if slices.Contains(env.deletedNamespaces, "extra") || slices.Contains(env.deletedNamespaces, "jobs") {
		t.Errorf("deleted %v", env.deletedNamespaces)
	}
}
