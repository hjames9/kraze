package providers

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"

	yaml "go.yaml.in/yaml/v3"
	"helm.sh/helm/v4/pkg/cli"
	"helm.sh/helm/v4/pkg/cli/values"
	"helm.sh/helm/v4/pkg/getter"

	"github.com/hjames9/kraze/internal/config"
)

// The values kraze passes to Helm must be what `helm install -f a.yaml -f
// b.yaml` would use. Helm's CLI is the oracle. (kraze used to parse values
// with yaml.v3 and merge them itself: the merge agreed with Helm, the parser
// didn't, see LoadHelmValues.)

// valuesSnippets are values-file fragments, each valid YAML on its own,
// covering what YAML parsers disagree on
var valuesSnippets = []string{
	"image:\n  tag: dev\n",
	"image:\n  tag: 20240101\n",
	"image:\n  tag: 1.10\n",
	"image: null\n",
	"image:\n  tag: null\n",
	"persistence:\n  enabled: no\n",
	"replicas: 3\n",
	"replicas: 1e3\n",
	"ports:\n  80: http\n  443: https\n",
	"flags:\n  true: on\n",
	"service:\n  annotations: {}\n",
	"service:\n  annotations:\n    a: \"1\"\n",
	"list: [a, b]\n",
	"list:\n  - c\n",
	"big: 12345678901234567890\n",
	"octal: 0755\n",
	"empty: ''\n",
	"when: 2024-01-01\n",
	"anchor: &x {a: 1}\nref: *x\n",
}

// normalize renders values as Helm stores and templates them: JSON
func normalize(t *testing.T, v map[string]interface{}) string {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		return "unencodable: " + err.Error()
	}
	return string(data)
}

func TestPropertyLoadHelmValuesMatchesHelmCLI(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	for run := 0; run < 500; run++ {
		dir := t.TempDir()
		var files []string
		for i := rng.Intn(3) + 1; i > 0; i-- {
			var b strings.Builder
			for j := rng.Intn(3) + 1; j > 0; j-- {
				b.WriteString(valuesSnippets[rng.Intn(len(valuesSnippets))])
			}
			path := filepath.Join(dir, fmt.Sprintf("v%d.yaml", len(files)))
			if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
				t.Fatal(err)
			}
			files = append(files, path)
		}

		helmVals, helmErr := (&values.Options{ValueFiles: files}).MergeValues(getter.All(cli.New()))
		svc := &config.ServiceConfig{Values: valuesFieldOf(t, files)}
		krazeVals, krazeErr := LoadHelmValues(svc)
		if (helmErr != nil) != (krazeErr != nil) {
			t.Fatalf("run %d: Helm error %v, kraze error %v\n%s", run, helmErr, krazeErr, describeFiles(files))
		}
		if helmErr != nil {
			continue
		}
		if got, want := normalize(t, krazeVals), normalize(t, helmVals); got != want {
			t.Fatalf("run %d: kraze passes\n  %s\nHelm would use\n  %s\n%s", run, got, want, describeFiles(files))
		}
	}
}

func describeFiles(files []string) string {
	var b strings.Builder
	for _, f := range files {
		data, _ := os.ReadFile(f)
		fmt.Fprintf(&b, "--- %s\n%s", filepath.Base(f), data)
	}
	return b.String()
}

func valuesFieldOf(t *testing.T, files []string) config.ValuesField {
	t.Helper()
	var field config.ValuesField
	data, _ := json.Marshal(files)
	if err := yaml.Unmarshal(data, &field); err != nil {
		t.Fatal(err)
	}
	return field
}
