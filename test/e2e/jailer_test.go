//go:build e2e && jailer

package e2e

import (
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

	"github.com/redwan-cpp/MicroVM/internal/clone"
	"github.com/redwan-cpp/MicroVM/internal/jailer"
	"github.com/redwan-cpp/MicroVM/internal/snapshot"
	"github.com/redwan-cpp/MicroVM/internal/state"
	"github.com/redwan-cpp/MicroVM/internal/vmm"
)

// The jailer test needs root, and nothing is ever built or run with go as
// root. So `make e2e-jailer` builds this test into one binary as you, and
// only that binary runs with sudo:
//
//	make e2e-jailer
//	  = go test -c -tags 'e2e jailer' -o bin/e2e-jailer.test ./test/e2e
//	    sudo bin/e2e-jailer.test -test.run Jailer -test.v
//
// It reuses the snapshot store that `make snapshots` built as you, and as
// root it creates only a jail folder under <state>/jail/ and a cgroup
// under /sys/fs/cgroup/microvm/, both removed at the end.

const jailerHowTo = "build and run it with: make e2e-jailer " +
	"(or: go test -c -tags 'e2e jailer' -o bin/e2e-jailer.test ./test/e2e && sudo bin/e2e-jailer.test -test.run Jailer -test.v)"

func TestJailerRestore(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("jailer mode needs root; " + jailerHowTo)
	}
	a := findArtifacts(t)
	jailerBin := filepath.Join(a.repo, "third_party", "firecracker", "jailer")
	if _, err := os.Stat(jailerBin); err != nil {
		t.Skipf("no jailer (%v): run make firecracker", err)
	}
	// The store must already exist, built by you: as root this test must
	// not create it (the files would belong to root).
	stateDir := state.Dir(filepath.Join(a.repo, state.DefaultDir))
	for _, sub := range []string{"snapshots/small/current", "runs", "jail"} {
		if _, err := os.Stat(filepath.Join(stateDir, sub)); err != nil {
			t.Skipf("no snapshot store in %s (%v): run make snapshots as yourself first, without sudo", stateDir, err)
		}
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	root, err := state.Open(stateDir, state.Options{Jailer: true, Firecracker: a.firecracker})
	if err != nil {
		t.Fatalf("open the state folder for jailer mode: %v", err)
	}
	fcVersion, err := vmm.BinaryVersion(ctx, a.firecracker)
	if err != nil {
		t.Fatal(err)
	}
	store, err := snapshot.Open(ctx, root, fcVersion)
	if err != nil {
		t.Fatalf("open the store (run make snapshots as yourself): %v", err)
	}
	small, err := snapshot.LookupClass("small")
	if err != nil {
		t.Fatal(err)
	}

	// Every jail folder and every clone cgroup goes into one of these two
	// shared parents, which the jailer creates as root. The cleanup is
	// registered before Restore, so that a run that fails halfway also
	// removes them, or says exactly what is left and how to remove it.
	jailParent := filepath.Join(realPath(t, root.JailDir()), "firecracker")
	cgroupParent := filepath.Join(cgroupRoot(t), jailer.ParentCgroup)
	var c *clone.Clone
	t.Cleanup(func() {
		if c != nil {
			if err := c.Destroy(); err != nil {
				t.Errorf("Destroy: %v", err)
			}
		}
		// rmdir only removes an empty folder or cgroup, so this never
		// deletes anything that was left behind; errors are expected then.
		_ = syscall.Rmdir(jailParent)
		_ = syscall.Rmdir(cgroupParent)
		reportLeftovers(t, jailParent, cgroupParent)
	})

	c, err = clone.Restore(ctx, store, "small", clone.Options{
		Firecracker: a.firecracker,
		Jailer:      &clone.JailerOptions{Binary: jailerBin, UID: jailer.DefaultUID, GID: jailer.DefaultGID},
	})
	if err != nil {
		t.Fatalf("restore in jailer mode: %v", err)
	}
	jailDir, cgroupDir, pid := c.JailDir(), c.CgroupDir(), c.Pid()
	t.Logf("restore: %v", c.Timings())
	if c.Mode() != clone.Jailer || c.Timings().Mode != clone.Jailer {
		t.Errorf("mode %q, timings mode %q; want jailer", c.Mode(), c.Timings().Mode)
	}

	t.Run("RunsAsTheJailedUser", func(t *testing.T) {
		for key, want := range map[string]int{"Uid:": jailer.DefaultUID, "Gid:": jailer.DefaultGID} {
			ids := procStatus(t, pid, key) // real, effective, saved, filesystem
			for _, id := range ids {
				if id == "0" || id != strconv.Itoa(want) {
					t.Errorf("Firecracker %s %v, want all %d (never 0)", key, ids, want)
					break
				}
			}
		}
		// No supplementary groups either: none of root's may survive.
		if groups := procStatus(t, pid, "Groups:"); len(groups) != 0 {
			t.Errorf("Firecracker keeps the supplementary groups %v, want none", groups)
		}
	})

	t.Run("InItsCgroupWithTheClassLimits", func(t *testing.T) {
		cg, err := os.ReadFile(fmt.Sprintf("/proc/%d/cgroup", pid))
		if err != nil {
			t.Fatal(err)
		}
		if want := "0::/" + jailer.ParentCgroup + "/" + c.ID() + "\n"; string(cg) != want {
			t.Errorf("/proc/%d/cgroup = %q, want %q", pid, cg, want)
		}
		for file, want := range map[string]string{
			"cpu.max":    fmt.Sprintf("%d 100000", small.VCPUs*100000),
			"memory.max": strconv.Itoa(small.CgroupMemoryMiB << 20),
			"pids.max":   strconv.Itoa(small.CgroupPids),
		} {
			got, err := os.ReadFile(filepath.Join(cgroupDir, file)) //nolint:gosec // G304: a control file of our own clone's cgroup
			if err != nil || strings.TrimSpace(string(got)) != want {
				t.Errorf("%s = %q (%v), want %q", file, strings.TrimSpace(string(got)), err, want)
			}
		}
	})

	t.Run("SharedFilesAreReadOnlyLinks", func(t *testing.T) {
		if c.Dir() != filepath.Join(jailDir, "root") {
			t.Errorf("chroot %s, want %s/root", c.Dir(), jailDir)
		}
		for _, name := range []string{snapshot.StateName, snapshot.MemName, snapshot.RootfsName} {
			var inStore, inJail syscall.Stat_t
			if syscall.Stat(c.Version().Path(name), &inStore) != nil || syscall.Stat(filepath.Join(c.Dir(), name), &inJail) != nil {
				t.Fatalf("%s is missing in the store or the chroot", name)
			}
			if inStore.Ino != inJail.Ino || inStore.Nlink < 2 {
				t.Errorf("%s in the chroot is not a hard link to the store's (inode %d vs %d, %d links)",
					name, inJail.Ino, inStore.Ino, inStore.Nlink)
			}
			// The jailed user is not the owner and the file is 0444, so
			// Firecracker can read it but never write it or chmod it.
			if inStore.Uid == jailer.DefaultUID || inStore.Mode&0o777 != 0o444 {
				t.Errorf("%s: owner %d, mode %04o; want it not owned by the jailed user, and 0444",
					name, inStore.Uid, inStore.Mode&0o777)
			}
		}
		var scratch syscall.Stat_t
		if err := syscall.Stat(filepath.Join(c.Dir(), snapshot.ScratchName), &scratch); err != nil ||
			scratch.Uid != jailer.DefaultUID {
			t.Errorf("scratch disk owner %d (%v), want the jailed user %d", scratch.Uid, err, jailer.DefaultUID)
		}
	})

	t.Run("EchoHi", func(t *testing.T) {
		mountCtx, cancelMount := context.WithTimeout(ctx, 2*time.Second)
		defer cancelMount()
		if err := c.MountScratch(mountCtx); err != nil {
			t.Fatalf("MountScratch: %v", err)
		}
		if _, out := cloneExec(t, c, "sh", "-c", "touch mine && ls"); out != "lost+found\nmine\n" {
			t.Errorf("ls /work = %q, want lost+found and mine", out)
		}
		res, out := cloneExec(t, c, "echo", "hi")
		if out != "hi\n" || !res.Exited || res.ExitCode != 0 || res.Mode != clone.Jailer {
			t.Errorf("echo hi: %q, %+v; want hi, exit 0, mode jailer", out, res)
		}
		_, out = cloneExec(t, c, "date", "+%s")
		guest, err := strconv.ParseInt(strings.TrimSpace(out), 10, 64)
		if err != nil {
			t.Fatalf("date printed %q", out)
		}
		if diff := time.Now().Unix() - guest; diff < -2 || diff > 2 {
			t.Errorf("guest clock is %d s off the host's", diff)
		}
	})

	// Destroy: the cgroup is killed, the chroot and the cgroup are gone.
	if err := c.Destroy(); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Errorf("Firecracker (pid %d) still exists after Destroy (kill -0: %v)", pid, err)
	}
	for _, dir := range []string{jailDir, cgroupDir} {
		if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s still exists after Destroy (%v)", dir, err)
		}
	}
}

// procStatus returns the fields after key in /proc/<pid>/status.
func procStatus(t *testing.T, pid int, key string) []string {
	t.Helper()
	status, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(status), "\n") {
		if rest, ok := strings.CutPrefix(line, key); ok {
			return strings.Fields(rest)
		}
	}
	t.Fatalf("no %s line in /proc/%d/status", key, pid)
	return nil
}

// reportLeftovers fails the test for everything still in the two shared
// parents (or the parents themselves) and prints the exact commands that
// remove it by hand.
func reportLeftovers(t *testing.T, jailParent, cgroupParent string) {
	t.Helper()
	if entries, err := os.ReadDir(jailParent); err == nil {
		for _, e := range entries {
			path := filepath.Join(jailParent, e.Name())
			t.Errorf("left behind %s; remove it with: sudo rm -rf %s", path, path)
		}
		t.Errorf("left behind %s; remove it with: sudo rmdir %s", jailParent, jailParent)
	}
	if entries, err := os.ReadDir(cgroupParent); err == nil {
		for _, e := range entries {
			if !e.IsDir() {
				continue // the cgroup's own control files
			}
			path := filepath.Join(cgroupParent, e.Name())
			t.Errorf("left behind the cgroup %s; remove it with: sudo rmdir %s "+
				"(if processes are still in it, first: echo 1 | sudo tee %s/cgroup.kill)", path, path, path)
		}
		t.Errorf("left behind the cgroup %s; remove it with: sudo rmdir %s", cgroupParent, cgroupParent)
	}
}

// cgroupRoot finds the cgroup v2 mount, as the jailer does.
func cgroupRoot(t *testing.T) string {
	t.Helper()
	mounts, err := os.Open("/proc/self/mounts")
	if err != nil {
		t.Fatal(err)
	}
	defer mounts.Close() //nolint:errcheck // read-only file
	dir, err := jailer.CgroupRoot(mounts)
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

// realPath resolves symlinks in path, as the jailer does with its
// --chroot-base-dir.
func realPath(t *testing.T, path string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}
