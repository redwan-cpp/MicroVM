package vmm

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"
)

// The names of Firecracker's own files in its run folder. Firecracker runs
// with the run folder as its working directory, and every path we give it
// is one of these relative names (or another one, like rootfs.ext4), never
// an absolute path: then the same names work unchanged in a jailer chroot
// and in every clone of a snapshot (ADR-0003, ADR-0005).
const (
	// APISocketName is Firecracker's API socket.
	APISocketName = "api.sock"
	// VsockName is the conventional vsock Unix socket name (Vsock.UDSPath).
	VsockName = "v.sock"
	// LogName is Firecracker's own log file.
	LogName = "firecracker.log"
)

const (
	// consoleSize is how much of the guest's serial console we keep.
	consoleSize = 64 << 10
	// socketWait is how long Start waits for Firecracker's API socket.
	socketWait = 2 * time.Second
	// stopGrace is how long Stop waits after SIGTERM before SIGKILL.
	stopGrace = 2 * time.Second
	// maxSocketPath is the longest Unix socket path Linux accepts.
	maxSocketPath = 107
)

// Config describes how to start Firecracker.
type Config struct {
	// Binary is the path to the firecracker program, for example
	// third_party/firecracker/firecracker.
	Binary string
	// RunDir is this VM's private run folder, for example a fresh
	// state.Root.NewRunDir. It must already exist, on the same filesystem
	// as the snapshot store. It is Firecracker's working directory and
	// holds the API socket, the vsock socket, the log and the VM's files,
	// all by relative names. Start takes it over: Stop, or a failed Start,
	// deletes it with everything inside.
	RunDir string

	// Launcher, if set, starts Firecracker through another program, the
	// jailer (internal/jailer), instead of running Binary directly. That
	// program must exec Firecracker in place (the same PID, the same
	// stdout and stderr) with RunDir as Firecracker's working directory;
	// the jailer does, since RunDir is its chroot.
	Launcher *Launcher
	// LogOwner, if set, becomes the owner of the log file Start creates,
	// so a Firecracker running as that (jailed) user can write it.
	LogOwner *Owner
}

// Launcher is a program that starts Firecracker for us.
type Launcher struct {
	// Path is the program, for example third_party/firecracker/jailer.
	Path string
	// Args are its arguments, ending with "--". Start appends
	// Firecracker's own arguments (--api-sock api.sock ...).
	Args []string
}

// Owner is a numeric user and group ID.
type Owner struct {
	UID, GID int
}

// Process is one running Firecracker, with its own private run folder
// holding the API socket, the vsock socket and the log. Create it with
// Start and always call Stop.
type Process struct {
	cmd     *exec.Cmd
	dir     string
	console *RingBuffer
	client  *Client
	exited  chan struct{} // closed when the process has exited

	stopGrace time.Duration // stopGrace, shorter in tests
	stopOnce  sync.Once
	stopErr   error
}

// Start launches an empty Firecracker: no VM yet, just the API socket,
// waiting for instructions (use Client, then Client().Start). ctx only
// bounds the start-up; the process keeps running until Stop.
func Start(ctx context.Context, cfg Config) (*Process, error) {
	// The run folder comes from the caller (a fresh one per VM, mode 0700,
	// under the state folder's runs/): no other user can see or touch the
	// sockets inside, and it is on the same filesystem as the snapshot
	// store, so the VM's files can be hard links.
	dir := cfg.RunDir
	if dir == "" {
		return nil, fmt.Errorf("vmm: Config.RunDir is empty")
	}
	p := &Process{
		dir:       dir,
		console:   NewRingBuffer(consoleSize),
		client:    NewClient(filepath.Join(dir, APISocketName)),
		exited:    make(chan struct{}),
		stopGrace: stopGrace,
	}
	if err := p.start(ctx, cfg); err != nil {
		_ = p.Stop() // kills Firecracker if it runs, deletes the folder
		return nil, err
	}
	return p, nil
}

func (p *Process) start(ctx context.Context, cfg Config) error {
	if len(p.APISocketPath()) > maxSocketPath {
		return fmt.Errorf("vmm: run folder path %s is too long for Unix sockets; use a shorter state folder", p.dir)
	}

	// Firecracker wants its log file to exist already.
	logFile, err := os.OpenFile(p.LogPath(), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("vmm: create log file: %w", err)
	}
	if err := logFile.Close(); err != nil {
		return fmt.Errorf("vmm: create log file: %w", err)
	}
	if o := cfg.LogOwner; o != nil {
		if err := os.Lchown(p.LogPath(), o.UID, o.GID); err != nil {
			return fmt.Errorf("vmm: give the log file to %d:%d: %w", o.UID, o.GID, err)
		}
	}

	// Relative names only: Firecracker resolves them against its working
	// directory, the run folder.
	args := []string{
		"--api-sock", APISocketName,
		"--log-path", LogName,
		"--level", "Info",
	}
	program := cfg.Binary
	if l := cfg.Launcher; l != nil {
		program = l.Path
		args = append(slices.Clone(l.Args), args...)
	}
	// os/exec would resolve a relative program path against cmd.Dir, the
	// run folder, so make it absolute first.
	program, err = filepath.Abs(program)
	if err != nil {
		return fmt.Errorf("vmm: %w", err)
	}
	cmd := exec.Command(program, args...) //nolint:gosec // G204: the program path comes from our own configuration
	cmd.Dir = p.dir
	// stdin is /dev/null (cmd.Stdin is nil). stdout and stderr carry the
	// guest's serial console: the guest decides what is written there, so
	// it goes into the ring buffer, which os/exec reads continuously.
	cmd.Stdout = p.console
	cmd.Stderr = p.console
	cmd.SysProcAttr = &syscall.SysProcAttr{
		// Its own process group: a Ctrl-C meant for us doesn't hit it.
		Setpgid: true,
		// If we die (a crashed test, a killed daemon), the kernel kills
		// Firecracker too, so no VM is left behind (threat model T12).
		// Linux sends it when the *thread* that started the process ends;
		// Go keeps its threads alive, so in practice that means our process.
		// The kernel forgets this setting when a process changes its user,
		// so it does not survive the jailer's switch to the jailed UID:
		// jailer mode cleans up through the clone's cgroup instead.
		Pdeathsig: syscall.SIGKILL,
	}
	// Firecracker has no children, so its console pipe closes when it
	// exits. WaitDelay is only a safety net.
	cmd.WaitDelay = time.Second
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("vmm: start %s: %w", program, err)
	}
	p.cmd = cmd
	go func() {
		_ = cmd.Wait() // the exit status of a VM process tells us nothing useful
		close(p.exited)
	}()

	return p.waitForSocket(ctx)
}

// waitForSocket waits until the API socket accepts connections.
// Firecracker opens it a few milliseconds after it starts, and every
// restore waits for this, so it looks often: every millisecond.
func (p *Process) waitForSocket(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, socketWait)
	defer cancel()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()

	var d net.Dialer
	for {
		conn, err := d.DialContext(ctx, "unix", p.APISocketPath())
		if err == nil {
			_ = conn.Close()
			return nil
		}
		select {
		case <-p.exited:
			return fmt.Errorf("vmm: firecracker exited before opening its API socket; console:\n%s", p.Console())
		case <-ctx.Done():
			return fmt.Errorf("vmm: firecracker did not open its API socket: %w", ctx.Err())
		case <-tick.C:
		}
	}
}

// Stop ends Firecracker (SIGTERM, then SIGKILL after 2 s), waits for it to
// exit and deletes the run folder. The VM and everything in it are gone
// afterwards. Stop may be called any number of times.
func (p *Process) Stop() error {
	p.stopOnce.Do(func() { p.stopErr = p.stop() })
	return p.stopErr
}

func (p *Process) stop() error {
	if p.cmd != nil {
		select {
		case <-p.exited: // already gone (for example the guest rebooted)
		default:
			_ = p.cmd.Process.Signal(syscall.SIGTERM)
			select {
			case <-p.exited:
			case <-time.After(p.stopGrace):
				_ = p.cmd.Process.Kill()
				<-p.exited
			}
		}
	}
	if err := os.RemoveAll(p.dir); err != nil {
		return fmt.Errorf("vmm: delete run folder: %w", err)
	}
	return nil
}

// Client returns the API client for this Firecracker.
func (p *Process) Client() *Client { return p.client }

// Done is closed when the Firecracker process has exited, for example
// because the guest powered itself off.
func (p *Process) Done() <-chan struct{} { return p.exited }

// Pid is Firecracker's process ID.
func (p *Process) Pid() int { return p.cmd.Process.Pid }

// Dir is the private run folder. Stop deletes it.
func (p *Process) Dir() string { return p.dir }

// APISocketPath is the host path of Firecracker's API socket.
func (p *Process) APISocketPath() string { return filepath.Join(p.dir, APISocketName) }

// VsockPath is the host path of the vsock Unix socket, for a VM whose
// Vsock.UDSPath is VsockName. It lives in the run folder, so Stop removes
// it. Dial the agent here (agentclient.Dial).
func (p *Process) VsockPath() string { return filepath.Join(p.dir, VsockName) }

// LogPath is the host path of Firecracker's own log file.
func (p *Process) LogPath() string { return filepath.Join(p.dir, LogName) }

// Console returns the last 64 KiB of the guest's serial console. The guest
// wrote it, so it may hold anything, including terminal escape sequences:
// clean it before showing it on a terminal (ConsoleTail does).
func (p *Process) Console() []byte { return p.console.Bytes() }

// ConsoleTail returns the last n bytes (at most) of the guest's console,
// with control characters other than newline and tab removed, so it is
// safe to put into an error message or a log.
func (p *Process) ConsoleTail(n int) string {
	return CleanConsole(p.console.Bytes(), n)
}

// CleanConsole returns the last n bytes (at most) of console output, with
// control characters other than newline and tab removed: the guest wrote
// it, and could use them to fake or hide terminal output.
func CleanConsole(console []byte, n int) string {
	if len(console) > n {
		console = console[len(console)-n:]
	}
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' || !unicode.IsControl(r) {
			return r
		}
		return -1
	}, string(console))
}
