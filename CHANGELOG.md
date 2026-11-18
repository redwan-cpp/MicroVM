# Changelog

All notable changes to this project are listed here.
The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and versions follow [Semantic Versioning](https://semver.org/).

## [Unreleased]

### Added
- Project foundations: build plan, ADRs 0001–0003, threat model v0, `SECURITY.md`.
- Learning guide and beginner foundation docs 00–04.
- Go module `github.com/redwan-cpp/MicroVM` with stubs for `sandboxd`, `sandboxctl` and `sandbox-agent`. So far they only print their version.
- Makefile (`build`, `test`, `lint`, `fmt`), pinned golangci-lint, GitHub Actions CI, Apache-2.0 license.
- `make firecracker`: downloads Firecracker + jailer v1.17.0 and checks them against a pinned SHA-256 and against the release's own `SHA256SUMS`.
- `make guest`: pinned guest kernel 6.1.155 from Firecracker's CI (SHA-256 pinned on first download) and a rootless Alpine 3.24.4 root disk built with `mkfs.ext4 -d` inside a user namespace.
- `make boot`: boots one microVM through Firecracker's REST API with `curl`, runs one command, then destroys the VM and its run folder. Read-only root disk, no network card.
- `make demo`: step-by-step live demo for presentations.
- The host-agent API, `proto/sandbox/agent/v1/agent.proto` (Health, Exec, SyncTime, Reseed), with `make generate` (pinned buf v1.75.0, protoc-gen-go v1.36.12, protoc-gen-go-grpc v1.6.2) and `make generate-check` in CI.
- `sandbox-agent` (`internal/agent`): runs inside the guest and serves the API on vsock port 1024. Clean environment, fixed PATH, timeout and output cap, process-group kill, clock sync and RNG reseed.
- `internal/vsockconn`: the host side of Firecracker's vsock `CONNECT` handshake.
- `internal/agentclient`: the host-side client. It enforces deadlines, the output cap and the message size itself, and treats what the agent reports as untrusted.
- `internal/vmm`: Firecracker REST client and process manager (private run folder, console ring buffer, cleanup).
- `make e2e`: boots a real microVM, talks to the agent over vsock, and destroys the VM (needs KVM; not run in GitHub CI).
- Learning docs 06 (Firecracker and its API) and 08 (the guest agent and vsock); ADR-0004 (Alpine guest root disk).

### Changed
- The guest root disk (64 MiB) now contains `sandbox-agent`; `make guest` builds it first. The init script starts the agent when the kernel command line says `sandbox_mode=agent`.
- `scripts/boot-vm.sh` removes control characters from guest console lines, accepts only a 1-3 digit exit-code marker, uses a private `mktemp -d` run folder and gives every API call a 5 s timeout.
