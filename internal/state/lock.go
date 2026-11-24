package state

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// ErrLocked means another process holds the state folder's lock: a
// running sandboxd (or a moment of `sandboxd build-snapshots` pruning).
var ErrLocked = errors.New("state: the state folder is locked by another process (is a sandboxd running on it?)")

// Lock is a held lock on a state folder. Release it with Release.
type Lock struct {
	f *os.File
}

// Lock takes the state folder's lock, <state>/lock, without waiting: if
// another process holds it, Lock returns ErrLocked at once.
//
// It is an exclusive flock(2) lock. sandboxd holds it for as long as it
// runs, so two daemons can never share a state folder (and its audit log),
// whatever their sockets; `sandboxd build-snapshots` takes it only to
// prune old snapshot versions, and skips pruning while a daemon holds it.
// The kernel releases the lock when its holder exits, even after a crash,
// so a lock is never left stale. The file itself stays.
//
// Run as root on a state folder that belongs to someone else (jailer mode
// on the user's state folder), Lock gives the lock file to that owner, so
// it never leaves a root-owned file outside jail/.
func (r *Root) Lock() (*Lock, error) {
	path := filepath.Join(r.dir, lockName)
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|unix.O_NOFOLLOW, 0o600) //nolint:gosec // G304: our own state folder
	if err != nil {
		return nil, fmt.Errorf("state: open the lock: %w", err)
	}
	if err := flock(f, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, unix.EWOULDBLOCK) {
			return nil, fmt.Errorf("%w: %s", ErrLocked, path)
		}
		return nil, fmt.Errorf("state: lock %s: %w", path, err)
	}
	if os.Geteuid() == 0 {
		uid, gid, err := r.Owner()
		if err == nil {
			err = f.Chown(uid, gid)
		}
		if err != nil {
			_ = f.Close()
			return nil, fmt.Errorf("state: give the lock to the state folder's owner: %w", err)
		}
	}
	return &Lock{f: f}, nil
}

// Release lets the lock go. It may be called more than once.
func (l *Lock) Release() error {
	if l == nil || l.f == nil {
		return nil
	}
	err := l.f.Close() // closing the last descriptor releases a flock lock
	l.f = nil
	return err
}

// flock calls flock(2) on f's descriptor.
func flock(f *os.File, how int) error {
	raw, err := f.SyscallConn()
	if err != nil {
		return err
	}
	var lockErr error
	if err := raw.Control(func(fd uintptr) {
		for {
			lockErr = unix.Flock(int(fd), how)
			if !errors.Is(lockErr, unix.EINTR) {
				return
			}
		}
	}); err != nil {
		return err
	}
	return lockErr
}

// Owner returns the user and group that own the state folder.
func (r *Root) Owner() (uid, gid int, err error) {
	var st unix.Stat_t
	if err := unix.Stat(r.dir, &st); err != nil {
		return 0, 0, fmt.Errorf("state: %w", err)
	}
	return int(st.Uid), int(st.Gid), nil
}
