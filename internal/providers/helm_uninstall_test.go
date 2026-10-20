package providers

import (
	"errors"
	"fmt"
	"io"
	"testing"

	"helm.sh/helm/v4/pkg/action"
	kubefake "helm.sh/helm/v4/pkg/kube/fake"
	"helm.sh/helm/v4/pkg/release"
	"helm.sh/helm/v4/pkg/storage"
	"helm.sh/helm/v4/pkg/storage/driver"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// failingQueryDriver is a release store whose lookups fail, as listing Helm's
// release Secrets does when kraze isn't allowed to or can't reach the cluster
type failingQueryDriver struct {
	*driver.Memory
	err error
}

func (d failingQueryDriver) Query(map[string]string) ([]release.Releaser, error) {
	return nil, fmt.Errorf("query: failed to query with labels: %w", d.err)
}

// uninstallError runs Helm's uninstall against store, wrapped as Uninstall
// wraps it
func uninstallError(store driver.Driver, name string) error {
	cfg := &action.Configuration{
		Releases:   storage.Init(store),
		KubeClient: &kubefake.PrintingKubeClient{Out: io.Discard},
	}
	if _, err := action.NewUninstall(cfg).Run(name); err != nil {
		return fmt.Errorf("failed to uninstall chart: %w", err)
	}
	return nil
}

// IsReleaseNotFound is checked against the errors Helm's uninstall returns,
// not ones built to match it
func TestIsReleaseNotFound(t *testing.T) {
	missing := uninstallError(driver.NewMemory(), "gateway")
	if missing == nil {
		t.Fatal("uninstalling a missing release should fail")
	}
	if !IsReleaseNotFound(missing) {
		t.Errorf("Helm's error for a missing release isn't recognised: %v", missing)
	}

	forbidden := apierrors.NewForbidden(schema.GroupResource{Resource: "secrets"}, "", errors.New("RBAC: access denied"))
	for name, err := range map[string]error{
		"forbidden":   uninstallError(failingQueryDriver{driver.NewMemory(), forbidden}, "gateway"),
		"unreachable": uninstallError(failingQueryDriver{driver.NewMemory(), errors.New("dial tcp 172.18.0.2:6443: connect: connection refused")}, "gateway"),
		"other":       errors.New("failed to uninstall chart: uninstallation completed with 1 error(s): timed out"),
		"nil":         nil,
	} {
		if IsReleaseNotFound(err) {
			t.Errorf("%s: %v must not count as an already removed release", name, err)
		}
	}
}
