# ADR-0005: Rootless mode and jailer mode, one snapshot for both

- **Status:** Accepted
- **Date:** 2026-11-18
- **Amends:** [ADR-0003](0003-snapshot-constraints.md), decision 2 ("one jailer chroot and one network namespace per clone")

## Context

ADR-0003 makes the Firecracker **jailer** a required part of every clone: a chroot, a UID drop and cgroups around each Firecracker. Week 3 showed three practical limits:

1. **The jailer must start as real root.** It creates the chroot, the device files inside it (`/dev/kvm`), the cgroup, and only then becomes an unprivileged user.
2. **On the development machine, sudo needs a password**, and nothing in this project may build or run Go as root. CI has no KVM at all.
3. So almost every test, and every day of development, has to run **without** the jailer.

What ADR-0003 really needs is narrower than "always the jailer": every clone of one snapshot must see the **same relative names** (`rootfs.ext4`, `scratch.ext4`, `mem.snap`, `vm.snap`, `v.sock`), each pointing at **its own files**.

The week-3 spike ([`test/e2e/spike_test.go`](../../test/e2e/spike_test.go)) checked what Firecracker v1.17.0 does with such names: it keeps relative paths exactly as written when it saves a snapshot, resolves them against its working directory when it loads one, and opens `mem.snap` and `vm.snap` read-only, so the store's files can be mode 0444.

## Decision

1. **Two modes, one snapshot.** Every snapshot is built with relative paths only. A clone runs Firecracker with a folder of its own as the working directory, and there those names are its own files:
   - **Rootless mode** (the default; all tests, `make e2e`): the folder is `<state>/runs/<id>/`. Firecracker runs as our own user.
   - **Jailer mode** (opt-in, needs root; `make e2e-jailer`): the folder is the jailer's chroot, `<state>/jail/firecracker/<id>/root/`, which becomes Firecracker's `/`. Firecracker runs as an unprivileged UID (70000) in its own cgroup v2 (`/sys/fs/cgroup/microvm/<id>`, with the size class's `cpu.max`, `memory.max` and `pids.max`).

   The snapshot does not know which mode restores it.
2. **One state folder on one filesystem.** `snapshots/`, `runs/` and `jail/` live under one folder (`MICROVM_STATE_DIR`, default `guest/out/state`), checked once to be on one filesystem, because hard links cannot cross filesystems.
3. **The VMM cannot change shared files.** Store files (`vm.snap`, `mem.snap`, `rootfs.ext4`) are mode 0444 and keep the owner who built them. In jailer mode they are hard-linked into the chroot unchanged; only the clone's own files (scratch disk, log) belong to the jailed user.
4. **No `--new-pid-ns` yet.** With it, the jailer forks and our process would lose Firecracker's PID. Without it, the jailer execs Firecracker in place. The jailer's change of user clears the parent-death signal, so jailer mode cleans up through the clone's cgroup (`cgroup.kill`), not `Pdeathsig`.
5. **The mode is always reported.** Every result that carries timings carries the mode (`rootless` or `jailer`), so rootless numbers are never shown as jailed ones. From week 4 the audit record of each run carries it too.
6. **Demo 1 (week 4) runs in jailer mode** on the presenter's machine, with a rootless fallback that is said out loud if the jailer cannot be used there.
7. **The escape suite (week 9) counts T9 results in jailer mode only** ([threat model](../threat-model.md)). A rootless run of a T9 test is reported, never counted as a pass.

## Consequences

**Good:**
- Everything except one test runs without root: building, unit tests, `make e2e`, CI.
- One snapshot per size class serves both modes, so there is nothing to keep in sync.
- Restores are fast in both modes: no copying, only hard links (about 15–20 ms rootless here).

**Bad:**
- **Rootless mode is weaker.** If code escapes the VM into Firecracker (T9), it runs as our user: it can read our home folder (T1), the base snapshots and the other clones' files (T3). Firecracker's own seccomp filters still apply, but there is no chroot, UID drop, namespace or cgroup around it. The threat model says so.
- The jailer test needs a person with sudo; it cannot run in CI.
- One fixed jailed UID for now: two jailed clones could signal each other. From week 4 each clone gets its own UID.

**Alternatives rejected:**
- *v1.17's `vsock_override` on LoadSnapshot and `PATCH /drives` after load:* they give each clone different **absolute** paths. Those paths would have to differ between rootless runs and the chroot, so a snapshot (or each restore) would depend on the mode, and they add API calls to every restore.
- *Always the jailer, tests with sudo:* every test run would need a password, and building as root is forbidden.
- *A user namespace instead of real root:* the jailer's cgroup and device-file steps need real privileges; moved to "Later" as rootless hardening.
- *Copying the memory file per clone:* breaks page sharing (ADR-0003, decision 3) and costs hundreds of milliseconds.

**Later:** rootless hardening (Landlock, a user namespace around Firecracker), and `--new-pid-ns` with pidfd-based process handling. Both are on the [build plan's](../build-plan.md) "Later" list.
