package config

import (
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

// InsecureRegistry is a parsed insecure_registries entry
type InsecureRegistry struct {
	// Scheme is "http" or "https" when the entry names one; empty means try
	// HTTPS without verifying the certificate, then plain HTTP, as Docker does
	// for its insecure registries
	Scheme string
	// Host is host[:port], as images name the registry
	Host string
}

var registryHostnamePattern = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9.-]*[A-Za-z0-9])?$`)

// ParseInsecureRegistry parses an insecure_registries entry: a registry host,
// optionally with a port and an http:// or https:// prefix
func ParseInsecureRegistry(entry string) (InsecureRegistry, error) {
	invalid := fmt.Errorf("invalid insecure registry %q: want a registry host, optionally with a port and an http:// or https:// prefix (e.g. registry.corp.com:5000)", entry)
	reg := InsecureRegistry{Host: entry}
	if scheme, host, found := strings.Cut(entry, "://"); found {
		reg.Scheme, reg.Host = strings.ToLower(scheme), host
		if reg.Scheme != "http" && reg.Scheme != "https" {
			return InsecureRegistry{}, invalid
		}
	}
	reg.Host = strings.TrimSuffix(reg.Host, "/")

	parsed, err := url.Parse("//" + reg.Host)
	if err != nil || parsed.Host != reg.Host || parsed.User != nil || parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return InsecureRegistry{}, invalid
	}
	hostname := parsed.Hostname()
	if net.ParseIP(hostname) == nil && !registryHostnamePattern.MatchString(hostname) {
		return InsecureRegistry{}, invalid
	}
	if port := parsed.Port(); port != "" {
		if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
			return InsecureRegistry{}, invalid
		}
	} else if strings.HasSuffix(reg.Host, ":") {
		return InsecureRegistry{}, invalid
	}
	return reg, nil
}
