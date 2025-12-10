.PHONY: all build test clean install fmt vet lint help run-tests coverage validate-examples release release-publish bump-patch bump-minor bump-major show-version

BINARY_NAME=kraze
VERSION?=$(shell cat VERSION 2>/dev/null || echo "dev")
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

# Platform targets for cross-compilation
PLATFORMS=linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64 windows/arm64

all: test build ## Run tests and build the binary

build: ## Build the binary for the current platform
	@echo "Building $(BINARY_NAME)..."
	@mkdir -p $(BUILD_DIR)
	$(GOBUILD) $(LDFLAGS) -o $(BUILD_DIR)/$(BINARY_NAME) $(CMD_DIR)
	@echo "Build complete: $(BUILD_DIR)/$(BINARY_NAME)"

build-all: ## Build binaries for all platforms (linux, darwin, windows)
	@echo "Building for all platforms..."
	@mkdir -p $(BUILD_DIR)
	@$(foreach platform,$(PLATFORMS), \
		$(eval GOOS=$(word 1,$(subst /, ,$(platform)))) \
		$(eval GOARCH=$(word 2,$(subst /, ,$(platform)))) \
		$(eval EXT=$(if $(filter windows,$(GOOS)),.exe,)) \
		echo "Building $(GOOS)/$(GOARCH)..." && \
		GOOS=$(GOOS) GOARCH=$(GOARCH) $(GOBUILD) $(LDFLAGS) -o $(BUILD_DIR)/$(BINARY_NAME)-$(GOOS)-$(GOARCH)$(EXT) $(CMD_DIR) || exit 1; \
	)
	@echo "Cross-compilation complete"

release: ## Build release binaries, create git tag, and draft GitHub release
	@echo "Building release $(VERSION)..."
	@if [ "$(VERSION)" = "dev" ]; then \
		echo "Error: VERSION is 'dev'. Please bump the version first using:"; \
		echo "  make bump-patch  # for bug fixes (0.4.1 -> 0.4.2)"; \
		echo "  make bump-minor  # for new features (0.4.1 -> 0.5.0)"; \
		echo "  make bump-major  # for breaking changes (0.4.1 -> 1.0.0)"; \
		exit 1; \
	fi
	@echo "Checking if git tag $(VERSION) already exists..."
	@if git rev-parse $(VERSION) >/dev/null 2>&1; then \
		echo "Error: Git tag $(VERSION) already exists!"; \
		echo "If you need to create a new release, bump the version first."; \
		exit 1; \
	fi
	@echo "Checking for uncommitted changes..."
	@if [ -n "$$(git status --porcelain)" ]; then \
		echo "Error: You have uncommitted changes. Please commit or stash them first."; \
		git status --short; \
		exit 1; \
	fi
	@mkdir -p $(BUILD_DIR)
	@echo "Building binaries..."
	@$(foreach platform,$(PLATFORMS), \
		$(eval GOOS=$(word 1,$(subst /, ,$(platform)))) \
		$(eval GOARCH=$(word 2,$(subst /, ,$(platform)))) \
		$(eval EXT=$(if $(filter windows,$(GOOS)),.exe,)) \
		echo "Building $(GOOS)/$(GOARCH)..." && \
		GOOS=$(GOOS) GOARCH=$(GOARCH) $(GOBUILD) $(LDFLAGS) -o $(BUILD_DIR)/$(BINARY_NAME)-$(VERSION)-$(GOOS)-$(GOARCH)$(EXT) $(CMD_DIR) || exit 1; \
	)
	@echo "Generating checksums..."
	@cd $(BUILD_DIR) && sha256sum $(BINARY_NAME)-$(VERSION)-* > $(BINARY_NAME)-$(VERSION)-checksums.txt
	@echo ""
	@echo "Creating git tag $(VERSION)..."
	git tag -a $(VERSION) -m "Release $(VERSION)"
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
	@echo "To publish the release and push the tag, run:"
	@echo "  make release-publish"

release-publish: ## Publish the draft release and push git tag
	@echo "Publishing release $(VERSION)..."
	@if [ "$(VERSION)" = "dev" ]; then \
		echo "Error: VERSION is 'dev'. Cannot publish."; \
		exit 1; \
	fi
	@echo "Pushing git tag $(VERSION) to origin..."
	git push origin $(VERSION)
	@echo ""
	@echo "Publishing GitHub release..."
	$(GHCMD) release edit $(VERSION) --draft=false
	@echo ""
	@echo "Release $(VERSION) published successfully!"
	@echo ""
	@echo "View at: https://github.com/$$($(GHCMD) repo view --json nameWithOwner -q .nameWithOwner)/releases/tag/$(VERSION)"

test: ## Run all tests
	@echo "Running tests..."
	$(GOTEST) -v ./...

test-coverage: ## Run tests with coverage report
	@echo "Running tests with coverage..."
	$(GOTEST) -v -coverprofile=$(BUILD_DIR)/coverage.out ./...
	$(GOCMD) tool cover -html=$(BUILD_DIR)/coverage.out -o $(BUILD_DIR)/coverage.html
	@echo "Coverage report generated: $(BUILD_DIR)/coverage.html"

test-short: ## Run short tests only
	@echo "Running short tests..."
	$(GOTEST) -short ./...

bench: ## Run benchmarks
	@echo "Running benchmarks..."
	$(GOTEST) -bench=. -benchmem ./...

install: build ## Install binary to GOPATH/bin
	@echo "Installing $(BINARY_NAME)..."
	@cp $(BUILD_DIR)/$(BINARY_NAME) $(GOPATH)/bin/$(BINARY_NAME)
	@echo "Installed to $(GOPATH)/bin/$(BINARY_NAME)"

clean: ## Remove build artifacts
	@echo "Cleaning..."
	$(GOCLEAN)
	@rm -rf $(BUILD_DIR)
	@echo "Clean complete"

fmt: ## Format Go source code
	@echo "Formatting code..."
	$(GOFMT) ./...

vet: ## Run go vet
	@echo "Running go vet..."
	$(GOVET) ./...

lint: ## Run golangci-lint
	@echo "Running linter..."
	golangci-lint run ./...

deps: ## Download and tidy dependencies
	@echo "Downloading dependencies..."
	$(GOMOD) download
	$(GOMOD) tidy

deps-upgrade: ## Upgrade all dependencies
	@echo "Upgrading dependencies..."
	$(GOGET) -u ./...
	$(GOMOD) tidy

verify: fmt vet test ## Run fmt, vet, and test

run: build ## Build and run the binary
	@echo "Running $(BINARY_NAME)..."
	./$(BUILD_DIR)/$(BINARY_NAME)

run-help: build ## Build and show help
	./$(BUILD_DIR)/$(BINARY_NAME) --help

run-version: build ## Build and show version
	./$(BUILD_DIR)/$(BINARY_NAME) version

validate-examples: build ## Validate all example configurations
	@echo "Validating all examples..."
	@for dir in examples/*/; do \
		if [ -f "$$dir/kraze.yml" ]; then \
			echo "Validating $$dir"; \
			./$(BUILD_DIR)/$(BINARY_NAME) validate --file "$$dir/kraze.yml" || exit 1; \
		fi \
	done
	@echo "All examples validated successfully"

help: ## Show this help message
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

show-version: ## Show current version
	@echo "Current version: $(VERSION)"

bump-patch: ## Bump patch version (e.g., 0.4.1 -> 0.4.2)
	@echo "Bumping patch version..."
	@current=$$(cat VERSION | sed 's/^v//'); \
	major=$$(echo $$current | cut -d. -f1); \
	minor=$$(echo $$current | cut -d. -f2); \
	patch=$$(echo $$current | cut -d. -f3); \
	new_patch=$$((patch + 1)); \
	new_version="v$$major.$$minor.$$new_patch"; \
	echo "$$new_version" > VERSION; \
	echo "Version bumped: $$current -> $$new_version"; \
	echo ""; \
	echo "Next steps:"; \
	echo "  1. Review changes: git diff VERSION"; \
	echo "  2. Commit the version bump: git add VERSION && git commit -m \"Bump version to $$new_version\""; \
	echo "  3. Create release: make release"

bump-minor: ## Bump minor version (e.g., 0.4.1 -> 0.5.0)
	@echo "Bumping minor version..."
	@current=$$(cat VERSION | sed 's/^v//'); \
	major=$$(echo $$current | cut -d. -f1); \
	minor=$$(echo $$current | cut -d. -f2); \
	new_minor=$$((minor + 1)); \
	new_version="v$$major.$$new_minor.0"; \
	echo "$$new_version" > VERSION; \
	echo "Version bumped: $$current -> $$new_version"; \
	echo ""; \
	echo "Next steps:"; \
	echo "  1. Review changes: git diff VERSION"; \
	echo "  2. Commit the version bump: git add VERSION && git commit -m \"Bump version to $$new_version\""; \
	echo "  3. Create release: make release"

bump-major: ## Bump major version (e.g., 0.4.1 -> 1.0.0)
	@echo "Bumping major version..."
	@current=$$(cat VERSION | sed 's/^v//'); \
	major=$$(echo $$current | cut -d. -f1); \
	new_major=$$((major + 1)); \
	new_version="v$$new_major.0.0"; \
	echo "$$new_version" > VERSION; \
	echo "Version bumped: $$current -> $$new_version"; \
	echo ""; \
	echo "Next steps:"; \
	echo "  1. Review changes: git diff VERSION"; \
	echo "  2. Commit the version bump: git add VERSION && git commit -m \"Bump version to $$new_version\""; \
	echo "  3. Create release: make release"
