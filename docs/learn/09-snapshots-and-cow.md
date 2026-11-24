# 09 · Snapshots and copy-on-write

> ⏱ 15 minutes
> **After this, you can explain:** what a Firecracker snapshot holds, how many clones share one copy of its memory, and why every clone must be "de-twinned" right after it wakes up.

---

## 1. A short story

A video game lets you **save** just before the hard level. Every time you fail, you load the save: same room, same health, same enemies, in a second instead of replaying the whole game.

A booted microVM with its agent running is our "just before the hard level". Booting it takes about 80 ms of kernel work. Loading a save of it takes about 15 ms. And because loading never changes the save, a thousand runs can all start from the same one.

There is one catch: every loaded copy also wakes up with the **same clock** and the **same dice**. We have to fix that before anyone plays.

## 2. What a snapshot holds

`make snapshots` boots one VM per size class, waits until the agent answers, **pauses** it and asks Firecracker to save it (`PUT /snapshot/create`). Two files come out:

| File | What is in it | Size (small) |
|------|---------------|--------------|
| `vm.snap` | The CPU registers, the state of every virtual device, and the **device configuration**, including every path we gave Firecracker | about 14 KiB |
| `mem.snap` | All of guest memory, byte for byte: the kernel, the init script, the running agent | 256 MiB |

The root disk is not in the snapshot; it is a separate file. So the store keeps its **own** hard link to the disk the snapshot was built from (next section), a formatted, empty scratch disk (`scratch.ext4`) that every clone gets its own copy of (doc 11), and a `manifest.json` with the SHA-256 of every file, the Firecracker version and the kernel's hash.

```
guest/out/state/snapshots/small/
  20261124T140456Z-32abab/   vm.snap  mem.snap  rootfs.ext4  scratch.ext4  manifest.json   (all mode 0444)
  current -> 20261124T140456Z-32abab
```

A snapshot only loads in the **Firecracker version that made it**, so opening the store refuses a mismatch with "run make snapshots". The hashes are checked once, when the store is opened, never during a restore.

## 3. Relative paths: one save, many players

`vm.snap` remembers the paths of the disks and of the vsock socket. If they were absolute (`/home/.../run-1/v.sock`), every clone would fight over the same files.

So the snapshot is built with **relative names only**: `rootfs.ext4`, `scratch.ext4`, `v.sock`. Firecracker resolves them against its **working directory**, and each clone runs Firecracker in a folder of its own. Same names, different files. The week-3 spike checked that Firecracker v1.17 keeps the names exactly as written ([ADR-0005](../adr/0005-rootless-and-jailer-modes.md)).

## 4. Copy-on-write: the shared textbook

Loading a snapshot does **not** read 256 MiB into memory. Firecracker *maps* `mem.snap` into the VM with `mmap(..., MAP_PRIVATE)`:

- Reading a page reads it from the **page cache**, Linux's memory copy of recently used file pages. Ten clones reading the same page all see **one** copy in the page cache.
- Writing a page makes a **private copy** for that clone only (copy-on-write). The file never changes, and no other clone sees the write.

Like a class sharing one textbook: you only photocopy the page you scribble on.

This is why clones are **hard links** to the store's `mem.snap`, never copies. A hard link is a second name for the *same* file (the same inode). A copy is a different file with its own page-cache pages, and the sharing is silently gone. And hard links only work **inside one filesystem**, so `snapshots/`, `runs/` and `jail/` all live in one state folder, checked once by `state.Open`. (On this machine `/tmp` is a separate in-memory filesystem, so the default is `guest/out/state`.)

One side effect: the store makes its `rootfs.ext4` read-only with `chmod 0444`, and because a hard link *is* the same file, `guest/out/rootfs.ext4` turns 0444 too. That is harmless: `make guest` never writes into the old file, it builds a new one and renames it over the name. The snapshot keeps the old one.

## 5. Measuring memory: PSS, not RSS

How much memory does one clone cost? The usual number, **RSS** (resident set size), counts every page a process has mapped, shared or not. Ten clones sharing 200 MiB of page cache would each report 200 MiB: 2 GiB in total, ten times too much.

**PSS** (proportional set size) splits each shared page among the processes that share it. Ten clones sharing 200 MiB count 20 MiB each, plus their private copies. That is the honest number, and the week-10 benchmarks report PSS (from `/proc/<pid>/smaps_rollup`).

## 6. De-twinning: the same clock, the same dice

Every clone wakes up exactly where the snapshot paused. Two things are wrong then:

**The clock.** The guest's clock stood still while the snapshot sat on disk. A clone restored a day later believes it is still yesterday. Firecracker can move the guest's kvmclock forward on restore (`clock_realtime: true` on `PUT /snapshot/load`), but that also makes the guest's "time since boot" jump, which upsets timers. We leave it off and have the agent set the wall clock instead: `SyncTime` with the host's time, about a millisecond.

**The dice.** The kernel's random-number generator state is in `mem.snap` too. Without help, every clone would produce the **same** "random" numbers: the same keys, the same UUIDs. Two mechanisms fix that on our kernel (6.1.155):

1. **The agent's `Reseed`**: the host sends 32 bytes from its own `crypto/rand`; the agent mixes them into the guest kernel's pool and forces a reseed (`RNDRESEEDCRNG`). The restore fails, and the clone is destroyed, if this does not work.
2. **VMGenID**: Firecracker gives the guest a "VM generation ID" device (`VMGENCTR`) and changes the ID on every restore. The guest's `vmgenid` driver notices and reseeds the kernel by itself; `dmesg` then says *"crng reseeded due to virtual machine fork"*. The spike found that this works here: two clones read different UUIDs even before the host sent anything. The manifest records it (`"vmgenid": true`).

We keep both: Reseed does not depend on the kernel build, and VMGenID covers the moment before the agent's call arrives.

## 7. Try it

```bash
make snapshots && cat guest/out/state/snapshots/small/current/manifest.json
```
👀 Builds both classes (about a second each) and shows the manifest: hashes, Firecracker version, `"vmgenid": true`.

```bash
ls -li guest/out/rootfs.ext4 guest/out/state/snapshots/*/current/rootfs.ext4
```
👀 The same inode number (first column) and a link count above 1: one file, several names. (After `make guest` the first name points at a new file; the snapshots keep the old one.)

```bash
make e2e 2>&1 | grep -E 'cold boot|restore [0-9]'
```
👀 Each restore's phases (prepare, process start, LoadSnapshot, agent ready, de-twin) next to the cold boot: about 80 ms against 15–20 ms.

## 8. In our project

| Piece | Where |
|-------|-------|
| Size classes, build, store, manifest | `internal/snapshot` |
| One state folder, one filesystem | `internal/state` |
| Restore, timings, de-twin, destroy | `internal/clone` |
| The scratch disk template, copied per clone | `internal/snapshot` (`scratch.go`) |
| The snapshot API calls | `internal/vmm` (`Pause`, `CreateSnapshot`, `LoadSnapshot`) |
| The facts we rely on, checked on a real VM | `test/e2e/spike_test.go`, `test/e2e/restore_test.go` |

## 9. Check yourself

<details><summary>1. Why must a clone hard-link mem.snap instead of copying it?</summary>

A copy is a different file with its own page-cache pages, so clones would stop sharing memory, and copying 256 MiB is slow. A hard link is the same file under another name, so every clone maps the same pages until it writes.
</details>

<details><summary>2. Ten clones each show 200 MiB of RSS. Do they use 2 GiB?</summary>

Probably not. RSS counts shared pages again in every clone. PSS divides shared pages among the clones that share them, and that is the number to report.
</details>

<details><summary>3. What would go wrong if we skipped de-twinning?</summary>

Every clone would have the snapshot's old clock, and, without VMGenID, the same random-number state: the same "random" keys and UUIDs in every run.
</details>

## 10. New words

| Word | Meaning |
|------|---------|
| Snapshot | A paused VM saved to files: its state (`vm.snap`) and its memory (`mem.snap`) |
| Restore | Loading a snapshot into a fresh Firecracker; the VM continues where it was paused |
| Page cache | Linux's in-memory copy of file pages, shared by everyone who reads them |
| Copy-on-write | Share a page until someone writes; then the writer gets a private copy |
| Hard link | A second name for the same file (same inode) |
| RSS / PSS | Memory counted with shared pages in full / divided among the sharers |
| De-twinning | Giving each clone its own clock and random state after a restore |
| VMGenID | A device whose ID changes on restore, so the guest kernel knows to reseed |

**Next:** 10 · the jailer: how each clone's Firecracker gets locked in.
