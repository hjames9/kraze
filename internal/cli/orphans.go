package cli

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/hjames9/kraze/internal/color"
	"github.com/hjames9/kraze/internal/config"
	"github.com/hjames9/kraze/internal/history"
	"github.com/hjames9/kraze/internal/providers"
	"github.com/hjames9/kraze/internal/state"
	"k8s.io/client-go/kubernetes"
)

// orphanBackend removes an orphan's resources from the cluster. It is an
// interface so tests can stand in for the providers.
type orphanBackend interface {
	// helmReleaseExists reports whether a Helm release named svc.Name exists
	// in svc's namespace
	helmReleaseExists(ctx context.Context, svc *config.ServiceConfig) (bool, error)
	// uninstallHelm uninstalls a Helm release, keeping its CRDs
	uninstallHelm(ctx context.Context, svc *config.ServiceConfig) error
	// deleteManifests deletes the objects a manifests service applied
	deleteManifests(ctx context.Context, name string, refs []state.ResourceRef) (int, error)
}

// providerOrphanBackend is the orphanBackend that uses the real providers
type providerOrphanBackend struct {
	opts *providers.ProviderOptions
}

func (b providerOrphanBackend) helmReleaseExists(ctx context.Context, svc *config.ServiceConfig) (bool, error) {
	helm, err := providers.NewHelmProvider(b.opts)
	if err != nil {
		return false, err
	}
	return helm.IsInstalled(ctx, svc)
}

func (b providerOrphanBackend) uninstallHelm(ctx context.Context, svc *config.ServiceConfig) error {
	helm, err := providers.NewHelmProvider(b.opts)
	if err != nil {
		return err
	}
	return helm.Uninstall(ctx, svc)
}

func (b providerOrphanBackend) deleteManifests(ctx context.Context, name string, refs []state.ResourceRef) (int, error) {
	manifests, err := providers.NewManifestsProvider(b.opts)
	if err != nil {
		return 0, err
	}
	return manifests.DeleteResources(ctx, name, refs)
}

// configuredServices returns the names of every service in the config,
// enabled or not. A disabled service is not an orphan: `down --disabled`
// removes it.
func configuredServices(cfg *config.Config) map[string]bool {
	names := make(map[string]bool, len(cfg.Services))
	for name := range cfg.Services {
		names[name] = true
	}
	return names
}

// configuredNamespaces returns the namespaces of every service in the config.
// `up --remove-orphans` keeps them: a renamed service's new name isn't
// installed yet when its old name is removed, but it needs the namespace.
func configuredNamespaces(cfg *config.Config) map[string]bool {
	namespaces := make(map[string]bool, len(cfg.Services))
	for _, svc := range cfg.Services {
		namespaces[svc.GetNamespace()] = true
	}
	return namespaces
}

// movedInstall returns a configured service's installed entry when it no
// longer matches the config: the service moved to another namespace or
// changed type since it was installed. Its installation must then be removed
// by what was recorded, not by the config, which would look in the wrong
// place. Entries from older kraze have no recorded type; only their
// namespace is compared.
func movedInstall(st *state.ClusterState, svc *config.ServiceConfig) (state.ServiceMetadata, bool) {
	meta, ok := st.GetService(svc.Name)
	if !ok {
		return state.ServiceMetadata{}, false
	}
	moved := meta.Namespace != svc.GetNamespace() || (meta.Type != "" && meta.Type != svc.Type)
	return meta, moved
}

// movedInstalls returns the installed entries of services that moved
// namespace or changed type, sorted by name
func movedInstalls(st *state.ClusterState, services map[string]config.ServiceConfig) []state.ServiceMetadata {
	var moved []state.ServiceMetadata
	for _, svc := range services {
		if meta, ok := movedInstall(st, &svc); ok {
			moved = append(moved, meta)
		}
	}
	slices.SortFunc(moved, func(a, b state.ServiceMetadata) int { return strings.Compare(a.Name, b.Name) })
	return moved
}

// describeMove says how a moved service's installation differs from its config
func describeMove(meta state.ServiceMetadata, svc config.ServiceConfig) string {
	from := meta.Type
	if from == "" {
		from = "installed"
	}
	return fmt.Sprintf("%s (%s in '%s', now %s in '%s')", meta.Name, from, meta.Namespace, svc.Type, svc.GetNamespace())
}

// movedServicesError refuses an `up` that would leave moved services' old
// installations behind, untracked
func movedServicesError(moved []state.ServiceMetadata, services map[string]config.ServiceConfig) error {
	descriptions := make([]string, 0, len(moved))
	for _, meta := range moved {
		descriptions = append(descriptions, describeMove(meta, services[meta.Name]))
	}
	return fmt.Errorf("service(s) changed namespace or type since they were installed:\n  %s\n"+
		"installing them again would leave the old installation behind. Run a full `kraze up --remove-orphans` "+
		"to remove the old installation first (its data is lost), or revert the change",
		strings.Join(descriptions, "\n  "))
}

// orphanNames lists the names of orphans
func orphanNames(orphans []state.ServiceMetadata) []string {
	names := make([]string, 0, len(orphans))
	for _, o := range orphans {
		names = append(names, o.Name)
	}
	return names
}

// printOrphanHint tells the user about services this project installed that
// are no longer in its config
func printOrphanHint(orphans []state.ServiceMetadata) {
	if len(orphans) == 0 {
		return
	}
	fmt.Printf("%s %d service(s) installed by this project are no longer in its config: %s (use --remove-orphans to remove them)\n",
		color.Warning(), len(orphans), strings.Join(orphanNames(orphans), ", "))
}

// orphanServiceConfig is the minimal config needed to act on an orphan.
// Orphan removal keeps CRDs: whether the service wanted them deleted
// (keep_crds) is no longer known, and keeping them is the safe direction.
func orphanServiceConfig(o state.ServiceMetadata, serviceType string) *config.ServiceConfig {
	keepCRDs := true
	return &config.ServiceConfig{Name: o.Name, Type: serviceType, Namespace: o.Namespace, KeepCRDs: &keepCRDs}
}

// removeOrphans removes this project's services that are no longer in its
// config, then deletes the namespaces kraze created for them that nothing else
// uses, other than keepNamespaces. A service is removed according to the type recorded when it was
// installed. One installed by an older kraze has no recorded type: it is
// removed as a Helm release if one exists by its name. Otherwise it was a
// manifests service whose objects are no longer known, so kraze forgets it and
// prints the command that deletes its objects by their kraze.service label.
func removeOrphans(ctx context.Context, st *state.ClusterState, clientset kubernetes.Interface, kubeconfig string,
	orphans []state.ServiceMetadata, keepNamespaces map[string]bool, backend orphanBackend, deleteUnmanaged bool, rec *history.Recorder, verbosef func(string, ...interface{})) error {
	if len(orphans) == 0 {
		return nil
	}
	fmt.Printf("\nRemoving old installation(s) of: %s\n", strings.Join(orphanNames(orphans), ", "))

	// Collect namespace candidates before removal, which drops the entries
	services := make([]*config.ServiceConfig, 0, len(orphans))
	for _, o := range orphans {
		services = append(services, orphanServiceConfig(o, o.Type))
	}
	candidates := collectNamespaceCandidates(st, services)
	for ns := range keepNamespaces {
		delete(candidates, ns)
	}

	var failed []string
	for _, o := range orphans {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		start := time.Now()
		removed, err := removeOrphan(ctx, o, backend, verbosef)
		if err != nil {
			rec.RecordChange(o.Name, history.ActionUninstall, time.Since(start), err)
			fmt.Printf("%s Failed to remove old installation of '%s': %v\n", color.Warning(), o.Name, err)
			failed = append(failed, o.Name)
			keepServiceNamespaces(candidates, o) // its resources may still be there
			continue
		}

		if removed {
			rec.RecordChange(o.Name, history.ActionUninstall, time.Since(start), nil)
			fmt.Printf("%s Removed old installation of '%s'\n", color.Checkmark(), o.Name)
		} else {
			delete(candidates, o.Namespace) // its objects are still there
			fmt.Printf("%s Forgot '%s'. It was installed by an older kraze that didn't record its objects; delete them with:\n"+
				"    kubectl delete all,configmap,secret,pvc,ingress -n %s -l kraze.service=%s\n"+
				"  plus any cluster-scoped objects it created.\n", color.Warning(), o.Name, o.Namespace, o.Name)
		}
		st.MarkServiceUninstalled(o.Name)
		if err := st.Save(ctx, clientset); err != nil {
			verbosef("Warning: failed to save cluster state: %v", err)
		}
	}

	cleanupNamespaces(ctx, clientset, kubeconfig, candidates, deleteUnmanaged, verbosef)

	if len(failed) > 0 {
		return fmt.Errorf("could not remove old installation(s) of: %s", strings.Join(failed, ", "))
	}
	return nil
}

// removeOrphan removes one orphan's resources. It returns false with no error
// for a service whose objects it doesn't know (a manifests service installed
// by an older kraze), which the caller only forgets.
func removeOrphan(ctx context.Context, o state.ServiceMetadata, backend orphanBackend, verbosef func(string, ...interface{})) (bool, error) {
	switch o.Type {
	case "helm":
		return true, uninstallOrphanRelease(ctx, orphanServiceConfig(o, "helm"), backend, verbosef)
	case "manifests":
		deleted, err := backend.deleteManifests(ctx, o.Name, o.Resources)
		verbosef("Deleted %d object(s) of '%s'", deleted, o.Name)
		return err == nil, err
	}

	svc := orphanServiceConfig(o, "helm")
	exists, err := backend.helmReleaseExists(ctx, svc)
	if err != nil {
		return false, err
	}
	if !exists {
		return false, nil
	}
	return true, uninstallOrphanRelease(ctx, svc, backend, verbosef)
}

// uninstallOrphanRelease uninstalls an orphan's Helm release. A release that is
// already gone counts as removed: deleting its namespace removes Helm's release
// records with it, and the release can also vanish after helmReleaseExists saw
// it. Any other error is a failure, and the caller keeps the orphan's entry.
func uninstallOrphanRelease(ctx context.Context, svc *config.ServiceConfig, backend orphanBackend, verbosef func(string, ...interface{})) error {
	err := backend.uninstallHelm(ctx, svc)
	if providers.IsReleaseNotFound(err) {
		verbosef("Helm release '%s' in namespace '%s' is already gone", svc.Name, svc.Namespace)
		return nil
	}
	return err
}
