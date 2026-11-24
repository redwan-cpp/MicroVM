// Package state is the one folder where MicroVM Sandbox keeps what it
// makes at run time: the snapshot store, a run folder per microVM, and (in
// jailer mode) the jailer's chroots.
//
//	<state>/
//	  snapshots/   base snapshots, one versioned folder per size class
//	  runs/        one private folder per rootless microVM
//	  jail/        the jailer's --chroot-base-dir (jailer mode)
//
// Everything is in one place because of hard links. A clone does not copy
// the snapshot's memory file, it links to it, so that every clone shares
// the same page-cache pages (ADR-0003, decision 3). A hard link can never
// cross from one filesystem to another, so the three folders must be on the
// same one. Open checks that once, with a clear error, instead of copying
// anything silently. (On many machines /tmp is a separate in-memory
// filesystem, which is why the default is under guest/out/ instead.)
package state

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/redwan-cpp/MicroVM/internal/jailer"
)

// EnvVar names the environment variable that moves the state folder.
const EnvVar = "MICROVM_STATE_DIR"

// DefaultDir is the state folder when EnvVar is not set, relative to the
// repository root. It is next to the guest image, on the same filesystem.
const DefaultDir = "guest/out/state"

// The names inside the state folder.
const (
	snapshotsName = "snapshots"
	runsName      = "runs"
	jailName      = "jail"
)

// IDLen is the length of every run ID: "r-" and 16 hex digits.
const IDLen = len("r-") + 16

// SocketNameLen is the length of the longest socket name in a run folder,
// "api.sock" (Firecracker's API socket; the vsock socket "v.sock" is
// shorter).
const SocketNameLen = len("api.sock")

// maxSocketPath is the longest Unix socket path Linux accepts: sun_path in
// struct sockaddr_un holds 108 bytes, including a closing NUL byte.
const maxSocketPath = 107

// Dir returns the state folder: $MICROVM_STATE_DIR if it is set, otherwise
// def (normally DefaultDir, resolved against the repository root).
func Dir(def string) string {
	if dir := os.Getenv(EnvVar); dir != "" {
		return dir
	}
	return def
}

// Options changes what Open checks.
type Options struct {
	// Jailer means the caller will start microVMs through the jailer. Then
	// Open also checks that the state folder's filesystem is mounted
	// neither "nodev" (the jailer creates /dev/kvm inside each chroot, and
	// nodev refuses to open device files) nor "noexec" (the jailer runs
	// its copy of firecracker from inside the chroot), and that socket
	// paths in a chroot fit.
	Jailer bool
	// Firecracker is the firecracker program the jailer will run; jailer
	// mode needs it. The jailer names each chroot folder after it, so its
	// name is part of every socket path in a chroot.
	Firecracker string
}

// Root is an opened, checked state folder.
type Root struct {
	dir    string
	jailer bool // Open checked what jailer mode needs
}

// Open creates the state folder dir and its subfolders if they are missing
// (mode 0700: only our user may look inside) and checks, once:
//
//   - snapshots/, runs/ and jail/ are all on the same filesystem, so hard
//     links between them work;
//   - the longest Unix socket path a microVM will get, in runs/ (and with
//     opts.Jailer in a chroot), fits in the 107 bytes Linux allows;
//   - with opts.Jailer, the filesystem is not mounted nodev or noexec.
func Open(dir string, opts Options) (*Root, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("state: %w", err)
	}
	r := &Root{dir: abs}

	devices := make(map[string]uint64)
	for _, sub := range []string{snapshotsName, runsName, jailName} {
		path := filepath.Join(abs, sub)
		if err := os.MkdirAll(path, 0o700); err != nil {
			return nil, fmt.Errorf("state: create %s: %w", path, err)
		}
		var st unix.Stat_t
		if err := unix.Stat(path, &st); err != nil {
			return nil, fmt.Errorf("state: %s: %w", path, err)
		}
		devices[path] = st.Dev
	}
	if err := sameFilesystem(devices); err != nil {
		return nil, err
	}
	if err := checkSocketPath(runSocketPath(r.RunsDir())); err != nil {
		return nil, err
	}
	if opts.Jailer {
		if err := r.checkJailer(opts.Firecracker); err != nil {
			return nil, err
		}
		r.jailer = true
	}
	return r, nil
}

// checkJailer checks what jailer mode needs from the state folder.
func (r *Root) checkJailer(firecracker string) error {
	if firecracker == "" {
		return errors.New("state: jailer mode needs Options.Firecracker")
	}
	// The jailer resolves symlinks in both paths, so we do too.
	fc, err := filepath.Abs(firecracker)
	if err == nil {
		fc, err = filepath.EvalSymlinks(fc)
	}
	if err != nil {
		return fmt.Errorf("state: firecracker program: %w", err)
	}
	base, err := filepath.EvalSymlinks(r.JailDir())
	if err != nil {
		return fmt.Errorf("state: %w", err)
	}
	if err := checkSocketPath(jailSocketPath(base, fc)); err != nil {
		return err
	}
	var sfs unix.Statfs_t
	if err := unix.Statfs(base, &sfs); err != nil {
		return fmt.Errorf("state: %s: %w", base, err)
	}
	return checkMountFlags(base, sfs.Flags)
}

// sameFilesystem checks that every folder in devices (path → st_dev) is on
// the same filesystem.
func sameFilesystem(devices map[string]uint64) error {
	var firstPath string
	var firstDev uint64
	for path, dev := range devices {
		if firstPath == "" {
			firstPath, firstDev = path, dev
			continue
		}
		if dev != firstDev {
			return fmt.Errorf("state: %s and %s are on different filesystems; "+
				"hard links cannot cross filesystems, so keep the whole state folder on one (set %s)",
				firstPath, path, EnvVar)
		}
	}
	return nil
}

// runSocketPath is the longest socket path of a rootless run:
// <runs>/<id>/api.sock.
func runSocketPath(runs string) string {
	return filepath.Join(runs, strings.Repeat("x", IDLen), "api.sock") // every ID has this length
}

// jailSocketPath is the longest socket path of a jailed run, in the chroot
// the jailer makes for the program firecracker under jailBase:
// <jail>/<binary name>/<id>/root/api.sock.
func jailSocketPath(jailBase, firecracker string) string {
	return filepath.Join(jailer.ChrootDir(jailBase, firecracker, strings.Repeat("x", IDLen)), "api.sock")
}

// checkSocketPath checks that path fits in a Unix socket address.
func checkSocketPath(path string) error {
	if len(path) > maxSocketPath {
		return fmt.Errorf("state: socket paths such as %s would be %d bytes, Linux allows at most %d; "+
			"use a shorter state folder (set %s)", path, len(path), maxSocketPath, EnvVar)
	}
	return nil
}

// checkMountFlags fails if flags (Statfs_t.Flags of dir's filesystem) say
// it is mounted nodev or noexec.
func checkMountFlags(dir string, flags int64) error {
	if flags&unix.ST_NODEV != 0 {
		return fmt.Errorf("state: %s is on a filesystem mounted nodev, so the jailer's /dev/kvm "+
			"inside a chroot could not be opened; move the state folder (set %s)", dir, EnvVar)
	}
	if flags&unix.ST_NOEXEC != 0 {
		return fmt.Errorf("state: %s is on a filesystem mounted noexec, so the jailer could not run "+
			"its copy of firecracker inside a chroot; move the state folder (set %s)", dir, EnvVar)
	}
	return nil
}

// CheckedForJailer reports whether Open checked this folder for jailer
// mode (Options.Jailer).
func (r *Root) CheckedForJailer() bool { return r.jailer }

// Dir is the state folder itself, as an absolute path.
func (r *Root) Dir() string { return r.dir }

// SnapshotsDir holds the snapshot store (internal/snapshot).
func (r *Root) SnapshotsDir() string { return filepath.Join(r.dir, snapshotsName) }

// RunsDir holds one folder per rootless microVM.
func (r *Root) RunsDir() string { return filepath.Join(r.dir, runsName) }

// JailDir is the jailer's --chroot-base-dir. The jailer puts each chroot at
// <JailDir>/firecracker/<id>/root.
func (r *Root) JailDir() string { return filepath.Join(r.dir, jailName) }

// NewID returns a fresh random run ID, "r-" and 16 hex digits. The jailer
// accepts it as a jail ID (letters, digits and "-", at most 64).
func NewID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b) // never fails (crypto/rand documents this)
	return "r-" + hex.EncodeToString(b)
}

// NewRunDir creates a fresh, private (mode 0700) run folder
// <runs>/<id> and returns its ID and path. Nobody else can create,
// see or change anything inside it.
func (r *Root) NewRunDir() (id, dir string, err error) {
	for range 3 {
		id = NewID()
		dir = filepath.Join(r.RunsDir(), id)
		err = os.Mkdir(dir, 0o700)
		if !errors.Is(err, fs.ErrExist) {
			break // 64 random bits: a clash is practically impossible
		}
	}
	if err != nil {
		return "", "", fmt.Errorf("state: create run folder: %w", err)
	}
	return id, dir, nil
}
