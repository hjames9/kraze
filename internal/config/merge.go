package config

import (
	"fmt"
	"strings"
)

// ParseMultiple parses one or more config files and merges them into a single Config.
// When called with a single path it behaves identically to Parse.
// When called with multiple paths:
//   - Services are merged; duplicate names across files are an error.
//   - Cluster configs are merged according to per-field rules (see mergeClusterConfigs).
//   - Cross-file dependency references are validated after merging.
func ParseMultiple(paths []string) (*Config, error) {
	if len(paths) == 0 {
		return nil, fmt.Errorf("at least one config file path is required")
	}
	if len(paths) == 1 {
		return Parse(paths[0])
	}

	// Parse each file individually (with path resolution but without cross-reference checks).
	configs := make([]*Config, 0, len(paths))
	for _, p := range paths {
		cfg, err := parseWithoutCrossRefValidation(p)
		if err != nil {
			return nil, fmt.Errorf("failed to parse config file '%s': %w", p, err)
		}
		configs = append(configs, cfg)
	}

	// Merge cluster configs (first file's name wins; other fields follow specific rules).
	merged := &Config{}
	mergedCluster, err := mergeClusterConfigs(configs)
	if err != nil {
		return nil, err
	}
	merged.Cluster = mergedCluster

	// Merge services (duplicate names across files = error).
	merged.Services = make(map[string]ServiceConfig)
	for i, cfg := range configs {
		for name, svc := range cfg.Services {
			if _, exists := merged.Services[name]; exists {
				return nil, fmt.Errorf("service '%s' is defined in multiple config files (conflict at '%s')", name, paths[i])
			}
			merged.Services[name] = svc
		}
	}

	// Run cross-reference validation on the fully merged config.
	if err := merged.validateCrossRefs(); err != nil {
		return nil, fmt.Errorf("validation failed: %w", err)
	}

	return merged, nil
}

// parseWithoutCrossRefValidation parses a single file and validates individual
// service configs but skips cross-reference checks (dependency existence,
// enabled/disabled constraints). Used as the first pass in ParseMultiple so
// that services in one file can legitimately reference services in another.
func parseWithoutCrossRefValidation(configPath string) (*Config, error) {
	data, err := readAndExpand(configPath)
	if err != nil {
		return nil, err
	}

	var cfg Config
	if err := unmarshalConfig(data, &cfg); err != nil {
		return nil, err
	}

	// Set service names from map keys.
	for name, svc := range cfg.Services {
		svc.Name = name
		cfg.Services[name] = svc
	}

	// Validate GPU config if present (can be done per-file).
	if cfg.Cluster.GPU.IsAnyEnabled() && cfg.Cluster.IsExternal() {
		return nil, &ValidationError{
			Field:   "cluster.gpu",
			Message: "GPU support is only available for kind clusters, not external clusters",
		}
	}

	// Validate individual service configs (type, required fields) but not cross-refs.
	for _, svc := range cfg.Services {
		if err := svc.Validate(); err != nil {
			return nil, fmt.Errorf("service '%s': %w", svc.Name, err)
		}
	}

	// Resolve relative paths based on this file's directory.
	if err := cfg.ResolvePaths(configPath); err != nil {
		return nil, fmt.Errorf("failed to resolve paths: %w", err)
	}

	return &cfg, nil
}

// mergeClusterConfigs merges cluster configurations from multiple files.
func mergeClusterConfigs(configs []*Config) (ClusterConfig, error) {
	// Start with the first config's cluster as the base.
	base := configs[0].Cluster

	for i, cfg := range configs[1:] {
		fileIdx := i + 2 // human-readable file number (1-based, skipping first)
		other := cfg.Cluster

		// cluster.name: first-file wins, silently ignore others.
		// (already set from base)

		// version, node_image: first-file wins; error on conflict.
		if other.Version != "" {
			if base.Version == "" {
				base.Version = other.Version
			} else if base.Version != other.Version {
				return ClusterConfig{}, fmt.Errorf("cluster.version conflict between config file 1 (%s) and file %d (%s)", base.Version, fileIdx, other.Version)
			}
		}
		if other.NodeImage != "" {
			if base.NodeImage == "" {
				base.NodeImage = other.NodeImage
			} else if base.NodeImage != other.NodeImage {
				return ClusterConfig{}, fmt.Errorf("cluster.node_image conflict between config file 1 (%s) and file %d (%s)", base.NodeImage, fileIdx, other.NodeImage)
			}
		}

		// Lists: concatenate + deduplicate.
		base.CACertificates = unionStrings(base.CACertificates, other.CACertificates)
		base.InsecureRegistries = unionStrings(base.InsecureRegistries, other.InsecureRegistries)
		base.PreloadImages = unionStrings(base.PreloadImages, other.PreloadImages)

		// GPU: OR per-vendor enabled flags.
		base.GPU = mergeGPUConfigs(base.GPU, other.GPU)

		// Proxy: conflict detection for enabled flag and URLs.
		mergedProxy, err := mergeProxyConfigs(base.Proxy, other.Proxy, fileIdx)
		if err != nil {
			return ClusterConfig{}, err
		}
		base.Proxy = mergedProxy

		// Networking: first-file wins; error on conflict.
		if other.Networking != nil {
			if base.Networking == nil {
				base.Networking = other.Networking
			} else if *base.Networking != *other.Networking {
				return ClusterConfig{}, fmt.Errorf("cluster.networking conflict between config file 1 and file %d", fileIdx)
			}
		}

		// Kind node config: merge per-role (union port mappings, mounts, labels; error on scalar conflicts).
		mergedNodes, err := mergeKindNodes(base.Config, other.Config, fileIdx)
		if err != nil {
			return ClusterConfig{}, err
		}
		base.Config = mergedNodes

		// External cluster: must agree.
		if other.External != nil {
			if base.External == nil {
				base.External = other.External
			} else if base.External.Enabled != other.External.Enabled ||
				base.External.Kubeconfig != other.External.Kubeconfig ||
				base.External.Context != other.External.Context {
				return ClusterConfig{}, fmt.Errorf("cluster.external conflict between config file 1 and file %d", fileIdx)
			}
		}

		// Network identity fields: must agree if both set.
		if err := mergeStringField(&base.Network, other.Network, "cluster.network", fileIdx); err != nil {
			return ClusterConfig{}, err
		}
		if err := mergeStringField(&base.Subnet, other.Subnet, "cluster.subnet", fileIdx); err != nil {
			return ClusterConfig{}, err
		}
		if err := mergeStringField(&base.IPv4Address, other.IPv4Address, "cluster.ipv4_address", fileIdx); err != nil {
			return ClusterConfig{}, err
		}
	}

	return base, nil
}

// mergeGPUConfigs merges two GPU configs using OR logic per vendor.
func mergeGPUConfigs(a, b *GPUConfig) *GPUConfig {
	if a == nil && b == nil {
		return nil
	}
	result := &GPUConfig{}
	if a != nil {
		result.Nvidia = a.Nvidia
		result.AMD = a.AMD
	}
	if b == nil {
		return result
	}
	if b.Nvidia != nil {
		if result.Nvidia == nil {
			result.Nvidia = b.Nvidia
		} else {
			result.Nvidia = &GPUVendorConfig{Enabled: result.Nvidia.Enabled || b.Nvidia.Enabled}
		}
	}
	if b.AMD != nil {
		if result.AMD == nil {
			result.AMD = b.AMD
		} else {
			result.AMD = &GPUVendorConfig{Enabled: result.AMD.Enabled || b.AMD.Enabled}
		}
	}
	return result
}

// mergeProxyConfigs merges two proxy configs with conflict detection.
func mergeProxyConfigs(a, b *ProxyConfig, fileIdx int) (*ProxyConfig, error) {
	if a == nil && b == nil {
		return nil, nil
	}
	if a == nil {
		return b, nil
	}
	if b == nil {
		return a, nil
	}

	result := &ProxyConfig{}

	// enabled: explicit true vs explicit false = error.
	if a.Enabled != nil && b.Enabled != nil && *a.Enabled != *b.Enabled {
		return nil, fmt.Errorf("cluster.proxy.enabled conflict between config file 1 and file %d: one explicitly enables proxy while the other explicitly disables it", fileIdx)
	}
	if a.Enabled != nil {
		result.Enabled = a.Enabled
	} else {
		result.Enabled = b.Enabled
	}

	// http_proxy / https_proxy: must agree if both set.
	if err := mergeStringField(&result.HTTPProxy, a.HTTPProxy, "cluster.proxy.http_proxy", fileIdx); err != nil {
		return nil, err
	}
	if err := mergeStringField(&result.HTTPProxy, b.HTTPProxy, "cluster.proxy.http_proxy", fileIdx); err != nil {
		return nil, err
	}
	if err := mergeStringField(&result.HTTPSProxy, a.HTTPSProxy, "cluster.proxy.https_proxy", fileIdx); err != nil {
		return nil, err
	}
	if err := mergeStringField(&result.HTTPSProxy, b.HTTPSProxy, "cluster.proxy.https_proxy", fileIdx); err != nil {
		return nil, err
	}

	// no_proxy: split, union, rejoin.
	result.NoProxy = unionNoProxy(a.NoProxy, b.NoProxy)

	return result, nil
}

// mergeStringField sets *dst to src if *dst is empty, or errors if they differ.
func mergeStringField(dst *string, src, fieldName string, fileIdx int) error {
	if src == "" {
		return nil
	}
	if *dst == "" {
		*dst = src
		return nil
	}
	if *dst != src {
		return fmt.Errorf("%s conflict between config file 1 (%s) and file %d (%s)", fieldName, *dst, fileIdx, src)
	}
	return nil
}

// mergeKindNodes merges two slices of KindNode, matching by role.
// For each role present in both, per-field rules apply:
//   - replicas: error on conflict if both non-zero and differ
//   - extraPortMappings: union; error if same containerPort+protocol has conflicting hostPort/listenAddress
//   - extraMounts: union; error if same containerPath has conflicting hostPath/readOnly
//   - labels: union; error if same key has different value
//
// Nodes present in only one slice are included as-is.
func mergeKindNodes(base, other []KindNode, fileIdx int) ([]KindNode, error) {
	if len(other) == 0 {
		return base, nil
	}
	if len(base) == 0 {
		return other, nil
	}

	// Index base nodes by role for O(1) lookup.
	byRole := make(map[string]int, len(base))
	result := make([]KindNode, len(base))
	copy(result, base)
	for i, n := range result {
		byRole[n.Role] = i
	}

	for _, o := range other {
		idx, exists := byRole[o.Role]
		if !exists {
			// Role only in other — append as-is.
			result = append(result, o)
			continue
		}
		b := &result[idx]

		// replicas: error on conflict.
		if o.Replicas != 0 {
			if b.Replicas == 0 {
				b.Replicas = o.Replicas
			} else if b.Replicas != o.Replicas {
				return nil, fmt.Errorf("cluster.config role=%q replicas conflict between config file 1 (%d) and file %d (%d)", o.Role, b.Replicas, fileIdx, o.Replicas)
			}
		}

		// extraPortMappings: union with conflict detection.
		merged, err := mergePortMappings(b.ExtraPortMappings, o.ExtraPortMappings, o.Role, fileIdx)
		if err != nil {
			return nil, err
		}
		b.ExtraPortMappings = merged

		// extraMounts: union with conflict detection.
		mergedMounts, err := mergeMounts(b.ExtraMounts, o.ExtraMounts, o.Role, fileIdx)
		if err != nil {
			return nil, err
		}
		b.ExtraMounts = mergedMounts

		// labels: union with conflict detection.
		mergedLabels, err := mergeLabels(b.Labels, o.Labels, o.Role, fileIdx)
		if err != nil {
			return nil, err
		}
		b.Labels = mergedLabels
	}

	return result, nil
}

// mergePortMappings unions two PortMapping slices.
// Conflict: same containerPort+protocol with different hostPort or listenAddress.
func mergePortMappings(base, other []PortMapping, role string, fileIdx int) ([]PortMapping, error) {
	if len(other) == 0 {
		return base, nil
	}

	// Key: containerPort + "/" + protocol (empty protocol treated as TCP).
	type key struct {
		port  int32
		proto string
	}
	index := make(map[key]PortMapping, len(base))
	result := make([]PortMapping, len(base))
	copy(result, base)
	for _, pm := range base {
		index[key{pm.ContainerPort, pm.Protocol}] = pm
	}

	for _, pm := range other {
		k := key{pm.ContainerPort, pm.Protocol}
		if existing, exists := index[k]; exists {
			if existing.HostPort != pm.HostPort || existing.ListenAddress != pm.ListenAddress {
				return nil, fmt.Errorf("cluster.config role=%q extraPortMappings containerPort=%d conflict between config file 1 and file %d", role, pm.ContainerPort, fileIdx)
			}
			// Identical — skip duplicate.
			continue
		}
		index[k] = pm
		result = append(result, pm)
	}

	return result, nil
}

// mergeMounts unions two Mount slices.
// Conflict: same containerPath with different hostPath or readOnly.
func mergeMounts(base, other []Mount, role string, fileIdx int) ([]Mount, error) {
	if len(other) == 0 {
		return base, nil
	}

	index := make(map[string]Mount, len(base))
	result := make([]Mount, len(base))
	copy(result, base)
	for _, m := range base {
		index[m.ContainerPath] = m
	}

	for _, m := range other {
		if existing, exists := index[m.ContainerPath]; exists {
			if existing.HostPath != m.HostPath || existing.ReadOnly != m.ReadOnly {
				return nil, fmt.Errorf("cluster.config role=%q extraMounts containerPath=%q conflict between config file 1 and file %d", role, m.ContainerPath, fileIdx)
			}
			continue
		}
		index[m.ContainerPath] = m
		result = append(result, m)
	}

	return result, nil
}

// mergeLabels unions two label maps.
// Conflict: same key with different value.
func mergeLabels(base, other map[string]string, role string, fileIdx int) (map[string]string, error) {
	if len(other) == 0 {
		return base, nil
	}

	result := make(map[string]string, len(base)+len(other))
	for k, v := range base {
		result[k] = v
	}
	for k, v := range other {
		if existing, exists := result[k]; exists && existing != v {
			return nil, fmt.Errorf("cluster.config role=%q labels key=%q conflict between config file 1 (%q) and file %d (%q)", role, k, existing, fileIdx, v)
		}
		result[k] = v
	}

	return result, nil
}

// unionStrings returns the union of two string slices with duplicates removed.
func unionStrings(a, b []string) []string {
	seen := make(map[string]bool, len(a)+len(b))
	result := make([]string, 0, len(a)+len(b))
	for _, s := range append(a, b...) {
		if !seen[s] {
			seen[s] = true
			result = append(result, s)
		}
	}
	return result
}

// unionNoProxy merges two comma-separated no_proxy strings, deduplicating entries.
func unionNoProxy(a, b string) string {
	if a == "" {
		return b
	}
	if b == "" {
		return a
	}
	parts := strings.Split(a+","+b, ",")
	seen := make(map[string]bool, len(parts))
	result := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" && !seen[p] {
			seen[p] = true
			result = append(result, p)
		}
	}
	return strings.Join(result, ",")
}

// validateCrossRefs validates dependency references and enabled/disabled constraints
// across the fully merged service map.
func (cfg *Config) validateCrossRefs() error {
	for _, svc := range cfg.Services {
		for _, dep := range svc.DependsOn {
			if _, exists := cfg.Services[dep]; !exists {
				return &ValidationError{
					Field:   fmt.Sprintf("service '%s' depends_on", svc.Name),
					Message: fmt.Sprintf("dependency '%s' not found in services", dep),
				}
			}
		}
	}

	for _, svc := range cfg.Services {
		if !svc.IsEnabled() {
			continue
		}
		for _, depName := range svc.DependsOn {
			if depSvc, exists := cfg.Services[depName]; exists && !depSvc.IsEnabled() {
				return &ValidationError{
					Field:   fmt.Sprintf("service '%s' depends_on", svc.Name),
					Message: fmt.Sprintf("depends on disabled service '%s'", depName),
				}
			}
		}
	}

	return nil
}
