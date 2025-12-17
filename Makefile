# SLB Makefile
# Simultaneous Launch Button - Two-person rule for dangerous commands

.PHONY: all build build-all install dev run watch test test-unit test-integration test-race test-coverage test-coverage-check lint fmt vet check release snapshot clean help

# Default target
all: check build

# Build variables
BINARY := slb
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
COMMIT := $(shell git rev-parse --short HEAD 2>/dev/null || echo "none")
DATE := $(shell date -u +"%Y-%m-%dT%H:%M:%SZ")
LDFLAGS := -ldflags "-X github.com/Dicklesworthstone/slb/internal/cli.version=$(VERSION) -X github.com/Dicklesworthstone/slb/internal/cli.commit=$(COMMIT) -X github.com/Dicklesworthstone/slb/internal/cli.date=$(DATE)"

# Build directories
BUILD_DIR := ./build
DIST_DIR := ./dist

# Go environment
GOBIN := $(shell go env GOBIN)
ifeq ($(GOBIN),)
	GOBIN := $(shell go env GOPATH)/bin
endif

# ============================================================================
# Build targets
# ============================================================================

## build: Build slb binary for current platform
build:
	@echo "Building $(BINARY)..."
	@go build $(LDFLAGS) -o $(BUILD_DIR)/$(BINARY) ./cmd/slb

## build-all: Build for all platforms (linux, darwin, windows)
build-all:
	@echo "Building for all platforms..."
	@mkdir -p $(BUILD_DIR)
	GOOS=linux GOARCH=amd64 go build $(LDFLAGS) -o $(BUILD_DIR)/$(BINARY)-linux-amd64 ./cmd/slb
	GOOS=linux GOARCH=arm64 go build $(LDFLAGS) -o $(BUILD_DIR)/$(BINARY)-linux-arm64 ./cmd/slb
	GOOS=darwin GOARCH=amd64 go build $(LDFLAGS) -o $(BUILD_DIR)/$(BINARY)-darwin-amd64 ./cmd/slb
	GOOS=darwin GOARCH=arm64 go build $(LDFLAGS) -o $(BUILD_DIR)/$(BINARY)-darwin-arm64 ./cmd/slb
	GOOS=windows GOARCH=amd64 go build $(LDFLAGS) -o $(BUILD_DIR)/$(BINARY)-windows-amd64.exe ./cmd/slb

## install: Install slb to GOBIN
install: build
	@echo "Installing $(BINARY) to $(GOBIN)..."
	@cp $(BUILD_DIR)/$(BINARY) $(GOBIN)/$(BINARY)

# ============================================================================
# Development targets
# ============================================================================

## dev: Build with race detector for development
dev:
	@echo "Building with race detector..."
	@go build -race $(LDFLAGS) -o $(BUILD_DIR)/$(BINARY) ./cmd/slb

## run: Build and run with arguments (use: make run ARGS="command args")
run: build
	@$(BUILD_DIR)/$(BINARY) $(ARGS)

## watch: Watch files and rebuild on changes (requires watchexec)
watch:
	@watchexec -e go -r -- make build

# ============================================================================
# Testing targets
# ============================================================================

## test: Run all tests
test:
	@echo "Running tests..."
	@go test -v ./...

## test-unit: Run unit tests only (exclude integration)
test-unit:
	@echo "Running unit tests..."
	@go test -v -short ./...

## test-integration: Run integration tests only
test-integration:
	@echo "Running integration tests..."
	@go test -v -run Integration ./...

## test-race: Run tests with race detector
test-race:
	@echo "Running tests with race detector..."
	@go test -v -race ./...

## test-coverage: Generate coverage report
test-coverage:
	@echo "Generating coverage report..."
	@go test -coverprofile=coverage.out ./...
	@go tool cover -html=coverage.out -o coverage.html
	@echo "Coverage report: coverage.html"

## test-coverage-check: Check coverage meets threshold (80%)
test-coverage-check:
	@echo "Checking coverage threshold..."
	@go test -coverprofile=coverage.out ./...
	@COVERAGE=$$(go tool cover -func=coverage.out | grep total | awk '{print $$3}' | sed 's/%//'); \
	echo "Coverage: $${COVERAGE}%"; \
	if [ $$(echo "$${COVERAGE} < 80" | bc -l) -eq 1 ]; then \
		echo "Coverage $${COVERAGE}% is below 80% threshold"; \
		exit 1; \
	fi; \
	echo "Coverage $${COVERAGE}% meets 80% threshold"

# ============================================================================
# Quality targets
# ============================================================================

## lint: Run golangci-lint
lint:
	@echo "Running linter..."
	@golangci-lint run ./...

## fmt: Format code
fmt:
	@echo "Formatting code..."
	@go fmt ./...
	@gofumpt -l -w .

## vet: Run go vet
vet:
	@echo "Running go vet..."
	@go vet ./...

## check: Run all quality checks (fmt, vet, lint, test)
check: fmt vet lint test

# ============================================================================
# Release targets
# ============================================================================

## release: Run goreleaser for production release
release:
	@echo "Creating release..."
	@goreleaser release --clean

## snapshot: Build snapshot release (no publish)
snapshot:
	@echo "Creating snapshot..."
	@goreleaser release --snapshot --clean

# ============================================================================
# Cleanup targets
# ============================================================================

## clean: Remove build artifacts
clean:
	@echo "Cleaning..."
	@rm -rf $(BUILD_DIR) $(DIST_DIR) coverage.out coverage.html

# ============================================================================
# Help
# ============================================================================

## help: Show this help message
help:
	@echo "SLB Makefile targets:"
	@echo ""
	@sed -n 's/^##//p' $(MAKEFILE_LIST) | column -t -s ':' | sed 's/^/  /'
