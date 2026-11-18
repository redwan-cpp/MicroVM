# Makefile: the project's buttons. Type `make` (or `make help`) to list them.
#
# A Makefile is a list of named jobs ("targets"). `make build` runs the
# commands under `build:`. Each `## text` comment becomes the help line.

MODULE  := github.com/redwan-cpp/MicroVM

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
#
# buf turns the .proto files in proto/ into Go code, with the help of two
# plugins: protoc-gen-go (the messages) and protoc-gen-go-grpc (the gRPC
# client and server). Keep PROTOC_GEN_GO_VERSION equal to the
# google.golang.org/protobuf version in go.mod.
GOLANGCI_LINT_VERSION      := v2.15.0
BUF_VERSION                := v1.75.0
PROTOC_GEN_GO_VERSION      := v1.36.12
PROTOC_GEN_GO_GRPC_VERSION := v1.6.2

TOOLS_BIN          := $(CURDIR)/bin/tools
GOLANGCI_LINT      := $(TOOLS_BIN)/golangci-lint-$(GOLANGCI_LINT_VERSION)
BUF                := $(TOOLS_BIN)/buf-$(BUF_VERSION)
PROTOC_GEN_GO      := $(TOOLS_BIN)/protoc-gen-go-$(PROTOC_GEN_GO_VERSION)
PROTOC_GEN_GO_GRPC := $(TOOLS_BIN)/protoc-gen-go-grpc-$(PROTOC_GEN_GO_GRPC_VERSION)
TOOLS              := $(GOLANGCI_LINT) $(BUF) $(PROTOC_GEN_GO) $(PROTOC_GEN_GO_GRPC)

# Where the generated Go code goes (see buf.gen.yaml). It is committed, so
# a plain `go build` works without any of these tools.
GEN_DIR := internal/gen

.DEFAULT_GOAL := help
.PHONY: help build test lint fmt generate generate-check tools firecracker guest snapshots boot demo e2e e2e-jailer bench clean

help: ## Show this list
	@grep -E '^[a-zA-Z0-9_-]+:.*## ' $(MAKEFILE_LIST) | \
		awk 'BEGIN {FS = ":.*## "}; {printf "  make %-15s %s\n", $$1, $$2}'

build: ## Build sandboxd, sandboxctl and sandbox-agent into bin/
	@mkdir -p bin
	CGO_ENABLED=0 go build -trimpath -ldflags '$(LDFLAGS)' -o bin/ ./cmd/...

test: ## Run the unit tests (with the race detector)
	go test -race -count=1 ./...

# `-tags e2e` also checks test/e2e, which only builds with that tag (it
# needs KVM, so it never runs in CI, but it must still compile), and
# `-tags 'e2e jailer'` adds the jailer test (it needs root as well).
lint: $(GOLANGCI_LINT) $(BUF) ## Check the code for mistakes and style problems
	$(BUF) lint
	go vet ./...
	go vet -tags e2e ./...
	go vet -tags 'e2e jailer' ./...
	$(GOLANGCI_LINT) run ./...

fmt: $(GOLANGCI_LINT) ## Auto-format all Go code
	$(GOLANGCI_LINT) fmt ./...

# buf.gen.yaml names the plugins without versions ("protoc-gen-go"). So we
# put version-free links to the pinned files in a fresh temporary folder,
# put that folder first on PATH for this one command, and delete the links
# and the folder afterwards.
generate: $(BUF) $(PROTOC_GEN_GO) $(PROTOC_GEN_GO_GRPC) ## Regenerate the Go code from proto/
	@plugins="$$(mktemp -d)" || exit 1; \
	ln -s $(PROTOC_GEN_GO) "$$plugins/protoc-gen-go" && \
	ln -s $(PROTOC_GEN_GO_GRPC) "$$plugins/protoc-gen-go-grpc" && \
	echo "buf generate" && PATH="$$plugins:$$PATH" $(BUF) generate; \
	status=$$?; \
	rm -f "$$plugins/protoc-gen-go" "$$plugins/protoc-gen-go-grpc"; rmdir "$$plugins"; \
	exit $$status

# CI runs this: if someone edits a .proto file but forgets `make generate`,
# the committed Go code no longer matches and this fails. `git status`
# also catches new files that were never committed.
generate-check: generate ## Fail if the committed generated code is out of date
	@git diff --exit-code -- $(GEN_DIR) && \
	test -z "$$(git status --porcelain -- $(GEN_DIR))" || \
	{ git status --short -- $(GEN_DIR); \
	  echo "error: generated code is out of date; run make generate and commit the result" >&2; \
	  exit 1; }

tools: $(TOOLS) ## Install the pinned developer tools into bin/tools

# Each tool is built from source by `go install` at the pinned version, then
# renamed to include that version.
$(GOLANGCI_LINT):
	@mkdir -p $(TOOLS_BIN)
	GOBIN=$(TOOLS_BIN) go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)
	mv $(TOOLS_BIN)/golangci-lint $@

$(BUF):
	@mkdir -p $(TOOLS_BIN)
	GOBIN=$(TOOLS_BIN) go install github.com/bufbuild/buf/cmd/buf@$(BUF_VERSION)
	mv $(TOOLS_BIN)/buf $@

$(PROTOC_GEN_GO):
	@mkdir -p $(TOOLS_BIN)
	GOBIN=$(TOOLS_BIN) go install google.golang.org/protobuf/cmd/protoc-gen-go@$(PROTOC_GEN_GO_VERSION)
	mv $(TOOLS_BIN)/protoc-gen-go $@

$(PROTOC_GEN_GO_GRPC):
	@mkdir -p $(TOOLS_BIN)
	GOBIN=$(TOOLS_BIN) go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@$(PROTOC_GEN_GO_GRPC_VERSION)
	mv $(TOOLS_BIN)/protoc-gen-go-grpc $@

firecracker: ## Download the pinned Firecracker + jailer (fingerprint-checked)
	./scripts/fetch-firecracker.sh

# The root disk contains bin/sandbox-agent, so build it first.
guest: build ## Download the guest kernel and build the guest root disk (no sudo)
	./guest/fetch-kernel.sh
	./guest/build-rootfs.sh

# One base snapshot per size class, in the state folder (guest/out/state,
# or $MICROVM_STATE_DIR). Each build cold-boots a VM, so it needs KVM.
snapshots: guest ## Build the base snapshots (small, medium) into guest/out/state (needs KVM)
	./bin/sandboxd build-snapshots

boot: ## Boot one microVM by hand through Firecracker's API, then destroy it
	./scripts/boot-vm.sh

demo: ## Live demo, step by step (press Enter between steps)
	./scripts/demo.sh

# They need KVM and the guest image, so they never run in GitHub CI (CI only
# checks that they compile, through `make lint`).
# Each test builds the snapshot it needs in its own state folder under
# guest/out/, so `make snapshots` is not needed first.
e2e: guest ## Boot and restore real microVMs and test the agent inside them (needs KVM)
	go test -tags e2e -count=1 -v ./test/e2e/...

# Jailer mode needs root, and nothing is ever built as root. So the test is
# compiled into one binary as you, against the snapshots `make snapshots`
# built as you, and only that binary runs with sudo. As root it creates
# only a jail folder in guest/out/state/jail/ and a cgroup, and removes both.
e2e-jailer: snapshots ## Test jailer mode on a real microVM: builds as you, then runs one test binary with sudo
	go test -c -tags 'e2e jailer' -o bin/e2e-jailer.test ./test/e2e
	sudo bin/e2e-jailer.test -test.run Jailer -test.v

bench: ## Speed benchmarks (not built yet: week 10)
	@echo "bench: skipped, nothing to run yet (arrives in week 10)"

clean: ## Delete everything `make` built
	rm -rf bin
