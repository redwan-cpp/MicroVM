# ADR-0006: Daemon socket and state ownership

- **Status:** Accepted
- **Date:** 2026-11-24
- **Amends:** [ADR-0002](0002-grpc-for-apis.md) (where the CLI↔daemon socket lives)

## Context

ADR-0002 put the daemon's socket at `/run/sandboxd/sandboxd.sock`, with access controlled by the group `sandbox`. Week 4 builds the daemon, and two facts from ADR-0005 change the picture:

1. **Most runs are rootless.** Every test, `make e2e` and the rootless Demo 1 run `sandboxd` as the user. A user cannot create `/run/sandboxd/`, and should not need to.
2. **Jailer mode runs the daemon as root on the user's state folder.** The snapshot store, built by the user with `make snapshots`, lives in `guest/out/state`, and the jailer-mode daemon restores from it. Anything root creates there would be root's, and the user could no longer verify the audit log, prune snapshots or go back to rootless mode without sudo.

## Decision

1. **Two socket locations, one per mode.**
   - **Rootless:** `$XDG_RUNTIME_DIR/sandboxd/sandboxd.sock`. `XDG_RUNTIME_DIR` (usually `/run/user/<uid>`) is the user's own folder for sockets: mode 0700, on a memory filesystem, emptied at logout. The daemon makes `sandboxd/` mode 0700 and the socket 0600: only the user can connect. If `XDG_RUNTIME_DIR` is not set, the daemon refuses to start and asks for `--socket`.
   - **Jailer mode:** `/run/sandboxd/sandboxd.sock`, folder 0750 and socket 0660, both owned by root and the group `sandbox` if it exists (or the group given with `--socket-group`). Without such a group, the folder is 0700 and the socket 0600: root only.
   - `sandboxctl` finds the daemon through `--socket`, then `$SANDBOXD_SOCKET`, then the rootless socket if it exists, then the jailer-mode one.
2. **One daemon per state folder.** The daemon holds an exclusive `flock` on `<state>/lock` for as long as it runs, so a second daemon on the same state folder refuses to start, whatever its socket. On its socket, a live daemon is never pushed aside (the new one refuses), and a stale socket (nobody answers) is removed.
3. **The state folder stays its owner's.** When a root daemon creates anything outside `jail/` (the `audit/` folder and its log, the lock file), it gives it to the state folder's owner. Only the jailer's own chroots under `jail/` belong to root and the jailed UIDs, and the daemon removes each one when its clone is destroyed. `make e2e-jailer` checks that no root-owned file appears anywhere else under the state folder.
4. **Who called is recorded, not claimed.** The daemon reads the caller's UID from the socket (`SO_PEERCRED`), never from the request, and writes it into the audit record and `sandboxctl ps`.
5. **Root needs jailer mode.** A daemon started as root without `--jailer` refuses to start: rootless mode as root would run every Firecracker as root with nothing around it.

## Consequences

**Good:**
- Rootless mode needs no setup at all: no group, no folder in `/run`, no sudo.
- A user can switch between rootless and jailer mode on the same state folder, and verify the audit log as themselves, after a jailer-mode run too.
- Socket permissions are the access control, as ADR-0002 intended, and the audit log names the real caller.

**Bad:**
- Two locations to know about; `sandboxctl` guesses in a fixed order, and `--socket` overrides it.
- The `sandbox` group is not created by anything in this project. Without it, the jailer-mode daemon is root-only unless `--socket-group` names a group (`scripts/demo1.sh --jailer` passes the user's own group).
- In jailer mode the audit log belongs to the user, so the user can rewrite it. The hash chain detects edits inside the chain, not a full rewrite ([threat model](../threat-model.md), T11); anchoring the newest hash elsewhere is on the "Later" list.

**Alternatives rejected:**
- *Always `/run/sandboxd/`:* rootless mode would need root to create the folder, or a systemd unit (week 7) to do it.
- *A socket inside the state folder:* the state folder may be on a path too long for a Unix socket address (107 bytes), and a jailer-mode socket there would be a root-owned file in the user's folder.
- *A second state folder for jailer mode:* every snapshot would have to be built twice, once by root, which breaks "nothing is built as root".
