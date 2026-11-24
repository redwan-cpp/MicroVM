//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/redwan-cpp/MicroVM/internal/audit"
	"github.com/redwan-cpp/MicroVM/internal/daemon"
	daemonv1 "github.com/redwan-cpp/MicroVM/internal/gen/sandbox/daemon/v1"
	"github.com/redwan-cpp/MicroVM/internal/snapshot"
	"github.com/redwan-cpp/MicroVM/internal/state"
)

// Demo 1, as a test (build plan, week 4): three clones run at once from
// one snapshot, each with its own writes, and everything is cleaned up
// afterwards. The daemon runs in this test process (internal/daemon, the
// same code as `sandboxd serve`), on its own socket and state folder;
// the test talks to it with the same gRPC client sandboxctl uses.

// testLogger sends the daemon's log lines to the test log.
type testLogger struct{ t *testing.T }

func (l testLogger) Write(p []byte) (int, error) {
	l.t.Log(strings.TrimRight(string(p), "\n"))
	return len(p), nil
}

// buildSmall builds the small snapshot in root.
func buildSmall(ctx context.Context, t *testing.T, a artifacts, root *state.Root) *snapshot.Version {
	t.Helper()
	small, err := snapshot.LookupClass("small")
	if err != nil {
		t.Fatal(err)
	}
	built, err := snapshot.Build(ctx, root, small, snapshot.BuildConfig{Firecracker: a.firecracker, Kernel: a.kernel, Rootfs: a.rootfs})
	if err != nil {
		t.Fatalf("build the small snapshot: %v", err)
	}
	return built.Version
}

// startDaemon starts a daemon on root, with its socket in root. Cleanup
// shuts it down if the test has not.
func startDaemon(t *testing.T, opts daemon.Options) (*daemon.Daemon, daemonv1.SandboxServiceClient) {
	t.Helper()
	if opts.Logger == nil {
		opts.Logger = log.New(testLogger{t}, "sandboxd: ", 0)
	}
	if opts.Version == "" {
		opts.Version = "sandboxd e2e"
	}
	d, err := daemon.Start(opts)
	if err != nil {
		t.Fatalf("start the daemon: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), daemon.ShutdownTimeout)
		defer cancel()
		_ = d.Shutdown(ctx)
	})
	cc, err := grpc.NewClient("unix://"+d.Socket(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cc.Close() })
	return d, daemonv1.NewSandboxServiceClient(cc)
}

// runResult is everything one Run stream delivered.
type runResult struct {
	result         *daemonv1.RunResult
	stdout, stderr string
	states         []*daemonv1.StateChange
}

// runOne runs req to its end.
func runOne(ctx context.Context, client daemonv1.SandboxServiceClient, req *daemonv1.RunRequest) (*runResult, error) {
	stream, err := client.Run(ctx, req)
	if err != nil {
		return nil, err
	}
	var out, errOut bytes.Buffer
	r := &runResult{}
	for {
		msg, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			if r.result == nil {
				return r, errors.New("the stream ended without a result")
			}
			r.stdout, r.stderr = out.String(), errOut.String()
			return r, nil
		}
		if err != nil {
			return r, err
		}
		switch ev := msg.GetEvent().(type) {
		case *daemonv1.RunResponse_State:
			r.states = append(r.states, ev.State)
		case *daemonv1.RunResponse_Stdout:
			out.Write(ev.Stdout)
		case *daemonv1.RunResponse_Stderr:
			errOut.Write(ev.Stderr)
		case *daemonv1.RunResponse_Result:
			r.result = ev.Result
		}
	}
}

// entered returns when a run entered state s.
func (r *runResult) entered(s daemonv1.RunState) time.Time {
	for _, st := range r.states {
		if st.GetState() == s {
			return st.GetAt().AsTime()
		}
	}
	return time.Time{}
}

// demo1Command is what each of the three clones runs: write a secret of
// its own to /work (its scratch disk, the default working folder), list
// /work, hold on for $HOLD seconds so that all three run at once, and show
// what it can read there.
const demo1Command = `echo "$SECRET" > "secret-$N"; ls /work; sleep "$HOLD"; cat secret-*`

// runDemo1 runs the three clones at once, each holding on for hold
// seconds. It returns their results, their errors and the wall time for
// all three. It never stops the test, so it may run in a goroutine of its
// own; checkDemo1 checks what it returns.
func runDemo1(ctx context.Context, client daemonv1.SandboxServiceClient, hold string) ([]*runResult, []error, time.Duration) {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	results := make([]*runResult, 3)
	errs := make([]error, 3)
	var wg sync.WaitGroup
	began := time.Now()
	for i := range results {
		wg.Go(func() {
			results[i], errs[i] = runOne(ctx, client, &daemonv1.RunRequest{
				Argv: []string{"sh", "-c", demo1Command},
				Env:  map[string]string{"N": fmt.Sprint(i + 1), "SECRET": fmt.Sprintf("the secret of clone %d", i+1), "HOLD": hold},
			})
		})
	}
	wg.Wait()
	return results, errs, time.Since(began)
}

// checkDemo1 checks that each of the three clones exited 0 in wantMode and
// saw only its own file, and that all three ran at once; it logs their
// timings and the wall time.
func checkDemo1(t *testing.T, results []*runResult, errs []error, wall time.Duration, wantMode daemonv1.Mode) {
	t.Helper()
	for i, r := range results {
		if errs[i] != nil {
			t.Fatalf("clone %d: %v", i+1, errs[i])
		}
		res := r.result
		want := fmt.Sprintf("lost+found\nsecret-%d\nthe secret of clone %d\n", i+1, i+1)
		if r.stdout != want || r.stderr != "" {
			t.Errorf("clone %d saw %q (stderr %q), want only its own file: %q", i+1, r.stdout, r.stderr, want)
		}
		if res.GetOutcome() != daemonv1.Outcome_OUTCOME_EXITED || !res.GetExited() || res.GetExitCode() != 0 || res.GetMode() != wantMode {
			t.Errorf("clone %d: %v, want EXITED with 0 in %v", i+1, res, wantMode)
		}
		if last := r.states[len(r.states)-1].GetState(); last != daemonv1.RunState_RUN_STATE_DONE {
			t.Errorf("clone %d ended %v, want DONE", i+1, last)
		}
	}
	// All three ran at once: each was RUNNING before any of them stopped.
	var lastStart, firstStop time.Time
	for _, r := range results {
		if start := r.entered(daemonv1.RunState_RUN_STATE_RUNNING); start.After(lastStart) {
			lastStart = start
		}
		if stop := r.entered(daemonv1.RunState_RUN_STATE_STOPPING); firstStop.IsZero() || stop.Before(firstStop) {
			firstStop = stop
		}
	}
	if !lastStart.Before(firstStop) {
		t.Errorf("the three runs did not overlap: the last started at %v, the first stopped at %v", lastStart, firstStop)
	}
	for i, r := range results {
		t.Logf("Demo 1 clone %d (%s): %s", i+1, r.result.GetRunId(), timingsLine(r.result))
	}
	t.Logf("Demo 1: three clones at once, wall time for all three: %v", wall.Round(100*time.Microsecond))
}

// timingsLine formats a result's phases.
func timingsLine(r *daemonv1.RunResult) string {
	t := r.GetTimings()
	f := func(us uint64) string { return fmt.Sprintf("%.1fms", float64(us)/1000) }
	return fmt.Sprintf("%s: queued %s, restore %s (prepare %s, scratch %s, process start %s, LoadSnapshot %s, agent ready %s, "+
		"de-twin %s), mount %s, exec %s, destroy %s, total %s",
		strings.ToLower(strings.TrimPrefix(r.GetMode().String(), "MODE_")), f(t.GetQueuedUs()), f(t.GetRestoreUs()),
		f(t.GetPrepareUs()), f(t.GetScratchUs()), f(t.GetProcessStartUs()), f(t.GetLoadSnapshotUs()), f(t.GetAgentReadyUs()),
		f(t.GetDeTwinUs()), f(t.GetMountUs()), f(t.GetExecUs()), f(t.GetDestroyUs()), f(t.GetTotalUs()))
}

// checkPsAndLogs checks that List shows the runs DONE and Logs returns
// their output.
func checkPsAndLogs(t *testing.T, client daemonv1.SandboxServiceClient, results []*runResult) {
	t.Helper()
	ctx := t.Context()
	list, err := client.List(ctx, &daemonv1.ListRequest{All: true})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	states := map[string]daemonv1.RunState{}
	for _, r := range list.GetRuns() {
		states[r.GetRunId()] = r.GetState()
	}
	for i, r := range results {
		id := r.result.GetRunId()
		if states[id] != daemonv1.RunState_RUN_STATE_DONE {
			t.Errorf("ps: clone %d (%s) is %v, want DONE", i+1, id, states[id])
		}
		logs, err := client.Logs(ctx, &daemonv1.LogsRequest{RunId: id})
		if err != nil {
			t.Fatalf("Logs(%s): %v", id, err)
		}
		var got strings.Builder
		for _, c := range logs.GetChunks() {
			got.Write(c.GetStdout())
		}
		if got.String() != r.stdout || !logs.GetFinished() || logs.GetCut() || logs.GetDropped() {
			t.Errorf("logs %s: %q (%v), want what the run streamed, %q", id, got.String(), logs, r.stdout)
		}
	}
	if active, err := client.List(ctx, &daemonv1.ListRequest{}); err != nil || len(active.GetRuns()) != 0 {
		t.Errorf("ps without --all: %v, %v; want no active runs", active.GetRuns(), err)
	}
}

// firecrackersIn lists the Firecracker processes whose working folder is
// under dir (each clone's Firecracker runs in its run folder or chroot).
func firecrackersIn(dir string) []string {
	var found []string
	procs, _ := os.ReadDir("/proc")
	for _, p := range procs {
		cwd, err := os.Readlink(filepath.Join("/proc", p.Name(), "cwd"))
		if err == nil && strings.HasPrefix(cwd, dir) {
			found = append(found, p.Name()+" in "+cwd)
		}
	}
	return found
}

// checkNothingLeft checks that the daemon left nothing behind in root:
// no Firecracker, an empty runs/ (and jail/firecracker), and the store's
// memory file back to its own single link.
func checkNothingLeft(t *testing.T, root *state.Root, v *snapshot.Version) {
	t.Helper()
	if left := firecrackersIn(root.Dir()); len(left) != 0 {
		t.Errorf("Firecracker processes left: %v", left)
	}
	if entries, err := os.ReadDir(root.RunsDir()); err != nil || len(entries) != 0 {
		t.Errorf("runs/ holds %v (%v), want nothing", entries, err)
	}
	if entries, err := os.ReadDir(filepath.Join(root.JailDir(), "firecracker")); err == nil && len(entries) != 0 {
		t.Errorf("jail/firecracker holds %v, want nothing", entries)
	}
	var st syscall.Stat_t
	if err := syscall.Stat(v.Path(snapshot.MemName), &st); err != nil || st.Nlink != 1 {
		t.Errorf("the store's mem.snap has %d links (%v), want 1: a clone still holds it", st.Nlink, err)
	}
}

func TestDemo1ThreeClonesAtOnce(t *testing.T) {
	a := findArtifacts(t)
	root := newStateRoot(t, a)
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	version := buildSmall(ctx, t, a, root)

	d, client := startDaemon(t, daemon.Options{
		StateDir: root.Dir(), Socket: filepath.Join(root.Dir(), "d.sock"), Firecracker: a.firecracker, MaxConcurrent: 4,
	})
	if d.Mode() != "rootless" {
		t.Fatalf("mode %s, want rootless", d.Mode())
	}

	results, errs, wall := runDemo1(ctx, client, "0.5")
	checkDemo1(t, results, errs, wall, daemonv1.Mode_MODE_ROOTLESS)
	checkPsAndLogs(t, client, results)

	t.Run("Timeout", func(t *testing.T) {
		r, err := runOne(ctx, client, &daemonv1.RunRequest{Argv: []string{"sleep", "10"}, TimeoutMs: 300})
		if err != nil {
			t.Fatal(err)
		}
		res := r.result
		if res.GetOutcome() != daemonv1.Outcome_OUTCOME_TIMED_OUT || res.GetExited() || !res.GetTimedOut() || res.GetExitCode() != 0 {
			t.Errorf("result %v, want TIMED_OUT without an exit code", res)
		}
		if total := res.GetTimings().GetTotalUs(); total > 3_000_000 {
			t.Errorf("a 300 ms timeout took %d µs in all", total)
		}
	})

	t.Run("KilledClient", func(t *testing.T) {
		// The caller goes away while its command runs: the daemon stops
		// the run, destroys the clone and records it.
		callCtx, hangUp := context.WithCancel(ctx)
		stream, err := client.Run(callCtx, &daemonv1.RunRequest{Argv: []string{"sleep", "30"}})
		if err != nil {
			t.Fatal(err)
		}
		for {
			msg, err := stream.Recv()
			if err != nil {
				t.Fatalf("before RUNNING: %v", err)
			}
			if msg.GetState().GetState() == daemonv1.RunState_RUN_STATE_RUNNING {
				break
			}
		}
		hangUp()
		deadline := time.Now().Add(10 * time.Second)
		for {
			list, err := client.List(ctx, &daemonv1.ListRequest{})
			if err == nil && len(list.GetRuns()) == 0 {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("the abandoned run is still active: %v", list.GetRuns())
			}
			time.Sleep(20 * time.Millisecond)
		}
		all, _ := client.List(ctx, &daemonv1.ListRequest{All: true})
		last := all.GetRuns()[len(all.GetRuns())-1]
		if last.GetState() != daemonv1.RunState_RUN_STATE_FAILED || !strings.Contains(last.GetResult().GetError(), "the caller went away") {
			t.Errorf("the abandoned run: %v, want FAILED because the caller went away", last)
		}
		if left := firecrackersIn(root.Dir()); len(left) != 0 {
			t.Errorf("its Firecracker is still there: %v", left)
		}
	})

	shutdownCtx, cancelShutdown := context.WithTimeout(ctx, daemon.ShutdownTimeout)
	defer cancelShutdown()
	if err := d.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	// The audit chain verifies: the three Demo 1 runs, the timeout and
	// the abandoned run.
	sum, err := audit.Verify(audit.Path(root.AuditDir()))
	if err != nil || sum.Records != 5 {
		t.Errorf("audit log: %+v, %v; want 5 records in a valid chain", sum, err)
	}
	t.Logf("audit chain OK: %d records, newest hash %s", sum.Records, sum.Head)
	checkNothingLeft(t, root, version)
	if _, err := os.Lstat(d.Socket()); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the socket is still there after Shutdown: %v", err)
	}
}

// TestDemo1WithTheBinaries runs the programs themselves: bin/sandboxd
// serve in the background and bin/sandboxctl for each step, as
// scripts/demo1.sh does, and checks sandboxctl's exit codes.
func TestDemo1WithTheBinaries(t *testing.T) {
	a := findArtifacts(t)
	sandboxd, sandboxctl := filepath.Join(a.repo, "bin", "sandboxd"), filepath.Join(a.repo, "bin", "sandboxctl")
	for _, bin := range []string{sandboxd, sandboxctl} {
		if _, err := os.Stat(bin); err != nil {
			t.Skipf("no %s (%v): run make build", bin, err)
		}
	}
	root := newStateRoot(t, a)
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	version := buildSmall(ctx, t, a, root)

	socket := filepath.Join(root.Dir(), "d.sock")
	var daemonLog bytes.Buffer
	cmd := exec.Command(sandboxd, "serve", "--state-dir", root.Dir(), "--socket", socket, "--firecracker", a.firecracker) //nolint:gosec // G204: our own binary
	cmd.Stdout, cmd.Stderr = &daemonLog, &daemonLog
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	stopped := false
	stop := func() {
		if !stopped {
			stopped = true
			_ = cmd.Process.Signal(syscall.SIGTERM)
			_ = cmd.Wait()
		}
	}
	t.Cleanup(func() {
		stop()
		t.Logf("sandboxd's log:\n%s", daemonLog.String())
	})
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(socket); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("sandboxd did not open its socket")
		}
		time.Sleep(10 * time.Millisecond)
	}

	ctl := func(args ...string) (int, string, string) {
		c := exec.CommandContext(ctx, sandboxctl, args...) //nolint:gosec // G204: our own binary
		c.Env = append(os.Environ(), "SANDBOXD_SOCKET="+socket)
		var out, errOut bytes.Buffer
		c.Stdout, c.Stderr = &out, &errOut
		err := c.Run()
		var exitErr *exec.ExitError
		if err != nil && !errors.As(err, &exitErr) {
			t.Fatalf("sandboxctl %q: %v", args, err)
		}
		return c.ProcessState.ExitCode(), out.String(), errOut.String()
	}
	for _, tc := range []struct {
		args []string
		code int
		out  string
	}{
		{[]string{"run", "--", "sh", "-c", "echo hi > /work/note; cat note"}, 0, "hi\n"},
		{[]string{"run", "--", "sh", "-c", "exit 3"}, 3, ""},
		{[]string{"run", "--timeout", "300ms", "--", "sleep", "10"}, 124, ""},
		{[]string{"run", "--max-output", "1KiB", "--", "head", "-c", "100000", "/dev/zero"}, 123, strings.Repeat("\x00", 1024)},
		{[]string{"run", "--", "no-such-program"}, 125, ""},
	} {
		code, out, errOut := ctl(tc.args...)
		if code != tc.code || out != tc.out {
			t.Errorf("sandboxctl %q: exit %d, stdout %q (stderr %q); want %d, %q", tc.args, code, out, errOut, tc.code, tc.out)
		}
	}
	code, out, _ := ctl("ps", "--all")
	if code != 0 || strings.Count(out, "\n") != 6 || !strings.Contains(out, "TIMED_OUT") || !strings.Contains(out, "EXITED 3") {
		t.Errorf("sandboxctl ps --all: exit %d\n%s", code, out)
	}
	first := strings.Fields(strings.Split(out, "\n")[1])[0]
	if code, out, _ := ctl("logs", first); code != 0 || out != "hi\n" {
		t.Errorf("sandboxctl logs %s: exit %d, %q", first, code, out)
	}

	stop()
	if code, out, errOut := ctl("audit", "verify", "--state-dir", root.Dir()); code != 0 || !strings.Contains(out, "5 records, hash chain OK") {
		t.Errorf("sandboxctl audit verify: exit %d, %q %q", code, out, errOut)
	}
	if code, _, errOut := ctl("ps"); code != 1 || !strings.Contains(errOut, "sandboxd is not running") {
		t.Errorf("sandboxctl ps without a daemon: exit %d, %q", code, errOut)
	}
	checkNothingLeft(t, root, version)
}
