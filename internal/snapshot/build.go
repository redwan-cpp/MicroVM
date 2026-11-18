package snapshot

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/redwan-cpp/MicroVM/internal/agentclient"
	agentv1 "github.com/redwan-cpp/MicroVM/internal/gen/sandbox/agent/v1"
	"github.com/redwan-cpp/MicroVM/internal/state"
	"github.com/redwan-cpp/MicroVM/internal/vmm"
)

const (
	// agentReadyTimeout is how long the build VM's agent may take to answer
	// after InstanceStart. A cold boot takes well under a second.
	agentReadyTimeout = 10 * time.Second
	// createTimeout bounds CreateSnapshot, which writes and syncs all of
	// guest memory.
	createTimeout = 2 * time.Minute
	// probeTimeout bounds the VMGenID probe in the build VM.
	probeTimeout = 5 * time.Second
	// consoleTail is how much of the guest console an error message shows.
	consoleTail = 2048
)

// storeFileMode is the mode of every file in a snapshot version: readable
// by everyone (a jailed Firecracker runs as another user), writable by
// nobody. The VM a clone runs must never be able to change what the next
// clone starts from.
const storeFileMode = 0o444

// BuildConfig is what Build needs from the outside.
type BuildConfig struct {
	// Firecracker is the firecracker program.
	Firecracker string
	// Kernel is the guest kernel (guest/out/vmlinux).
	Kernel string
	// Rootfs is the guest root disk (guest/out/rootfs.ext4). The store
	// keeps a hard link to it, so it must be on the state folder's
	// filesystem.
	Rootfs string
}

// BuildResult describes a finished build.
type BuildResult struct {
	// Version is the new snapshot version, already "current".
	Version *Version
	// Boot is the cold boot of the build VM: from InstanceStart to the
	// agent's first answer.
	Boot time.Duration
	// Total is the whole build, hashing included.
	Total time.Duration
}

// Build makes a new base snapshot of class and makes it the current one.
//
//  1. Hard-link the root disk into a new version folder. The store keeps
//     that link: a later `make guest` writes a new file and renames it over
//     guest/out/rootfs.ext4, so the disk under this snapshot never changes.
//  2. Cold-boot a VM in a fresh run folder (sandbox_mode=agent), with the
//     store's root disk and a blank scratch disk under the relative names
//     every clone will use (rootfs.ext4, scratch.ext4, v.sock).
//  3. Wait until the agent answers, check whether the guest has VMGenID,
//     then close the host's connection: no connection may be frozen into
//     the snapshot.
//  4. Pause the VM and write vm.snap and mem.snap; stop it.
//  5. Hash every file, make them 0444, write manifest.json, and point
//     "current" at the new version.
func Build(ctx context.Context, root *state.Root, class Class, cfg BuildConfig) (_ *BuildResult, err error) {
	began := time.Now()
	fcVersion, err := vmm.BinaryVersion(ctx, cfg.Firecracker)
	if err != nil {
		return nil, err
	}
	// Firecracker runs in the run folder, so a relative kernel path would
	// point into it.
	if cfg.Kernel, err = filepath.Abs(cfg.Kernel); err != nil {
		return nil, fmt.Errorf("snapshot: %w", err)
	}

	classDir := filepath.Join(root.SnapshotsDir(), class.Name)
	if err := os.MkdirAll(classDir, 0o700); err != nil {
		return nil, fmt.Errorf("snapshot: %w", err)
	}
	// Build into a hidden folder and rename it when it is complete, so a
	// half-built version never looks like a real one.
	id := newVersionID(began)
	tmp := filepath.Join(classDir, ".build-"+id)
	if err := os.Mkdir(tmp, 0o700); err != nil {
		return nil, fmt.Errorf("snapshot: %w", err)
	}
	defer func() {
		if err != nil {
			_ = os.RemoveAll(tmp)
		}
	}()

	if err := linkFile(cfg.Rootfs, filepath.Join(tmp, RootfsName)); err != nil {
		return nil, err
	}
	vm, err := bootAndSnapshot(ctx, root, class, cfg, tmp)
	if err != nil {
		return nil, fmt.Errorf("snapshot: build %s: %w", class.Name, err)
	}

	m := &Manifest{
		Format:             ManifestFormat,
		Class:              class.Name,
		VCPUs:              class.VCPUs,
		MemoryMiB:          class.MemoryMiB,
		ScratchMiB:         class.ScratchMiB,
		FirecrackerVersion: fcVersion,
		AgentVersion:       vm.agentVersion,
		VMGenID:            vm.vmgenid,
		CreatedAt:          began.UTC().Truncate(time.Second),
	}
	for _, h := range []struct {
		path string
		sum  *string
	}{
		{cfg.Kernel, &m.KernelSHA256},
		{filepath.Join(tmp, RootfsName), &m.RootfsSHA256},
		{filepath.Join(tmp, StateName), &m.StateSHA256},
		{filepath.Join(tmp, MemName), &m.MemSHA256},
	} {
		if *h.sum, err = hashFile(ctx, h.path); err != nil {
			return nil, err
		}
	}
	// chmod on the root disk's hard link also makes guest/out/rootfs.ext4
	// read-only: it is the same file (inode). That is harmless, because
	// `make guest` replaces that file instead of writing into it.
	for _, name := range []string{RootfsName, StateName, MemName} {
		if err := os.Chmod(filepath.Join(tmp, name), storeFileMode); err != nil {
			return nil, fmt.Errorf("snapshot: %w", err)
		}
	}
	if err := writeManifest(tmp, m); err != nil {
		return nil, err
	}

	dir := filepath.Join(classDir, id)
	if err := os.Rename(tmp, dir); err != nil {
		return nil, fmt.Errorf("snapshot: %w", err)
	}
	if err := setCurrent(classDir, id); err != nil {
		return nil, err
	}
	return &BuildResult{
		Version: &Version{ID: id, Dir: dir, Manifest: m},
		Boot:    vm.boot,
		Total:   time.Since(began),
	}, nil
}

// builtVM is what bootAndSnapshot learned from the build VM.
type builtVM struct {
	boot         time.Duration
	agentVersion string
	vmgenid      bool
}

// bootAndSnapshot runs the build VM in a fresh run folder and moves its
// vm.snap and mem.snap into storeDir, which must already hold the root
// disk. The run folder is deleted afterwards.
func bootAndSnapshot(ctx context.Context, root *state.Root, class Class, cfg BuildConfig, storeDir string) (builtVM, error) {
	_, runDir, err := root.NewRunDir()
	if err != nil {
		return builtVM{}, err
	}
	err = linkFile(filepath.Join(storeDir, RootfsName), filepath.Join(runDir, RootfsName))
	if err == nil {
		err = CreateScratch(filepath.Join(runDir, ScratchName), class.ScratchMiB)
	}
	if err != nil {
		_ = os.RemoveAll(runDir)
		return builtVM{}, err
	}
	proc, err := vmm.Start(ctx, vmm.Config{Binary: cfg.Firecracker, RunDir: runDir})
	if err != nil {
		return builtVM{}, err
	}
	defer proc.Stop() //nolint:errcheck // the explicit Stop below reports the error
	withConsole := func(err error) error {
		return fmt.Errorf("%w\nguest console (end):\n%s", err, proc.ConsoleTail(consoleTail))
	}

	// The machine. Paths are relative: Firecracker resolves them in the
	// run folder, and saves them as written into vm.snap.
	api := proc.Client()
	for _, step := range []func() error{
		func() error {
			return api.PutMachineConfig(ctx, vmm.MachineConfig{VCPUCount: class.VCPUs, MemSizeMiB: class.MemoryMiB})
		},
		func() error {
			return api.PutBootSource(ctx, vmm.BootSource{KernelImagePath: cfg.Kernel, BootArgs: BootArgs})
		},
		func() error {
			return api.PutDrive(ctx, vmm.Drive{DriveID: "rootfs", PathOnHost: RootfsName, IsRootDevice: true, IsReadOnly: true})
		},
		func() error {
			return api.PutDrive(ctx, vmm.Drive{DriveID: "scratch", PathOnHost: ScratchName})
		},
		func() error {
			return api.PutVsock(ctx, vmm.Vsock{GuestCID: GuestCID, UDSPath: vmm.VsockName})
		},
	} {
		if err := step(); err != nil {
			return builtVM{}, err
		}
	}
	started := time.Now()
	if err := api.Start(ctx); err != nil {
		return builtVM{}, withConsole(err)
	}

	readyCtx, cancel := context.WithTimeout(ctx, agentReadyTimeout)
	defer cancel()
	agent, health, err := agentclient.WaitReady(readyCtx, proc.VsockPath(), proc.Done())
	if err != nil {
		return builtVM{}, withConsole(fmt.Errorf("wait for the agent (is it in the image? run make guest): %w", err))
	}
	vm := builtVM{boot: time.Since(started), agentVersion: health.GetVersion()}
	vm.vmgenid, err = probeVMGenID(ctx, agent)
	// The host's connection must not be part of the snapshot: a clone
	// would wake up with a connection to a host side that is long gone.
	_ = agent.Close()
	if err != nil {
		return builtVM{}, withConsole(err)
	}

	if err := api.Pause(ctx); err != nil {
		return builtVM{}, withConsole(err)
	}
	createCtx, cancel := context.WithTimeout(ctx, createTimeout)
	defer cancel()
	if err := api.CreateSnapshot(createCtx, vmm.SnapshotCreate{SnapshotPath: StateName, MemFilePath: MemName}); err != nil {
		return builtVM{}, withConsole(err)
	}
	for _, name := range []string{StateName, MemName} {
		if err := os.Rename(filepath.Join(runDir, name), filepath.Join(storeDir, name)); err != nil {
			return builtVM{}, fmt.Errorf("move %s into the store: %w", name, err)
		}
	}
	return vm, proc.Stop()
}

// probeVMGenID reports whether the guest's vmgenid driver is bound to a
// device: then the guest kernel reseeds its RNG by itself on restore. On
// kernel 6.1.155 the driver is a platform driver; older kernels had it on
// the ACPI bus, so both places are listed.
func probeVMGenID(ctx context.Context, agent *agentclient.Client) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	var out strings.Builder
	res, err := agent.Exec(ctx, &agentv1.ExecRequest{
		Argv:           []string{"sh", "-c", "ls /sys/bus/platform/drivers/vmgenid /sys/bus/acpi/drivers/vmgenid 2>/dev/null; true"},
		MaxOutputBytes: 4096,
	}, &out, nil)
	if err != nil {
		return false, fmt.Errorf("probe VMGenID: %w", err)
	}
	if !res.Exited || res.ExitCode != 0 {
		return false, fmt.Errorf("probe VMGenID: %+v", res)
	}
	return vmgenidBound(out.String()), nil
}

// vmgenidBound reads the `ls` output of the vmgenid driver folders: the
// driver's own files are bind, unbind and uevent; any other entry is a
// device bound to it (for example VMGENCTR:00).
func vmgenidBound(ls string) bool {
	for _, line := range strings.Split(ls, "\n") {
		name := strings.TrimSpace(line)
		switch {
		case name == "", strings.HasSuffix(name, ":"): // blank, or a folder heading
		case name == "bind", name == "unbind", name == "uevent", name == "new_id", name == "remove_id", name == "module":
		default:
			return true
		}
	}
	return false
}

// linkFile hard-links from to to, with a clear error if they are on
// different filesystems.
func linkFile(from, to string) error {
	if err := os.Link(from, to); err != nil {
		if errors.Is(err, syscall.EXDEV) {
			return fmt.Errorf("snapshot: cannot hard-link %s into %s: they are on different filesystems; "+
				"keep the state folder (%s) on the same filesystem as the guest image", from, filepath.Dir(to), state.EnvVar)
		}
		return fmt.Errorf("snapshot: %w", err)
	}
	return nil
}

// newVersionID returns a version folder name for a build that began at t:
// the time (so names sort by age) and 6 random hex digits.
func newVersionID(t time.Time) string {
	b := make([]byte, 3)
	_, _ = rand.Read(b) // never fails (crypto/rand documents this)
	return t.UTC().Format("20060102T150405Z") + "-" + hex.EncodeToString(b)
}

// setCurrent points classDir/current at version id, atomically: a new
// symlink under a temporary name, renamed over the old one. Anyone reading
// "current" sees the old version or the new one, never neither.
func setCurrent(classDir, id string) error {
	tmp := filepath.Join(classDir, ".current-"+id)
	if err := os.Symlink(id, tmp); err != nil {
		return fmt.Errorf("snapshot: %w", err)
	}
	if err := os.Rename(tmp, filepath.Join(classDir, currentName)); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("snapshot: %w", err)
	}
	return nil
}
