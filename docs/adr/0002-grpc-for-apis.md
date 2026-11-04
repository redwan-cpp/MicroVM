# ADR-0002: gRPC for the CLI↔daemon and daemon↔agent APIs

- **Status:** Accepted
- **Date:** 2026-11-04

## Context

There are two internal APIs:
1. **`sandboxctl` → `sandboxd`** over a local Unix socket.
2. **`sandboxd` → `sandbox-agent`** over vsock. On the host side, this goes through Firecracker's Unix socket with a `CONNECT <port>\n` handshake.

Both need **streaming**: the user should see stdout and stderr while the command runs, not only at the end. Both also need typed messages, deadlines and cancellation (timeouts are a core feature).

## Decision

Use **gRPC with Protocol Buffers** for both APIs. The `.proto` files live in `proto/` and are generated with `buf`. `Exec` is a server-streaming RPC: it sends output chunks, then a final message with the exit status.

The transport is a plain `net.Conn` in both cases:
- CLI→daemon: `unix:///run/sandboxd/sandboxd.sock`. Access is controlled by file permissions (group `sandbox`).
- Daemon→agent: a custom dialer in `internal/vsockconn` that does the Firecracker `CONNECT` handshake, then gives the connection to gRPC.

## Consequences

**Good:**
- One technology for both APIs. Deadlines and cancellation travel with each call.
- Schemas are versioned and checked by `buf lint` and `buf breaking` in CI.
- This is how real systems are built: containerd uses gRPC, and Kata uses ttrpc, a lighter gRPC.

**Bad:**
- Generated code and a protoc/buf toolchain. Hidden by `make generate`.
- The gRPC dependency in the guest agent makes the binary bigger, about 10 MB static. That is fine for a disk image, and we can revisit with a hand-written framing protocol if size or startup time ever matter.

**Alternatives rejected:**
- *HTTP + JSON with chunked streaming:* simpler, but no types and we would build deadlines ourselves.
- *Our own length-prefixed framing:* the smallest guest binary, but we would reinvent streaming, errors and cancellation.
