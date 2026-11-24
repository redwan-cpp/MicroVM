//go:build e2e && jailer

package e2e

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/redwan-cpp/MicroVM/internal/audit"
	"github.com/redwan-cpp/MicroVM/internal/clone"
	"github.com/redwan-cpp/MicroVM/internal/daemon"
	daemonv1 "github.com/redwan-cpp/MicroVM/internal/gen/sandbox/daemon/v1"
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
// root it creates only jail folders under <state>/jail/ and cgroups under
// /sys/fs/cgroup/microvm/, all removed at the end. TestJailerDemo1 also
// runs a jailer-mode daemon inside this binary, on that state folder: it
// adds three records to your audit log, which stays yours, and a socket
// in a temporary folder outside the state folder.

const jailerHowTo = "build and run it with: make e2e-jailer " +
	"(or: go test -c -tags 'e2e jailer' -o bin/e2e-jailer.test ./test/e2e && sudo bin/e2e-jailer.test -test.run Jailer -test.v)"

// jailerSetup skips the test unless it runs as root with the jailer and
// a snapshot store built by the user (`make snapshots`), and returns the
// artifacts, the jailer and the state folder.
func jailerSetup(t *testing.T) (artifacts, string, string) {
	t.Helper()
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
	return a, jailerBin, stateDir
}

// cleanJailParents registers a cleanup that removes the shared parents of
// every jail folder and clone cgroup (which the jailer creates as root)
// once they are empty, and reports anything left in them. Register it
// before anything that makes jails, so it runs after their cleanup.
func cleanJailParents(t *testing.T, root *state.Root) {
	t.Helper()
	jailParent := filepath.Join(realPath(t, root.JailDir()), "firecracker")
	cgroupParent := filepath.Join(cgroupRoot(t), jailer.ParentCgroup)
	t.Cleanup(func() {
		// rmdir only removes an empty folder or cgroup, so this never
		// deletes anything that was left behind; errors are expected then.
		_ = syscall.Rmdir(jailParent)
		_ = syscall.Rmdir(cgroupParent)
		reportLeftovers(t, jailParent, cgroupParent)
	})
}

func TestJailerRestore(t *testing.T) {
	a, jailerBin, stateDir := jailerSetup(t)
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

	// Every jail folder and every clone cgroup goes into one of two shared
	// parents, which the jailer creates as root. The cleanup is
	// registered before Restore, so that a run that fails halfway also
	// removes them, or says exactly what is left and how to remove it.
	cleanJailParents(t, root)
	var c *clone.Clone
	t.Cleanup(func() {
		if c != nil {
			if err := c.Destroy(); err != nil {
				t.Errorf("Destroy: %v", err)
			}
		}
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

// TestJailerDemo1 is Demo 1 through a jailer-mode daemon: the daemon runs
// in this test binary (the only thing running as root; it was built as
// you), on the store `make snapshots` built as you. The three clones must
// run as three different jailed UIDs, each in a cgroup of its own, and
// afterwards nothing may be left: no jail, no cgroup, and no root-owned
// file anywhere in the state folder outside jail/ (the audit log and the
// lock stay yours).
func TestJailerDemo1(t *testing.T) {
	a, jailerBin, stateDir := jailerSetup(t)
	root, err := state.Open(stateDir, state.Options{Jailer: true, Firecracker: a.firecracker})
	if err != nil {
		t.Fatalf("open the state folder for jailer mode: %v", err)
	}
	ownerUID, _, err := root.Owner()
	if err != nil {
		t.Fatal(err)
	}
	rootOwnedBefore := rootOwnedOutsideJail(t, root)
	auditBefore, err := audit.Verify(audit.Path(root.AuditDir()))
	if err != nil {
		t.Fatalf("the audit log is broken before the test: %v", err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	fcVersion, err := vmm.BinaryVersion(ctx, a.firecracker)
	if err != nil {
		t.Fatal(err)
	}
	store, err := snapshot.Open(ctx, root, fcVersion)
	if err != nil {
		t.Fatalf("open the store (run make snapshots as yourself): %v", err)
	}
	version, err := store.Current("small")
	if err != nil {
		t.Fatal(err)
	}

	cleanJailParents(t, root)
	// The socket goes outside the state folder, which must not get a
	// root-owned file.
	sockDir, err := os.MkdirTemp("", "sandboxd-e2e-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(sockDir) })
	d, client := startDaemon(t, daemon.Options{
		StateDir: stateDir, Socket: filepath.Join(sockDir, "sandboxd.sock"), Firecracker: a.firecracker,
		Jailer: true, JailerBinary: jailerBin, MaxConcurrent: 4,
	})
	if d.Mode() != clone.Jailer {
		t.Fatalf("mode %s, want jailer", d.Mode())
	}

	// While the three run, look at their Firecrackers: the UID each runs
	// as, and its cgroup.
	type demoOut struct {
		results []*runResult
		errs    []error
		wall    time.Duration
	}
	done := make(chan demoOut, 1)
	go func() {
		results, errs, wall := runDemo1(ctx, client, "2")
		done <- demoOut{results, errs, wall}
	}()
	uids := map[int]string{}
	deadline := time.Now().Add(20 * time.Second)
	for len(uids) < 3 {
		if time.Now().After(deadline) {
			t.Fatalf("did not see three running jailed clones (saw %v)", uids)
		}
		time.Sleep(20 * time.Millisecond)
		list, err := client.List(ctx, &daemonv1.ListRequest{})
		if err != nil {
			t.Fatal(err)
		}
		running := 0
		for _, r := range list.GetRuns() {
			if r.GetState() == daemonv1.RunState_RUN_STATE_RUNNING {
				running++
			}
		}
		if running < 3 {
			continue
		}
		for _, r := range list.GetRuns() {
			cg := filepath.Join(cgroupRoot(t), jailer.ParentCgroup, r.GetRunId())
			procs, err := os.ReadFile(filepath.Join(cg, "cgroup.procs")) //nolint:gosec // G304: our own clone's cgroup
			if err != nil {
				t.Fatalf("run %s has no cgroup of its own: %v", r.GetRunId(), err)
			}
			pids := strings.Fields(string(procs))
			if len(pids) == 0 {
				t.Fatalf("run %s: its cgroup %s is empty", r.GetRunId(), cg)
			}
			pid, _ := strconv.Atoi(pids[0])
			ids := procStatus(t, pid, "Uid:")
			uid, _ := strconv.Atoi(ids[0])
			if uid < jailer.DefaultUID || uid >= jailer.DefaultUID+4 {
				t.Errorf("run %s: Firecracker runs as UID %v, want one of %d-%d", r.GetRunId(), ids, jailer.DefaultUID, jailer.DefaultUID+3)
			}
			if other, ok := uids[uid]; ok && other != r.GetRunId() {
				t.Errorf("runs %s and %s share the jailed UID %d", other, r.GetRunId(), uid)
			}
			uids[uid] = r.GetRunId()
			t.Logf("run %s: Firecracker pid %d, UID %d, cgroup %s", r.GetRunId(), pid, uid, cg)
		}
	}
	out := <-done
	checkDemo1(t, out.results, out.errs, out.wall, daemonv1.Mode_MODE_JAILER)
	checkPsAndLogs(t, client, out.results)

	shutdownCtx, cancelShutdown := context.WithTimeout(ctx, daemon.ShutdownTimeout)
	defer cancelShutdown()
	if err := d.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	sum, err := audit.Verify(audit.Path(root.AuditDir()))
	if err != nil || sum.Records != auditBefore.Records+3 {
		t.Errorf("audit log: %+v, %v; want %d records in a valid chain", sum, err, auditBefore.Records+3)
	}
	checkNothingLeft(t, root, version)
	for _, r := range out.results {
		if _, err := os.Stat(filepath.Join(cgroupRoot(t), jailer.ParentCgroup, r.result.GetRunId())); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("the cgroup of %s is still there (%v)", r.result.GetRunId(), err)
		}
	}
	// What the root daemon created outside jail/ (the audit log, the
	// lock) belongs to the state folder's owner, so the user can still
	// verify the log and run rootless.
	for _, path := range []string{audit.Path(root.AuditDir()), root.AuditDir(), filepath.Join(root.Dir(), "lock")} {
		var st syscall.Stat_t
		if err := syscall.Lstat(path, &st); err != nil || int(st.Uid) != ownerUID {
			t.Errorf("%s belongs to uid %d (%v), want the state folder's owner %d", path, st.Uid, err, ownerUID)
		}
	}
	for _, path := range rootOwnedOutsideJail(t, root) {
		if !slices.Contains(rootOwnedBefore, path) {
			t.Errorf("the test left a root-owned file outside jail/: %s; remove it with: sudo rm -rf %s", path, path)
		}
	}
}

// rootOwnedOutsideJail lists every file and folder under the state folder,
// outside jail/, that belongs to root.
func rootOwnedOutsideJail(t *testing.T, root *state.Root) []string {
	t.Helper()
	var found []string
	err := filepath.WalkDir(root.Dir(), func(path string, _ fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == root.JailDir() {
			return filepath.SkipDir
		}
		var st syscall.Stat_t
		if err := syscall.Lstat(path, &st); err != nil {
			return err
		}
		if st.Uid == 0 {
			found = append(found, path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return found
}
