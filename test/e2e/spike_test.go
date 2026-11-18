//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	agentv1 "github.com/redwan-cpp/MicroVM/internal/gen/sandbox/agent/v1"
	"github.com/redwan-cpp/MicroVM/internal/vmm"
)

// The week-3 spike. ADR-0003 needs every clone of a snapshot to see the
// same names (rootfs.ext4, scratch.ext4, mem.snap, vm.snap, v.sock)
// pointing at its own files. The cheapest way: build the snapshot with
// paths *relative* to Firecracker's working directory, and give each clone
// its own working directory. That only works if Firecracker v1.17 keeps
// the paths as written, instead of turning them into absolute ones when it
// saves the snapshot. This test checks exactly that, and two more facts
// the design depends on:
//
//   - the store files can be mode 0444: Firecracker opens mem.snap and
//     vm.snap read-only (for its owner, opening a 0444 file for writing
//     fails with EACCES, so a successful restore proves it);
//   - whether the guest kernel reseeds its random number generator by
//     itself on restore (VMGenID), or only through the agent's Reseed.
//
// It drives Firecracker through internal/vmm only, the thinnest layer we
// have, so it checks Firecracker's behaviour, not our snapshot code.
//
// What it found (2026-11-18, Firecracker v1.17.0, guest kernel 6.1.155):
//
//   - Relative paths survive snapshot create and load unchanged. vm.snap
//     names rootfs.ext4, scratch.ext4 and v.sock as written, never the
//     build folder, and a clone in another folder finds its own files.
//   - A restore from 0444 store files works: mem.snap and vm.snap (and the
//     read-only root disk) are opened read-only.
//   - VMGenID works: Firecracker exposes the VMGENCTR device, the guest's
//     vmgenid driver binds to it, and on every restore the guest kernel
//     logs "crng reseeded due to virtual machine fork". Two clones read
//     different random UUIDs before the host reseeds anything. The agent's
//     Reseed stays anyway (ADR-0003, decision 5): it costs one call, and it
//     does not depend on the kernel build.

// startFirecracker starts an empty Firecracker in dir, which becomes its
// working directory. Cleanup stops it (and deletes dir).
func startFirecracker(t *testing.T, binary, dir string) *vmm.Process {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	proc, err := vmm.Start(ctx, vmm.Config{Binary: binary, RunDir: dir})
	if err != nil {
		t.Fatalf("start firecracker: %v", err)
	}
	t.Cleanup(func() { _ = proc.Stop() })
	return proc
}

// must fails the test if an API call failed.
func must(t *testing.T, proc *vmm.Process, what string, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s: %v\nconsole:\n%s", what, err, consoleTail(proc.Console()))
	}
}

func TestSpikeSnapshotPaths(t *testing.T) {
	a := findArtifacts(t)
	// Everything lives in one folder under guest/out, on the same
	// filesystem as the root disk, so hard links work. (/tmp is often a
	// different filesystem, tmpfs.)
	base, err := os.MkdirTemp(filepath.Dir(a.rootfs), "e2e-spike-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	store := mkdir(t, base, "store")
	build := mkdir(t, base, "build")

	// The store keeps its own hard link to the root disk; the build VM
	// gets another one, under the same name every clone will use.
	link(t, a.rootfs, filepath.Join(store, "rootfs.ext4"))
	link(t, filepath.Join(store, "rootfs.ext4"), filepath.Join(build, "rootfs.ext4"))
	sparseFile(t, filepath.Join(build, "scratch.ext4"), 64<<20)

	// 1. Cold-boot the build VM with relative paths only, wait for the
	// agent, probe VMGenID, then snapshot it.
	ctx := t.Context()
	fc := startFirecracker(t, a.firecracker, build)
	api := fc.Client()
	must(t, fc, "machine config", api.PutMachineConfig(ctx, vmm.MachineConfig{VCPUCount: 1, MemSizeMiB: 256}))
	must(t, fc, "boot source", api.PutBootSource(ctx, vmm.BootSource{KernelImagePath: a.kernel, BootArgs: bootArgs}))
	must(t, fc, "root disk", api.PutDrive(ctx, vmm.Drive{
		DriveID: "rootfs", PathOnHost: "rootfs.ext4", IsRootDevice: true, IsReadOnly: true,
	}))
	must(t, fc, "scratch disk", api.PutDrive(ctx, vmm.Drive{DriveID: "scratch", PathOnHost: "scratch.ext4"}))
	must(t, fc, "vsock", api.PutVsock(ctx, vmm.Vsock{GuestCID: guestCID, UDSPath: vmm.VsockName}))
	started := time.Now()
	must(t, fc, "InstanceStart", api.Start(ctx))
	agent := waitForAgent(t, fc.VsockPath(), fc.Done(), fc.Console, started)
	t.Logf("cold boot: InstanceStart → agent ready: %v", time.Since(started).Round(time.Millisecond))

	v := &vm{agent: agent, console: fc.Console}
	_, drivers, _ := v.exec(t, 10*time.Second, &agentv1.ExecRequest{Argv: []string{"sh", "-c",
		"ls /sys/bus/platform/drivers/vmgenid /sys/bus/acpi/drivers/vmgenid 2>/dev/null"}})
	vmgenid := strings.Contains(drivers, "VMGENCTR")
	t.Logf("VMGenID: device bound to the guest's vmgenid driver: %v (%q)", vmgenid, strings.TrimSpace(drivers))

	// No host connection may be frozen into the snapshot.
	_ = agent.Close()
	must(t, fc, "pause", api.Pause(ctx))
	must(t, fc, "create snapshot", api.CreateSnapshot(ctx, vmm.SnapshotCreate{SnapshotPath: "vm.snap", MemFilePath: "mem.snap"}))

	// 2. The relative names were kept: the state file names the files as
	// we wrote them and never mentions the build folder.
	state, err := os.ReadFile(filepath.Join(build, "vm.snap")) //nolint:gosec // G304: our own test folder
	if err != nil {
		t.Fatalf("vm.snap is not in Firecracker's working folder: %v", err)
	}
	if bytes.Contains(state, []byte(base)) {
		t.Errorf("vm.snap contains the absolute path %s: Firecracker made the paths absolute", base)
	}
	for _, name := range []string{"rootfs.ext4", "scratch.ext4", "v.sock"} {
		if !bytes.Contains(state, []byte(name)) {
			t.Errorf("vm.snap does not mention %q", name)
		}
	}

	// 3. Move the snapshot into the store, make every store file 0444, and
	// stop the build VM, which deletes the build folder: a clone can only
	// work if it finds its files by the relative names, in its own folder.
	for _, name := range []string{"vm.snap", "mem.snap"} {
		if err := os.Rename(filepath.Join(build, name), filepath.Join(store, name)); err != nil {
			t.Fatal(err)
		}
	}
	if err := fc.Stop(); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"vm.snap", "mem.snap", "rootfs.ext4"} {
		if err := os.Chmod(filepath.Join(store, name), 0o444); err != nil { //nolint:gosec // G302: store files are read-only for everyone on purpose (a jailed Firecracker reads them as another user)
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(build); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the build folder is still there (%v)", err)
	}

	// 4. Restore two clones side by side, each in its own folder, and read
	// a random UUID in each *before* any reseed from the host.
	uuids := make([]string, 2)
	reseeded := make([]bool, 2)
	for i := range 2 {
		dir := mkdir(t, base, fmt.Sprintf("clone%d", i+1))
		for _, name := range []string{"vm.snap", "mem.snap", "rootfs.ext4"} {
			link(t, filepath.Join(store, name), filepath.Join(dir, name))
		}
		sparseFile(t, filepath.Join(dir, "scratch.ext4"), 64<<20)

		fc := startFirecracker(t, a.firecracker, dir)
		started := time.Now()
		must(t, fc, "load snapshot", fc.Client().LoadSnapshot(ctx, vmm.SnapshotLoad{
			SnapshotPath: "vm.snap",
			MemBackend:   vmm.MemoryBackend{BackendType: "File", BackendPath: "mem.snap"},
			ResumeVM:     true,
		}))
		loaded := time.Since(started)
		agent := waitForAgent(t, fc.VsockPath(), fc.Done(), fc.Console, started)
		t.Logf("clone %d: LoadSnapshot %v, agent ready %v after it", i+1,
			loaded.Round(time.Millisecond), time.Since(started).Round(time.Millisecond))
		v := &vm{agent: agent, console: fc.Console}
		t.Cleanup(func() { _ = agent.Close() })

		_, uuid, _ := v.exec(t, 10*time.Second, &agentv1.ExecRequest{Argv: []string{"cat", "/proc/sys/kernel/random/uuid"}})
		uuids[i] = strings.TrimSpace(uuid)
		res, out, _ := v.exec(t, 10*time.Second, &agentv1.ExecRequest{Argv: []string{"echo", "hi"}})
		if out != "hi\n" || !res.Exited || res.ExitCode != 0 {
			t.Errorf("clone %d: echo hi: %q, %+v", i+1, out, res)
		}
		_, dmesg, _ := v.exec(t, 10*time.Second, &agentv1.ExecRequest{Argv: []string{"dmesg"}})
		reseeded[i] = strings.Contains(dmesg, "crng reseeded due to virtual machine fork")

		var st syscall.Stat_t
		if err := syscall.Stat(filepath.Join(store, "mem.snap"), &st); err != nil {
			t.Fatal(err)
		}
		if st.Nlink < 2 {
			t.Errorf("store mem.snap has %d links while clone %d runs, want more than 1", st.Nlink, i+1)
		}
	}

	t.Logf("relative paths: kept as written; store files at mode 0444: restored fine")
	t.Logf("clone UUIDs before any host reseed: %s and %s (equal: %v)", uuids[0], uuids[1], uuids[0] == uuids[1])
	t.Logf("guest kernel logged \"crng reseeded due to virtual machine fork\": clone 1 %v, clone 2 %v", reseeded[0], reseeded[1])
}

func mkdir(t *testing.T, parent, name string) string {
	t.Helper()
	dir := filepath.Join(parent, name)
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

func link(t *testing.T, from, to string) {
	t.Helper()
	if err := os.Link(from, to); err != nil {
		if errors.Is(err, syscall.EXDEV) {
			t.Fatalf("hard link %s → %s crosses filesystems: %v", from, to, err)
		}
		t.Fatal(err)
	}
}

// sparseFile creates a file of size bytes that takes no disk space: all
// zeros, with no blocks allocated until something writes.
func sparseFile(t *testing.T, path string, size int64) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600) //nolint:gosec // G304: our own test folder
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(size); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}
