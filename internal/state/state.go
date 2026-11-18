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
	// Jailer means the caller will start microVMs through the jailer. The
	// jailer creates device files (/dev/kvm) inside each chroot, and a
	// filesystem mounted "nodev" refuses to open them, so Open checks
	// that the state folder's filesystem is not mounted that way.
	Jailer bool
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
//   - the longest Unix socket path a microVM will get, in runs/ or in a
//     jailer chroot, fits in the 107 bytes Linux allows;
//   - with opts.Jailer, the filesystem is not mounted nodev.
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
	if err := checkSocketPaths(r.RunsDir(), r.JailDir()); err != nil {
		return nil, err
	}
	if opts.Jailer {
		var sfs unix.Statfs_t
		if err := unix.Statfs(r.JailDir(), &sfs); err != nil {
			return nil, fmt.Errorf("state: %s: %w", r.JailDir(), err)
		}
		if err := checkNodev(r.JailDir(), sfs.Flags); err != nil {
			return nil, err
		}
		r.jailer = true
	}
	return r, nil
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

// checkSocketPaths checks that the longest socket path of a rootless run
// (<runs>/<id>/api.sock) and of a jailed one
// (<jail>/firecracker/<id>/root/api.sock) fit in a Unix socket address.
func checkSocketPaths(runs, jail string) error {
	id := strings.Repeat("x", IDLen) // every ID has this length
	for _, path := range []string{
		filepath.Join(runs, id, "api.sock"),
		filepath.Join(jail, "firecracker", id, "root", "api.sock"),
	} {
		if len(path) > maxSocketPath {
			return fmt.Errorf("state: socket paths under %s would be %d bytes, Linux allows at most %d; "+
				"use a shorter state folder (set %s)", filepath.Dir(filepath.Dir(path)), len(path), maxSocketPath, EnvVar)
		}
	}
	return nil
}

// checkNodev fails if flags (Statfs_t.Flags of dir's filesystem) say it is
// mounted nodev.
func checkNodev(dir string, flags int64) error {
	if flags&unix.ST_NODEV != 0 {
		return fmt.Errorf("state: %s is on a filesystem mounted nodev, so the jailer's /dev/kvm "+
			"inside a chroot could not be opened; move the state folder (set %s)", dir, EnvVar)
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
