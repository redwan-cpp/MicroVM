// Package jailer builds the command line for Firecracker's jailer, the
// program that locks Firecracker in before it runs (ADR-0003, ADR-0005):
//
//   - a chroot of its own, <base>/firecracker/<id>/root, which holds only
//     what this one VM needs (hard links to the snapshot files, its own
//     scratch disk and log) and becomes Firecracker's "/";
//   - an unprivileged user: the jailer starts as root, then runs
//     Firecracker as a numeric UID and GID that no user or file on the
//     host has;
//   - a cgroup v2 of its own, /sys/fs/cgroup/microvm/<id>, with the size
//     class's cpu.max, memory.max and pids.max.
//
// Everything here is pure: it computes arguments and paths and checks
// them, and runs nothing. internal/clone does the work, as root.
// docs/learn/10-the-jailer.md explains each step.
package jailer

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strconv"
	"strings"
)

// ParentCgroup is the cgroup (under the cgroup v2 root) that holds one
// child cgroup per jailed clone.
const ParentCgroup = "microvm"

// DefaultUID and DefaultGID are the identity jailed Firecrackers run as.
// No user or group with these numbers exists, and none is created. They
// are outside every range something else hands out: systemd's dynamic
// service users (61184-65519), Debian's on-demand system users
// (60000-64999) and the subordinate IDs useradd gives each user for user
// namespaces (from 100000; see /etc/subuid). From week 4, each clone gets
// its own UID from this one upwards.
const (
	DefaultUID = 70000
	DefaultGID = 70000
)

// cpuPeriodUs is the cgroup cpu.max period: CPU time is counted per 100 ms.
const cpuPeriodUs = 100_000

// maxIDLen is the longest jail ID the jailer accepts.
const maxIDLen = 64

// Limits are the cgroup v2 limits for one jailed Firecracker.
type Limits struct {
	// CPUs caps CPU time at this many full CPUs (cpu.max).
	CPUs int
	// MemoryBytes caps memory, page cache included (memory.max).
	MemoryBytes int64
	// Pids caps the number of processes and threads (pids.max).
	Pids int
}

// Spec is everything one jailed Firecracker needs.
type Spec struct {
	// ID names the jail, the chroot folder and the cgroup. Letters, digits
	// and "-" only, at most 64 (state.NewID makes such IDs).
	ID string
	// UID and GID are the unprivileged identity Firecracker runs as. 0
	// (root) is refused.
	UID, GID int
	// ChrootBase is the jailer's --chroot-base-dir, <state>/jail. It must
	// be absolute and free of symlinks: the jailer resolves symlinks, and
	// the paths here must match the ones it uses.
	ChrootBase string
	// Firecracker is the firecracker program (--exec-file), absolute and
	// free of symlinks for the same reason. The jailer copies it into the
	// chroot and names the chroot folder after it.
	Firecracker string
	// Limits are the cgroup limits.
	Limits Limits
	// FirecrackerArgs are passed to Firecracker after the jailer's own
	// (--id and start times, which the jailer adds).
	FirecrackerArgs []string
	// Shared are files linked into the chroot root unchanged, keeping
	// their owner and mode: the snapshot's store files, which belong to
	// the user who built them and are mode 0444, so the jailed user can
	// read them and never change them.
	Shared []string
	// Owned are the names of the per-clone files in the chroot root (the
	// scratch disk, the log). The caller creates them and gives them to
	// UID:GID, because Firecracker opens them for writing.
	Owned []string
}

// Link is one hard link to make: From (outside) to To (in the chroot).
type Link struct {
	From, To string
}

// Plan is what to run and where things go, computed from a Spec.
type Plan struct {
	// Args are the jailer's arguments (without the program name).
	Args []string
	// JailDir is <ChrootBase>/<exec name>/<ID>, which the jailer creates;
	// delete it after the VM is gone.
	JailDir string
	// ChrootDir is JailDir/root: Firecracker's "/" and working directory.
	// The jailer makes it 0700 and gives it to UID:GID, so Firecracker can
	// create its sockets there.
	ChrootDir string
	// Links are the Shared files, linked into ChrootDir under their base
	// names.
	Links []Link
	// Owned are the Owned files' paths in ChrootDir.
	Owned []string
	// Cgroup is the clone's cgroup, relative to the cgroup v2 root:
	// microvm/<ID>.
	Cgroup string
}

// Build checks spec and computes its Plan.
func Build(spec Spec) (*Plan, error) {
	if err := validID(spec.ID); err != nil {
		return nil, err
	}
	if spec.UID <= 0 || spec.GID <= 0 {
		return nil, fmt.Errorf("jailer: uid %d, gid %d: Firecracker must not run as root", spec.UID, spec.GID)
	}
	for _, p := range []string{spec.ChrootBase, spec.Firecracker} {
		if !filepath.IsAbs(p) || filepath.Clean(p) != p {
			return nil, fmt.Errorf("jailer: %q must be a clean absolute path", p)
		}
	}
	l := spec.Limits
	if l.CPUs <= 0 || l.MemoryBytes <= 0 || l.Pids <= 0 {
		return nil, fmt.Errorf("jailer: limits %+v must all be positive", l)
	}

	execName := filepath.Base(spec.Firecracker)
	jailDir := filepath.Join(spec.ChrootBase, execName, spec.ID)
	plan := &Plan{
		JailDir:   jailDir,
		ChrootDir: filepath.Join(jailDir, "root"),
		Cgroup:    ParentCgroup + "/" + spec.ID,
	}
	seen := make(map[string]bool)
	for _, from := range spec.Shared {
		name := filepath.Base(from)
		if !filepath.IsAbs(from) || seen[name] {
			return nil, fmt.Errorf("jailer: shared file %q must be absolute and its name unique", from)
		}
		seen[name] = true
		plan.Links = append(plan.Links, Link{From: from, To: filepath.Join(plan.ChrootDir, name)})
	}
	for _, name := range spec.Owned {
		if name != filepath.Base(name) || name == "." || name == ".." || seen[name] {
			return nil, fmt.Errorf("jailer: owned file %q must be a unique plain name", name)
		}
		seen[name] = true
		plan.Owned = append(plan.Owned, filepath.Join(plan.ChrootDir, name))
	}

	plan.Args = []string{
		"--id", spec.ID,
		"--exec-file", spec.Firecracker,
		"--uid", strconv.Itoa(spec.UID),
		"--gid", strconv.Itoa(spec.GID),
		"--chroot-base-dir", spec.ChrootBase,
		"--cgroup-version", "2",
		"--parent-cgroup", ParentCgroup,
		"--cgroup", fmt.Sprintf("cpu.max=%d %d", l.CPUs*cpuPeriodUs, cpuPeriodUs),
		"--cgroup", fmt.Sprintf("memory.max=%d", l.MemoryBytes),
		"--cgroup", fmt.Sprintf("pids.max=%d", l.Pids),
		// No --new-pid-ns: with it the jailer forks, and the PID we
		// started would not be Firecracker's (ADR-0005). No --daemonize:
		// we keep Firecracker's stdout and stderr, the guest console.
		"--",
	}
	plan.Args = append(plan.Args, spec.FirecrackerArgs...)
	return plan, nil
}

// validID checks a jail ID the way the jailer does, but ASCII only (the
// jailer accepts any Unicode letter).
func validID(id string) error {
	if id == "" || len(id) > maxIDLen {
		return fmt.Errorf("jailer: ID %q must be 1 to %d characters", id, maxIDLen)
	}
	for _, r := range id {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '-' {
			return fmt.Errorf("jailer: ID %q may only hold ASCII letters, digits and '-'", id)
		}
	}
	return nil
}

// CgroupRoot returns where the cgroup v2 hierarchy is mounted, read from
// mounts (the contents of /proc/self/mounts): the first cgroup2 mount, the
// same one the jailer picks.
func CgroupRoot(mounts io.Reader) (string, error) {
	sc := bufio.NewScanner(mounts)
	for sc.Scan() {
		// device mountpoint fstype options dump pass
		fields := strings.Fields(sc.Text())
		if len(fields) >= 3 && fields[2] == "cgroup2" {
			return unescapeMount(fields[1]), nil
		}
	}
	if err := sc.Err(); err != nil {
		return "", fmt.Errorf("jailer: read mounts: %w", err)
	}
	return "", errors.New("jailer: no cgroup v2 filesystem is mounted")
}

// unescapeMount undoes the octal escapes (\040 for a space) in a mount
// point from /proc/self/mounts.
func unescapeMount(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			if n, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(n))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// CheckIdentity makes sure uid and gid belong to nobody: no user in
// passwd, no group in group, and inside no one's subordinate range in
// subuid and subgid (the /etc files of those names). A jailed Firecracker
// sharing its UID with anything else could be signalled, traced or have
// its files read by it. A nil reader skips that check.
func CheckIdentity(uid, gid int, passwd, group, subuid, subgid io.Reader) error {
	for _, c := range []struct {
		file string
		r    io.Reader
		id   int
		ids  func(fields []string) (lo, hi int, ok bool)
	}{
		{"/etc/passwd", passwd, uid, idField(2)},
		{"/etc/group", group, gid, idField(2)},
		{"/etc/subuid", subuid, uid, subRange},
		{"/etc/subgid", subgid, gid, subRange},
	} {
		if c.r == nil {
			continue
		}
		sc := bufio.NewScanner(c.r)
		for sc.Scan() {
			fields := strings.Split(strings.TrimSpace(sc.Text()), ":")
			if lo, hi, ok := c.ids(fields); ok && c.id >= lo && c.id <= hi {
				return fmt.Errorf("jailer: ID %d is already used by %q in %s; jailed Firecrackers need an ID nobody else has",
					c.id, fields[0], c.file)
			}
		}
		if err := sc.Err(); err != nil {
			return fmt.Errorf("jailer: read %s: %w", c.file, err)
		}
	}
	return nil
}

// idField reads the numeric ID in field i of a passwd or group line.
func idField(i int) func([]string) (int, int, bool) {
	return func(fields []string) (int, int, bool) {
		if len(fields) <= i {
			return 0, 0, false
		}
		id, err := strconv.Atoi(fields[i])
		return id, id, err == nil
	}
}

// subRange reads "name:start:count" from /etc/subuid or /etc/subgid.
func subRange(fields []string) (int, int, bool) {
	if len(fields) != 3 {
		return 0, 0, false
	}
	start, err1 := strconv.Atoi(fields[1])
	count, err2 := strconv.Atoi(fields[2])
	if err1 != nil || err2 != nil || count <= 0 {
		return 0, 0, false
	}
	return start, start + count - 1, true
}
