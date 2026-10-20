package providers

import (
	"fmt"
	"math/rand"
	"reflect"
	"strings"
	"testing"
)

func TestSplitYAMLDocumentsEdgeCases(t *testing.T) {
	long := strings.Repeat("x", 200_000) // over bufio.Scanner's 64 KiB line limit
	tests := []struct {
		name  string
		input string
		want  []string // the metadata.name of each object, in order
	}{
		{name: "line longer than 64 KiB",
			input: "apiVersion: v1\nkind: ConfigMap\nmetadata: {name: big}\ndata:\n  dash.json: '" + long + "'\n---\napiVersion: v1\nkind: ConfigMap\nmetadata: {name: after}\n",
			want:  []string{"big", "after"}},
		{name: "indented --- inside a block scalar",
			input: "apiVersion: v1\nkind: ConfigMap\nmetadata: {name: cfg}\ndata:\n  app.yaml: |\n    a: 1\n    ---\n    b: 2\n",
			want:  []string{"cfg"}},
		{name: "separator with a comment",
			input: "apiVersion: v1\nkind: ConfigMap\nmetadata: {name: a}\n--- # second\napiVersion: v1\nkind: ConfigMap\nmetadata: {name: b}\n",
			want:  []string{"a", "b"}},
		{name: "leading and trailing separators",
			input: "---\napiVersion: v1\nkind: ConfigMap\nmetadata: {name: a}\n---\n",
			want:  []string{"a"}},
		{name: "CRLF line endings",
			input: "apiVersion: v1\r\nkind: ConfigMap\r\nmetadata: {name: a}\r\n---\r\napiVersion: v1\r\nkind: ConfigMap\r\nmetadata: {name: b}\r\n",
			want:  []string{"a", "b"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			objects, err := parseManifestsYAML(tt.input)
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, obj := range objects {
				got = append(got, obj.GetName())
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("objects = %v, want %v", got, tt.want)
			}
		})
	}

	// The content survives intact, not just the object count
	objects, _ := parseManifestsYAML(tests[0].input)
	if data, _, _ := unstructuredString(objects[0].Object, "data", "dash.json"); data != long {
		t.Errorf("long value truncated to %d bytes", len(data))
	}
	objects, _ = parseManifestsYAML(tests[1].input)
	if data, _, _ := unstructuredString(objects[0].Object, "data", "app.yaml"); data != "a: 1\n---\nb: 2\n" {
		t.Errorf("block scalar = %q", data)
	}
}

func unstructuredString(obj map[string]interface{}, fields ...string) (string, bool, error) {
	var cur interface{} = obj
	for _, f := range fields {
		m, ok := cur.(map[string]interface{})
		if !ok {
			return "", false, nil
		}
		cur = m[f]
	}
	s, ok := cur.(string)
	return s, ok, nil
}

// Property: render random ConfigMaps (with long values, embedded documents,
// comments and blank documents) as one multi-document file; splitting and
// parsing it gives back exactly those ConfigMaps
func TestPropertySplitYAMLRoundTrip(t *testing.T) {
	rng := rand.New(rand.NewSource(5))
	pieces := []string{"plain", "a: 1\n---\nb: 2", "--- not a separator", strings.Repeat("y", 70_000), "# not a comment", "multi\nline\n"}
	for run := 0; run < 200; run++ {
		var b strings.Builder
		var want []map[string]string
		for i := rng.Intn(5); i >= 0; i-- {
			if rng.Intn(3) == 0 {
				b.WriteString("# a comment between documents\n")
			}
			data := map[string]string{}
			b.WriteString(fmt.Sprintf("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: cm%d\ndata:\n", i))
			for k := rng.Intn(3); k >= 0; k-- {
				key := fmt.Sprintf("k%d", k)
				value := pieces[rng.Intn(len(pieces))]
				data[key] = value
				b.WriteString("  " + key + ": |-\n")
				for _, line := range strings.Split(value, "\n") {
					b.WriteString("    " + line + "\n")
				}
			}
			want = append(want, data)
			b.WriteString("---\n")
			if rng.Intn(4) == 0 {
				b.WriteString("---\n") // blank document
			}
		}

		objects, err := parseManifestsYAML(b.String())
		if err != nil {
			t.Fatalf("run %d: %v", run, err)
		}
		if len(objects) != len(want) {
			t.Fatalf("run %d: got %d objects, want %d", run, len(objects), len(want))
		}
		for i, obj := range objects {
			for key, value := range want[i] {
				if got, _, _ := unstructuredString(obj.Object, "data", key); got != strings.TrimSuffix(value, "\n") {
					t.Fatalf("run %d: %s data[%s] = %.60q, want %.60q", run, obj.GetName(), key, got, value)
				}
			}
		}
	}
}
