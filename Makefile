.PHONY: all build test clean install fmt vet lint help run-tests coverage validate-examples release release-publish

BINARY_NAME=kraze
VERSION?=dev
GIT_COMMIT?=$(shell git rev-parse --short HEAD 2>/dev/null || echo "unknown")
BUILD_DATE?=$(shell date -u +"%Y-%m-%dT%H:%M:%SZ")
LDFLAGS=-ldflags "-X main.Version=$(VERSION) -X main.GitCommit=$(GIT_COMMIT) -X main.BuildDate=$(BUILD_DATE)"

GOCMD=go
GOBUILD=$(GOCMD) build
GOCLEAN=$(GOCMD) clean
GOTEST=$(GOCMD) test
GOGET=$(GOCMD) get
GOMOD=$(GOCMD) mod
GOFMT=$(GOCMD) fmt
GOVET=$(GOCMD) vet

GHCMD=gh

BUILD_DIR=build
CMD_DIR=./cmd/$(BINARY_NAME)

all: test build

build:
	@echo "Building $(BINARY_NAME)..."
	@mkdir -p $(BUILD_DIR)
	$(GOBUILD) $(LDFLAGS) -o $(BUILD_DIR)/$(BINARY_NAME) $(CMD_DIR)
	@echo "Build complete: $(BUILD_DIR)/$(BINARY_NAME)"

build-all:
	@echo "Building for all platforms..."
	@mkdir -p $(BUILD_DIR)
	GOOS=linux GOARCH=amd64 $(GOBUILD) $(LDFLAGS) -o $(BUILD_DIR)/$(BINARY_NAME)-linux-amd64 $(CMD_DIR)
	GOOS=linux GOARCH=arm64 $(GOBUILD) $(LDFLAGS) -o $(BUILD_DIR)/$(BINARY_NAME)-linux-arm64 $(CMD_DIR)
	GOOS=darwin GOARCH=amd64 $(GOBUILD) $(LDFLAGS) -o $(BUILD_DIR)/$(BINARY_NAME)-darwin-amd64 $(CMD_DIR)
	GOOS=darwin GOARCH=arm64 $(GOBUILD) $(LDFLAGS) -o $(BUILD_DIR)/$(BINARY_NAME)-darwin-arm64 $(CMD_DIR)
	GOOS=windows GOARCH=amd64 $(GOBUILD) $(LDFLAGS) -o $(BUILD_DIR)/$(BINARY_NAME)-windows-amd64.exe $(CMD_DIR)
	GOOS=windows GOARCH=arm64 $(GOBUILD) $(LDFLAGS) -o $(BUILD_DIR)/$(BINARY_NAME)-windows-arm64.exe $(CMD_DIR)
	@echo "Cross-compilation complete"

release:
	@echo "Building release $(VERSION)..."
	@if [ "$(VERSION)" = "dev" ]; then \
		echo "Error: VERSION must be set (e.g., make release VERSION=v0.1.0)"; \
		exit 1; \
	fi
	@mkdir -p $(BUILD_DIR)
	@echo "Building binaries..."
	GOOS=linux GOARCH=amd64 $(GOBUILD) $(LDFLAGS) -o $(BUILD_DIR)/$(BINARY_NAME)-$(VERSION)-linux-amd64 $(CMD_DIR)
	GOOS=linux GOARCH=arm64 $(GOBUILD) $(LDFLAGS) -o $(BUILD_DIR)/$(BINARY_NAME)-$(VERSION)-linux-arm64 $(CMD_DIR)
	GOOS=darwin GOARCH=amd64 $(GOBUILD) $(LDFLAGS) -o $(BUILD_DIR)/$(BINARY_NAME)-$(VERSION)-darwin-amd64 $(CMD_DIR)
	GOOS=darwin GOARCH=arm64 $(GOBUILD) $(LDFLAGS) -o $(BUILD_DIR)/$(BINARY_NAME)-$(VERSION)-darwin-arm64 $(CMD_DIR)
	GOOS=windows GOARCH=amd64 $(GOBUILD) $(LDFLAGS) -o $(BUILD_DIR)/$(BINARY_NAME)-$(VERSION)-windows-amd64.exe $(CMD_DIR)
	GOOS=windows GOARCH=arm64 $(GOBUILD) $(LDFLAGS) -o $(BUILD_DIR)/$(BINARY_NAME)-$(VERSION)-windows-arm64.exe $(CMD_DIR)
	@echo "Generating checksums..."
	@cd $(BUILD_DIR) && sha256sum $(BINARY_NAME)-$(VERSION)-* > $(BINARY_NAME)-$(VERSION)-checksums.txt
	@echo ""
	@echo "Release $(VERSION) built successfully!"
	@echo ""
	@echo "Files:"
	@ls -lh $(BUILD_DIR)/$(BINARY_NAME)-$(VERSION)-*
	@echo ""
	@echo "Creating draft GitHub release..."
	$(GHCMD) release create $(VERSION) \
		--draft \
		--title "$(VERSION)" \
		--generate-notes \
		$(BUILD_DIR)/$(BINARY_NAME)-$(VERSION)-*
	@echo ""
	@echo "Draft release $(VERSION) created successfully!"
	@echo ""
	@echo "To publish the release, run:"
	@echo "  make release-publish VERSION=$(VERSION)"

release-publish:
	@echo "Publishing release $(VERSION)..."
	@if [ "$(VERSION)" = "dev" ]; then \
		echo "Error: VERSION must be set (e.g., make release-publish VERSION=v0.1.0)"; \
		exit 1; \
	fi
	$(GHCMD) release edit $(VERSION) --draft=false
	@echo ""
	@echo "Release $(VERSION) published successfully!"
	@echo ""
	@echo "View at: https://github.com/$$($(GHCMD) repo view --json nameWithOwner -q .nameWithOwner)/releases/tag/$(VERSION)"

test:
	@echo "Running tests..."
	$(GOTEST) -v ./...

test-coverage:
	@echo "Running tests with coverage..."
	$(GOTEST) -v -coverprofile=$(BUILD_DIR)/coverage.out ./...
	$(GOCMD) tool cover -html=$(BUILD_DIR)/coverage.out -o $(BUILD_DIR)/coverage.html
	@echo "Coverage report generated: $(BUILD_DIR)/coverage.html"

test-short:
	@echo "Running short tests..."
	$(GOTEST) -short ./...

bench:
	@echo "Running benchmarks..."
	$(GOTEST) -bench=. -benchmem ./...

install: build
	@echo "Installing $(BINARY_NAME)..."
	@cp $(BUILD_DIR)/$(BINARY_NAME) $(GOPATH)/bin/$(BINARY_NAME)
	@echo "Installed to $(GOPATH)/bin/$(BINARY_NAME)"

clean:
	@echo "Cleaning..."
	$(GOCLEAN)
	@rm -rf $(BUILD_DIR)
	@echo "Clean complete"

fmt:
	@echo "Formatting code..."
	$(GOFMT) ./...

vet:
	@echo "Running go vet..."
	$(GOVET) ./...

lint:
	@echo "Running linter..."
	golangci-lint run ./...

deps:
	@echo "Downloading dependencies..."
	$(GOMOD) download
	$(GOMOD) tidy

deps-upgrade:
	@echo "Upgrading dependencies..."
	$(GOGET) -u ./...
	$(GOMOD) tidy

verify: fmt vet test

run: build
	@echo "Running $(BINARY_NAME)..."
	./$(BUILD_DIR)/$(BINARY_NAME)

run-help: build
	./$(BUILD_DIR)/$(BINARY_NAME) --help

run-version: build
	./$(BUILD_DIR)/$(BINARY_NAME) version

validate-examples: build
	@echo "Validating all examples..."
	@for dir in examples/*/; do \
		if [ -f "$$dir/kraze.yml" ]; then \
			echo "Validating $$dir"; \
			./$(BUILD_DIR)/$(BINARY_NAME) validate --file "$$dir/kraze.yml" || exit 1; \
		fi \
	done
	@echo "All examples validated successfully"

help:
	@echo "$(BINARY_NAME) - Makefile help"
	@echo ""
	@echo "Usage: make [target]"
	@echo ""
	@echo "Targets:"
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | sort | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-20s\033[0m %s\n", $$1, $$2}'
	@echo ""
	@echo "Build info:"
	@echo "  Version:    $(VERSION)"
	@echo "  Git Commit: $(GIT_COMMIT)"
	@echo "  Build Date: $(BUILD_DATE)"
