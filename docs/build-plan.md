# Build plan

Week-by-week plan for the semester. Week 0 is the week of **2026-11-04**. Budget: about 12 h/week, about 156 h in total.
Tick items as they land. Each week ends with a commit, this file updated, and a learning doc in [`learn/`](learn/). The [learning guide](learn/README.md) gives the reading order.

> Weeks 0–1 include the **foundation docs (00–04)**, which assume zero background. Plan a few extra study hours in those weeks.

> **Feature freeze after week 4 (Demo 1).** New ideas go to the "Later" list at the bottom, not into the current week.

## Milestones

| Week | Milestone | Done when | Tag |
|------|-----------|-----------|-----|
| 4 | **Demo 1** | Three clones run at once from one snapshot, each with its own writes, and everything is cleaned up afterwards | `v0.1.0` |
| 8 | **Demo 2** | Networking is off by default and the policy changes the limits | `v0.2.0` |
| 9 | **Escape suite** | Every escape test is blocked or logged ([criteria](threat-model.md#escape-test-pass-criterion)) | `v0.3.0` |
| 10 | **Benchmarks** | p50/p99 restore table with a per-phase breakdown, plus clones per GB | `v0.4.0` |
| 13 | **Final** | Report and live demo | `v1.0.0` |

## Week by week

### Week 0: foundations
- [x] Check the host: KVM, nested virtualization, vhost-vsock, cgroup v2 (all present)
- [x] `git init`, repository skeleton, `.gitignore`
- [x] Build plan, ADR-0001 (Go), ADR-0002 (gRPC), ADR-0003 (snapshot constraints)
- [x] Threat model v0
- [x] Learning guide and foundation docs 00–04 (big picture, inside a computer, operating system, terminal, containers vs VMs)
- [x] Toolchain: Go 1.27, build-essential, python3-venv, golangci-lint v2.15.0 (pinned in the Makefile). mmdebstrap was planned but is not installed (installing it needs sudo), so week 1 builds the guest from Alpine instead
- [x] `go.mod` (`github.com/redwan-cpp/MicroVM`), three program stubs, Makefile, CI workflow, LICENSE, CHANGELOG
- [x] Pinned Firecracker + jailer v1.17.0 with a two-level SHA-256 check (`make firecracker`)

### Week 1: boot a microVM by hand
- [x] `guest/fetch-kernel.sh`: pinned guest kernel (Firecracker CI 6.1.155), SHA-256 pinned on first download
- [x] `guest/build-rootfs.sh`: Alpine minirootfs (SHA-256 checked) → `mkfs.ext4 -d` inside a user namespace (rootless). Alpine instead of mmdebstrap for now: mmdebstrap isn't installed on the dev machine and installing it needs sudo. Revisit if the guest needs Debian packages.
- [x] Boot the guest with `curl` on the Firecracker API socket: `scripts/boot-vm.sh` (`make boot`) prints every call
- [x] Write those calls up in learning doc 06 (done in week 2: [`learn/06-firecracker-api.md`](learn/06-firecracker-api.md))
- [x] `make demo`: step-by-step live demo for presentations (`scripts/demo.sh`)
- [ ] Learning docs 05 (KVM) and 07 (reading Go code): moved to a separate docs pass, not tied to a code week (06 is done, see above)

### Week 2: guest agent and VMM client
- [x] buf, protoc-gen-go and protoc-gen-go-grpc pinned in the Makefile (`make generate`; CI runs `make generate-check`)
- [x] `proto/sandbox/agent/v1/agent.proto`: Exec (streams stdout/stderr, then the exit code), Health, SyncTime, Reseed
- [x] `cmd/sandbox-agent` + `internal/agent`: static binary, gRPC over vsock port 1024, started by the guest's init (`sandbox_mode=agent`)
- [x] `internal/vsockconn`: host-side `CONNECT <port>` dialer
- [x] `internal/agentclient`: host-side client that enforces deadlines and output caps itself and trusts nothing the agent reports
- [x] `internal/vmm`: Firecracker REST client over a Unix socket, plus the Firecracker process (run folder, console ring buffer, cleanup)
- [x] Go integration test: cold boot → exec `echo hi` → shut down (`test/e2e`, `make e2e`; agent ready about 85 ms after InstanceStart)
- [x] Learning doc 08 (booting a guest, vsock and the agent); [ADR-0004](adr/0004-alpine-guest-rootfs.md) (Alpine guest root disk)
- [x] Week-1 review follow-ups: `scripts/boot-vm.sh` cleans guest console output, private `mktemp -d` run folder, curl timeouts

### Week 3: snapshots and the jailer
- [x] Spike (`test/e2e/spike_test.go`): Firecracker v1.17 keeps relative paths in snapshots, restores from 0444 store files, and VMGenID reseeds the guest kernel on restore
- [x] One state folder on one filesystem (`internal/state`, `MICROVM_STATE_DIR`, default `guest/out/state`): `snapshots/`, `runs/`, `jail/`, checked for hard links, socket-path length and (jailer mode) nodev
- [x] `internal/snapshot`: build a base snapshot per size class (small, medium), versioned store with a `current` symlink, files 0444, `manifest.json` with hashes checked once when the store is opened; `make snapshots`
- [x] Restore a single clone and measure it (`internal/clone`): hard links, LoadSnapshot, agent, de-twin (SyncTime + Reseed); about 15–20 ms in rootless mode against an 80 ms cold boot (InstanceStart → agent ready)
- [x] Jailer integration (`internal/jailer`, jailer mode in `internal/clone`): chroot per clone, UID drop (70000), cgroup v2 limits per size class, hard-linked read-only memory file, `cgroup.kill` cleanup. Two modes, one snapshot: [ADR-0005](adr/0005-rootless-and-jailer-modes.md)
  - Verified by **one manual root run** on the dev machine on 2026-11-18 (`sudo bin/e2e-jailer.test -test.run Jailer -test.v`, built from 1eb5fd7): `TestJailerRestore` and all four subtests passed, and no jail folder, `microvm` cgroup or root-owned file was left afterwards. Jailer-mode restore: prepare 0.4 ms, process start 36.6 ms, LoadSnapshot 7.2 ms, agent ready 6.0 ms, de-twin 1.2 ms, total 51.4 ms (rootless, same machine: process start about 1.4 ms, total about 15–20 ms; the jailer's own start is the difference).
  - The root run is manual and never in CI. Repeated after the review fixes (5c248e3, be7943e) on 2026-11-24, several times, with the week-4 binaries (built from 9245d10): `TestJailerRestore` passed every time with all subtests, the `Groups:` check included; jailed restore totals 28.6–60.5 ms across the runs (latest, in `guest/out/jailer-test.log`: 37.6 ms, of which process start 22.6 ms).
- [x] `buf breaking` in `make lint` and CI against `origin/main` (`.git#branch=origin/main`, checkout with `fetch-depth: 0`)
- [x] Learning docs 09 (snapshots and copy-on-write) and 10 (the jailer); threat model: T9 counted in jailer mode only
- [x] Week-2 review follow-ups: CI tools cache keyed on `go.mod` too, exit code (-1..255) and signal (0..64) checks in `agentclient`, Firecracker's log in the run folder under the state folder, a `LockOSThread` warning on `vmm.Start`

### Week 4: **Demo 1**
- [x] `proto/sandbox/daemon/v1/sandbox.proto` (Run streams state changes, output and one result; List, Logs, Get) + `sandboxd serve` (`internal/daemon`) on `$XDG_RUNTIME_DIR/sandboxd/sandboxd.sock` rootless, `/run/sandboxd/sandboxd.sock` in jailer mode ([ADR-0006](adr/0006-daemon-socket-and-state-ownership.md)); caller UID from `SO_PEERCRED`; one daemon per state folder (`<state>/lock`); at most `--max-concurrent` clones (default 4, at most 32) and 64 waiting runs; graceful shutdown within 10 s
- [x] Run state machine with timestamped transitions (`internal/run`): PENDING → RESTORING → MOUNTING → RUNNING → STOPPING → DONE/FAILED; every run ends with STOPPING, its clone destroyed exactly once; outcomes judged by the host only
- [x] `sandboxctl run | ps | logs | audit verify` (exit codes: the command's own, 124 timeout, 123 output cap, 125 failure)
- [x] Per-clone sparse scratch disk mounted at `/work`: a formatted ext4 template per class in the store (`make snapshots`, rootless `mkfs.ext4 -d`), copied per clone keeping holes (about 0.3 ms); the agent drops cached placeholder blocks (`BLKFLSBUF`) and mounts it `nodev,nosuid` (MountScratch, additive to agent/v1)
- [x] Destroy and cleanup; audit log v1 (`internal/audit`): hash-chained JSONL, verified at start, fails closed (a broken chain stops the start, a failed write stops admissions; `sandboxd rotate-audit` is the way out)
- [x] Firecracker's log bounded: a FIFO drained into a 64 KiB ring buffer (week-3 risk); one jailed UID per clone, 70000 + slot (ADR-0005)
- [x] Demo script (`make demo1`, `scripts/demo1.sh [--jailer]`) and Demo 1 as a test (`make e2e`; jailer mode in `make e2e-jailer`). On the dev machine, rootless: three clones at once in about 0.65 s wall time, each restore about 18–23 ms, mount about 4 ms, destroy 50–140 ms (Firecracker's own exit, three at once)
- [x] The root run of `make e2e-jailer` (now also Demo 1 through a jailer-mode daemon), by the user on 2026-11-24, several times, binaries built from 9245d10: `TestJailerRestore` (all subtests) and `TestJailerDemo1` passed every time: three jailed clones as UIDs 70000-70002, each in its own cgroup, all exit 0, about 2.2 s wall time for three 2 s commands, per-clone restores 40–48 ms (latest log: 46.7–47.4 ms, destroy 59–127 ms), no jail, cgroup or root-owned file left
- [ ] `scripts/demo1.sh --jailer` end to end, by the user (it cannot run here); then tag `v0.1.0` (the user pushes tags)
- [x] **Freeze the feature list**: new ideas go to "Later" below
- [x] Learning doc 11 (the daemon and the life of one run)

### Week 5: policy
- [ ] Policy schema v1 (YAML, deny by default), validation with clear errors
- [ ] Size classes, cgroup CPU and memory limits, timeout, output cap, pid limit
- [ ] Learning doc 12 (policies)

### Week 6: networking, part 1
- [ ] Network namespace per clone, `tap0`, veth pair
- [ ] Default: no route, plus an nftables drop
- [ ] Learning docs 13 (networking from zero) and 14 (Linux networking: tap, netns, veth, nftables)

### Week 7: networking, part 2, and robustness
- [ ] Egress allowlist: hostnames resolved on the host and pinned to IPs
- [ ] Reconciler: remove orphaned chroots, taps, cgroups and rules when the daemon starts
- [ ] `sandboxd.toml` config and systemd unit
- [ ] Learning doc 15 (crash recovery and cleanup)

### Week 8: **Demo 2**
- [ ] Demo script: network off by default, allowlist policy, limits visible in the guest
- [ ] Tag `v0.2.0`

### Week 9: **Escape suite**
- [ ] `tests/escape/` pytest suite covering every category in the threat model
- [ ] Fix what it finds; threat model v1
- [ ] Tag `v0.3.0`; learning doc 16 (threat models and escape tests)

### Week 10: **Benchmarks**
- [ ] `bench/restore.py`: p50/p99 with a per-phase breakdown, cold boot vs restore
- [ ] Clones per GB from PSS (`/proc/<pid>/smaps_rollup`); concurrency sweep
- [ ] `docs/benchmarks.md` table and charts; tag `v0.4.0`; learning doc 17 (benchmarks)

### Week 11: hardening
- [ ] Prometheus metrics, Go fuzzing of the policy parser, graceful shutdown, lint-clean
- [ ] Learning doc 18 (production-quality Go)

### Week 12: design and presentation
- [ ] `docs/design/multi-host.md`: Stage 2 on paper (worker registry, memory-aware scheduler)
- [ ] README: diagrams, quickstart, recorded demo, benchmark table

### Week 13: **Final**
- [ ] Final report and demo; tag `v1.0.0`

## Risk register

| Risk | Status | Fallback |
|------|--------|----------|
| KVM missing on the host | **Closed (wk 0):** KVM, nested virtualization and vhost-vsock all work | — |
| Snapshot restore is slow or fails | **Closed (wk 3):** a restore takes about 15–20 ms, rootless | Report cold-boot numbers only |
| Snapshot paths collide between clones | **Closed (wk 3):** relative paths in every snapshot, one folder per clone ([ADR-0005](adr/0005-rootless-and-jailer-modes.md)) | Restore clones one at a time |
| Jailer needs root on the demo machine | Open, mostly retired: the week-4 jailer code (per-clone UIDs, the log pipe, the scratch copy, the jailer-mode daemon) passed several manual root runs of `make e2e-jailer` on the dev machine on 2026-11-24 (restore 28.6–60.5 ms; Demo 1 with three jailed UIDs). It is not in CI, and `scripts/demo1.sh --jailer` has not been run end to end yet: do that before Demo 1 | Rootless mode, said out loud: `scripts/demo1.sh` prints "rootless mode (fallback)" ([ADR-0005](adr/0005-rootless-and-jailer-modes.md)) |
| Networking takes too long | — | Keep networking off and say so in the report |
| Scope creep | Freeze after week 4 | "Later" list |
| Overclaiming security | [Threat model](threat-model.md), [SECURITY.md](../SECURITY.md) | State that it is not audited |

## Hour log

| Week | Hours | Notes |
|------|-------|-------|
| 0 | | |

## Later (after the freeze)
- Anchor the audit log's newest hash outside the file (for example in a second file root keeps, or a remote log), so removing the newest records or rewriting the whole chain is detected too (threat model T11)
- Keep `sandboxctl ps` and `logs` history across daemon restarts (today: the last 100 runs, in memory)
- Shorter clone teardown: Firecracker's own exit takes about 60–100 ms per clone (kernel teardown of guest memory), which is most of a short run's STOPPING
- A `sandbox` group and the jailer-mode socket set up by an installer or the systemd unit (week 7); today `--socket-group` names one
- Rootless hardening: Landlock and a user namespace around Firecracker, so rootless mode gets some of jailer mode's confinement
- Jailer `--new-pid-ns`, with pidfd-based process handling (the jailer forks, so the PID changes)
- Lazy memory loading with userfaultfd (UFFD) for faster restores
- Our own minimal guest kernel config
- Stage 2 multi-host prototype (worker registry and scheduler)
