package config

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	"go.yaml.in/yaml/v3"
)

// Expansion follows the shell's rules for the syntax it supports
// (`${VAR}`, `${VAR:-default}`), checked against what sh does rather than
// against kraze's implementation
func TestExpandEnvVarsShellSemantics(t *testing.T) {
	tests := []struct {
		name  string
		input string
		env   map[string]string // "" values are set but empty
		want  string
	}{
		{name: "unset without default is empty (as in sh)", input: "a${UNSET_X}b", want: "ab"},
		{name: "set but empty takes the :- default (as in sh)", input: "${EMPTY_X:-fallback}", env: map[string]string{"EMPTY_X": ""}, want: "fallback"},
		{name: "set but empty without default", input: "a${EMPTY_X}b", env: map[string]string{"EMPTY_X": ""}, want: "ab"},
		{name: "default may contain colons and spaces", input: "${UNSET_X:-http://h:80/a b}", want: "http://h:80/a b"},
		{name: "a value is not expanded again", input: "${OUTER_X}", env: map[string]string{"OUTER_X": "${INNER_X}", "INNER_X": "no"}, want: "${INNER_X}"},
		{name: "$VAR without braces is left alone", input: "$PLAIN_X", env: map[string]string{"PLAIN_X": "no"}, want: "$PLAIN_X"},
		{name: "lowercase names are not expanded", input: "${lower_x}", env: map[string]string{"lower_x": "no"}, want: "${lower_x}"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for k, v := range tt.env {
				t.Setenv(k, v)
			}
			if got := ExpandEnvVars(tt.input); got != tt.want {
				t.Errorf("ExpandEnvVars(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

// writeServiceConfig writes a minimal config with one helm service whose extra keys
// are svcExtra (indented by 4 spaces) and returns its path
func writeServiceConfig(t *testing.T, svcExtra string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "kraze.yml")
	config := "project: p\ncluster:\n  name: c\nservices:\n  s:\n    type: helm\n    chart: x\n    repo: https://e.x\n" + svcExtra
	if err := os.WriteFile(path, []byte(config), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// Values that are YAML syntax, quotes or escapes, all of which changed the
// document when the file's text was expanded before parsing
var awkwardValues = []string{"pass #1", "a: b", "*star", "&anchor", "!bang", "{x}", "[a]", "  padded  ",
	"'quoted'", `"double"`, `back\slash`, "% pct", "@at", "- dash", "|", ">", "line1\nline2", "\t\n", "${NOT_AGAIN}"}

// Expansion happens after parsing, so a value arrives exactly as set,
// whether or not the reference is quoted
func TestEnvVarValuesArriveIntact(t *testing.T) {
	for _, value := range awkwardValues {
		t.Setenv("VALUE_X", value)
		for _, ref := range []string{"${VALUE_X}", `"${VALUE_X}"`, "'${VALUE_X}'", "pre-${VALUE_X}"} {
			cfg, err := Parse(writeServiceConfig(t, "    labels:\n      v: "+ref+"\n"))
			if err != nil {
				t.Errorf("%q as %s: %v", value, ref, err)
				continue
			}
			want := strings.Replace(strings.Trim(ref, `"'`), "${VALUE_X}", value, 1)
			if got := cfg.Services["s"].Labels["v"]; got != want {
				t.Errorf("%q as %s arrived as %q", value, ref, got)
			}
		}
	}
}

// values_inline is YAML that Helm parses again, so it is expanded as YAML
// too: an unquoted reference inside it is a single value as well
func TestEnvVarValuesInValuesInlineArriveIntact(t *testing.T) {
	for _, value := range awkwardValues {
		t.Setenv("VALUE_X", value)
		cfg, err := Parse(writeServiceConfig(t, "    values_inline: |\n      # a comment\n      auth:\n        password: ${VALUE_X}\n        user: admin\n"))
		if err != nil {
			t.Errorf("%q: %v", value, err)
			continue
		}
		var values struct {
			Auth map[string]string `yaml:"auth"`
		}
		if err := yaml.Unmarshal([]byte(cfg.Services["s"].ValuesInline), &values); err != nil {
			t.Errorf("%q: values_inline no longer parses: %v\n%s", value, err, cfg.Services["s"].ValuesInline)
			continue
		}
		if values.Auth["password"] != value || values.Auth["user"] != "admin" {
			t.Errorf("%q: values_inline decoded as %v", value, values.Auth)
		}
	}

	// Without references the text is left exactly as written
	inline := "    values_inline: |\n      a:   1   # spaced\n"
	cfg, err := Parse(writeServiceConfig(t, inline))
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Services["s"].ValuesInline; got != "a:   1   # spaced\n" {
		t.Errorf("values_inline without references changed: %q", got)
	}

	// Inline text that is only valid YAML once expanded is expanded as text
	t.Setenv("STRUCT_X", "{a: 1}")
	cfg, err = Parse(writeServiceConfig(t, "    values_inline: \"x: ${STRUCT_X}\\n[\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Services["s"].ValuesInline; got != "x: {a: 1}\n[" {
		t.Errorf("unparseable values_inline should be expanded as text, got %q", got)
	}
}

// An unquoted reference is typed by its value, like a value written out in
// the file; a quoted one is a string
func TestEnvVarTyping(t *testing.T) {
	t.Setenv("WAIT_X", "false")
	cfg, err := Parse(writeServiceConfig(t, "    wait: ${WAIT_X}\n    keep_crds: ${UNSET_X:-true}\n"))
	if err != nil {
		t.Fatal(err)
	}
	svc := cfg.Services["s"]
	if svc.Wait == nil || *svc.Wait || svc.KeepCRDs == nil || !*svc.KeepCRDs {
		t.Errorf("wait = %v, keep_crds = %v", svc.Wait, svc.KeepCRDs)
	}

	// A string field takes any value, typed or not
	t.Setenv("NUM_X", "42")
	cfg, err = Parse(writeServiceConfig(t, "    version: ${NUM_X}\n"))
	if err != nil || cfg.Services["s"].Version != "42" {
		t.Errorf("version = %q, %v", cfg.Services["s"].Version, err)
	}

	// A quoted reference stays a string, which a bool field rejects (with
	// the reference's line)
	_, err = Parse(writeServiceConfig(t, "    wait: \"${WAIT_X}\"\n"))
	if err == nil || !strings.Contains(err.Error(), "line 9") {
		t.Errorf("a quoted reference in a bool field should fail on line 9, got %v", err)
	}
}

// Unknown keys are still rejected, with their line, when the file has
// references (the unknown-key check runs on the file's text)
func TestUnknownKeyWithEnvVars(t *testing.T) {
	t.Setenv("WAIT_X", "true")
	_, err := Parse(writeServiceConfig(t, "    wait: ${WAIT_X}\n    waitt: true\n"))
	if err == nil || !strings.Contains(err.Error(), "line 10: field waitt not found in service") {
		t.Errorf("want the unknown key reported on line 10, got %v", err)
	}
}

// Anchors and aliases share a node, which is expanded once
func TestEnvVarInAnchor(t *testing.T) {
	t.Setenv("OWNER_X", "team #1")
	cfg, err := Parse(writeServiceConfig(t, "    labels: &l\n      owner: ${OWNER_X}\n  t:\n    type: helm\n    chart: y\n    repo: https://e.x\n    labels: *l\n"))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"s", "t"} {
		if got := cfg.Services[name].Labels["owner"]; got != "team #1" {
			t.Errorf("%s: owner = %q", name, got)
		}
	}
}

// ParseIdentity expands references too, so config resolution sees the same
// project and cluster as Parse
func TestParseIdentityExpandsEnvVars(t *testing.T) {
	t.Setenv("CLUSTER_X", "dev #2")
	path := filepath.Join(t.TempDir(), "kraze.yml")
	if err := os.WriteFile(path, []byte("project: ${PROJECT_X:-shop}\ncluster:\n  name: ${CLUSTER_X}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	id, err := ParseIdentity(path)
	if err != nil || id.Project != "shop" || id.Cluster != "dev #2" {
		t.Errorf("identity = %+v, %v", id, err)
	}
}

func FuzzExpandEnvVars(f *testing.F) {
	for _, seed := range []string{"", "plain", "${A}", "${A:-b}", "${A:-}", "$${A}", "${", "${A", "${A:-${B}}", "}${A}{", "${_}", "${1A}"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, input string) {
		got := ExpandEnvVars(input)
		if !strings.Contains(input, "${") && got != input {
			t.Errorf("text without references changed: %q -> %q", input, got)
		}
		// Every reference is replaced in one pass, so with nothing set the
		// output holds no reference with a default-free valid name
		if strings.Contains(got, "${") && envVarPattern.MatchString(got) && !strings.Contains(input, "}") {
			t.Errorf("reference left unexpanded: %q -> %q", input, got)
		}
	})
}

// Any value substituted into values_inline decodes back to itself (unquoted,
// YAML's null spellings are null, as when written out in the file)
func FuzzEnvVarValueInYAMLText(f *testing.F) {
	for _, seed := range awkwardValues {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, value string) {
		if strings.ContainsRune(value, 0) || !utf8.ValidString(value) {
			t.Skip("not a possible environment variable value")
		}
		t.Setenv("FUZZ_X", value)
		out := expandEnvVarsInYAMLText("a:\n  k: ${FUZZ_X}\n  q: \"${FUZZ_X}\"\n")
		var got struct {
			A map[string]string `yaml:"a"`
		}
		if err := yaml.Unmarshal([]byte(out), &got); err != nil {
			t.Fatalf("%q: %v\n%s", value, err, out)
		}
		wantUnquoted := value
		if slices.Contains([]string{"", "~", "null", "Null", "NULL"}, value) {
			wantUnquoted = ""
		}
		if got.A["k"] != wantUnquoted || got.A["q"] != value {
			t.Errorf("%q decoded as %q / %q\n%s", value, got.A["k"], got.A["q"], out)
		}
	})
}
