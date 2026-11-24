# Threat model (v0, 2026-11-04; updated 2026-11-18 for the two modes of ADR-0005)

> **Status: draft.** v1 is due in week 9, together with the escape suite. This system has **not been security audited**. It is a research and teaching project, not a production multi-tenant service.

## What we protect

| Asset | Why it matters |
|-------|----------------|
| Host filesystem and secrets | The main thing untrusted code must never read or change |
| Host network and LAN | Sandboxed code must not reach the host, the LAN or the cloud metadata service (`169.254.169.254`) unless the policy allows it |
| Other sandboxes | A clone must not see another clone's files, memory or output |
| Host availability | One run must not exhaust the host's CPU, memory, disk or process table |
| Audit log integrity | Runs must be reviewable afterwards, and tampering should be detectable |

## Who the attacker is

**The attacker fully controls the code inside the guest**, including root inside the VM. The command is untrusted. It may be malicious or AI-generated, and it may be deliberately hostile to the guest agent, for example killing it or flooding its output.

**Trusted:** the host kernel, KVM, the user running `sandboxctl` (a member of group `sandbox`), and `sandboxd`.

## Trust boundaries

```
 untrusted                         │ boundary 1 │  semi-trusted          │ boundary 2 │  trusted
 guest code (root in VM) ── KVM + Firecracker ── firecracker process ── jailer: chroot, uid, ── host / sandboxd
 sandbox-agent (in guest)    virtio devices        (unprivileged)          netns, cgroups, seccomp
```

1. **Guest → VMM.** Hardware virtualization (KVM) and Firecracker's small device model: virtio-block, virtio-net, virtio-vsock, serial. This is the main isolation boundary. Its strength comes from KVM and Firecracker, which we do not modify.
2. **VMM → host.** If boundary 1 fails, the attacker lands inside a Firecracker process. In **jailer mode** the jailer limits it: own chroot, non-root UID, own mount namespace (PID and network namespaces follow in later weeks), cgroup limits, and Firecracker's built-in seccomp filters. In **rootless mode** only the seccomp filters remain (see below).
3. **Agent → daemon.** The daemon treats **everything the agent sends as untrusted input**. It caps output size, enforces deadlines, and never runs anything the agent asks for.
4. **CLI → daemon.** Access to the Unix socket is controlled by file permissions. Policies are validated, and invalid or unknown fields are rejected (deny by default).

## Threats and planned mitigations

| # | Threat | Mitigation | Escape test (wk 9) |
|---|--------|------------|--------------------|
| T1 | Read or write host files | No host directories are shared into the guest. Rootfs is read-only; the scratch disk is a fresh per-run file | Filesystem |
| T2 | Data left behind between runs | A new clone and a new scratch disk every run; both destroyed afterwards | Filesystem |
| T3 | Cross-clone data leak | Separate scratch files. Memory pages are shared only read-only, and copy-on-write gives each clone private copies of pages it writes | Cross-clone |
| T4 | Network access when it is off | Clone in its own netns with no route, plus an nftables drop | Network (off) |
| T5 | Reaching non-allowlisted hosts | Egress-only allowlist of IPs pinned at run start, so DNS rebinding can't widen it | Network (allowlist) |
| T6 | Resource exhaustion (fork bomb, memory hog, disk fill, CPU spin) | Size class + cgroup v2 `cpu.max`/`memory.max`/`pids.max`, fixed scratch size, wall-clock timeout, output cap | Resources |
| T7 | Hostile agent behavior (killing it, flooding it, malformed messages) | Deadlines on every call, output caps, the run ends and the VM is destroyed if the agent disappears | Host interface |
| T8 | Guest-initiated vsock connections to the host | No host listeners on the vsock socket path; connection attempts fail | Host interface |
| T9 | VMM escape, then host takeover | **Jailer mode only:** chroot, UID drop, namespaces, seccomp, cgroups. Rootless mode has seccomp alone (see "The two modes") | Process confinement (counted in jailer mode only) |
| T10 | Predictable randomness in clones (same RNG state) | Agent reseeds the RNG on every restore, and the restore fails if it cannot. VMGenID works too with Firecracker v1.17 and kernel 6.1.155: the guest kernel reseeds itself on restore (week-3 spike) | Cross-clone |
| T11 | Tampering with the audit log | Append-only JSONL with a hash chain: each record includes the SHA-256 of the previous one | Audit check |
| T12 | Leftover resources after a crash (taps, cgroups, rules, chroots) | Cleanup is idempotent; in jailer mode it kills the clone's whole cgroup (`cgroup.kill`), since the parent-death signal does not survive the jailer's change of user. Reconciler runs on daemon start (week 7) | Reconciler test |

## The two modes (ADR-0005)

Clones run in one of two modes, and every result says which ([ADR-0005](adr/0005-rootless-and-jailer-modes.md)):

- **Jailer mode** (needs root): each Firecracker runs in its own chroot, as an unprivileged UID that owns nothing on the host, in its own cgroup with the size class's limits. The snapshot files in its chroot are read-only hard links it cannot change. This is the mode the threat model describes, and the only one in which T9 escape tests count.
- **Rootless mode** (the default for development and all automated tests): Firecracker runs as the user who started it. Its seccomp filters still apply, but there is **no chroot, no UID drop, no namespaces and no cgroups**. Said plainly: code that escapes the VM into Firecracker (T9) can read and write whatever that user can, including their home folder and its secrets (T1), the base snapshots in the store, and other clones' run folders (T3). The store files are mode 0444, but their owner can change that. Use rootless mode only for code you would also run as yourself.

## Escape-test pass criterion

A test **passes** when all three hold:
1. **The attempt fails inside the guest.** It gets an error, a timeout or a kill.
2. **The host is unchanged.** No new files outside the run's directory, no leftover devices or processes.
3. **The audit record for the run shows the outcome**, for example `timed_out`, `oom_killed` or `exit_code`.

## Out of scope

- Finding or defending against KVM, Firecracker or host-kernel zero-days. We rely on upstream for those and track their versions.
- Side channels (Spectre-class, cache timing), beyond following Firecracker's production host setup guidance where practical.
- Multi-tenant production use, a security audit, and macOS or Windows hosts.
- A malicious user with access to `sandboxctl`. They are trusted to submit work, though their policies are still validated.
