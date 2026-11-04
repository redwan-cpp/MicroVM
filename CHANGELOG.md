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
