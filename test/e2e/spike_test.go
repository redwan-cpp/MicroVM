//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
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
// It drives Firecracker with raw API calls on purpose: it checks
// Firecracker, not our own packages.
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

// rawFirecracker is a Firecracker started directly, with dir as its
// working directory and every path relative to it.
type rawFirecracker struct {
	dir     string
	cmd     *exec.Cmd
	console *vmm.RingBuffer
	api     *http.Client
	exited  chan struct{}
}

// startRawFirecracker starts Firecracker in dir (which must exist) and
// waits for its API socket. Cleanup kills it.
func startRawFirecracker(t *testing.T, binary, dir string) *rawFirecracker {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "firecracker.log"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	fc := &rawFirecracker{dir: dir, console: vmm.NewRingBuffer(64 << 10), exited: make(chan struct{})}
	fc.cmd = exec.Command(binary, "--api-sock", "api.sock", "--log-path", "firecracker.log", "--level", "Info") //nolint:gosec // G204: our own pinned binary
	fc.cmd.Dir = dir
	fc.cmd.Stdout, fc.cmd.Stderr = fc.console, fc.console
	fc.cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
	if err := fc.cmd.Start(); err != nil {
		t.Fatalf("start firecracker: %v", err)
	}
	go func() { _ = fc.cmd.Wait(); close(fc.exited) }()
	t.Cleanup(fc.kill)

	sock := filepath.Join(dir, "api.sock")
	fc.api = &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", sock)
		},
	}}
	deadline := time.Now().Add(2 * time.Second)
	for {
		if conn, err := net.Dial("unix", sock); err == nil {
			_ = conn.Close()
			return fc
		}
		if time.Now().After(deadline) {
			t.Fatalf("no API socket in %s\nconsole:\n%s", dir, consoleTail(fc.console.Bytes()))
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// call sends one API request and fails the test on a non-2xx answer.
func (fc *rawFirecracker) call(t *testing.T, method, path string, body any) {
	t.Helper()
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, "http://localhost"+path, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := fc.api.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close() //nolint:errcheck // test code
	answer, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode/100 != 2 {
		t.Fatalf("%s %s: HTTP %d: %s\nconsole:\n%s", method, path, resp.StatusCode, answer, consoleTail(fc.console.Bytes()))
	}
}

// kill ends Firecracker and waits for it.
func (fc *rawFirecracker) kill() {
	select {
	case <-fc.exited:
	default:
		_ = fc.cmd.Process.Kill()
		<-fc.exited
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
	fc := startRawFirecracker(t, a.firecracker, build)
	fc.call(t, "PUT", "/machine-config", map[string]any{"vcpu_count": 1, "mem_size_mib": 256})
	fc.call(t, "PUT", "/boot-source", map[string]any{"kernel_image_path": a.kernel, "boot_args": bootArgs})
	fc.call(t, "PUT", "/drives/rootfs", map[string]any{
		"drive_id": "rootfs", "path_on_host": "rootfs.ext4", "is_root_device": true, "is_read_only": true,
	})
	fc.call(t, "PUT", "/drives/scratch", map[string]any{
		"drive_id": "scratch", "path_on_host": "scratch.ext4", "is_root_device": false, "is_read_only": false,
	})
	fc.call(t, "PUT", "/vsock", map[string]any{"guest_cid": guestCID, "uds_path": "v.sock"})
	started := time.Now()
	fc.call(t, "PUT", "/actions", map[string]any{"action_type": "InstanceStart"})
	agent := waitForAgent(t, filepath.Join(build, "v.sock"), fc.exited, fc.console.Bytes, started)
	t.Logf("cold boot: InstanceStart → agent ready: %v", time.Since(started).Round(time.Millisecond))

	v := &vm{agent: agent, console: fc.console.Bytes}
	_, drivers, _ := v.exec(t, 10*time.Second, &agentv1.ExecRequest{Argv: []string{"sh", "-c",
		"ls /sys/bus/platform/drivers/vmgenid /sys/bus/acpi/drivers/vmgenid 2>/dev/null"}})
	vmgenid := strings.Contains(drivers, "VMGENCTR")
	t.Logf("VMGenID: device bound to the guest's vmgenid driver: %v (%q)", vmgenid, strings.TrimSpace(drivers))

	// No host connection may be frozen into the snapshot.
	_ = agent.Close()
	fc.call(t, "PATCH", "/vm", map[string]any{"state": "Paused"})
	fc.call(t, "PUT", "/snapshot/create", map[string]any{
		"snapshot_type": "Full", "snapshot_path": "vm.snap", "mem_file_path": "mem.snap",
	})
	fc.kill()

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
	// delete the build folder: a clone can only work if it finds its
	// files by the relative names, in its own folder.
	for _, name := range []string{"vm.snap", "mem.snap"} {
		if err := os.Rename(filepath.Join(build, name), filepath.Join(store, name)); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"vm.snap", "mem.snap", "rootfs.ext4"} {
		if err := os.Chmod(filepath.Join(store, name), 0o444); err != nil { //nolint:gosec // G302: store files are read-only for everyone on purpose (a jailed Firecracker reads them as another user)
			t.Fatal(err)
		}
	}
	if err := os.RemoveAll(build); err != nil {
		t.Fatal(err)
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

		fc := startRawFirecracker(t, a.firecracker, dir)
		started := time.Now()
		fc.call(t, "PUT", "/snapshot/load", map[string]any{
			"snapshot_path": "vm.snap",
			"mem_backend":   map[string]any{"backend_type": "File", "backend_path": "mem.snap"},
			"resume_vm":     true,
		})
		loaded := time.Since(started)
		agent := waitForAgent(t, filepath.Join(dir, "v.sock"), fc.exited, fc.console.Bytes, started)
		t.Logf("clone %d: LoadSnapshot %v, agent ready %v after it", i+1,
			loaded.Round(time.Millisecond), time.Since(started).Round(time.Millisecond))
		v := &vm{agent: agent, console: fc.console.Bytes}
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
