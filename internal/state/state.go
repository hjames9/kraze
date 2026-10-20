package state

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/retry"
)

const (
	// ConfigMapName is the name of the ConfigMap storing kraze metadata
	ConfigMapName = "kraze-metadata"

	// ConfigMapNamespace is the namespace where kraze metadata is stored
	ConfigMapNamespace = "kube-system"

	// ConfigMapDataKey is the key in the ConfigMap data field
	ConfigMapDataKey = "metadata"

	// CurrentStateVersion is the current version of the state format
	CurrentStateVersion = 4
)

// ClusterState represents the state of deployed services stored in the cluster.
//
// Several projects (kraze.yml files with different `project:` keys) can share
// one cluster. Services are keyed by serviceKey(project, name), so two projects
// can each have a service with the same name. A ClusterState acts on behalf of
// one project at a time, set with SetProject; the service methods below use it.
type ClusterState struct {
	Version           int                        `json:"version"` // State format version
	ClusterName       string                     `json:"cluster_name"`
	IsExternal        bool                       `json:"is_external"`                   // Whether this is an external cluster
	NvidiaGPUEnabled  bool                       `json:"nvidia_gpu_enabled,omitempty"`  // Whether cluster was created with NVIDIA GPU support
	NvidiaGPUCount    int                        `json:"nvidia_gpu_count,omitempty"`    // Number of NVIDIA GPUs configured at creation
	AMDGPUEnabled     bool                       `json:"amd_gpu_enabled,omitempty"`     // Whether cluster was created with AMD GPU support
	AMDGPUCount       int                        `json:"amd_gpu_count,omitempty"`       // Number of AMD GPUs configured at creation
	ClusterConfigHash string                     `json:"cluster_config_hash,omitempty"` // Hash of the cluster settings the cluster was created with
	CreatedBy         string                     `json:"created_by,omitempty"`          // Project that created the cluster ("" for anonymous)
	ConfigPaths       []string                   `json:"config_paths,omitempty"`        // v3 only: migrated into Projects[""] on load
	Projects          map[string]ProjectMetadata `json:"projects,omitempty"`            // Per-project metadata, keyed by project ("" for anonymous)
	Services          map[string]ServiceMetadata `json:"services"`
	LastUpdated       time.Time                  `json:"last_updated"`

	mu             sync.Mutex
	project        string          // project this process acts as
	dirtyMeta      bool            // cluster-level fields changed since last save
	dirtyServices  map[string]bool // service keys changed (set or deleted) since last save
	dirtyProjects  map[string]bool // project keys changed since last save
	pendingCreated bool            // state was created by New and never saved
}

// ProjectMetadata records what kraze knows about one project using the cluster
type ProjectMetadata struct {
	ConfigPaths []string  `json:"config_paths,omitempty"` // Absolute paths to the project's config files
	UpdatedAt   time.Time `json:"updated_at"`
}

// ServiceMetadata represents the metadata for a single service
type ServiceMetadata struct {
	Name             string            `json:"name"`
	Project          string            `json:"project,omitempty"` // Owning project; empty for anonymous or pre-project entries
	Installed        bool              `json:"installed"`
	UpdatedAt        time.Time         `json:"updated_at"`
	Namespace        string            `json:"namespace,omitempty"`         // The namespace this service is in
	CreatedNamespace bool              `json:"created_namespace,omitempty"` // Whether we created the namespace
	ImageHashes      map[string]string `json:"image_hashes,omitempty"`      // Map of image name to SHA256 hash
	Type             string            `json:"type,omitempty"`              // Service type ("helm" or "manifests"); empty for entries written by older kraze
	Resources        []ResourceRef     `json:"resources,omitempty"`         // Objects a manifests service applied, so it can be removed without its config
}

// ResourceRef identifies one Kubernetes object
type ResourceRef struct {
	APIVersion string `json:"api_version"`
	Kind       string `json:"kind"`
	Namespace  string `json:"namespace,omitempty"`
	Name       string `json:"name"`
	// Completed is set for a Job that `up` saw succeed, so `status` can tell
	// one deleted by its ttlSecondsAfterFinished from one that never ran
	Completed bool `json:"completed,omitempty"`
}

// serviceKey returns the state map key for a project's service. The anonymous
// project uses the bare service name, which is also how entries written before
// projects existed are keyed, so those need no migration.
func serviceKey(project, name string) string {
	if project == "" {
		return name
	}
	return project + "/" + name
}

// New creates a new empty cluster state
func New(clusterName string, isExternal bool, nvidiaGPUEnabled bool, nvidiaGPUCount int, amdGPUEnabled bool, amdGPUCount int) *ClusterState {
	return &ClusterState{
		Version:          CurrentStateVersion,
		ClusterName:      clusterName,
		IsExternal:       isExternal,
		NvidiaGPUEnabled: nvidiaGPUEnabled,
		NvidiaGPUCount:   nvidiaGPUCount,
		AMDGPUEnabled:    amdGPUEnabled,
		AMDGPUCount:      amdGPUCount,
		Projects:         make(map[string]ProjectMetadata),
		Services:         make(map[string]ServiceMetadata),
		LastUpdated:      time.Now(),
		dirtyMeta:        true,
		pendingCreated:   true,
	}
}

// Load reads the cluster state from a ConfigMap in the cluster
func Load(ctx context.Context, clientset kubernetes.Interface, clusterName string) (*ClusterState, error) {
	// Try to get the ConfigMap
	cm, err := clientset.CoreV1().ConfigMaps(ConfigMapNamespace).Get(ctx, ConfigMapName, metav1.GetOptions{})
	if err != nil {
		if errors.IsNotFound(err) {
			// ConfigMap doesn't exist yet, return nil (caller will create new state)
			return nil, nil
		}
		// Other errors (connection issues, permission denied, etc.)
		return nil, fmt.Errorf("failed to read cluster state ConfigMap: %w", err)
	}

	// Get the metadata from the ConfigMap
	metadataJSON, exists := cm.Data[ConfigMapDataKey]
	if !exists {
		// ConfigMap exists but has no data, return empty state
		return nil, nil
	}

	return decode(metadataJSON)
}

// decode parses and migrates a serialized state
func decode(metadataJSON string) (*ClusterState, error) {
	var state ClusterState
	if err := json.Unmarshal([]byte(metadataJSON), &state); err != nil {
		return nil, fmt.Errorf("failed to parse cluster state: %w", err)
	}

	// Handle migration from older versions
	if err := state.migrate(); err != nil {
		return nil, fmt.Errorf("failed to migrate cluster state: %w", err)
	}

	if state.Services == nil {
		state.Services = make(map[string]ServiceMetadata)
	}
	if state.Projects == nil {
		state.Projects = make(map[string]ProjectMetadata)
	}

	return &state, nil
}

// migrate handles migration from older state versions to the current version
func (cs *ClusterState) migrate() error {
	// Migrate from v0 (no version field) to v1
	if cs.Version == 0 {
		// v0 state had no version field
		// All existing fields are compatible with v1, just set the version
		cs.Version = 1
	}

	// Migrate from v1 to v2
	if cs.Version == 1 {
		// v1 state had no GPU fields.
		// NvidiaGPUEnabled, NvidiaGPUCount, AMDGPUEnabled, AMDGPUCount all default
		// to zero values (no GPU), which is correct for pre-GPU clusters.
		cs.Version = 2
	}

	// Migrate from v2 to v3
	if cs.Version == 2 {
		// v2 state had no config_paths field.
		// ConfigPaths defaults to nil — commands fall back to requiring -f or cwd/kraze.yml.
		cs.Version = 3
	}

	// Migrate from v3 to v4
	if cs.Version == 3 {
		// v3 state had one cluster-wide config_paths list and no projects.
		// Those paths belonged to the anonymous project. Services keep their bare
		// keys, which are the anonymous project's keys, and are claimed by a named
		// project the next time it runs (see Claim).
		if len(cs.ConfigPaths) > 0 {
			if cs.Projects == nil {
				cs.Projects = make(map[string]ProjectMetadata)
			}
			cs.Projects[""] = ProjectMetadata{ConfigPaths: cs.ConfigPaths, UpdatedAt: cs.LastUpdated}
		}
		cs.ConfigPaths = nil
		cs.Version = 4
	}

	// Check if version is supported
	if cs.Version > CurrentStateVersion {
		return fmt.Errorf("cluster state version %d is newer than supported version %d - please upgrade kraze",
			cs.Version, CurrentStateVersion)
	}

	return nil
}

// Save writes this process's changes to the cluster state ConfigMap.
//
// Several kraze runs (possibly for different projects) may write the state at
// once, so Save never writes its in-memory copy wholesale. It re-reads the
// ConfigMap, applies only the changes made through this ClusterState since the
// last save, and updates with the fetched resourceVersion, retrying on conflict.
// Afterwards the in-memory copy reflects the merged result.
func (cs *ClusterState) Save(ctx context.Context, clientset kubernetes.Interface) error {
	cs.mu.Lock()
	defer cs.mu.Unlock()

	configMaps := clientset.CoreV1().ConfigMaps(ConfigMapNamespace)
	retriable := func(err error) bool {
		return errors.IsConflict(err) || errors.IsAlreadyExists(err)
	}

	var merged *ClusterState
	err := retry.OnError(retry.DefaultRetry, retriable, func() error {
		cm, err := configMaps.Get(ctx, ConfigMapName, metav1.GetOptions{})
		if err != nil && !errors.IsNotFound(err) {
			return fmt.Errorf("failed to get cluster state ConfigMap: %w", err)
		}
		create := err != nil

		var base *ClusterState
		if !create {
			if data, ok := cm.Data[ConfigMapDataKey]; ok {
				// A corrupt state is replaced by ours rather than blocking every command
				base, _ = decode(data)
			}
		}
		merged = cs.applyChanges(base)
		merged.Version = CurrentStateVersion
		merged.LastUpdated = time.Now()

		data, err := json.MarshalIndent(merged, "", "  ")
		if err != nil {
			return fmt.Errorf("failed to marshal cluster state: %w", err)
		}

		if create {
			_, err = configMaps.Create(ctx, &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      ConfigMapName,
					Namespace: ConfigMapNamespace,
					Labels: map[string]string{
						"app.kubernetes.io/managed-by": "kraze",
					},
				},
				Data: map[string]string{
					ConfigMapDataKey: string(data),
				},
			}, metav1.CreateOptions{})
			return err
		}

		if cm.Data == nil {
			cm.Data = map[string]string{}
		}
		cm.Data[ConfigMapDataKey] = string(data)
		_, err = configMaps.Update(ctx, cm, metav1.UpdateOptions{})
		return err
	})
	if err != nil {
		return fmt.Errorf("failed to save cluster state: %w", err)
	}

	// Adopt the merged view so later reads see other runs' changes too
	cs.Version = merged.Version
	cs.LastUpdated = merged.LastUpdated
	cs.copyMetaFrom(merged)
	cs.Services = merged.Services
	cs.Projects = merged.Projects
	cs.dirtyMeta = false
	cs.dirtyServices = nil
	cs.dirtyProjects = nil
	cs.pendingCreated = false
	return nil
}

// applyChanges returns base with this process's unsaved changes applied.
// With no base (no ConfigMap yet, or unreadable), the result is a copy of cs.
// Caller holds cs.mu.
func (cs *ClusterState) applyChanges(base *ClusterState) *ClusterState {
	merged := &ClusterState{
		Projects: make(map[string]ProjectMetadata),
		Services: make(map[string]ServiceMetadata),
	}
	if base == nil {
		merged.copyMetaFrom(cs)
		for k, v := range cs.Services {
			merged.Services[k] = v
		}
		for k, v := range cs.Projects {
			merged.Projects[k] = v
		}
		return merged
	}

	merged.copyMetaFrom(base)
	for k, v := range base.Services {
		merged.Services[k] = v
	}
	for k, v := range base.Projects {
		merged.Projects[k] = v
	}

	// A state created by New only fills in cluster-level fields another run
	// hasn't already recorded; it never overrides them.
	if cs.dirtyMeta {
		if cs.pendingCreated {
			merged.fillMetaFrom(cs)
		} else {
			merged.copyMetaFrom(cs)
		}
	}
	for k := range cs.dirtyServices {
		if v, ok := cs.Services[k]; ok {
			merged.Services[k] = v
		} else {
			delete(merged.Services, k)
		}
	}
	for k := range cs.dirtyProjects {
		if v, ok := cs.Projects[k]; ok {
			merged.Projects[k] = v
		} else {
			delete(merged.Projects, k)
		}
	}
	return merged
}

// copyMetaFrom copies the cluster-level fields from other
func (cs *ClusterState) copyMetaFrom(other *ClusterState) {
	cs.ClusterName = other.ClusterName
	cs.IsExternal = other.IsExternal
	cs.NvidiaGPUEnabled = other.NvidiaGPUEnabled
	cs.NvidiaGPUCount = other.NvidiaGPUCount
	cs.AMDGPUEnabled = other.AMDGPUEnabled
	cs.AMDGPUCount = other.AMDGPUCount
	cs.ClusterConfigHash = other.ClusterConfigHash
	cs.CreatedBy = other.CreatedBy
}

// fillMetaFrom copies cluster-level fields from other only where cs has none
func (cs *ClusterState) fillMetaFrom(other *ClusterState) {
	if cs.ClusterName == "" {
		cs.ClusterName = other.ClusterName
	}
	if cs.ClusterConfigHash == "" {
		cs.ClusterConfigHash = other.ClusterConfigHash
		cs.CreatedBy = other.CreatedBy
	}
}

// markService records that a service key changed. Caller holds cs.mu.
func (cs *ClusterState) markService(key string) {
	if cs.dirtyServices == nil {
		cs.dirtyServices = make(map[string]bool)
	}
	cs.dirtyServices[key] = true
}

// markProject records that a project key changed. Caller holds cs.mu.
func (cs *ClusterState) markProject(project string) {
	if cs.dirtyProjects == nil {
		cs.dirtyProjects = make(map[string]bool)
	}
	cs.dirtyProjects[project] = true
}

// Delete removes the cluster state ConfigMap from the cluster
func Delete(ctx context.Context, clientset kubernetes.Interface) error {
	err := clientset.CoreV1().ConfigMaps(ConfigMapNamespace).Delete(ctx, ConfigMapName, metav1.DeleteOptions{})
	if err != nil {
		if errors.IsNotFound(err) {
			// ConfigMap doesn't exist, that's fine
			return nil
		}
		return fmt.Errorf("failed to delete cluster state ConfigMap: %w", err)
	}
	return nil
}

// SetProject sets the project this state acts on behalf of. Call it right
// after Load or New; the service methods below are scoped to it.
func (cs *ClusterState) SetProject(project string) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	cs.project = project
}

// Project returns the project this state acts on behalf of
func (cs *ClusterState) Project() string {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	return cs.project
}

// MarkServiceInstalledWithNamespace marks a service as installed and tracks namespace info.
// CreatedNamespace is sticky: once kraze has created a service's namespace, a
// later upgrade (when the namespace naturally already exists) keeps that record.
func (cs *ClusterState) MarkServiceInstalledWithNamespace(serviceName, namespace string, createdNamespace bool) {
	cs.mu.Lock()
	defer cs.mu.Unlock()

	key := serviceKey(cs.project, serviceName)

	// Preserve existing image hashes and source if they exist
	existingMetadata, exists := cs.Services[key]
	imageHashes := make(map[string]string)
	if exists {
		imageHashes = existingMetadata.ImageHashes
		if existingMetadata.CreatedNamespace && existingMetadata.Namespace == namespace {
			createdNamespace = true
		}
	}

	cs.Services[key] = ServiceMetadata{
		Name:             serviceName,
		Project:          cs.project,
		Installed:        true,
		UpdatedAt:        time.Now(),
		Namespace:        namespace,
		CreatedNamespace: createdNamespace,
		ImageHashes:      imageHashes,
		Type:             existingMetadata.Type,
		Resources:        existingMetadata.Resources,
	}
	cs.markService(key)
}

// SetServiceSource records how an installed service was installed: its type
// and, for a manifests service, the objects it applied. This is what lets an
// orphan (a service no longer in the config) be removed.
func (cs *ClusterState) SetServiceSource(serviceName, serviceType string, resources []ResourceRef) {
	cs.mu.Lock()
	defer cs.mu.Unlock()

	key := serviceKey(cs.project, serviceName)
	svc, exists := cs.Services[key]
	if !exists {
		return
	}
	svc.Type = serviceType
	svc.Resources = resources
	cs.Services[key] = svc
	cs.markService(key)
}

// Orphans returns the current project's installed services that are not in
// configured, sorted by name. Unowned entries (written before projects
// existed) are never orphans: no project has claimed them.
func (cs *ClusterState) Orphans(configured map[string]bool) []ServiceMetadata {
	cs.mu.Lock()
	defer cs.mu.Unlock()

	var orphans []ServiceMetadata
	if cs.project == "" {
		return orphans
	}
	for _, svc := range cs.Services {
		if svc.Project == cs.project && svc.Installed && !configured[svc.Name] {
			orphans = append(orphans, svc)
		}
	}
	sort.Slice(orphans, func(i, j int) bool { return orphans[i].Name < orphans[j].Name })
	return orphans
}

// MarkServiceUninstalled marks a service as uninstalled (removes it from state)
func (cs *ClusterState) MarkServiceUninstalled(serviceName string) {
	cs.mu.Lock()
	defer cs.mu.Unlock()

	key := serviceKey(cs.project, serviceName)
	delete(cs.Services, key)
	cs.markService(key)
}

// IsServiceInstalled checks if a service is marked as installed
func (cs *ClusterState) IsServiceInstalled(serviceName string) bool {
	cs.mu.Lock()
	defer cs.mu.Unlock()

	svc, exists := cs.Services[serviceKey(cs.project, serviceName)]
	return exists && svc.Installed
}

// GetService returns the current project's metadata for a service
func (cs *ClusterState) GetService(serviceName string) (ServiceMetadata, bool) {
	cs.mu.Lock()
	defer cs.mu.Unlock()

	svc, exists := cs.Services[serviceKey(cs.project, serviceName)]
	return svc, exists
}

// Claim adopts an unowned entry for serviceName into the current project.
// Entries written before projects existed, or by a config without `project:`,
// are keyed by bare name; when a named project's config contains the service,
// that project takes it over. Returns true if an entry was claimed.
func (cs *ClusterState) Claim(serviceName string) bool {
	cs.mu.Lock()
	defer cs.mu.Unlock()

	if cs.project == "" {
		return false
	}
	key := serviceKey(cs.project, serviceName)
	if _, exists := cs.Services[key]; exists {
		return false
	}
	legacy, exists := cs.Services[serviceName]
	if !exists {
		return false
	}

	legacy.Project = cs.project
	cs.Services[key] = legacy
	delete(cs.Services, serviceName)
	cs.markService(key)
	cs.markService(serviceName)
	return true
}

// OtherProjects returns the named projects, other than the current one, that
// own services in this cluster, sorted. A project that has run a full `down`
// keeps its config paths but no longer counts as using the cluster.
func (cs *ClusterState) OtherProjects() []string {
	cs.mu.Lock()
	defer cs.mu.Unlock()

	seen := make(map[string]bool)
	for _, svc := range cs.Services {
		if svc.Project != "" && svc.Project != cs.project {
			seen[svc.Project] = true
		}
	}

	projects := make([]string, 0, len(seen))
	for project := range seen {
		projects = append(projects, project)
	}
	sort.Strings(projects)
	return projects
}

// UnownedServices returns the names of services with no owning project, sorted
func (cs *ClusterState) UnownedServices() []string {
	cs.mu.Lock()
	defer cs.mu.Unlock()

	var names []string
	for _, svc := range cs.Services {
		if svc.Project == "" {
			names = append(names, svc.Name)
		}
	}
	sort.Strings(names)
	return names
}

// ConflictingOwner reports another project that has a service with the same
// name in the same namespace. Helm release names are the service name, so
// installing it would take over that project's release.
func (cs *ClusterState) ConflictingOwner(serviceName, namespace string) (string, bool) {
	cs.mu.Lock()
	defer cs.mu.Unlock()

	own := serviceKey(cs.project, serviceName)
	for key, svc := range cs.Services {
		if key == own || svc.Name != serviceName || svc.Namespace != namespace {
			continue
		}
		// An unowned entry is claimable by a named project, not a conflict
		if svc.Project == "" && cs.project != "" {
			continue
		}
		return svc.Project, true
	}
	return "", false
}

// RemoveProject removes the current project's services and metadata
func (cs *ClusterState) RemoveProject() {
	cs.mu.Lock()
	defer cs.mu.Unlock()

	for key, svc := range cs.Services {
		if svc.Project == cs.project {
			delete(cs.Services, key)
			cs.markService(key)
		}
	}
	delete(cs.Projects, cs.project)
	cs.markProject(cs.project)
}

// AllServices returns every service in the cluster across all projects,
// sorted by project and then name
func (cs *ClusterState) AllServices() []ServiceMetadata {
	cs.mu.Lock()
	defer cs.mu.Unlock()

	services := make([]ServiceMetadata, 0, len(cs.Services))
	for _, svc := range cs.Services {
		services = append(services, svc)
	}
	sort.Slice(services, func(i, j int) bool {
		if services[i].Project != services[j].Project {
			return services[i].Project < services[j].Project
		}
		return services[i].Name < services[j].Name
	})
	return services
}

// GetCreatedNamespaces returns a map of namespaces we created and should clean up
// The map key is namespace name, value is count of services using it
func (cs *ClusterState) GetCreatedNamespaces() map[string]int {
	cs.mu.Lock()
	defer cs.mu.Unlock()

	namespaces := make(map[string]int)
	for _, svc := range cs.Services {
		if svc.CreatedNamespace && svc.Namespace != "" {
			namespaces[svc.Namespace]++
		}
	}
	return namespaces
}

// NamespaceCreatedByKraze reports whether any service's entry, in any project,
// records that kraze created the namespace
func (cs *ClusterState) NamespaceCreatedByKraze(namespace string) bool {
	cs.mu.Lock()
	defer cs.mu.Unlock()

	for _, svc := range cs.Services {
		if svc.CreatedNamespace && svc.Namespace == namespace {
			return true
		}
	}
	return false
}

// GetNamespacesForServices returns namespaces used by the current project's
// named services. Returns map of namespace name to the count of other installed
// services, in any project, still using it; only a count of 0 may be deleted.
func (cs *ClusterState) GetNamespacesForServices(serviceNames []string) map[string]int {
	cs.mu.Lock()
	defer cs.mu.Unlock()

	targetKeys := make(map[string]bool, len(serviceNames))
	for _, name := range serviceNames {
		targetKeys[serviceKey(cs.project, name)] = true
	}

	// Get namespaces used by the specified services
	namespaceCounts := make(map[string]int)
	for key := range targetKeys {
		if svc, exists := cs.Services[key]; exists && svc.Namespace != "" {
			namespaceCounts[svc.Namespace] = 0
		}
	}

	// Count how many OTHER installed services use these namespaces
	for key, svc := range cs.Services {
		if targetKeys[key] || !svc.Installed {
			continue
		}
		if _, tracked := namespaceCounts[svc.Namespace]; tracked {
			namespaceCounts[svc.Namespace]++
		}
	}

	return namespaceCounts
}

// CountOtherServicesInNamespace returns how many installed services in the
// namespace are not among the current project's named services
func (cs *ClusterState) CountOtherServicesInNamespace(namespace string, serviceNames []string) int {
	cs.mu.Lock()
	defer cs.mu.Unlock()

	count := 0
	for key, svc := range cs.Services {
		if svc.Installed && svc.Namespace == namespace && !slices.ContainsFunc(serviceNames, func(name string) bool {
			return serviceKey(cs.project, name) == key
		}) {
			count++
		}
	}
	return count
}

// GetImageHashes returns the stored image hashes for a service
func (cs *ClusterState) GetImageHashes(serviceName string) map[string]string {
	cs.mu.Lock()
	defer cs.mu.Unlock()

	if svc, exists := cs.Services[serviceKey(cs.project, serviceName)]; exists && svc.ImageHashes != nil {
		return svc.ImageHashes
	}
	return make(map[string]string)
}

// SetImageHashes replaces the stored image hashes for an installed service.
// It does nothing if the service has no entry.
func (cs *ClusterState) SetImageHashes(serviceName string, hashes map[string]string) bool {
	cs.mu.Lock()
	defer cs.mu.Unlock()

	key := serviceKey(cs.project, serviceName)
	svc, exists := cs.Services[key]
	if !exists {
		return false
	}
	svc.ImageHashes = hashes
	cs.Services[key] = svc
	cs.markService(key)
	return true
}

// HasImageHashChanged checks if an image's hash has changed since last installation
// Returns true if the image is new or the hash has changed
func (cs *ClusterState) HasImageHashChanged(serviceName, imageName, currentHash string) bool {
	storedHashes := cs.GetImageHashes(serviceName)
	storedHash, exists := storedHashes[imageName]

	// If image wasn't tracked before, it's new (changed)
	if !exists {
		return true
	}

	// Compare hashes
	return storedHash != currentHash
}

// SetConfigPaths stores the absolute paths to the current project's config files.
// It replaces any previously stored paths for that project. A config file
// belongs to one project, so the paths are dropped from any other project that
// recorded them (as the anonymous project does before `project:` is added).
func (cs *ClusterState) SetConfigPaths(absPaths []string) {
	cs.mu.Lock()
	defer cs.mu.Unlock()

	if cs.Projects == nil {
		cs.Projects = make(map[string]ProjectMetadata)
	}
	for project, meta := range cs.Projects {
		if project == cs.project {
			continue
		}
		remaining := slices.DeleteFunc(slices.Clone(meta.ConfigPaths), func(p string) bool {
			return slices.Contains(absPaths, p)
		})
		if len(remaining) == len(meta.ConfigPaths) {
			continue
		}
		if len(remaining) == 0 {
			delete(cs.Projects, project)
		} else {
			meta.ConfigPaths = remaining
			cs.Projects[project] = meta
		}
		cs.markProject(project)
	}
	cs.Projects[cs.project] = ProjectMetadata{ConfigPaths: absPaths, UpdatedAt: time.Now()}
	cs.markProject(cs.project)
}

// GetConfigPaths returns the stored config file paths for the current project.
func (cs *ClusterState) GetConfigPaths() []string {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	return cs.Projects[cs.project].ConfigPaths
}

// HasConfigPaths returns true if at least one config path is stored for the current project.
func (cs *ClusterState) HasConfigPaths() bool {
	return len(cs.GetConfigPaths()) > 0
}

// AllProjectConfigPaths returns the stored config paths of every project that has any
func (cs *ClusterState) AllProjectConfigPaths() map[string][]string {
	cs.mu.Lock()
	defer cs.mu.Unlock()

	paths := make(map[string][]string)
	for project, meta := range cs.Projects {
		if len(meta.ConfigPaths) > 0 {
			paths[project] = meta.ConfigPaths
		}
	}
	return paths
}

// SetClusterConfig records the settings hash and creating project of a newly
// created cluster
func (cs *ClusterState) SetClusterConfig(hash, createdBy string) {
	cs.mu.Lock()
	defer cs.mu.Unlock()

	cs.ClusterConfigHash = hash
	cs.CreatedBy = createdBy
	cs.dirtyMeta = true
}
