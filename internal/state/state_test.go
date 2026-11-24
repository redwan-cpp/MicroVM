package state

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func TestOpenCreatesPrivateFolders(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	r, err := Open(dir, Options{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	for _, sub := range []string{r.Dir(), r.SnapshotsDir(), r.RunsDir(), r.JailDir()} {
		info, err := os.Stat(sub)
		if err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 {
			t.Errorf("%s: %v, %v; want a folder with mode 0700", sub, info, err)
		}
	}
	// A second Open of the same folder is fine.
	if _, err := Open(dir, Options{}); err != nil {
		t.Errorf("second Open: %v", err)
	}
}

func TestOpenMakesThePathAbsolute(t *testing.T) {
	t.Chdir(t.TempDir())
	r, err := Open("state", Options{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if !filepath.IsAbs(r.Dir()) {
		t.Errorf("Dir() = %q, want an absolute path", r.Dir())
	}
}

func TestOpenRefusesFoldersOnDifferentFilesystems(t *testing.T) {
	// /dev/shm is its own (in-memory) filesystem on Linux. A jail/ that is
	// really a link to a folder there must be refused.
	other, err := os.MkdirTemp("/dev/shm", "state-test-")
	if err != nil {
		t.Skipf("no /dev/shm: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(other) })
	dir := t.TempDir()
	var a, b unix.Stat_t
	if unix.Stat(dir, &a) != nil || unix.Stat(other, &b) != nil || a.Dev == b.Dev {
		t.Skip("the test folder and /dev/shm are on the same filesystem here")
	}
	if err := os.Symlink(other, filepath.Join(dir, "jail")); err != nil {
		t.Fatal(err)
	}

	_, err = Open(dir, Options{})
	if err == nil || !strings.Contains(err.Error(), "different filesystems") {
		t.Errorf("err = %v, want a different-filesystems error", err)
	}
}

func TestCheckSocketPaths(t *testing.T) {
	if err := checkSocketPath(runSocketPath("/srv/state/runs")); err != nil {
		t.Errorf("short run path: %v", err)
	}
	if err := checkSocketPath(jailSocketPath("/srv/state/jail", "/opt/firecracker")); err != nil {
		t.Errorf("short jail path: %v", err)
	}

	// A jailed socket is <jail>/<binary name>/<id>/root/api.sock. Find the
	// longest jail folder that still fits with a binary called
	// "firecracker", then go one over.
	room := maxSocketPath - len("/firecracker/") - IDLen - len("/root/api.sock")
	fits := "/" + strings.Repeat("j", room-1)
	if err := checkSocketPath(jailSocketPath(fits, "/opt/firecracker")); err != nil {
		t.Errorf("jail folder of %d bytes: %v, want it to fit exactly", len(fits), err)
	}
	if err := checkSocketPath(jailSocketPath(fits+"j", "/opt/firecracker")); err == nil {
		t.Errorf("jail folder of %d bytes: no error, want one", len(fits)+1)
	}
	// The binary's name counts: a longer one no longer fits.
	if err := checkSocketPath(jailSocketPath(fits, "/opt/firecracker-v1.17.0")); err == nil {
		t.Error("a longer binary name still fits, want an error")
	}
}

func TestOpenRefusesALongPath(t *testing.T) {
	dir := filepath.Join(t.TempDir(), strings.Repeat("d", 100))
	_, err := Open(dir, Options{})
	if err == nil || !strings.Contains(err.Error(), "socket paths") {
		t.Errorf("err = %v, want a socket path error", err)
	}
}

func TestCheckMountFlags(t *testing.T) {
	if err := checkMountFlags("/x", unix.ST_RELATIME|unix.ST_NOSUID); err != nil {
		t.Errorf("without nodev or noexec: %v", err)
	}
	if err := checkMountFlags("/x", unix.ST_NODEV|unix.ST_NOSUID); err == nil || !strings.Contains(err.Error(), "nodev") {
		t.Errorf("with nodev: %v, want a nodev error", err)
	}
	if err := checkMountFlags("/x", unix.ST_NOEXEC); err == nil || !strings.Contains(err.Error(), "noexec") {
		t.Errorf("with noexec: %v, want a noexec error", err)
	}
}

func TestOpenJailerNeedsTheBinary(t *testing.T) {
	dir := t.TempDir()
	if _, err := Open(dir, Options{Jailer: true}); err == nil || !strings.Contains(err.Error(), "Options.Firecracker") {
		t.Errorf("jailer Open without a binary: %v, want an error", err)
	}
	if _, err := Open(dir, Options{Jailer: true, Firecracker: filepath.Join(dir, "missing")}); err == nil {
		t.Error("jailer Open with a missing binary succeeded, want an error")
	}
}

func TestOpenChecksNodevInJailerMode(t *testing.T) {
	// /dev/shm is mounted nodev on most Linux systems.
	var sfs unix.Statfs_t
	if err := unix.Statfs("/dev/shm", &sfs); err != nil || sfs.Flags&unix.ST_NODEV == 0 {
		t.Skip("/dev/shm is missing or not mounted nodev here")
	}
	dir, err := os.MkdirTemp("/dev/shm", "state-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	if r, err := Open(dir, Options{}); err != nil || r.CheckedForJailer() {
		t.Errorf("rootless Open: %v, want nodev to be fine (and not checked for the jailer)", err)
	}
	if _, err := Open(dir, Options{Jailer: true, Firecracker: os.Args[0]}); err == nil || !strings.Contains(err.Error(), "nodev") {
		t.Errorf("jailer Open: %v, want a nodev error", err)
	}
}

func TestCheckedForJailer(t *testing.T) {
	// Not t.TempDir(): its name includes the test's, and the socket paths
	// would get too long under a longer $TMPDIR.
	dir, err := os.MkdirTemp("", "st-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	var sfs unix.Statfs_t
	if err := unix.Statfs(dir, &sfs); err != nil || sfs.Flags&(unix.ST_NODEV|unix.ST_NOEXEC) != 0 {
		t.Skip("the test folder is on a nodev or noexec filesystem here")
	}
	r, err := Open(dir, Options{Jailer: true, Firecracker: os.Args[0]})
	if err != nil || !r.CheckedForJailer() {
		t.Errorf("Open with Jailer: %v, checked %v; want it checked", err, r != nil && r.CheckedForJailer())
	}
}

func TestNewRunDir(t *testing.T) {
	r, err := Open(t.TempDir(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	seen := make(map[string]bool)
	for range 20 {
		id, dir, err := r.NewRunDir()
		if err != nil {
			t.Fatalf("NewRunDir: %v", err)
		}
		if !regexp.MustCompile(`^r-[0-9a-f]{16}$`).MatchString(id) || len(id) != IDLen {
			t.Errorf("id %q, want r- and 16 hex digits", id)
		}
		if dir != filepath.Join(r.RunsDir(), id) {
			t.Errorf("dir %s, want %s", dir, filepath.Join(r.RunsDir(), id))
		}
		if info, err := os.Stat(dir); err != nil || info.Mode().Perm() != 0o700 {
			t.Errorf("%s: %v, %v; want mode 0700", dir, info, err)
		}
		if seen[id] {
			t.Errorf("id %s handed out twice", id)
		}
		seen[id] = true
	}
}

func TestDir(t *testing.T) {
	t.Setenv(EnvVar, "")
	if got := Dir("/default"); got != "/default" {
		t.Errorf("Dir = %q without %s, want the default", got, EnvVar)
	}
	t.Setenv(EnvVar, "/elsewhere")
	if got := Dir("/default"); got != "/elsewhere" {
		t.Errorf("Dir = %q with %s set, want /elsewhere", got, EnvVar)
	}
}
