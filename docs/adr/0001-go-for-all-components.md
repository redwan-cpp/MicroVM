# ADR-0001: Go for the daemon, the CLI and the guest agent

- **Status:** Accepted
- **Date:** 2026-11-04

## Context

The foundation doc chose Python 3 for `sandboxd` and `sandboxctl` ("fast to write and test, can move to Go later") and left Python vs Go vs Rust as an open question for the supervisor. The guest agent has to be a small binary that runs inside a minimal rootfs.

What matters for the choice:
- **The guest agent must be self-contained.** A Python agent means a Python interpreter in every guest image. That makes the image bigger and the boot and snapshot heavier.
- **The daemon does systems work:** Unix sockets, vsock, process supervision, netlink, cgroups and many concurrent VMs.
- **About 156 hours in total.** The language has to be learnable in the first two weeks.
- **Portfolio value.** The project should read like real infrastructure code.

## Decision

All three components are written in **Go**. Python stays for the **escape tests (pytest)** and the **benchmark scripts**, where it is the better tool.

## Consequences

**Good:**
- The guest agent is one static binary (`CGO_ENABLED=0`). The guest image needs no runtime.
- Goroutines and `context.Context` fit "many VMs, each with timeouts and cancellation".
- Strong ecosystem for this exact space: containerd, Kata, Firecracker's own Go SDK, `mdlayher/vsock`, `vishvananda/netlink`, `google/nftables`.
- Built-in race detector, fuzzing and benchmarks give industry-style testing for free.
- One language across the daemon, CLI and agent means shared protobuf types.

**Bad:**
- Go is new to the author. We budget learning time in weeks 0–2 and explain the Go idioms in the learning docs.
- More boilerplate than Python. The daemon will be a bit longer.

**Alternatives rejected:**
- *Python everywhere:* fastest start, but needs a heavy guest image and reads less like systems work.
- *Python daemon + Go agent:* two languages and two sets of tooling for little gain.
- *Rust:* matches Firecracker itself, but the learning curve is too steep for this budget.

We **do not** use `firecracker-go-sdk`. We write a thin REST client ourselves, so the API calls stay visible and can be learned. We may revisit this after Demo 1.
