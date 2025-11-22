package config

import (
	"fmt"
	"os"
	"regexp"
	"strings"
)

// envVarPattern matches ${VAR_NAME} or ${VAR_NAME:-default}
var envVarPattern = regexp.MustCompile(`\$\{([A-Z_][A-Z0-9_]*)(:-([^}]*))?\}`)

// ExpandEnvVars expands environment variables in the given string
// Supports formats:
//
//	${VAR_NAME} - expands to value of VAR_NAME or empty string if not set
//	${VAR_NAME:-default} - expands to value of VAR_NAME or 'default' if not set
func ExpandEnvVars(s string) string {
	return envVarPattern.ReplaceAllStringFunc(s, func(match string) string {
		// Extract variable name and default value
		matches := envVarPattern.FindStringSubmatch(match)
		if len(matches) < 2 {
			return match // Should not happen, but return as-is if pattern doesn't match
		}

		varName := matches[1]
		defaultValue := ""
		if len(matches) >= 4 {
			defaultValue = matches[3]
		}

		// Get environment variable value
		value, exists := os.LookupEnv(varName)
		if exists {
			return value
		}

		return defaultValue
	})
}

// ExpandEnvVarsInBytes expands environment variables in byte slice
// This is useful for expanding before YAML parsing
func ExpandEnvVarsInBytes(data []byte) []byte {
	return []byte(ExpandEnvVars(string(data)))
}

// ValidateEnvVarFormat checks if the string has valid environment variable syntax
func ValidateEnvVarFormat(str string) error {
	// Find all matches
	matches := envVarPattern.FindAllStringSubmatch(str, -1)
	for _, match := range matches {
		if len(match) < 2 {
			continue
		}
		varName := match[1]

		// Check variable name format
		if !isValidEnvVarName(varName) {
			return fmt.Errorf("invalid environment variable name: %s", varName)
		}
	}
	return nil
}

// isValidEnvVarName checks if a string is a valid environment variable name
func isValidEnvVarName(name string) bool {
	if len(name) == 0 {
		return false
	}

	// Must start with letter or underscore
	if !isLetter(rune(name[0])) && name[0] != '_' {
		return false
	}

	// Rest must be letters, digits, or underscores
	for _, c := range name {
		if !isLetter(c) && !isDigit(c) && c != '_' {
			return false
		}
	}

	return true
}

func isLetter(c rune) bool {
	return (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z')
}

func isDigit(c rune) bool {
	return c >= '0' && c <= '9'
}

// ExpandEnvVarsInConfig expands environment variables in all string fields of the config
func ExpandEnvVarsInConfig(cfg *Config) {
	// Expand cluster config
	cfg.Cluster.Name = ExpandEnvVars(cfg.Cluster.Name)
	cfg.Cluster.Version = ExpandEnvVars(cfg.Cluster.Version)

	// Expand networking config
	if cfg.Cluster.Networking != nil {
		cfg.Cluster.Networking.PodSubnet = ExpandEnvVars(cfg.Cluster.Networking.PodSubnet)
		cfg.Cluster.Networking.ServiceSubnet = ExpandEnvVars(cfg.Cluster.Networking.ServiceSubnet)
	}

	// Expand preload images
	for itr := range cfg.Cluster.PreloadImages {
		cfg.Cluster.PreloadImages[itr] = ExpandEnvVars(cfg.Cluster.PreloadImages[itr])
	}

	// Expand service configs
	for name, svc := range cfg.Services {
		svc.Namespace = ExpandEnvVars(svc.Namespace)
		svc.Repo = ExpandEnvVars(svc.Repo)
		svc.Chart = ExpandEnvVars(svc.Chart)
		svc.Version = ExpandEnvVars(svc.Version)
		svc.Path = ExpandEnvVars(svc.Path)

		// Expand values files
		if !svc.Values.IsEmpty() {
			expandedFiles := make([]string, 0, len(svc.Values.Files()))
			for _, valuesFile := range svc.Values.Files() {
				expandedFiles = append(expandedFiles, ExpandEnvVars(valuesFile))
			}
			svc.Values = ValuesField{files: expandedFiles}
		}

		// Expand paths array
		for itr := range svc.Paths {
			svc.Paths[itr] = ExpandEnvVars(svc.Paths[itr])
		}

		// Expand labels
		for key, val := range svc.Labels {
			svc.Labels[key] = ExpandEnvVars(val)
		}

		cfg.Services[name] = svc
	}
}

// Helper function to check if a string contains environment variable references
func ContainsEnvVars(str string) bool {
	return strings.Contains(str, "${")
}
