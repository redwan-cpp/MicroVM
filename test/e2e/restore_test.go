//go:build e2e

package e2e

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/redwan-cpp/MicroVM/internal/clone"
	"github.com/redwan-cpp/MicroVM/internal/snapshot"
	"github.com/redwan-cpp/MicroVM/internal/vmm"
)

// clockGap is how long the test waits between taking the snapshot and the
// first restore. A restored guest's clock resumes where the snapshot
// stopped it, so without SyncTime it would be at least this far behind:
// more than the 2 s the test allows.
const clockGap = 3 * time.Second

// TestRestoreFromSnapshot builds a small snapshot in a fresh state folder
// (the real layout, under guest/out/), restores two clones from it one
// after the other in rootless mode, and checks each: the agent answers,
// de-twinning worked (the clock is right; the two clones' random numbers
// differ), the files are hard links, its own scratch disk mounts at /work
// with nothing of the previous clone on it, and Destroy leaves nothing
// behind.
// It logs every restore phase next to the cold boot of the build VM.
func TestRestoreFromSnapshot(t *testing.T) {
	a := findArtifacts(t)
	root := newStateRoot(t, a)
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()

	small, err := snapshot.LookupClass("small")
	if err != nil {
		t.Fatal(err)
	}
	built, err := snapshot.Build(ctx, root, small, snapshot.BuildConfig{
		Firecracker: a.firecracker, Kernel: a.kernel, Rootfs: a.rootfs,
	})
	if err != nil {
		t.Fatalf("build the small snapshot: %v", err)
	}
	snapshotTaken := time.Now()
	m := built.Version.Manifest
	t.Logf("built %s/%s in %v; cold boot: InstanceStart → agent ready %v",
		m.Class, built.Version.ID, built.Total.Round(time.Millisecond), built.Boot.Round(time.Millisecond))

	// Open the store as sandboxd will: once, with the version of the
	// Firecracker that will load it.
	fcVersion, err := vmm.BinaryVersion(ctx, a.firecracker)
	if err != nil {
		t.Fatal(err)
	}
	opened := time.Now()
	store, err := snapshot.Open(ctx, root, fcVersion)
	if err != nil {
		t.Fatalf("open the store: %v", err)
	}
	t.Logf("store opened and every hash checked in %v (not part of any restore)", time.Since(opened).Round(time.Millisecond))

	time.Sleep(time.Until(snapshotTaken.Add(clockGap)))
	uuids := make([]string, 2)
	timings := make([]clone.Timings, 2)
	for i := range uuids {
		c, err := clone.Restore(ctx, store, "small", clone.Options{Firecracker: a.firecracker})
		if err != nil {
			t.Fatalf("restore %d: %v", i+1, err)
		}
		t.Cleanup(func() { _ = c.Destroy() })
		// Restore returns only after SyncTime and Reseed both succeeded in
		// the guest; it destroys the clone otherwise.
		t.Logf("restore %d: %v", i+1, c.Timings())
		timings[i] = c.Timings()
		if c.Mode() != clone.Rootless || c.Timings().Mode != clone.Rootless {
			t.Errorf("mode %q, want rootless", c.Mode())
		}
		if c.Timings().DeTwin <= 0 {
			t.Errorf("de-twin took %v: it did not run", c.Timings().DeTwin)
		}
		// Firecracker's log reaches its ring buffer through the pipe, which
		// it opened after we did (vmm.Process.openLog).
		if log := c.LogTail(4096); !strings.Contains(log, "Running Firecracker") {
			t.Errorf("restore %d: Firecracker's log did not arrive through its pipe: %q", i+1, log)
		}

		// The clone's files are hard links to the store's: one more link
		// each while the clone runs.
		var st syscall.Stat_t
		if err := syscall.Stat(c.Version().Path(snapshot.MemName), &st); err != nil {
			t.Fatal(err)
		}
		if st.Nlink < 2 {
			t.Errorf("store mem.snap has %d link(s) while a clone runs, want more than 1", st.Nlink)
		}

		res, out := cloneExec(t, c, "echo", "hi")
		if out != "hi\n" || !res.Exited || res.ExitCode != 0 {
			t.Errorf("echo hi: %q, %+v", out, res)
		}
		if res.Mode != clone.Rootless {
			t.Errorf("exec result mode %q, want rootless", res.Mode)
		}

		// The clone's own scratch disk, mounted at /work: commands run
		// there, and the second clone does not see the first one's file.
		mountCtx, cancelMount := context.WithTimeout(ctx, 2*time.Second)
		err = c.MountScratch(mountCtx)
		cancelMount()
		if err != nil {
			t.Fatalf("MountScratch: %v\nconsole:\n%s", err, c.ConsoleTail(4096))
		}
		_, out = cloneExec(t, c, "sh", "-c", fmt.Sprintf("pwd; echo %d > clone-%d; ls; grep -w /work /proc/mounts", i+1, i+1))
		want := fmt.Sprintf("/work\nclone-%d\nlost+found\n/dev/vdb /work ext4 rw,nosuid,nodev", i+1)
		if !strings.HasPrefix(out, want) {
			t.Errorf("restore %d, /work: %q, want it to start with %q", i+1, out, want)
		}

		// SyncTime landed: the guest clock is within 2 s of the host's,
		// although the snapshot is more than clockGap old.
		_, out = cloneExec(t, c, "date", "+%s")
		guest, err := strconv.ParseInt(strings.TrimSpace(out), 10, 64)
		if err != nil {
			t.Fatalf("date printed %q", out)
		}
		if diff := time.Now().Unix() - guest; diff < -2 || diff > 2 {
			t.Errorf("restore %d: guest clock is %d s off the host's", i+1, diff)
		}

		_, out = cloneExec(t, c, "cat", "/proc/sys/kernel/random/uuid")
		uuids[i] = strings.TrimSpace(out)

		// Destroy, and check that nothing is left on the host.
		dir, pid := c.Dir(), c.Pid()
		if err := c.Destroy(); err != nil {
			t.Fatalf("Destroy: %v", err)
		}
		if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
			t.Errorf("Firecracker (pid %d) still exists after Destroy (kill -0: %v)", pid, err)
		}
		if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("run folder %s still exists after Destroy (%v)", dir, err)
		}
		if err := syscall.Stat(c.Version().Path(snapshot.MemName), &st); err != nil || st.Nlink != 1 {
			t.Errorf("store mem.snap has %d link(s) after Destroy (%v), want 1", st.Nlink, err)
		}
	}
	if entries, _ := os.ReadDir(root.RunsDir()); len(entries) != 0 {
		t.Errorf("runs/ is not empty after both clones were destroyed: %v", entries)
	}

	// Two clones of one snapshot must not produce the same random numbers.
	if uuids[0] == uuids[1] || uuids[0] == "" {
		t.Errorf("both clones read the random UUID %q", uuids[0])
	}
	t.Logf("random UUIDs: %s, %s (different); reseeded by the agent's Reseed, and by VMGenID as well: %v",
		uuids[0], uuids[1], m.VMGenID)

	// Side by side: the cold boot's "InstanceStart → agent ready" against
	// the restore's "LoadSnapshot → agent ready", and the whole restore.
	for i, tm := range timings {
		t.Logf("cold boot %v (InstanceStart → agent ready) vs restore %d, %s: %v (LoadSnapshot → agent ready), %v in all",
			built.Boot.Round(100*time.Microsecond), i+1, tm.Mode,
			(tm.LoadSnapshot + tm.AgentReady).Round(100*time.Microsecond), tm.Total.Round(100*time.Microsecond))
	}
}
