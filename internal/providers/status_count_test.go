package providers

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/hjames9/kraze/internal/config"
)

// A comment-only document (the license header that starts many upstream
// install files, e.g. Gateway API's standard-install.yaml) is not a
// resource: once everything is applied the service is ready
func TestStatusIgnoresDocumentsWithoutObjects(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.yaml")
	manifest := "# Copyright The Kubernetes Authors.\n# Licensed under the Apache License\n---\n" + ownershipManifests + "\n---\n# trailing comment\n"
	if err := os.WriteFile(path, []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	noCreate := false
	svc := &config.ServiceConfig{Name: "app", Type: "manifests", Namespace: "apps", Path: path, CreateNamespace: &noCreate}
	mp := fakeManifestsProvider("web")
	ctx := context.Background()

	before, err := mp.Status(ctx, svc)
	if err != nil {
		t.Fatal(err)
	}
	if before.Installed || before.Ready || before.Message != "0/2 resources exist" {
		t.Errorf("before install: %+v", before)
	}

	if err := mp.Install(ctx, svc); err != nil {
		t.Fatal(err)
	}
	after, err := mp.Status(ctx, svc)
	if err != nil {
		t.Fatal(err)
	}
	if !after.Installed || !after.Ready || after.Message != "2/2 resources exist" {
		t.Errorf("after install: %+v", after)
	}
}
