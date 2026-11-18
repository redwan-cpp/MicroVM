package clone

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/redwan-cpp/MicroVM/internal/jailer"
	"github.com/redwan-cpp/MicroVM/internal/snapshot"
	"github.com/redwan-cpp/MicroVM/internal/state"
	"github.com/redwan-cpp/MicroVM/internal/vmm"
)

// cgroupRmdirWait is how long Destroy keeps trying to remove a clone's
// cgroup. The kernel may take a moment to notice that its last process is
// gone (rmdir fails with EBUSY until then).
const cgroupRmdirWait = time.Second

// jail is what jailer mode adds to a clone, so Destroy can remove it.
type jail struct {
	dir    string // <state>/jail/firecracker/<id>
	cgroup string // /sys/fs/cgroup/microvm/<id>
}

// prepareJailed checks that jailer mode can work, then builds the chroot:
//
//   - hard links to the snapshot's files, which keep their owner (the user
//     who built the store) and mode 0444: the jailed user can read them
//     and never change them;
//   - a new scratch disk, given to the jailed user (Firecracker writes it);
//     vmm gives the log file to that user too (vmm.Config.LogOwner).
//
// The jailer does the rest when it starts: it copies Firecracker in,
// creates /dev/kvm, makes the chroot 0700 and the jailed user's, sets up
// the cgroup and changes to the jailed user.
func (c *Clone) prepareJailed(root *state.Root, opts Options) (vmm.Config, error) {
	j := opts.Jailer
	if euid := os.Geteuid(); euid != 0 {
		return vmm.Config{}, fmt.Errorf("jailer mode needs root (running as uid %d)", euid)
	}
	if !root.CheckedForJailer() {
		return vmm.Config{}, errors.New("jailer mode needs the state folder opened with state.Options{Jailer: true}")
	}
	if err := checkIdentity(j.UID, j.GID); err != nil {
		return vmm.Config{}, err
	}
	cgroupRoot := j.CgroupRoot
	if cgroupRoot == "" {
		var err error
		if cgroupRoot, err = readCgroupRoot(); err != nil {
			return vmm.Config{}, err
		}
	}
	// The jailer resolves symlinks in both paths; ours must match.
	firecracker, err := realPath(opts.Firecracker)
	if err != nil {
		return vmm.Config{}, err
	}
	base, err := realPath(root.JailDir())
	if err != nil {
		return vmm.Config{}, err
	}

	v := c.version
	plan, err := jailer.Build(jailer.Spec{
		ID:          state.NewID(),
		UID:         j.UID,
		GID:         j.GID,
		ChrootBase:  base,
		Firecracker: firecracker,
		Limits: jailer.Limits{
			CPUs:        c.class.VCPUs,
			MemoryBytes: int64(c.class.CgroupMemoryMiB) << 20,
			Pids:        c.class.CgroupPids,
		},
		Shared: []string{v.Path(snapshot.StateName), v.Path(snapshot.MemName), v.Path(snapshot.RootfsName)},
		Owned:  []string{snapshot.ScratchName},
	})
	if err != nil {
		return vmm.Config{}, err
	}
	c.id = filepath.Base(plan.JailDir)
	// A fresh random ID: if its folder exists, something is badly wrong,
	// and Destroy must not delete what is there.
	if _, err := os.Lstat(plan.JailDir); err == nil {
		return vmm.Config{}, fmt.Errorf("jail folder %s already exists", plan.JailDir)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return vmm.Config{}, fmt.Errorf("check the jail folder: %w", err)
	}
	c.dir = plan.ChrootDir
	c.jail = &jail{dir: plan.JailDir, cgroup: filepath.Join(cgroupRoot, plan.Cgroup)}

	if err := os.MkdirAll(plan.ChrootDir, 0o700); err != nil {
		return vmm.Config{}, err
	}
	for _, l := range plan.Links {
		if err := os.Link(l.From, l.To); err != nil {
			return vmm.Config{}, fmt.Errorf("link %s into the chroot: %w", filepath.Base(l.From), err)
		}
	}
	for _, path := range plan.Owned { // the scratch disk
		if err := snapshot.CreateScratch(path, c.class.ScratchMiB); err != nil {
			return vmm.Config{}, err
		}
		if err := os.Lchown(path, j.UID, j.GID); err != nil {
			return vmm.Config{}, fmt.Errorf("give %s to the jailed user: %w", filepath.Base(path), err)
		}
	}
	return vmm.Config{
		Binary:   firecracker,
		RunDir:   plan.ChrootDir,
		Launcher: &vmm.Launcher{Path: j.Binary, Args: plan.Args},
		LogOwner: &vmm.Owner{UID: j.UID, GID: j.GID},
	}, nil
}

// kill kills every process in the clone's cgroup at once (cgroup.kill,
// Linux 5.14+). Firecracker runs as another user and the jailer's change
// of user cleared its parent-death signal, so this, not Pdeathsig, is what
// makes sure nothing of the clone keeps running. A cgroup that does not
// exist (the jailer never got that far, or it is already gone) is fine.
func (j *jail) kill() error {
	f, err := os.OpenFile(filepath.Join(j.cgroup, "cgroup.kill"), os.O_WRONLY, 0)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	_, err = f.WriteString("1")
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	return err
}

// remove deletes the jail folder and the (by now empty) cgroup.
func (j *jail) remove() error {
	err := os.RemoveAll(j.dir)
	deadline := time.Now().Add(cgroupRmdirWait)
	for {
		rmErr := syscall.Rmdir(j.cgroup)
		switch {
		case rmErr == nil, errors.Is(rmErr, syscall.ENOENT):
			return err
		case errors.Is(rmErr, syscall.EBUSY) && time.Now().Before(deadline):
			time.Sleep(10 * time.Millisecond)
		default:
			return errors.Join(err, fmt.Errorf("remove cgroup %s: %w", j.cgroup, rmErr))
		}
	}
}

// checkIdentity refuses a jailed UID or GID that the host's user, group
// or subordinate ID files already use.
func checkIdentity(uid, gid int) error {
	var readers [4]io.Reader
	for i, name := range []string{"/etc/passwd", "/etc/group", "/etc/subuid", "/etc/subgid"} {
		f, err := os.Open(name) //nolint:gosec // G304: a fixed list of system files
		if errors.Is(err, fs.ErrNotExist) {
			continue // no such file: nothing in it can clash
		}
		if err != nil {
			return err
		}
		defer f.Close() //nolint:errcheck // read-only file
		readers[i] = f
	}
	return jailer.CheckIdentity(uid, gid, readers[0], readers[1], readers[2], readers[3])
}

// readCgroupRoot finds the cgroup v2 mount, the way the jailer does.
func readCgroupRoot() (string, error) {
	f, err := os.Open("/proc/self/mounts")
	if err != nil {
		return "", err
	}
	defer f.Close() //nolint:errcheck // read-only file
	return jailer.CgroupRoot(f)
}

// realPath returns path made absolute, with every symlink resolved.
func realPath(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(abs)
}
