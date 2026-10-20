package config

import (
	"os"
	"regexp"
	"strings"
	"unicode"

	"go.yaml.in/yaml/v3"
)

// envVarPattern matches ${VAR_NAME} or ${VAR_NAME:-default}
var envVarPattern = regexp.MustCompile(`\$\{([A-Z_][A-Z0-9_]*)(:-([^}]*))?\}`)

// ExpandEnvVars expands environment variables in the given string
// Supports formats:
//
//	${VAR_NAME} - expands to value of VAR_NAME or empty string if not set
//	${VAR_NAME:-default} - expands to value of VAR_NAME or 'default' if not set or empty
func ExpandEnvVars(s string) string {
	return envVarPattern.ReplaceAllStringFunc(s, func(match string) string {
		// Extract variable name and default value
		matches := envVarPattern.FindStringSubmatch(match)
		if len(matches) < 2 {
			return match // Should not happen, but return as-is if pattern doesn't match
		}

		// As in sh, ${VAR:-default} takes the default when VAR is unset or
		// empty, and ${VAR} is empty when VAR is unset
		value := os.Getenv(matches[1])
		if value == "" && len(matches) >= 4 {
			return matches[3]
		}
		return value
	})
}

// expandEnvVarsInNode expands environment variables in the scalars of a parsed
// YAML document, keys included. Expanding after parsing means a value is only
// ever a value: a password containing " #", ": " or a leading "!" arrives
// intact instead of changing the document (as it did when the file's text was
// expanded before parsing). A plain (unquoted) scalar is typed by its expanded
// value, so `replicas: ${N}` is still a number and `wait: ${W:-true}` a bool;
// a quoted one stays a string.
func expandEnvVarsInNode(node *yaml.Node) {
	expandEnvVarsInNodeTree(node, false, false)
}

// expandEnvVarsInNodeTree expands node and its children. inServices is set
// for the services mapping; forOutput for a document that will be written out
// again (see expandScalar).
func expandEnvVarsInNodeTree(node *yaml.Node, inServices, forOutput bool) {
	switch node.Kind {
	case yaml.DocumentNode, yaml.SequenceNode:
		for _, child := range node.Content {
			expandEnvVarsInNodeTree(child, false, forOutput)
		}
	case yaml.MappingNode:
		for itr := 0; itr+1 < len(node.Content); itr += 2 {
			key, value := node.Content[itr], node.Content[itr+1]
			expandEnvVarsInNodeTree(key, false, forOutput)
			switch {
			case inServices && value.Kind == yaml.MappingNode:
				expandServiceNode(value)
			default:
				expandEnvVarsInNodeTree(value, key.Value == "services" && !forOutput, forOutput)
			}
		}
	case yaml.ScalarNode:
		expandScalar(node, forOutput)
	}
	// Alias nodes share their anchor's node, which is expanded where it's defined
}

// expandServiceNode expands a service's mapping. Its values_inline is YAML
// that Helm parses again later, so it is expanded the same way, as YAML
func expandServiceNode(service *yaml.Node) {
	for itr := 0; itr+1 < len(service.Content); itr += 2 {
		key, value := service.Content[itr], service.Content[itr+1]
		expandEnvVarsInNodeTree(key, false, false)
		if key.Value == "values_inline" && value.Kind == yaml.ScalarNode {
			value.Value = expandEnvVarsInYAMLText(value.Value)
			continue
		}
		expandEnvVarsInNodeTree(value, false, false)
	}
}

// expandScalar expands a scalar node. forOutput double-quotes an expanded
// value holding control characters: yaml.v3 writes some of them (a tab
// before a newline) as a block scalar it can't read back, and a double-quoted
// string can hold anything.
func expandScalar(node *yaml.Node, forOutput bool) {
	expanded := ExpandEnvVars(node.Value)
	if expanded == node.Value {
		return
	}
	node.Value = expanded
	if node.Style&(yaml.DoubleQuotedStyle|yaml.SingleQuotedStyle|yaml.LiteralStyle|yaml.FoldedStyle) == 0 &&
		node.Style&yaml.TaggedStyle == 0 {
		// Resolve the type from the expanded value, as for a value written
		// out in the file
		node.Tag = ""
	}
	if forOutput && strings.ContainsFunc(expanded, unicode.IsControl) {
		node.Style = yaml.DoubleQuotedStyle
		node.Tag = "!!str"
	}
}

// expandEnvVarsInYAMLText expands environment variables in the values of a
// YAML document held as text. Text that isn't valid YAML until it is expanded
// (a reference that supplies structure) is expanded as text, as it always was.
func expandEnvVarsInYAMLText(text string) string {
	if !envVarPattern.MatchString(text) {
		return text
	}
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(text), &doc); err != nil || doc.Kind == 0 {
		return ExpandEnvVars(text)
	}
	expandEnvVarsInNodeTree(&doc, false, true)
	out, err := yaml.Marshal(&doc)
	if err != nil {
		return ExpandEnvVars(text)
	}
	return string(out)
}
