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
