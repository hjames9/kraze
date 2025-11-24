package state

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

const (
	// StateFileName is the name of the state file
	StateFileName = ".kraze.state"

	// CurrentStateVersion is the current version of the state file format
	CurrentStateVersion = 1
)

// State represents the state of deployed services
type State struct {
	Version     int                     `json:"version"`      // State file format version
	ClusterName string                  `json:"cluster_name"`
	IsExternal  bool                    `json:"is_external"`  // Whether this is an external cluster
	Services    map[string]ServiceState `json:"services"`
	LastUpdated time.Time               `json:"last_updated"`
}

// ServiceState represents the state of a single service
type ServiceState struct {
	Name             string            `json:"name"`
	Installed        bool              `json:"installed"`
	UpdatedAt        time.Time         `json:"updated_at"`
	Namespace        string            `json:"namespace,omitempty"`         // The namespace this service is in
	CreatedNamespace bool              `json:"created_namespace,omitempty"` // Whether we created the namespace
	ImageHashes      map[string]string `json:"image_hashes,omitempty"`      // Map of image name to SHA256 hash
}

// New creates a new empty state
func New(clusterName string, isExternal bool) *State {
	return &State{
		Version:     CurrentStateVersion,
		ClusterName: clusterName,
		IsExternal:  isExternal,
		Services:    make(map[string]ServiceState),
		LastUpdated: time.Now(),
	}
}

// Load reads the state file from disk
func Load(stateFilePath string) (*State, error) {
	data, err := os.ReadFile(stateFilePath)
	if err != nil {
		if os.IsNotExist(err) {
			// State file doesn't exist yet, return empty state
			return nil, nil
		}
		return nil, fmt.Errorf("failed to read state file: %w", err)
	}

	var state State
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, fmt.Errorf("failed to parse state file: %w", err)
	}

	// Handle migration from older versions
	if err := state.migrate(); err != nil {
		return nil, fmt.Errorf("failed to migrate state file: %w", err)
	}

	return &state, nil
}

// migrate handles migration from older state file versions to the current version
func (state *State) migrate() error {
	originalVersion := state.Version

	// Migrate from v0 (no version field) to v1
	if state.Version == 0 {
		// v0 state files had no version field
		// All existing fields are compatible with v1, just set the version
		state.Version = 1
	}

	// Check if version is supported
	if state.Version > CurrentStateVersion {
		return fmt.Errorf("state file version %d is newer than supported version %d - please upgrade kraze",
			state.Version, CurrentStateVersion)
	}

	// Log migration if it occurred
	if originalVersion != state.Version && originalVersion != 0 {
		fmt.Printf("Migrated state file from version %d to %d\n", originalVersion, state.Version)
	}

	return nil
}

// Save writes the state file to disk
func (state *State) Save(stateFilePath string) error {
	// Ensure version is set to current version
	state.Version = CurrentStateVersion
	state.LastUpdated = time.Now()

	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal state: %w", err)
	}

	// Ensure directory exists
	dir := filepath.Dir(stateFilePath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("failed to create state directory: %w", err)
	}

	if err := os.WriteFile(stateFilePath, data, 0644); err != nil {
		return fmt.Errorf("failed to write state file: %w", err)
	}

	return nil
}

// Delete removes the state file from disk
func Delete(stateFilePath string) error {
	if err := os.Remove(stateFilePath); err != nil {
		if os.IsNotExist(err) {
			// File doesn't exist, that's fine
			return nil
		}
		return fmt.Errorf("failed to delete state file: %w", err)
	}
	return nil
}

// MarkServiceInstalled marks a service as installed
func (state *State) MarkServiceInstalled(serviceName string) {
	state.Services[serviceName] = ServiceState{
		Name:      serviceName,
		Installed: true,
		UpdatedAt: time.Now(),
	}
}

// MarkServiceInstalledWithNamespace marks a service as installed and tracks namespace info
func (state *State) MarkServiceInstalledWithNamespace(serviceName, namespace string, createdNamespace bool) {
	// Preserve existing image hashes if they exist
	existingState, exists := state.Services[serviceName]
	imageHashes := make(map[string]string)
	if exists {
		imageHashes = existingState.ImageHashes
	}

	state.Services[serviceName] = ServiceState{
		Name:             serviceName,
		Installed:        true,
		UpdatedAt:        time.Now(),
		Namespace:        namespace,
		CreatedNamespace: createdNamespace,
		ImageHashes:      imageHashes,
	}
}

// MarkServiceInstalledWithImages marks a service as installed and tracks namespace and image info
func (state *State) MarkServiceInstalledWithImages(serviceName, namespace string, createdNamespace bool, imageHashes map[string]string) {
	state.Services[serviceName] = ServiceState{
		Name:             serviceName,
		Installed:        true,
		UpdatedAt:        time.Now(),
		Namespace:        namespace,
		CreatedNamespace: createdNamespace,
		ImageHashes:      imageHashes,
	}
}

// MarkServiceUninstalled marks a service as uninstalled (removes it from state)
func (state *State) MarkServiceUninstalled(serviceName string) {
	delete(state.Services, serviceName)
}

// IsServiceInstalled checks if a service is marked as installed
func (state *State) IsServiceInstalled(serviceName string) bool {
	svc, exists := state.Services[serviceName]
	return exists && svc.Installed
}

// GetInstalledServices returns a list of all installed service names
func (state *State) GetInstalledServices() []string {
	installed := make([]string, 0, len(state.Services))
	for name, svc := range state.Services {
		if svc.Installed {
			installed = append(installed, name)
		}
	}
	return installed
}

// GetStateFilePath returns the path to the state file relative to the config file
func GetStateFilePath(configDir string) string {
	return filepath.Join(configDir, StateFileName)
}

// GetCreatedNamespaces returns a map of namespaces we created and should clean up
// The map key is namespace name, value is count of services using it
func (state *State) GetCreatedNamespaces() map[string]int {
	namespaces := make(map[string]int)
	for _, svc := range state.Services {
		if svc.CreatedNamespace && svc.Namespace != "" {
			namespaces[svc.Namespace]++
		}
	}
	return namespaces
}

// GetAllNamespacesUsed returns a map of all namespaces used by installed services
// The map key is namespace name, value is count of installed services using it
// This includes namespaces we created AND namespaces that existed before
func (state *State) GetAllNamespacesUsed() map[string]int {
	namespaces := make(map[string]int)
	for _, svc := range state.Services {
		if svc.Installed && svc.Namespace != "" {
			namespaces[svc.Namespace]++
		}
	}
	return namespaces
}

// GetAllNamespacesUsedForCleanup returns all unique namespaces used by any service
// For "uninstall all" scenarios, returns map with count 0 for all namespaces
// since we're uninstalling everything
func (state *State) GetAllNamespacesUsedForCleanup() map[string]int {
	namespaces := make(map[string]int)
	for _, svc := range state.Services {
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
func (state *State) GetNamespacesForServices(serviceNames []string) map[string]int {
	// Get namespaces used by the specified services
	targetNamespaces := make(map[string]bool)
	for _, name := range serviceNames {
		if svc, exists := state.Services[name]; exists && svc.Namespace != "" {
			targetNamespaces[svc.Namespace] = true
		}
	}

	// Count how many OTHER installed services use these namespaces
	// If count is 0, the namespace can be safely deleted
	namespaceCounts := make(map[string]int)
	for ns := range targetNamespaces {
		count := 0
		for _, svc := range state.Services {
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
func (state *State) GetImageHashes(serviceName string) map[string]string {
	if svc, exists := state.Services[serviceName]; exists {
		if svc.ImageHashes == nil {
			return make(map[string]string)
		}
		return svc.ImageHashes
	}
	return make(map[string]string)
}

// HasImageHashChanged checks if an image's hash has changed since last installation
// Returns true if the image is new or the hash has changed
func (state *State) HasImageHashChanged(serviceName, imageName, currentHash string) bool {
	storedHashes := state.GetImageHashes(serviceName)
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
func (state *State) GetChangedImages(serviceName string, currentHashes map[string]string) []string {
	changed := make([]string, 0)
	storedHashes := state.GetImageHashes(serviceName)

	for imageName, currentHash := range currentHashes {
		storedHash, exists := storedHashes[imageName]
		if !exists || storedHash != currentHash {
			changed = append(changed, imageName)
		}
	}

	return changed
}
