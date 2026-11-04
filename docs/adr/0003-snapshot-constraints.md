# ADR-0003: Design around what a Firecracker snapshot freezes

- **Status:** Accepted
- **Date:** 2026-11-04

## Context

The foundation doc's run path is "load the base snapshot → fork → **apply the policy (vCPU, memory, scratch disk, network)** → execute". A Firecracker snapshot does not allow all of that:

1. **vCPU count and memory size are part of the snapshot.** A restored VM has exactly the shape it was snapshotted with.
2. **Device configuration is saved with host paths:** the drive backing files, the vsock Unix socket path and the tap device name. Every clone restored from one snapshot would try to use the *same* host paths.
3. **Guest state is frozen too.** A filesystem mounted at snapshot time has its state in guest memory. The clock stops. The kernel's random-number state is identical in every clone.

## Decision

1. **Size classes.** Build one base snapshot per class (`small` = 1 vCPU/256 MiB, `medium` = 2 vCPU/512 MiB; more later). A policy picks a class. Finer limits come from **cgroup v2** on the Firecracker process (`cpu.max`, `memory.max`, `pids.max`), set through the jailer.
2. **One jailer chroot and one network namespace per clone.** Every clone sees the *same relative paths* (`rootfs.ext4`, `scratch.ext4`, `v.sock`, `tap0`), but in its own chroot and netns they point to different resources. This is how Firecracker's own docs handle network clones.
3. **The memory file is hard-linked, never copied, into each chroot.** Firecracker maps the snapshot memory file `MAP_PRIVATE`. All clones share the same page-cache pages until they write (copy-on-write). A copy would give each clone a different file and silently stop the sharing.
4. **The rootfs is read-only and shared. The scratch disk is blank per clone and not mounted at snapshot time.** The agent mounts `/dev/vdb` at `/work` after restore. `/tmp` is tmpfs.
5. **De-twinning on restore.** On every run the agent sets the guest clock from the host and adds host-supplied entropy to the kernel's RNG. If the pinned Firecracker version and guest kernel support VMGenID, the kernel reseeds by itself as well.

## Consequences

- Policies can't ask for arbitrary vCPU or memory sizes, only a class plus cgroup limits. This is documented in the policy reference.
- Each class adds a snapshot, about the size of its guest memory, on disk.
- The jailer becomes a required part of the system. That also gives defense in depth: chroot, UID drop, namespaces and seccomp around Firecracker.
- `sandboxd` must run as root to create chroots, cgroups and network namespaces. We keep the daemon small and put it at the center of the threat model.
- Benchmarks must report memory per clone as **PSS**, not RSS. RSS counts the shared pages again in every clone.
