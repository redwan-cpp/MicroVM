# 10 · The jailer

> ⏱ 15 minutes
> **After this, you can explain:** what Firecracker's jailer does before a VM runs, what it protects if code ever escapes the VM, and why we never build anything as root.

---

## 1. A short story

The mini-office (the VM) is very strong. But suppose a stranger finds a crack in its wall and climbs out. Where do they land?

Without a guard, they land in the warehouse with **our** keycard: every room we can open, they can open. With a guard, they land in a locked cupboard that holds only this one office's papers, wearing a visitor's badge that opens nothing, with a timer that limits how much electricity they may use.

The **jailer** is that guard. It takes Firecracker's keycard and locks it in **before** the VM starts.

## 2. Two modes

| | Rootless mode | Jailer mode |
|---|---|---|
| Who starts it | you | root (the jailer needs it) |
| Firecracker runs as | you | UID 70000, which owns nothing |
| Its "/" | the whole host | its own chroot folder |
| Limits | none | its own cgroup: CPU, memory, processes |
| Used by | every test, development | the jailer test, Demo 1 |

Both restore the **same** snapshot ([ADR-0005](../adr/0005-rootless-and-jailer-modes.md)): the snapshot uses relative names, and in jailer mode the chroot is simply the folder where those names live. Every timing says which mode it was measured in.

## 3. What the jailer does, step by step

We start `jailer --id r-… --exec-file …/firecracker --uid 70000 --gid 70000 --chroot-base-dir guest/out/state/jail --cgroup-version 2 --parent-cgroup microvm --cgroup cpu.max=… -- --api-sock api.sock …` as root. Then it:

1. **Cleans itself up**: closes every inherited file except stdin, stdout and stderr (our console pipe), and empties its environment.
2. **Copies Firecracker** into the chroot, `jail/firecracker/<id>/root/firecracker`.
3. **Makes a cgroup**, `/sys/fs/cgroup/microvm/<id>`, writes our limits into it and moves itself in. Everything it becomes stays inside.
4. **Changes root**: a new mount namespace, then `pivot_root` into the chroot folder. From now on `/` is that folder; the rest of the host is out of sight.
5. **Gives the chroot to UID 70000** (mode 0700, with its own `/dev` and `/run`), so Firecracker can create its sockets there.
6. **Creates device files** inside: `/dev/kvm` (to run VMs), `/dev/net/tun`, `/dev/urandom`. That is why the state folder must not be on a filesystem mounted `nodev`: there, device files cannot be opened. `state.Open` checks that in jailer mode.
7. **Becomes UID 70000 and execs Firecracker.** Same process, same PID, no root left.

Before step 1, our code (`internal/clone`) has already put the VM's files in the chroot: **hard links** to the store's `vm.snap`, `mem.snap` and `rootfs.ext4`, and a fresh scratch disk.

## 4. The VMM cannot change shared files

The store's files are shared by every clone. If one jailed Firecracker could write `mem.snap`, the next clone would start from poisoned memory. So:

- store files stay owned by **you** (who ran `make snapshots`) and are mode **0444**: UID 70000 may read them, never write or `chmod` them;
- only the clone's own files, the scratch disk and the log, belong to UID 70000.

UID 70000 is chosen so that **nobody else has it**: no user, no group, outside systemd's range for temporary service users (61184–65519), Debian's on-demand system users (60000–64999) and the subordinate IDs that rootless containers use (from 100000, see `/etc/subuid`). The code checks those files and refuses an ID that is taken. Since week 4 each running clone gets a UID of its own, 70000 to 70031 (one per daemon slot), so two jailed clones cannot signal each other; `sandboxd` checks the whole range when it starts, and a UID whose clone could not be cleaned up is not used again until the daemon restarts.

## 5. cgroups: the electricity timer

A **cgroup** (control group) is a set of processes the kernel limits together. Ours come from the size class:

| File | small | What it limits |
|------|-------|----------------|
| `cpu.max` | `100000 100000` | 100 ms of CPU time per 100 ms: one full CPU |
| `memory.max` | 320 MiB | guest memory (256) plus room for Firecracker |
| `pids.max` | 16 | processes and threads: Firecracker needs a handful |

The cgroup also makes cleanup reliable. Writing `1` to its `cgroup.kill` kills every process inside at once. We need that: a process normally dies with its parent thanks to the *parent-death signal*, but Linux forgets that setting when a process changes its user, as the jailer does in step 7. So `Destroy` in jailer mode kills the cgroup, waits, deletes the chroot and removes the cgroup.

## 6. Why no new PID namespace yet

The jailer can also put Firecracker in its own **PID namespace** (`--new-pid-ns`), so it cannot even see other processes. But to do that the jailer *forks*: Firecracker gets a new PID, and the jailer exits. Our process manager (`internal/vmm`) waits on the PID it started, so it would think the VM had ended. Doing this right means finding the new PID safely (with a *pidfd*), which is on the "Later" list. Until then the jailer execs Firecracker in place.

## 7. Why builds never run as root

`sudo make …` or `sudo go test …` would run the compiler, every test helper and every downloaded module **as root**, and leave root-owned files in the repository. A bug, or a poisoned dependency, would get the whole machine.

So only one thing ever runs as root: a test binary built by you.

```bash
make snapshots                                                     # as you
go test -c -tags 'e2e jailer' -o bin/e2e-jailer.test ./test/e2e    # as you
sudo bin/e2e-jailer.test -test.run Jailer -test.v                  # as root, this binary only
```

(`make e2e-jailer` does exactly these three.) As root, the test creates only the jail folder and the cgroup, and removes both. If a cleanup fails, it prints the `sudo rm -rf …` / `sudo rmdir …` to finish by hand.

Firecracker's own log is not a file either (since week 4): it is a named pipe in the chroot, given to the jailed user, which the host reads into a 64 KiB ring buffer like the guest console, so it can never fill the disk.

## 8. Try it

```bash
third_party/firecracker/jailer --help | head -20
```
👀 The options we use: `--id`, `--exec-file`, `--uid`, `--gid`, `--chroot-base-dir`, `--cgroup-version`, `--parent-cgroup`, `--cgroup`.

```bash
cat /sys/fs/cgroup/cgroup.controllers
```
👀 The controllers this machine offers; `cpu`, `memory` and `pids` must be there.

```bash
make e2e-jailer
```
👀 Asks for your sudo password once, restores one clone in jailer mode and shows its UID (70000), its cgroup limits and its timings; then runs Demo 1 through a jailer-mode daemon: three clones at once, as three different UIDs. (Needs KVM and sudo.)

## 9. In our project

| Piece | Where |
|-------|-------|
| The jailer's command line, checked | `internal/jailer` |
| Jailer mode: chroot, links, cleanup | `internal/clone` (`jail.go`) |
| The nodev and one-filesystem checks | `internal/state` |
| The test, run with sudo | `test/e2e/jailer_test.go` |
| Why two modes | [ADR-0005](../adr/0005-rootless-and-jailer-modes.md), [threat model](../threat-model.md) (T9) |

## 10. Check yourself

<details><summary>1. Code escapes the VM in rootless mode. What can it reach?</summary>

Everything your user can: your home folder, the snapshot store, other clones' files. Only Firecracker's seccomp filters limit it. In jailer mode it would be UID 70000 in a chroot with read-only snapshot files and a cgroup.
</details>

<details><summary>2. Why are the store files owned by you and not by the jailed UID?</summary>

An owner can `chmod` a file and then write it. The jailed UID must only read the shared files, so it must not own them.
</details>

<details><summary>3. Why does Destroy write to cgroup.kill instead of relying on the parent-death signal?</summary>

The kernel clears the parent-death signal when a process changes its user, which the jailer does. The cgroup still holds everything Firecracker started, and `cgroup.kill` ends it all at once.
</details>

## 11. New words

| Word | Meaning |
|------|---------|
| Jailer | Firecracker's helper that locks Firecracker in before the VM runs |
| chroot / pivot_root | Make a folder the process's "/", so it cannot see the rest |
| UID drop | Switch from root to an unprivileged user ID |
| FIFO | A named pipe: a file name that one process writes into and another reads from |
| cgroup | A group of processes the kernel limits and can kill together |
| nodev | A mount option that forbids using device files on that filesystem |
| PID namespace | A separate process-ID world: processes inside see only each other |
