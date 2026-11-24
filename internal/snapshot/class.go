// Package snapshot builds and keeps the base snapshots that every clone is
// restored from: one per size class (ADR-0003).
//
// A snapshot is a paused VM saved to two files: vm.snap (vCPU registers,
// device state, and the device configuration with every path given to
// Firecracker) and mem.snap (all of guest memory). Restoring it gives a VM
// whose agent is already running, in about 15 ms instead of a cold boot.
//
// The store lives in the state folder (internal/state):
//
//	snapshots/<class>/
//	  <version>/          one folder per build, never changed afterwards
//	    vm.snap           \
//	    mem.snap           \ mode 0444: nobody may write them, not even us
//	    rootfs.ext4        / (rootfs.ext4 is a hard link: the disk it was built from)
//	    scratch.ext4      /  the formatted scratch template, copied for each clone
//	    manifest.json     what was built from what, with SHA-256 hashes
//	  current -> <version>   a symlink, swapped atomically after each build
//
// Every path in the snapshot is relative (rootfs.ext4, scratch.ext4,
// v.sock), so one snapshot serves every clone, rootless or jailed: each
// clone runs Firecracker in its own folder, where those names are its own
// files (ADR-0005). docs/learn/09-snapshots-and-cow.md explains it all.
package snapshot

import (
	"fmt"
	"slices"
	"strings"
)

// The names every VM made from a snapshot sees in its working directory.
// They are written into vm.snap, so they are part of the snapshot format:
// never change them without rebuilding every snapshot.
const (
	// RootfsName is the read-only root disk, /dev/vda in the guest.
	RootfsName = "rootfs.ext4"
	// ScratchName is the per-clone writable disk, /dev/vdb in the guest,
	// mounted at /work. In a version folder it is the formatted template
	// that every clone gets its own copy of.
	ScratchName = "scratch.ext4"
	// StateName is the VM state file.
	StateName = "vm.snap"
	// MemName is the guest memory file.
	MemName = "mem.snap"
	// ManifestName describes a snapshot version.
	ManifestName = "manifest.json"
)

// GuestCID is the guest's vsock address in every snapshot. 0-2 are
// reserved; each VM has its own vsock device, so every VM may use 3.
const GuestCID = 3

// BootArgs is the kernel command line of the VM a snapshot is made from:
// the one from scripts/boot-vm.sh (docs/learn/06-firecracker-api.md
// explains each word) plus sandbox_mode=agent, which makes
// guest/init/demo-init start sandbox-agent.
const BootArgs = "console=ttyS0 reboot=k panic=1 pci=off nomodule init=/sbin/demo-init" +
	" i8042.noaux i8042.nomux i8042.nopnp i8042.dumbkbd quiet loglevel=1" +
	" sandbox_mode=agent"

// Class is a size class: the shape of a VM, fixed when its snapshot is
// made. A restored VM always has exactly the vCPUs and memory of its
// snapshot (ADR-0003, decision 1), so a policy picks a class, and cgroup
// limits on the Firecracker process narrow it further.
type Class struct {
	// Name is the class's name, for example "small".
	Name string
	// VCPUs is the number of virtual CPUs.
	VCPUs int
	// MemoryMiB is the guest's memory.
	MemoryMiB int
	// ScratchMiB is the size of each clone's blank scratch disk.
	ScratchMiB int

	// The cgroup v2 limits for one clone's Firecracker process, used in
	// jailer mode (ADR-0003, decision 1; internal/jailer). CPU time is
	// capped at VCPUs full CPUs (cpu.max).
	//
	// CgroupMemoryMiB is memory.max: guest memory plus room for
	// Firecracker itself.
	CgroupMemoryMiB int
	// CgroupPids is pids.max: Firecracker's threads (one per vCPU plus a
	// few of its own), with room to spare, and nothing more.
	CgroupPids int
}

// Classes are all size classes, smallest first. This is the only place
// they are defined.
var Classes = []Class{
	{Name: "small", VCPUs: 1, MemoryMiB: 256, ScratchMiB: 64, CgroupMemoryMiB: 256 + 64, CgroupPids: 16},
	{Name: "medium", VCPUs: 2, MemoryMiB: 512, ScratchMiB: 128, CgroupMemoryMiB: 512 + 64, CgroupPids: 16},
}

// LookupClass returns the class called name.
func LookupClass(name string) (Class, error) {
	i := slices.IndexFunc(Classes, func(c Class) bool { return c.Name == name })
	if i < 0 {
		return Class{}, fmt.Errorf("snapshot: unknown size class %q (known: %s)", name, strings.Join(ClassNames(), ", "))
	}
	return Classes[i], nil
}

// ClassNames returns the names of all classes, smallest first.
func ClassNames() []string {
	names := make([]string, len(Classes))
	for i, c := range Classes {
		names[i] = c.Name
	}
	return names
}
