package cli

import "testing"

func TestFormatImageSize(test *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{name: "gigabytes", input: "8800000000", expected: "8.2 GB"},
		{name: "megabytes", input: "52428800", expected: "50.0 MB"},
		{name: "kilobytes", input: "10240", expected: "10.0 KB"},
		{name: "bytes", input: "512", expected: "512 B"},
		{name: "zero", input: "0", expected: "0 B"},
		{name: "empty string", input: "", expected: "unknown"},
		{name: "non-numeric", input: "n/a", expected: "n/a"},
	}

	for _, tt := range tests {
		test.Run(tt.name, func(test *testing.T) {
			result := formatImageSize(tt.input)
			if result != tt.expected {
				test.Errorf("formatImageSize(%q) = %q, want %q", tt.input, result, tt.expected)
			}
		})
	}
}
