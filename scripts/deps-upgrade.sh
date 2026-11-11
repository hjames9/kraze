#!/usr/bin/env bash

set -euo pipefail

KIND_FORK=github.com/hjames9/kind

cd "$(dirname "$0")/.."

echo "Upgrading dependencies..."
go get -u ./...

apimachinery=$(go list -m -f '{{.Version}}' k8s.io/apimachinery)
if go mod graph | grep -q "^k8s.io/apimachinery@$apimachinery sigs.k8s.io/structured-merge-diff/v6@"; then
  openapi=$(go mod graph | awk -v m="k8s.io/apimachinery@$apimachinery" '$1 == m && $2 ~ /^k8s.io\/kube-openapi@/ { sub(/.*@/, "", $2); print $2 }')
  if [ -z "$openapi" ]; then
    echo "error: can't find the k8s.io/kube-openapi version k8s.io/apimachinery@$apimachinery requires" >&2
    exit 1
  fi
  echo "Holding k8s.io/kube-openapi at $openapi (k8s.io/apimachinery $apimachinery uses structured-merge-diff v6)"
  go get "k8s.io/kube-openapi@$openapi"
fi

fork=$(GOFLAGS=-mod=mod go list -m -f '{{.Version}}' "$KIND_FORK@main")
echo "Pointing sigs.k8s.io/kind at $KIND_FORK@$fork"
go mod edit -replace "sigs.k8s.io/kind=$KIND_FORK@$fork"

go mod tidy
go build ./...
