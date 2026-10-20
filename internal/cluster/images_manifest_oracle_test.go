package cluster

import (
	"slices"
	"testing"
)

// Container images in a manifest are found however the YAML spells them; the
// oracle is the parsed pod spec (containerImages)
func TestManifestImagesMatchParsedPodSpecs(t *testing.T) {
	manifests := map[string]string{
		"trailing comment": `apiVersion: apps/v1
kind: Deployment
metadata: {name: a}
spec:
  template:
    spec:
      containers:
        - name: app
          image: myapp:dev   # built locally
`,
		"flow style": `apiVersion: v1
kind: Pod
metadata: {name: a}
spec: {containers: [{name: app, image: "myapp:dev"}]}
`,
		"key after image in flow map": `apiVersion: v1
kind: Pod
metadata: {name: a}
spec:
  containers:
    - {image: myapp:dev, name: app}
`,
		"JSON document": `{"apiVersion": "v1", "kind": "Pod", "metadata": {"name": "a"},
 "spec": {"containers": [{"name": "app", "image": "myapp:dev"}]}}
`,
		"folded scalar": `apiVersion: v1
kind: Pod
metadata: {name: a}
spec:
  containers:
    - name: app
      image: >-
        myapp:dev
`,
	}
	im := NewImageManager(false)
	for name, manifest := range manifests {
		t.Run(name, func(t *testing.T) {
			want := renderedManifestImages(t, manifest)
			if len(want) == 0 {
				t.Fatal("oracle found no images")
			}
			got := im.extractImagesFromManifest(manifest)
			for _, image := range want {
				if !slices.Contains(got, image) {
					t.Errorf("missed %s (found %v)", image, got)
				}
			}
		})
	}
}

// An unquoted numeric tag in values is a number, not a missing tag
func TestExtractImagesFromValuesNumericTag(t *testing.T) {
	images, err := NewImageManager(false).ExtractImagesFromYAMLString("image:\n  repository: myapp\n  tag: 20240101\n")
	if err != nil || !slices.Contains(images, "myapp:20240101") {
		t.Errorf("got %v, %v", images, err)
	}
}
