//go:build e2e

// Package e2e holds the end-to-end tests: they boot real Firecracker
// microVMs with KVM and talk to sandbox-agent inside them, exactly as
// sandboxd will. Run them with `make e2e`, which builds the guest image
// first. They need /dev/kvm, so they never run in GitHub CI; there, the
// e2e build tag only makes sure they compile.
package e2e

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
	"unicode"

	"golang.org/x/sys/unix"

	"github.com/redwan-cpp/MicroVM/internal/agent"
	"github.com/redwan-cpp/MicroVM/internal/agentclient"
	agentv1 "github.com/redwan-cpp/MicroVM/internal/gen/sandbox/agent/v1"
	"github.com/redwan-cpp/MicroVM/internal/state"
	"github.com/redwan-cpp/MicroVM/internal/vmm"
	"github.com/redwan-cpp/MicroVM/internal/vsockconn"
)

const (
	// bootArgs is the kernel command line: the same as scripts/boot-vm.sh
	// (see the comments there), plus sandbox_mode=agent, which makes
	// guest/init/demo-init start sandbox-agent.
	bootArgs = "console=ttyS0 reboot=k panic=1 pci=off nomodule init=/sbin/demo-init" +
		" i8042.noaux i8042.nomux i8042.nopnp i8042.dumbkbd quiet loglevel=1" +
		" sandbox_mode=agent"

	// guestCID is the guest's vsock address. 0-2 are reserved; 3 is the
	// first free one. Each VM has its own vsock device, so every VM may
	// use 3.
	guestCID = 3

	// readyTimeout is how long the agent may take to answer after
	// InstanceStart. A cold boot takes well under a second.
	readyTimeout = 5 * time.Second
)

// artifacts are the files a VM is made of.
type artifacts struct {
	firecracker, kernel, rootfs string
}

// findArtifacts skips the test, with a hint, if anything is missing.
func findArtifacts(t *testing.T) artifacts {
	t.Helper()
	if err := unix.Access("/dev/kvm", unix.R_OK|unix.W_OK); err != nil {
		t.Skipf("no read-write access to /dev/kvm (%v): these tests need KVM", err)
	}
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	a := artifacts{
		firecracker: filepath.Join(root, "third_party", "firecracker", "firecracker"),
		kernel:      filepath.Join(root, "guest", "out", "vmlinux"),
		rootfs:      filepath.Join(root, "guest", "out", "rootfs.ext4"),
	}
	if _, err := os.Stat(a.firecracker); err != nil {
		t.Skipf("no Firecracker (%v): run make firecracker", err)
	}
	for _, f := range []string{a.kernel, a.rootfs} {
		if _, err := os.Stat(f); err != nil {
			t.Skipf("no guest image (%v): run make guest", err)
		}
	}
	return a
}

// newStateRoot opens a fresh state folder for one test, under guest/out/
// next to the guest image: the real layout, on the same filesystem as the
// root disk (never /tmp, which may be another filesystem). Cleanup deletes
// it.
func newStateRoot(t *testing.T, a artifacts) *state.Root {
	t.Helper()
	dir, err := os.MkdirTemp(filepath.Dir(a.rootfs), "e2e-state-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(dir); err != nil {
			t.Errorf("delete the test's state folder: %v", err)
		}
	})
	root, err := state.Open(dir, state.Options{})
	if err != nil {
		t.Fatalf("open the state folder: %v", err)
	}
	return root
}

// vm is one running microVM with a connected agent.
type vm struct {
	proc    *vmm.Process // nil for a VM the test started some other way
	agent   *agentclient.Client
	console func() []byte // the guest console, for error messages
}

// bootVM starts Firecracker, configures and starts a VM, and waits until
// the agent answers. Cleanup stops the VM even if the test fails.
func bootVM(t *testing.T, a artifacts, root *state.Root) *vm {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	_, dir, err := root.NewRunDir()
	if err != nil {
		t.Fatal(err)
	}
	proc, err := vmm.Start(ctx, vmm.Config{Binary: a.firecracker, RunDir: dir})
	if err != nil {
		t.Fatalf("start firecracker: %v", err)
	}
	t.Cleanup(func() { _ = proc.Stop() })

	// The same machine as `make boot`, plus a vsock device whose host-side
	// Unix socket lives in the run folder.
	api := proc.Client()
	for _, step := range []func() error{
		func() error {
			return api.PutMachineConfig(ctx, vmm.MachineConfig{VCPUCount: 1, MemSizeMiB: 256})
		},
		func() error {
			return api.PutBootSource(ctx, vmm.BootSource{KernelImagePath: a.kernel, BootArgs: bootArgs})
		},
		func() error {
			return api.PutDrive(ctx, vmm.Drive{DriveID: "rootfs", PathOnHost: a.rootfs, IsRootDevice: true, IsReadOnly: true})
		},
		func() error {
			return api.PutVsock(ctx, vmm.Vsock{GuestCID: guestCID, UDSPath: vmm.VsockName}) // in the run folder
		},
	} {
		if err := step(); err != nil {
			t.Fatalf("configure the VM: %v", err)
		}
	}

	started := time.Now()
	if err := api.Start(ctx); err != nil {
		t.Fatalf("InstanceStart: %v\nconsole:\n%s", err, consoleTail(proc.Console()))
	}
	client := waitForAgent(t, proc.VsockPath(), proc.Done(), proc.Console, started)
	t.Cleanup(func() { _ = client.Close() })
	t.Logf("InstanceStart → agent ready: %v", time.Since(started).Round(time.Millisecond))
	return &vm{proc: proc, agent: client, console: proc.Console}
}

// waitForAgent dials the agent behind the vsock socket vsockPath until it
// answers Health, for at most readyTimeout after started (InstanceStart or
// the snapshot load). While the guest boots, the vsock socket may not exist
// yet (ENOENT), or nothing may listen on the port yet
// (vsockconn.ErrNotReady); both are worth another try. done is closed when
// the VM's Firecracker exits; console returns its console output.
func waitForAgent(t *testing.T, vsockPath string, done <-chan struct{}, console func() []byte, started time.Time) *agentclient.Client {
	t.Helper()
	ctx, cancel := context.WithDeadline(t.Context(), started.Add(readyTimeout))
	defer cancel()

	for {
		client, err := agentclient.Dial(ctx, vsockPath, agent.Port)
		if err == nil {
			if _, err = client.Health(ctx); err == nil {
				return client
			}
			_ = client.Close()
			t.Fatalf("connected to the agent, but Health failed: %v\nconsole:\n%s", err, consoleTail(console()))
		}
		retry := errors.Is(err, vsockconn.ErrNotReady) ||
			errors.Is(err, syscall.ENOENT) || errors.Is(err, syscall.ECONNREFUSED)
		if !retry {
			t.Fatalf("dial the agent: %v\nconsole:\n%s", err, consoleTail(console()))
		}

		select {
		case <-done:
			t.Fatalf("the VM stopped before the agent answered (is the agent in the image? run make guest)\nconsole:\n%s",
				consoleTail(console()))
		case <-ctx.Done():
			t.Fatalf("the agent did not answer within %v of InstanceStart (last error: %v)\n"+
				"is the agent in the image? run make guest\nconsole:\n%s", readyTimeout, err, consoleTail(console()))
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// exec runs req in the VM with a host deadline of d.
func (v *vm) exec(t *testing.T, d time.Duration, req *agentv1.ExecRequest) (agentclient.Result, string, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), d)
	defer cancel()
	var stdout, stderr bytes.Buffer
	res, err := v.agent.Exec(ctx, req, &stdout, &stderr)
	if err != nil {
		t.Fatalf("Exec(%q): %v\nconsole:\n%s", req.GetArgv(), err, consoleTail(v.console()))
	}
	return res, stdout.String(), stderr.String()
}

// process is one line of `ps -o pid,stat,args` in the guest.
type process struct {
	pid  int
	stat string
	args string
}

// guestProcesses lists the processes running in the guest.
func (v *vm) guestProcesses(t *testing.T) []process {
	t.Helper()
	res, out, _ := v.exec(t, 10*time.Second, &agentv1.ExecRequest{Argv: []string{"ps", "-o", "pid,stat,args"}})
	if !res.Exited || res.ExitCode != 0 {
		t.Fatalf("ps: %+v", res)
	}
	var list []process
	for _, line := range strings.Split(strings.TrimSpace(out), "\n")[1:] { // skip the header
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		pid, err := strconv.Atoi(fields[0])
		if err != nil {
			t.Fatalf("ps line %q: %v", line, err)
		}
		list = append(list, process{pid: pid, stat: fields[1], args: strings.Join(fields[2:], " ")})
	}
	return list
}

func TestAgentInARealVM(t *testing.T) {
	a := findArtifacts(t)
	v := bootVM(t, a, newStateRoot(t, a))

	t.Run("Health", func(t *testing.T) {
		resp, err := v.agent.Health(t.Context())
		if err != nil {
			t.Fatalf("Health: %v", err)
		}
		if !strings.HasPrefix(resp.GetVersion(), "sandbox-agent ") {
			t.Errorf("version = %q, want it to start with %q", resp.GetVersion(), "sandbox-agent ")
		}
		t.Logf("agent: %s, up %d ms", resp.GetVersion(), resp.GetUptimeMs())
	})

	t.Run("SyncTimeAndReseed", func(t *testing.T) {
		if err := v.agent.SyncTime(t.Context(), time.Now()); err != nil {
			t.Fatalf("SyncTime: %v", err)
		}
		entropy := make([]byte, 64)
		if _, err := unix.Getrandom(entropy, 0); err != nil {
			t.Fatal(err)
		}
		if err := v.agent.Reseed(t.Context(), entropy); err != nil {
			t.Fatalf("Reseed: %v", err)
		}
		// The guest clock now matches the host's, to the second.
		_, out, _ := v.exec(t, 10*time.Second, &agentv1.ExecRequest{Argv: []string{"date", "+%s"}})
		guest, err := strconv.ParseInt(strings.TrimSpace(out), 10, 64)
		if err != nil {
			t.Fatalf("date printed %q", out)
		}
		if diff := time.Now().Unix() - guest; diff < -2 || diff > 2 {
			t.Errorf("guest clock is %d s off the host's", diff)
		}
	})

	t.Run("EchoHi", func(t *testing.T) {
		res, out, errOut := v.exec(t, 10*time.Second, &agentv1.ExecRequest{Argv: []string{"echo", "hi"}})
		if out != "hi\n" || errOut != "" {
			t.Errorf("stdout = %q, stderr = %q; want %q and nothing", out, errOut, "hi\n")
		}
		if !res.Exited || res.ExitCode != 0 || res.TimedOut || res.OutputTruncated {
			t.Errorf("result = %+v, want a clean exit 0", res)
		}
		t.Logf("echo hi: %v round trip", res.Duration.Round(time.Microsecond))
	})

	t.Run("HostDeadline", func(t *testing.T) {
		// The command asks for the default 30 s timeout, but the host only
		// waits 300ms. The host decides it timed out, and the agent kills
		// the command when the host cancels the call.
		res, _, _ := v.exec(t, 300*time.Millisecond, &agentv1.ExecRequest{Argv: []string{"sleep", "10"}})
		if !res.TimedOut || res.Exited {
			t.Errorf("result = %+v, want TimedOut and not Exited", res)
		}
		if res.Duration > 2*time.Second {
			t.Errorf("took %v, want about 300ms", res.Duration)
		}

		deadline := time.Now().Add(2 * time.Second)
		for {
			left := false
			for _, p := range v.guestProcesses(t) {
				left = left || p.args == "sleep 10"
			}
			if !left {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("`sleep 10` still runs in the guest after the host gave up on it")
			}
			time.Sleep(50 * time.Millisecond)
		}
	})

	t.Run("OutputCap", func(t *testing.T) {
		// 5 MiB of output against a 64 KiB cap: the host keeps 64 KiB, the
		// agent drains the rest, and the command runs to its end.
		res, out, _ := v.exec(t, 20*time.Second, &agentv1.ExecRequest{
			Argv:           []string{"head", "-c", strconv.Itoa(5 << 20), "/dev/zero"},
			MaxOutputBytes: 64 << 10,
		})
		if len(out) != 64<<10 || res.OutputBytes != 64<<10 {
			t.Errorf("got %d bytes (OutputBytes %d), want exactly 65536", len(out), res.OutputBytes)
		}
		if !res.OutputTruncated || res.TimedOut || !res.Exited || res.ExitCode != 0 {
			t.Errorf("result = %+v, want OutputTruncated, Exited with 0, not TimedOut", res)
		}
	})

	t.Run("NoZombies", func(t *testing.T) {
		// The shell leaves eight background children and one that escaped
		// with setsid. They all end up as orphans of PID 1 (the init
		// shell), which must reap them.
		res, out, _ := v.exec(t, 10*time.Second, &agentv1.ExecRequest{Argv: []string{"sh", "-c",
			"for i in 1 2 3 4 5 6 7 8; do sleep 0.2 & done; setsid sleep 0.3 & echo started"}})
		if !res.Exited || out != "started\n" {
			t.Fatalf("result = %+v, stdout %q", res, out)
		}

		time.Sleep(time.Second) // give everything time to end and be reaped
		var zombies []string
		for _, p := range v.guestProcesses(t) {
			if strings.HasPrefix(p.stat, "Z") {
				zombies = append(zombies, fmt.Sprintf("%d %s %s", p.pid, p.stat, p.args))
			}
		}
		if len(zombies) > 0 {
			t.Errorf("zombies in the guest:\n%s", strings.Join(zombies, "\n"))
		}
	})

	// Last: destroy the VM, and check that nothing is left on the host.
	pid, dir := v.proc.Pid(), v.proc.Dir()
	_ = v.agent.Close()
	if err := v.proc.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Errorf("Firecracker (pid %d) still exists after Stop (kill -0: %v)", pid, err)
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("run folder %s still exists after Stop (%v)", dir, err)
	}
}

// consoleTail returns the end of a guest console, cleaned for printing:
// the guest wrote it, so control characters (terminal escape sequences)
// are removed.
func consoleTail(console []byte) string {
	if len(console) > 4096 {
		console = console[len(console)-4096:]
	}
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' || !unicode.IsControl(r) {
			return r
		}
		return -1
	}, string(console))
}
