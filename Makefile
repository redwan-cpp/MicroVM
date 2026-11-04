# Makefile: the project's buttons. Type `make` (or `make help`) to list them.
#
# A Makefile is a list of named jobs ("targets"). `make build` runs the
# commands under `build:`. Each `## text` comment becomes the help line.

MODULE  := github.com/redwan-cpp/microvm-sandbox

# Which build is this? Taken from git, then stamped into the programs
# (see internal/version/version.go).
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
LDFLAGS := -s -w \
	-X $(MODULE)/internal/version.Version=$(VERSION) \
	-X $(MODULE)/internal/version.Commit=$(COMMIT)

# Developer tools are pinned to exact versions, so everyone (and CI) gets
# the same results. The version is part of the file name, so changing the
# pin here installs the new version automatically.
GOLANGCI_LINT_VERSION := v2.15.0
TOOLS_BIN     := $(CURDIR)/bin/tools
GOLANGCI_LINT := $(TOOLS_BIN)/golangci-lint-$(GOLANGCI_LINT_VERSION)

.DEFAULT_GOAL := help
.PHONY: help build test lint fmt tools e2e bench clean

help: ## Show this list
	@grep -E '^[a-zA-Z0-9_-]+:.*## ' $(MAKEFILE_LIST) | \
		awk 'BEGIN {FS = ":.*## "}; {printf "  make %-7s %s\n", $$1, $$2}'

build: ## Build sandboxd, sandboxctl and sandbox-agent into bin/
	@mkdir -p bin
	CGO_ENABLED=0 go build -trimpath -ldflags '$(LDFLAGS)' -o bin/ ./cmd/...

test: ## Run the unit tests (with the race detector)
	go test -race -count=1 ./...

lint: $(GOLANGCI_LINT) ## Check the code for mistakes and style problems
	go vet ./...
	$(GOLANGCI_LINT) run ./...

fmt: $(GOLANGCI_LINT) ## Auto-format all Go code
	$(GOLANGCI_LINT) fmt ./...

tools: $(GOLANGCI_LINT) ## Install the pinned developer tools into bin/tools

$(GOLANGCI_LINT):
	@mkdir -p $(TOOLS_BIN)
	GOBIN=$(TOOLS_BIN) go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)
	mv $(TOOLS_BIN)/golangci-lint $@

e2e: ## Tests with real microVMs (not built yet: week 2)
	@echo "e2e: skipped, nothing to run yet (arrives in week 2, needs KVM)"

bench: ## Speed benchmarks (not built yet: week 10)
	@echo "bench: skipped, nothing to run yet (arrives in week 10)"

clean: ## Delete everything `make` built
	rm -rf bin
