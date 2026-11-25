package clone

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"google.golang.org/grpc"

	"github.com/redwan-cpp/MicroVM/internal/agent"
	agentv1 "github.com/redwan-cpp/MicroVM/internal/gen/sandbox/agent/v1"
	"github.com/redwan-cpp/MicroVM/internal/snapshot"
	"github.com/redwan-cpp/MicroVM/internal/state"
)

// The tests restore clones without KVM. The "firecracker" they start is
// this test program itself (see TestMain), which acts as a small fake: on
// PUT /snapshot/load it checks that the snapshot's files are in its working
// folder under the right names, then serves a real agent (with a fake
// clock and RNG) behind a fake vsock socket, v.sock, just like a restored
// guest would.
const fakeEnv = "CLONE_TEST_FAKE_FIRECRACKER"

func TestMain(m *testing.M) {
	switch mode := os.Getenv(fakeEnv); mode {
	case "":
		os.Exit(m.Run())
	default:
		fakeFirecracker(mode)
	}
}

// fakeFirecracker serves the API socket until it is killed. mode is
// "serve", "load-fails", "reseed-fails" or "load-fails-undeletable" (the
// load fails after leaving a folder in the run folder that our user
// cannot empty, so cleaning up fails too).
func fakeFirecracker(mode string) {
	var apiSock string
	for i := 1; i+1 < len(os.Args); i++ {
		if os.Args[i] == "--api-sock" {
			apiSock = os.Args[i+1]
		}
	}
	lis, err := net.Listen("unix", apiSock)
	if err != nil {
		fmt.Fprintln(os.Stderr, "fake firecracker:", err)
		os.Exit(1)
	}
	srv := &http.Server{ReadHeaderTimeout: time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if mode == "load-fails-undeletable" {
			if err := os.Mkdir("locked", 0o700); err == nil {
				_ = os.WriteFile("locked/stuck", nil, 0o600)
				_ = os.Chmod("locked", 0o500) //nolint:gosec // G302: a folder whose entries cannot be removed, on purpose
			}
		}
		if r.URL.Path != "/snapshot/load" || mode == "load-fails" || mode == "load-fails-undeletable" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"fault_message": "fake: no"}`)
			return
		}
		var body struct {
			SnapshotPath string `json:"snapshot_path"`
			MemBackend   struct {
				BackendType string `json:"backend_type"`
				BackendPath string `json:"backend_path"`
			} `json:"mem_backend"`
			ResumeVM bool `json:"resume_vm"`
		}
		err := json.NewDecoder(r.Body).Decode(&body)
		if err == nil && (body.SnapshotPath != "vm.snap" || body.MemBackend.BackendType != "File" ||
			body.MemBackend.BackendPath != "mem.snap" || !body.ResumeVM) {
			err = fmt.Errorf("unexpected load request %+v", body)
		}
		for _, name := range []string{"vm.snap", "mem.snap", "rootfs.ext4", "scratch.ext4"} {
			if _, statErr := os.Stat(name); err == nil && statErr != nil {
				err = statErr // the clone's folder must hold every file, by relative name
			}
		}
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = fmt.Fprintf(w, `{"fault_message": %q}`, err.Error())
			return
		}
		if err := serveAgent(mode == "reseed-fails"); err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})}
	_ = srv.Serve(lis)
}

// serveAgent listens on v.sock like a restored guest's vsock device and
// serves a real agent behind the CONNECT handshake.
func serveAgent(reseedFails bool) error {
	lis, err := net.Listen("unix", "v.sock")
	if err != nil {
		return err
	}
	gs := grpc.NewServer()
	agentv1.RegisterAgentServiceServer(gs, agent.NewServer(agent.Config{
		Version: "fake agent",
		Clock:   fakeClock{},
		Entropy: fakeEntropy{fail: reseedFails},
		Scratch: fakeScratch{},
	}))
	go func() { _ = gs.Serve(handshakeListener{lis}) }()
	return nil
}

type fakeClock struct{}

func (fakeClock) SetTime(time.Time) error { return nil }

// fakeScratch checks the size the host asks for against the clone's
// scratch disk, in the fake's working folder (the run folder).
type fakeScratch struct{}

func (fakeScratch) Mount(size uint64) error {
	info, err := os.Stat("scratch.ext4")
	if err != nil {
		return err
	}
	if uint64(info.Size()) != size { //nolint:gosec // G115: a file size is never negative
		return fmt.Errorf("fake: scratch.ext4 is %d bytes, asked to mount %d", info.Size(), size)
	}
	return nil
}

func (fakeScratch) Dir() string { return "/" }

type fakeEntropy struct{ fail bool }

func (e fakeEntropy) AddEntropy(b []byte) error {
	if e.fail {
		return errors.New("fake: no RNG")
	}
	if len(b) != entropySize {
		return fmt.Errorf("fake: got %d bytes of entropy", len(b))
	}
	return nil
}

// handshakeListener answers "CONNECT <port>" with "OK", like Firecracker.
type handshakeListener struct{ net.Listener }

func (l handshakeListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	b := make([]byte, 1)
	for b[0] != '\n' {
		if _, err := conn.Read(b); err != nil {
			_ = conn.Close()
			return nil, err
		}
	}
	if _, err := io.WriteString(conn, "OK 1\n"); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return conn, nil
}

// newStore builds a store with a small snapshot made by hand (tiny files,
// real hashes, mode 0444) and opens it.
func newStore(t *testing.T) *snapshot.Store {
	t.Helper()
	dir, err := os.MkdirTemp("", "clone-test-") // short: socket paths
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	root, err := state.Open(dir, state.Options{})
	if err != nil {
		t.Fatal(err)
	}

	class := snapshot.Classes[0]
	const id = "20261118T090000Z-aaaaaa"
	version := filepath.Join(root.SnapshotsDir(), class.Name, id)
	if err := os.MkdirAll(version, 0o700); err != nil {
		t.Fatal(err)
	}
	arch, err := snapshot.HostArch()
	if err != nil {
		t.Fatal(err)
	}
	m := snapshot.Manifest{Format: snapshot.ManifestFormat, Arch: arch, Class: class.Name, VCPUs: class.VCPUs,
		MemoryMiB: class.MemoryMiB, ScratchMiB: class.ScratchMiB, FirecrackerVersion: "v1.17.0"}
	for name, sum := range map[string]*string{
		snapshot.StateName: &m.StateSHA256, snapshot.MemName: &m.MemSHA256, snapshot.RootfsName: &m.RootfsSHA256,
		snapshot.ScratchName: &m.ScratchSHA256,
	} {
		path := filepath.Join(version, name)
		if err := os.WriteFile(path, []byte("fake "+name), 0o600); err != nil {
			t.Fatal(err)
		}
		if name == snapshot.ScratchName {
			// A real template has the class's size; most of it is a hole.
			if err := os.Truncate(path, int64(class.ScratchMiB)<<20); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.Chmod(path, 0o444); err != nil { //nolint:gosec // G302: store files are 0444 by design
			t.Fatal(err)
		}
		*sum = sha256File(t, path)
	}
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(version, snapshot.ManifestName), data, 0o444); err != nil { //nolint:gosec // G306: store files are 0444 by design
		t.Fatal(err)
	}
	if err := os.Symlink(id, filepath.Join(root.SnapshotsDir(), class.Name, "current")); err != nil {
		t.Fatal(err)
	}

	store, err := snapshot.Open(t.Context(), root, "v1.17.0")
	if err != nil {
		t.Fatalf("snapshot.Open: %v", err)
	}
	return store
}

// sha256File returns the SHA-256 of the file at path, in hex.
func sha256File(t *testing.T, path string) string {
	t.Helper()
	f, err := os.Open(path) //nolint:gosec // G304: a test file
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close() //nolint:errcheck // read-only file
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func restore(t *testing.T, store *snapshot.Store, mode string) (*Clone, error) {
	t.Helper()
	t.Setenv(fakeEnv, mode)
	c, err := Restore(t.Context(), store, "small", Options{Firecracker: os.Args[0]})
	if c != nil {
		t.Cleanup(func() { _ = c.Destroy() })
	}
	return c, err
}

func TestRestoreAndDestroy(t *testing.T) {
	store := newStore(t)
	c, err := restore(t, store, "serve")
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}

	if c.Mode() != Rootless || c.Timings().Mode != Rootless {
		t.Errorf("mode %q, timings mode %q; want rootless", c.Mode(), c.Timings().Mode)
	}
	tm := c.Timings()
	if tm.Prepare <= 0 || tm.Scratch <= 0 || tm.ProcessStart <= 0 || tm.LoadSnapshot <= 0 || tm.AgentReady <= 0 || tm.DeTwin <= 0 ||
		tm.Total < tm.Prepare+tm.Scratch+tm.ProcessStart+tm.LoadSnapshot+tm.AgentReady+tm.DeTwin {
		t.Errorf("timings %+v: every phase must be positive and add up to at most Total", tm)
	}
	if !strings.HasPrefix(tm.String(), "rootless: prepare ") {
		t.Errorf("Timings.String() = %q, want it to start with the mode", tm.String())
	}

	// The clone's folder holds links to the store's files, not copies.
	v := c.Version()
	for _, name := range []string{snapshot.StateName, snapshot.MemName, snapshot.RootfsName} {
		var store, mine syscall.Stat_t
		if syscall.Stat(v.Path(name), &store) != nil || syscall.Stat(filepath.Join(c.Dir(), name), &mine) != nil ||
			store.Ino != mine.Ino {
			t.Errorf("%s in the clone is not a hard link to the store's", name)
		}
	}
	if filepath.Base(c.Dir()) != c.ID() || filepath.Dir(c.Dir()) != store.Root().RunsDir() {
		t.Errorf("run folder %s, want %s/%s", c.Dir(), store.Root().RunsDir(), c.ID())
	}
	// The scratch disk is the clone's own copy of the template, never a
	// link: what the clone writes stays its own.
	var tmpl, scratch syscall.Stat_t
	if syscall.Stat(v.Path(snapshot.ScratchName), &tmpl) != nil ||
		syscall.Stat(filepath.Join(c.Dir(), snapshot.ScratchName), &scratch) != nil {
		t.Fatal("a scratch disk is missing")
	}
	if tmpl.Ino == scratch.Ino || tmpl.Nlink != 1 || scratch.Size != tmpl.Size || scratch.Mode&0o777 != 0o600 {
		t.Errorf("scratch disk: inode %d (template %d), %d links, %d bytes, mode %04o; want a private 0600 copy",
			scratch.Ino, tmpl.Ino, tmpl.Nlink, scratch.Size, scratch.Mode&0o777)
	}

	// The fake's agent is the real one, running on the host here.
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if err := c.MountScratch(ctx); err != nil {
		t.Errorf("MountScratch: %v", err)
	}
	var out strings.Builder
	res, err := c.Exec(ctx, &agentv1.ExecRequest{Argv: []string{"echo", "hi"}}, &out, nil)
	if err != nil || out.String() != "hi\n" || !res.Exited || res.ExitCode != 0 {
		t.Errorf("Exec(echo hi) = %+v, %q, %v", res, out.String(), err)
	}
	if res.Mode != Rootless {
		t.Errorf("ExecResult.Mode = %q, want rootless", res.Mode)
	}

	dir, pid := c.Dir(), c.Pid()
	if err := c.Destroy(); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if err := c.Destroy(); err != nil {
		t.Errorf("second Destroy: %v, want nil", err)
	}
	assertGone(t, dir, pid)
	// The snapshot is untouched: one link each again.
	var st syscall.Stat_t
	if err := syscall.Stat(v.Path(snapshot.MemName), &st); err != nil || st.Nlink != 1 {
		t.Errorf("store mem.snap: %d links, %v; want 1 after Destroy", st.Nlink, err)
	}
}

func TestRestoreFailuresLeaveNothingBehind(t *testing.T) {
	for _, mode := range []string{"load-fails", "reseed-fails"} {
		t.Run(mode, func(t *testing.T) {
			store := newStore(t)
			c, err := restore(t, store, mode)
			if err == nil {
				t.Fatalf("Restore succeeded with a fake in mode %s, want an error", mode)
			}
			if c != nil {
				t.Errorf("Restore returned a clone with its error")
			}
			entries, _ := os.ReadDir(store.Root().RunsDir())
			if len(entries) != 0 {
				t.Errorf("runs/ holds %v after a failed restore, want nothing", entries)
			}
		})
	}
}

func TestRestoreReportsAFailedCleanup(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can delete anything, so cleanup cannot fail this way")
	}
	store := newStore(t)
	// Whatever happens, let the test's own cleanup delete the run folder.
	t.Cleanup(func() {
		_ = filepath.WalkDir(store.Root().RunsDir(), func(path string, d os.DirEntry, err error) error {
			if err == nil && d.IsDir() {
				_ = os.Chmod(path, 0o700) //nolint:gosec // G302: undo the test's 0500
			}
			return nil
		})
	})

	_, err := restore(t, store, "load-fails-undeletable")
	if err == nil {
		t.Fatal("Restore succeeded, want an error")
	}
	// Both failures are in the error: the load that went wrong, and the
	// cleanup that could not delete the run folder.
	if !strings.Contains(err.Error(), "load snapshot") || !strings.Contains(err.Error(), "destroy") ||
		!errors.Is(err, os.ErrPermission) {
		t.Errorf("err = %v; want the load error joined with Destroy's permission error", err)
	}
	var cleanup *CleanupError
	if !errors.As(err, &cleanup) || !strings.HasPrefix(cleanup.ID, "r-") {
		t.Errorf("err = %v; want a *CleanupError for the clone inside", err)
	}
}

func TestRestoreUsesTheGivenID(t *testing.T) {
	store := newStore(t)
	t.Setenv(fakeEnv, "serve")
	const id = "r-00000000000000aa"
	c, err := Restore(t.Context(), store, "small", Options{Firecracker: os.Args[0], ID: id})
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	t.Cleanup(func() { _ = c.Destroy() })
	if c.ID() != id || c.Dir() != filepath.Join(store.Root().RunsDir(), id) {
		t.Errorf("ID %s, folder %s; want %s", c.ID(), c.Dir(), id)
	}
	// The same ID again: its folder exists, and Restore must not touch it.
	if _, err := Restore(t.Context(), store, "small", Options{Firecracker: os.Args[0], ID: id}); err == nil {
		t.Error("a second Restore with the same ID succeeded")
	}
	if _, err := os.Stat(c.Dir()); err != nil {
		t.Errorf("the first clone's folder is gone after the failed second Restore: %v", err)
	}
	if _, err := Restore(t.Context(), store, "small", Options{Firecracker: os.Args[0], ID: "../x"}); err == nil {
		t.Error("Restore accepted an ID that is not a run ID")
	}
}

func TestRestoreUnknownClass(t *testing.T) {
	store := newStore(t)
	if _, err := Restore(t.Context(), store, "huge", Options{Firecracker: os.Args[0]}); err == nil {
		t.Error("Restore of an unknown class succeeded")
	}
	if _, err := Restore(t.Context(), store, "medium", Options{Firecracker: os.Args[0]}); err == nil ||
		!strings.Contains(err.Error(), "run make snapshots") {
		t.Errorf("Restore of an unbuilt class: %v, want run make snapshots", err)
	}
}

func assertGone(t *testing.T, dir string, pid int) {
	t.Helper()
	if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Errorf("process %d still exists after Destroy (kill -0: %v)", pid, err)
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("run folder %s still exists after Destroy (%v)", dir, err)
	}
}

func TestJailerModeNeedsRoot(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root")
	}
	store := newStore(t)
	_, err := Restore(t.Context(), store, "small", Options{
		Firecracker: os.Args[0],
		Jailer:      &JailerOptions{Binary: "/nonexistent/jailer", UID: 70000, GID: 70000},
	})
	if err == nil || !strings.Contains(err.Error(), "needs root") {
		t.Errorf("Restore in jailer mode without root: %v, want a needs-root error", err)
	}
	if entries, _ := os.ReadDir(store.Root().JailDir()); len(entries) != 0 {
		t.Errorf("jail/ holds %v, want nothing", entries)
	}
}

func TestJailCleanupIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	// A jail whose folder and cgroup never came to be: nothing to do.
	j := &jail{dir: filepath.Join(dir, "jail"), cgroup: filepath.Join(dir, "cgroup")}
	if err := j.kill(); err != nil {
		t.Errorf("kill without a cgroup: %v", err)
	}
	if err := j.remove(); err != nil {
		t.Errorf("remove without a folder or cgroup: %v", err)
	}

	// kill writes "1" to cgroup.kill.
	if err := os.Mkdir(j.cgroup, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(j.cgroup, "cgroup.kill"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := j.kill(); err != nil {
		t.Fatalf("kill: %v", err)
	}
	if got, _ := os.ReadFile(filepath.Join(j.cgroup, "cgroup.kill")); string(got) != "1" {
		t.Errorf("cgroup.kill holds %q, want 1", got)
	}
}
