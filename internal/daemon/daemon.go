package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
	"google.golang.org/grpc"

	"github.com/redwan-cpp/MicroVM/internal/audit"
	"github.com/redwan-cpp/MicroVM/internal/clone"
	daemonv1 "github.com/redwan-cpp/MicroVM/internal/gen/sandbox/daemon/v1"
	"github.com/redwan-cpp/MicroVM/internal/jailer"
	"github.com/redwan-cpp/MicroVM/internal/snapshot"
	"github.com/redwan-cpp/MicroVM/internal/state"
	"github.com/redwan-cpp/MicroVM/internal/vmm"
)

const (
	// DefaultMaxConcurrent is how many runs may have a clone at once.
	DefaultMaxConcurrent = 4
	// MaxPending is how many runs may wait for a slot on top of that.
	MaxPending = 64
	// MaxRecvMsgSize is the largest request the daemon accepts: a
	// RunRequest with the largest stdin (960 KiB) plus argv and env.
	MaxRecvMsgSize = 1 << 20
	// ShutdownTimeout bounds a graceful shutdown.
	ShutdownTimeout = 10 * time.Second

	// JailerSocket is the socket in jailer mode (ADR-0006).
	JailerSocket = "/run/sandboxd/sandboxd.sock"
	// socketName is the socket's name in its folder.
	socketName = "sandboxd.sock"
	// SocketGroup is the group whose members may use the jailer-mode
	// socket, if it exists.
	SocketGroup = "sandbox"
	// openTimeout bounds opening the store (it hashes every file).
	openTimeout = time.Minute
)

// DefaultSocket is where the daemon listens unless told otherwise
// (ADR-0006): /run/sandboxd/sandboxd.sock in jailer mode, and
// $XDG_RUNTIME_DIR/sandboxd/sandboxd.sock in rootless mode (a private
// folder of the user's that the system empties at logout).
func DefaultSocket(jailerMode bool) (string, error) {
	if jailerMode {
		return JailerSocket, nil
	}
	dir := os.Getenv("XDG_RUNTIME_DIR")
	if dir == "" {
		return "", errors.New("daemon: XDG_RUNTIME_DIR is not set, so there is no default socket for rootless mode; pass --socket PATH")
	}
	return filepath.Join(dir, "sandboxd", socketName), nil
}

// Options configure Start.
type Options struct {
	// StateDir is the state folder (internal/state).
	StateDir string
	// Socket is where to listen; empty means DefaultSocket.
	Socket string
	// Firecracker is the firecracker program.
	Firecracker string
	// Jailer selects jailer mode; it needs root.
	Jailer bool
	// JailerBinary is the jailer program (jailer mode).
	JailerBinary string
	// SocketGroup is the group that may use the socket in jailer mode
	// (name or number). Empty means SocketGroup if it exists, else
	// nobody but root.
	SocketGroup string
	// MaxConcurrent is how many runs may have a clone at once: 1 to
	// jailer.MaxClones. 0 means DefaultMaxConcurrent.
	MaxConcurrent int
	// Logger gets the daemon's messages. nil means the standard logger.
	Logger *log.Logger
	// Cloner replaces real clones (tests). nil means NewCloner.
	Cloner Cloner
	// Version is the daemon's build, for the audit log.
	Version string
}

// Daemon is a started sandboxd: the state folder is locked, the store and
// the audit log are open, and the socket is listening.
type Daemon struct {
	srv      *Server
	grpc     *grpc.Server
	lis      net.Listener
	socket   string
	lock     *state.Lock
	auditLog *audit.Log
	served   chan error
	mode     clone.Mode
	maxConc  int
	root     *state.Root
	// jailParents are the folders the jailer creates as root for every
	// clone (jailer mode): Shutdown removes them once they are empty.
	jailParents []string
}

// Start opens everything a daemon needs, in this order, and starts
// serving. Any failure undoes what was done and says what to do.
//
//  1. Check the mode: jailer mode needs root, and root needs jailer mode
//     (rootless mode as root would run Firecracker as root, unconfined).
//  2. Open the state folder and take its lock: one daemon per state
//     folder, whatever the sockets.
//  3. In jailer mode, check that the UIDs and GIDs 70000-70031 belong to
//     nobody.
//  4. Open the snapshot store: every hash is checked now, never per run.
//  5. Open the audit log: its whole chain is verified, and a broken chain
//     stops the start.
//  6. Listen on the socket: refuse if a live daemon answers there, remove
//     a stale socket, and set its permissions.
func Start(opts Options) (_ *Daemon, err error) {
	logger := opts.Logger
	if logger == nil {
		logger = log.Default()
	}
	maxConc := opts.MaxConcurrent
	if maxConc == 0 {
		maxConc = DefaultMaxConcurrent
	}
	if maxConc < 1 || maxConc > jailer.MaxClones {
		return nil, fmt.Errorf("daemon: --max-concurrent %d: it must be 1 to %d", maxConc, jailer.MaxClones)
	}
	euid := os.Geteuid()
	mode := clone.Rootless
	if opts.Jailer {
		mode = clone.Jailer
		if euid != 0 {
			return nil, fmt.Errorf("daemon: jailer mode needs root (running as uid %d); build as yourself, then run only the daemon with sudo: sudo bin/sandboxd serve --jailer", euid)
		}
	} else if euid == 0 {
		return nil, errors.New("daemon: running as root without --jailer would run every Firecracker as root with nothing around it; use --jailer, or run rootless mode as yourself")
	}
	socket := opts.Socket
	if socket == "" {
		if socket, err = DefaultSocket(opts.Jailer); err != nil {
			return nil, err
		}
	}

	d := &Daemon{socket: socket, mode: mode, maxConc: maxConc, served: make(chan error, 1)}
	defer func() {
		if err != nil {
			err = errors.Join(err, d.close())
		}
	}()

	// 2. The state folder and its lock.
	if d.root, err = state.Open(opts.StateDir, state.Options{Jailer: opts.Jailer, Firecracker: opts.Firecracker}); err != nil {
		return nil, err
	}
	if d.lock, err = d.root.Lock(); err != nil {
		if errors.Is(err, state.ErrLocked) {
			return nil, fmt.Errorf("daemon: another sandboxd already uses the state folder %s (%w)", d.root.Dir(), err)
		}
		return nil, err
	}

	// 3. The jailed IDs.
	var jailerOpts *clone.JailerOptions
	if opts.Jailer {
		if err := clone.CheckIdentityRange(jailer.DefaultUID, jailer.DefaultGID, jailer.MaxClones); err != nil {
			return nil, err
		}
		if _, err := os.Stat(opts.JailerBinary); err != nil {
			return nil, fmt.Errorf("daemon: the jailer: %w (run make firecracker)", err)
		}
		jailerOpts = &clone.JailerOptions{Binary: opts.JailerBinary, UID: jailer.DefaultUID, GID: jailer.DefaultGID}
		d.jailParents = jailParents(d.root.JailDir(), opts.Firecracker, readMounts)
	}

	// 4. The snapshot store.
	ctx, cancel := context.WithTimeout(context.Background(), openTimeout)
	defer cancel()
	fcVersion, err := vmm.BinaryVersion(ctx, opts.Firecracker)
	if err != nil {
		return nil, err
	}
	store, err := snapshot.Open(ctx, d.root, fcVersion)
	if err != nil {
		return nil, err
	}
	if !hasAnyClass(store) {
		return nil, fmt.Errorf("daemon: no snapshots in %s: run make snapshots (as yourself)", d.root.SnapshotsDir())
	}

	// 5. The audit log. As root on a user's state folder, it stays the
	// user's (ADR-0006).
	var owner *audit.Owner
	if euid == 0 {
		uid, gid, err := d.root.Owner()
		if err != nil {
			return nil, err
		}
		owner = &audit.Owner{UID: uid, GID: gid}
	}
	if d.auditLog, err = audit.Open(d.root.AuditDir(), owner); err != nil {
		if errors.Is(err, audit.ErrBroken) {
			return nil, fmt.Errorf("%w\nthe daemon will not add to a broken chain. Keep the file as evidence and start a new chain with:\n"+
				"  sandboxd rotate-audit --state-dir %s", err, d.root.Dir())
		}
		return nil, err
	}

	// 6. The socket.
	if d.lis, err = listen(socket, opts.Jailer, opts.SocketGroup, opts.Socket == ""); err != nil {
		return nil, err
	}

	cloner := opts.Cloner
	if cloner == nil {
		cloner = NewCloner(store, opts.Firecracker, jailerOpts)
	}
	d.srv = NewServer(Config{
		Store:         store,
		Cloner:        cloner,
		Audit:         d.auditLog,
		Mode:          mode,
		FirstUID:      jailer.DefaultUID,
		MaxConcurrent: maxConc,
		MaxPending:    MaxPending,
		Version:       opts.Version,
		Logger:        logger,

		FirecrackerVersion: fcVersion,
		Host:               DetectHost(),
	})
	d.grpc = grpc.NewServer(
		grpc.Creds(peerCreds{}),
		grpc.MaxRecvMsgSize(MaxRecvMsgSize),
	)
	daemonv1.RegisterSandboxServiceServer(d.grpc, d.srv)
	go func() { d.served <- d.grpc.Serve(d.lis) }()
	head, records := d.auditLog.Head()
	logger.Printf("listening on %s (%s mode, at most %d runs at once); state %s; audit log %d records, head %.12s",
		socket, mode, maxConc, d.root.Dir(), records, head)
	return d, nil
}

func hasAnyClass(store *snapshot.Store) bool {
	for _, c := range snapshot.Classes {
		if _, err := store.Current(c.Name); err == nil {
			return true
		}
	}
	return false
}

// Socket is where the daemon listens.
func (d *Daemon) Socket() string { return d.socket }

// Mode is the mode every clone runs in.
func (d *Daemon) Mode() clone.Mode { return d.mode }

// Done receives the error that stopped serving, if serving stops on its
// own.
func (d *Daemon) Done() <-chan error { return d.served }

// Shutdown stops the daemon within ctx (ShutdownTimeout is a good bound):
// it stops accepting runs, stops the running ones (each still has its
// clone destroyed and its record written), stops serving, closes the
// audit log, removes the socket and lets the state folder's lock go.
func (d *Daemon) Shutdown(ctx context.Context) error {
	stopped := make(chan struct{})
	go func() {
		d.grpc.GracefulStop() // no new connections; waits for the calls in progress
		close(stopped)
	}()
	err := d.srv.Shutdown(ctx)
	select {
	case <-stopped:
	case <-ctx.Done():
		d.grpc.Stop() // cut off what is left
		<-stopped
	}
	removeEmptyDirs(d.jailParents...)
	return errors.Join(err, d.close())
}

// jailParents returns the two shared folders the jailer creates, as
// root, around every clone's own jail and cgroup: <jail>/firecracker
// (named after the firecracker binary) and the cgroup v2 folder
// microvm/. mounts opens /proc/self/mounts; a folder that cannot be found
// is left out.
func jailParents(jailDir, firecracker string, mounts func() (io.ReadCloser, error)) []string {
	var dirs []string
	// The jailer resolves symlinks in both paths; so do we.
	base, err := filepath.EvalSymlinks(jailDir)
	if err == nil {
		var fc string
		if fc, err = filepath.Abs(firecracker); err == nil {
			if resolved, err := filepath.EvalSymlinks(fc); err == nil {
				fc = resolved
			}
			dirs = append(dirs, filepath.Dir(jailer.JailDir(base, fc, "x")))
		}
	}
	if f, err := mounts(); err == nil {
		root, err := jailer.CgroupRoot(f)
		_ = f.Close()
		if err == nil {
			dirs = append(dirs, filepath.Join(root, jailer.ParentCgroup))
		}
	}
	return dirs
}

func readMounts() (io.ReadCloser, error) { return os.Open("/proc/self/mounts") }

// removeEmptyDirs removes each folder if it is empty, so that a clean
// jailer-mode run leaves nothing of root's behind. rmdir(2) only removes
// an empty folder (and a cgroup with no child cgroups and no processes),
// so it never deletes what a clone left behind: that stays for the
// reconciler (week 7) or the cleanup commands in the README. Errors
// (not empty, not there) are expected and ignored.
func removeEmptyDirs(dirs ...string) {
	for _, d := range dirs {
		_ = syscall.Rmdir(d)
	}
}

// close undoes Start, in reverse.
func (d *Daemon) close() error {
	var errs []error
	if d.lis != nil {
		_ = d.lis.Close()
		if err := os.Remove(d.socket); err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, fmt.Errorf("daemon: remove the socket: %w", err))
		}
	}
	if d.auditLog != nil {
		errs = append(errs, d.auditLog.Close())
	}
	if d.lock != nil {
		errs = append(errs, d.lock.Release())
	}
	return errors.Join(errs...)
}

// listen makes the daemon's socket at path. If something is already
// there, it must be a socket nobody answers on (left by a daemon that
// crashed); then it is removed. A live daemon is never pushed aside.
//
// Who may connect is decided by permissions (ADR-0006). Rootless: the
// folder is 0700 and the socket 0600, so only our user. Jailer mode: the
// folder is 0750 and the socket 0660, both of root and group (the
// "sandbox" group, or group); without such a group, 0700 and 0600, so
// only root. The folder's mode is set when the daemon creates it, or
// when it is the default one (ownDir); a folder given with --socket that
// already exists is left as it is (the socket's own mode still applies).
func listen(path string, jailerMode bool, group string, ownDir bool) (net.Listener, error) {
	gid := -1
	if jailerMode {
		var err error
		if gid, err = lookupGroup(group); err != nil {
			return nil, err
		}
	}
	dirMode, sockMode := os.FileMode(0o700), os.FileMode(0o600)
	if gid >= 0 {
		dirMode, sockMode = 0o750, 0o660
	}

	dir := filepath.Dir(path)
	if err := os.Mkdir(dir, dirMode); err == nil || (ownDir && errors.Is(err, fs.ErrExist)) {
		// Our own folder (new, or the default one): set its mode and group.
		// Never through a symlink someone put in its place.
		info, err := os.Lstat(dir)
		if err != nil {
			return nil, fmt.Errorf("daemon: %w", err)
		}
		if !info.IsDir() {
			return nil, fmt.Errorf("daemon: the socket's folder %s is not a folder (a symlink?)", dir)
		}
		if err := os.Chmod(dir, dirMode); err != nil {
			return nil, fmt.Errorf("daemon: %w", err)
		}
		if jailerMode {
			if err := os.Lchown(dir, 0, max(gid, 0)); err != nil {
				return nil, fmt.Errorf("daemon: %w", err)
			}
		}
	} else if !errors.Is(err, fs.ErrExist) {
		return nil, fmt.Errorf("daemon: create the socket's folder: %w", err)
	}

	if err := clearStaleSocket(path); err != nil {
		return nil, err
	}
	// The socket gets the permissions the umask leaves of 0777, so the
	// umask sets its mode (0600, or 0660 with a group) as it is created:
	// it is never more open than wanted, not even for a moment, and no
	// chmod is needed afterwards (chmod would follow a symlink put in its
	// place). At start-up nothing else creates files, so changing the
	// process-wide umask for this one call is safe.
	old := unix.Umask(int(0o777 &^ sockMode))
	lis, err := net.Listen("unix", path)
	unix.Umask(old)
	if err != nil {
		return nil, fmt.Errorf("daemon: listen on %s: %w", path, err)
	}
	ok := false
	defer func() {
		if !ok {
			_ = lis.Close()
		}
	}()
	if gid >= 0 {
		if err := os.Lchown(path, 0, gid); err != nil { // Lchown: never through a symlink
			return nil, fmt.Errorf("daemon: %w", err)
		}
	}
	ok = true
	return lis, nil
}

// clearStaleSocket removes a socket at path that nobody answers on. A
// live one, or anything that is not a socket, is an error.
func clearStaleSocket(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("daemon: %w", err)
	}
	if info.Mode().Type() != fs.ModeSocket {
		return fmt.Errorf("daemon: %s exists and is not a socket; remove it or choose another --socket", path)
	}
	conn, err := net.DialTimeout("unix", path, time.Second)
	if err == nil {
		_ = conn.Close()
		return fmt.Errorf("daemon: a sandboxd already listens on %s; stop it first, or choose another --socket", path)
	}
	if !errors.Is(err, syscall.ECONNREFUSED) {
		return fmt.Errorf("daemon: check %s: %w", path, err)
	}
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("daemon: remove the stale socket %s: %w", path, err)
	}
	return nil
}

// lookupGroup returns the GID of group (a name or a number), or of
// SocketGroup if group is empty and it exists, or -1 (nobody but root).
func lookupGroup(group string) (int, error) {
	name := group
	if name == "" {
		name = SocketGroup
	}
	if id, err := strconv.Atoi(name); err == nil {
		return id, nil
	}
	g, err := user.LookupGroup(name)
	if err != nil {
		var unknown user.UnknownGroupError
		if group == "" && errors.As(err, &unknown) {
			return -1, nil // no "sandbox" group: root only
		}
		return 0, fmt.Errorf("daemon: socket group %q: %w", name, err)
	}
	return strconv.Atoi(g.Gid)
}
