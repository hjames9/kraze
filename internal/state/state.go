package state

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

const (
	// ConfigMapName is the name of the ConfigMap storing kraze metadata
	ConfigMapName = "kraze-metadata"

	// ConfigMapNamespace is the namespace where kraze metadata is stored
	ConfigMapNamespace = "kube-system"

	// ConfigMapDataKey is the key in the ConfigMap data field
	ConfigMapDataKey = "metadata"

	// CurrentStateVersion is the current version of the state format
	CurrentStateVersion = 1
)

// ClusterState represents the state of deployed services stored in the cluster
type ClusterState struct {
	Version     int                        `json:"version"`      // State format version
	ClusterName string                     `json:"cluster_name"`
	IsExternal  bool                       `json:"is_external"`  // Whether this is an external cluster
	Services    map[string]ServiceMetadata `json:"services"`
	LastUpdated time.Time                  `json:"last_updated"`
}

// ServiceMetadata represents the metadata for a single service
type ServiceMetadata struct {
	Name             string            `json:"name"`
	Installed        bool              `json:"installed"`
	UpdatedAt        time.Time         `json:"updated_at"`
	Namespace        string            `json:"namespace,omitempty"`         // The namespace this service is in
	CreatedNamespace bool              `json:"created_namespace,omitempty"` // Whether we created the namespace
	ImageHashes      map[string]string `json:"image_hashes,omitempty"`      // Map of image name to SHA256 hash
}

// New creates a new empty cluster state
func New(clusterName string, isExternal bool) *ClusterState {
	return &ClusterState{
		Version:     CurrentStateVersion,
		ClusterName: clusterName,
		IsExternal:  isExternal,
		Services:    make(map[string]ServiceMetadata),
		LastUpdated: time.Now(),
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

	// Unmarshal the JSON
	var state ClusterState
	if err := json.Unmarshal([]byte(metadataJSON), &state); err != nil {
		return nil, fmt.Errorf("failed to parse cluster state: %w", err)
	}

	// Handle migration from older versions
	if err := state.migrate(); err != nil {
		return nil, fmt.Errorf("failed to migrate cluster state: %w", err)
	}

	return &state, nil
}

// migrate handles migration from older state versions to the current version
func (cs *ClusterState) migrate() error {
	originalVersion := cs.Version

	// Migrate from v0 (no version field) to v1
	if cs.Version == 0 {
		// v0 state had no version field
		// All existing fields are compatible with v1, just set the version
		cs.Version = 1
	}

	// Check if version is supported
	if cs.Version > CurrentStateVersion {
		return fmt.Errorf("cluster state version %d is newer than supported version %d - please upgrade kraze",
			cs.Version, CurrentStateVersion)
	}

	// Log migration if it occurred
	if originalVersion != cs.Version && originalVersion != 0 {
		fmt.Printf("Migrated cluster state from version %d to %d\n", originalVersion, cs.Version)
	}

	return nil
}

// Save writes the cluster state to a ConfigMap in the cluster
func (cs *ClusterState) Save(ctx context.Context, clientset kubernetes.Interface) error {
	// Ensure version is set to current version
	cs.Version = CurrentStateVersion
	cs.LastUpdated = time.Now()

	// Marshal to JSON
	data, err := json.MarshalIndent(cs, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal cluster state: %w", err)
	}

	// Try to get existing ConfigMap
	cm, err := clientset.CoreV1().ConfigMaps(ConfigMapNamespace).Get(ctx, ConfigMapName, metav1.GetOptions{})
	if err != nil {
		if errors.IsNotFound(err) {
			// ConfigMap doesn't exist, create it
			cm = &corev1.ConfigMap{
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
			}
			_, err = clientset.CoreV1().ConfigMaps(ConfigMapNamespace).Create(ctx, cm, metav1.CreateOptions{})
			if err != nil {
				return fmt.Errorf("failed to create cluster state ConfigMap: %w", err)
			}
			return nil
		}
		return fmt.Errorf("failed to get cluster state ConfigMap: %w", err)
	}

	// ConfigMap exists, update it
	cm.Data[ConfigMapDataKey] = string(data)
	_, err = clientset.CoreV1().ConfigMaps(ConfigMapNamespace).Update(ctx, cm, metav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("failed to update cluster state ConfigMap: %w", err)
	}

	return nil
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

// MarkServiceInstalled marks a service as installed (basic version)
func (cs *ClusterState) MarkServiceInstalled(serviceName string) {
	cs.Services[serviceName] = ServiceMetadata{
		Name:      serviceName,
		Installed: true,
		UpdatedAt: time.Now(),
	}
}

// MarkServiceInstalledWithNamespace marks a service as installed and tracks namespace info
func (cs *ClusterState) MarkServiceInstalledWithNamespace(serviceName, namespace string, createdNamespace bool) {
	// Preserve existing image hashes if they exist
	existingMetadata, exists := cs.Services[serviceName]
	imageHashes := make(map[string]string)
	if exists {
		imageHashes = existingMetadata.ImageHashes
	}

	cs.Services[serviceName] = ServiceMetadata{
		Name:             serviceName,
		Installed:        true,
		UpdatedAt:        time.Now(),
		Namespace:        namespace,
		CreatedNamespace: createdNamespace,
		ImageHashes:      imageHashes,
	}
}

// MarkServiceInstalledWithImages marks a service as installed and tracks namespace and image info
func (cs *ClusterState) MarkServiceInstalledWithImages(serviceName, namespace string, createdNamespace bool, imageHashes map[string]string) {
	cs.Services[serviceName] = ServiceMetadata{
		Name:             serviceName,
		Installed:        true,
		UpdatedAt:        time.Now(),
		Namespace:        namespace,
		CreatedNamespace: createdNamespace,
		ImageHashes:      imageHashes,
	}
}

// MarkServiceUninstalled marks a service as uninstalled (removes it from state)
func (cs *ClusterState) MarkServiceUninstalled(serviceName string) {
	delete(cs.Services, serviceName)
}

// IsServiceInstalled checks if a service is marked as installed
func (cs *ClusterState) IsServiceInstalled(serviceName string) bool {
	svc, exists := cs.Services[serviceName]
	return exists && svc.Installed
}

// GetInstalledServices returns a list of all installed service names
func (cs *ClusterState) GetInstalledServices() []string {
	installed := make([]string, 0, len(cs.Services))
	for name, svc := range cs.Services {
		if svc.Installed {
			installed = append(installed, name)
		}
	}
	return installed
}

// GetCreatedNamespaces returns a map of namespaces we created and should clean up
// The map key is namespace name, value is count of services using it
func (cs *ClusterState) GetCreatedNamespaces() map[string]int {
	namespaces := make(map[string]int)
	for _, svc := range cs.Services {
		if svc.CreatedNamespace && svc.Namespace != "" {
			namespaces[svc.Namespace]++
		}
	}
	return namespaces
}

// GetAllNamespacesUsed returns a map of all namespaces used by installed services
// The map key is namespace name, value is count of installed services using it
// This includes namespaces we created AND namespaces that existed before
func (cs *ClusterState) GetAllNamespacesUsed() map[string]int {
	namespaces := make(map[string]int)
	for _, svc := range cs.Services {
		if svc.Installed && svc.Namespace != "" {
			namespaces[svc.Namespace]++
		}
	}
	return namespaces
}

// GetAllNamespacesUsedForCleanup returns all unique namespaces used by any service
// For "uninstall all" scenarios, returns map with count 0 for all namespaces
// since we're uninstalling everything
func (cs *ClusterState) GetAllNamespacesUsedForCleanup() map[string]int {
	namespaces := make(map[string]int)
	for _, svc := range cs.Services {
		if svc.Namespace != "" {
			// Set count to 0 since we're cleaning up everything
			namespaces[svc.Namespace] = 0
		}
	}
	return namespaces
}

// GetNamespacesForServices returns namespaces used by specific services
// For local dev environments, we aggressively clean up namespaces when uninstalling services
// Returns map of namespace name to count of installed services still using it
func (cs *ClusterState) GetNamespacesForServices(serviceNames []string) map[string]int {
	// Get namespaces used by the specified services
	targetNamespaces := make(map[string]bool)
	for _, name := range serviceNames {
		if svc, exists := cs.Services[name]; exists && svc.Namespace != "" {
			targetNamespaces[svc.Namespace] = true
		}
	}

	// Count how many OTHER installed services use these namespaces
	// If count is 0, the namespace can be safely deleted
	namespaceCounts := make(map[string]int)
	for ns := range targetNamespaces {
		count := 0
		for _, svc := range cs.Services {
			// Skip the services we're uninstalling
			isTargetService := false
			for _, name := range serviceNames {
				if svc.Name == name {
					isTargetService = true
					break
				}
			}
			if isTargetService {
				continue
			}

			// Count if this namespace is used by an installed service
			if svc.Installed && svc.Namespace == ns {
				count++
			}
		}
		namespaceCounts[ns] = count
	}

	return namespaceCounts
}

// GetImageHashes returns the stored image hashes for a service
func (cs *ClusterState) GetImageHashes(serviceName string) map[string]string {
	if svc, exists := cs.Services[serviceName]; exists {
		if svc.ImageHashes == nil {
			return make(map[string]string)
		}
		return svc.ImageHashes
	}
	return make(map[string]string)
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

// GetChangedImages compares current image hashes with stored hashes
// Returns a list of images that are new or have changed
func (cs *ClusterState) GetChangedImages(serviceName string, currentHashes map[string]string) []string {
	changed := make([]string, 0)
	storedHashes := cs.GetImageHashes(serviceName)

	for imageName, currentHash := range currentHashes {
		storedHash, exists := storedHashes[imageName]
		if !exists || storedHash != currentHash {
			changed = append(changed, imageName)
		}
	}

	return changed
}
