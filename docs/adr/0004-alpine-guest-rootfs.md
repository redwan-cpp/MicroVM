# ADR-0004: Alpine minirootfs for the guest root disk

- **Status:** Accepted
- **Date:** 2026-11-18

## Context

Every microVM boots from the same read-only root disk, `guest/out/rootfs.ext4`. The week-0 plan was to build it with **mmdebstrap** (a minimal Debian). In week 1 it turned out that mmdebstrap is not installed on the development machine, and installing it needs sudo.

What the guest needs:
- **A shell and basic tools**: the init script (`guest/init/demo-init`) is a shell script, and commands like `sh`, `ps`, `head` and `setsid` are used by the demo and the tests.
- **sandbox-agent**: a static Go binary ([ADR-0001](0001-go-for-all-components.md)), so the guest's C library does not matter to it.
- **Small and quick to build**: the disk is rebuilt often and copied into every snapshot class later.
- **Rootless**: files in a root filesystem must belong to uid 0, but the build must not need sudo.
- **Pinned and checked**, like every other download (Firecracker, the kernel).

## Decision

Build the root disk from **Alpine Linux's minirootfs** (3.24.4, a 3.7 MB tarball), pinned by SHA-256 from Alpine's own release list. `guest/build-rootfs.sh` unpacks it inside a **user namespace** (`unshare --map-root-user --map-auto`, where our user appears as uid 0), adds the init script and `bin/sandbox-agent`, and packs everything with `mkfs.ext4 -d`, which needs no mounting. The image is 64 MiB with about 35 MiB free. The VM mounts it read-only.

## Consequences

**Good:**
- No sudo and no extra packages on the host: `unshare`, `mkfs.ext4` and `curl` are enough.
- Tiny and fast: the whole build takes a few seconds; the guest reaches PID 1 about 70 ms after InstanceStart.
- busybox provides the tools the demo and the tests use (`sh`, `ps -o`, `setsid`, `head`).
- Nothing in the minirootfs starts on its own, so the guest runs only what we start.

**Bad:**
- Alpine uses musl, not glibc. Programs built for Debian or Ubuntu may not run in the guest. Our agent is static, so it is not affected, but user workloads could be.
- busybox tools differ in small ways from GNU ones (options, output formats).
- There is no package manager step yet. Adding Python (the README's planned example) means pinning Alpine packages (`.apk` files) with checksums too. Revisit this decision when the guest first needs packages.

**Alternatives rejected:**
- *mmdebstrap (Debian):* the original plan; needs sudo to install. Still a good choice if the guest needs Debian packages later.
- *Export a Docker image:* needs Docker on the host and its daemon running as root.
- *Firecracker's CI Ubuntu image:* much bigger, and not something we build or check ourselves.
- *Buildroot:* a fully custom minimal system, but long to learn and slow to build for this budget.
