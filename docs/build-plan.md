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
- [x] Toolchain: Go 1.27, build-essential, mmdebstrap, python3-venv, golangci-lint v2.15.0 (pinned in the Makefile)
- [x] `go.mod` (`github.com/redwan-cpp/MicroVM`), three program stubs, Makefile, CI workflow, LICENSE, CHANGELOG
- [x] Pinned Firecracker + jailer v1.17.0 with a two-level SHA-256 check (`make firecracker`)

### Week 1: boot a microVM by hand
- [ ] `guest/fetch-kernel.sh`: pinned guest kernel, SHA-256 checked
- [ ] `guest/build-rootfs.sh`: mmdebstrap → `mkfs.ext4 -d` (rootless)
- [ ] Boot the guest with `curl` on the Firecracker API socket, record every call in the learning doc
- [ ] Learning docs 05 (KVM), 06 (Firecracker and its API), 07 (reading Go code)

### Week 2: guest agent and VMM client
- [ ] buf + protoc-gen-go pinned in the Makefile (`make generate`)
- [ ] `proto/agent.proto`: Exec (streams stdout/stderr, then the exit code), Health, SyncTime, Reseed
- [ ] `cmd/sandbox-agent`: static binary, gRPC over vsock, started by the guest's init
- [ ] `internal/vsockconn`: host-side `CONNECT <port>` dialer
- [ ] `internal/vmm`: Firecracker REST client over a Unix socket
- [ ] Go integration test: cold boot → exec `echo hi` → shut down
- [ ] Learning doc 08 (booting a guest, vsock and the agent)

### Week 3: snapshots and the jailer
- [ ] `internal/snapshot`: build a base snapshot per size class, write `manifest.json`
- [ ] Restore a single clone and measure it
- [ ] Jailer integration: chroot per clone, UID drop, cgroup v2, hard-linked memory file
- [ ] Learning docs 09 (snapshots and copy-on-write) and 10 (the jailer)

### Week 4: **Demo 1**
- [ ] `proto/sandbox.proto` + `sandboxd` gRPC server on `/run/sandboxd/sandboxd.sock`
- [ ] Run state machine with timestamped transitions
- [ ] `sandboxctl run | ps | logs`
- [ ] Per-clone sparse scratch disk mounted at `/work`
- [ ] Destroy and cleanup; audit log v1 (JSONL)
- [ ] Demo script; tag `v0.1.0`; **freeze the feature list**
- [ ] Learning doc 11 (the daemon and the life of one run)

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
| Snapshot restore is slow or fails | Test in weeks 1–3 | Report cold-boot numbers only |
| Snapshot paths collide between clones | Designed for ([ADR-0003](adr/0003-snapshot-constraints.md)) | Restore clones one at a time |
| Networking takes too long | — | Keep networking off and say so in the report |
| Scope creep | Freeze after week 4 | "Later" list |
| Overclaiming security | [Threat model](threat-model.md), [SECURITY.md](../SECURITY.md) | State that it is not audited |

## Hour log

| Week | Hours | Notes |
|------|-------|-------|
| 0 | | |

## Later (after the freeze)
- Lazy memory loading with userfaultfd (UFFD) for faster restores
- Our own minimal guest kernel config
- Stage 2 multi-host prototype (worker registry and scheduler)
