package daemon

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	"github.com/redwan-cpp/MicroVM/internal/agentclient"
	"github.com/redwan-cpp/MicroVM/internal/audit"
	"github.com/redwan-cpp/MicroVM/internal/clone"
	agentv1 "github.com/redwan-cpp/MicroVM/internal/gen/sandbox/agent/v1"
	daemonv1 "github.com/redwan-cpp/MicroVM/internal/gen/sandbox/daemon/v1"
	"github.com/redwan-cpp/MicroVM/internal/snapshot"
	"github.com/redwan-cpp/MicroVM/internal/state"
)

// These tests run the real daemon (the gRPC service, its slots, registry
// and audit log, and Start with its socket and lock) with a fake Cloner:
// no VMs, no KVM. The fake VM's "commands" are scripted below.

// fakeFirecrackerEnv makes this test binary print a Firecracker version,
// for Start's vmm.BinaryVersion.
const fakeFirecrackerEnv = "DAEMON_TEST_FAKE_FIRECRACKER"

func TestMain(m *testing.M) {
	if os.Getenv(fakeFirecrackerEnv) != "" {
		fmt.Println("Firecracker v1.17.0")
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// fakeCloner restores fakeVMs and counts what happens.
type fakeCloner struct {
	store *snapshot.Store

	restoreErr error // returned by every Restore
	destroyErr error // returned by every Destroy
	mountErr   error // returned by every MountScratch

	mu        sync.Mutex
	active    int // clones that exist now
	maxActive int
	restores  int
	destroys  map[string]int // run ID → Destroy calls
	slots     []int
	gate      chan struct{} // "hold" waits on it
}

func newFakeCloner(store *snapshot.Store) *fakeCloner {
	return &fakeCloner{store: store, destroys: make(map[string]int), gate: make(chan struct{})}
}

func (f *fakeCloner) Restore(ctx context.Context, class, id string, slot int) (VM, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.restores++
	f.slots = append(f.slots, slot)
	if f.restoreErr != nil {
		return nil, f.restoreErr
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f.active++
	f.maxActive = max(f.maxActive, f.active)
	v, err := f.store.Current(class)
	if err != nil {
		return nil, err
	}
	return &fakeVM{f: f, id: id, version: v}, nil
}

func (f *fakeCloner) stats() (active, maxActive, restores int, destroys map[string]int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	d := make(map[string]int)
	for k, v := range f.destroys {
		d[k] = v
	}
	return f.active, f.maxActive, f.restores, d
}

type fakeVM struct {
	f       *fakeCloner
	id      string
	version *snapshot.Version
}

func (v *fakeVM) MountScratch(context.Context) error { return v.f.mountErr }

func (v *fakeVM) Timings() clone.Timings {
	return clone.Timings{Mode: clone.Rootless, Prepare: time.Millisecond, Total: 5 * time.Millisecond}
}

func (v *fakeVM) Version() *snapshot.Version { return v.version }

func (v *fakeVM) Destroy() error {
	v.f.mu.Lock()
	defer v.f.mu.Unlock()
	v.f.destroys[v.id]++
	v.f.active--
	return v.f.destroyErr
}

// Exec plays the command named by argv[0].
func (v *fakeVM) Exec(ctx context.Context, req *agentv1.ExecRequest, stdout, stderr io.Writer) (clone.ExecResult, error) {
	if _, ok := ctx.Deadline(); !ok {
		return clone.ExecResult{}, agentclient.ErrNoDeadline
	}
	argv := req.GetArgv()
	res := func(r agentclient.Result) (clone.ExecResult, error) {
		return clone.ExecResult{Result: r, Mode: clone.Rootless}, nil
	}
	switch argv[0] {
	case "echo":
		line := strings.Join(argv[1:], " ") + "\n"
		_, _ = io.WriteString(stdout, line)
		return res(agentclient.Result{Exited: true, OutputBytes: uint64(len(line))})
	case "both": // stdout, stderr, stdout, then exit 3
		_, _ = io.WriteString(stdout, "out1\n")
		_, _ = io.WriteString(stderr, "err\n")
		_, _ = io.WriteString(stdout, "out2\n")
		return res(agentclient.Result{Exited: true, ExitCode: 3, OutputBytes: 14})
	case "flood": // argv[1] bytes, in 32 KiB chunks, then the cap
		n, _ := strconv.Atoi(argv[1])
		chunk := bytes.Repeat([]byte("x"), 32<<10)
		for left := n; left > 0; left -= len(chunk) {
			_, _ = stdout.Write(chunk[:min(left, len(chunk))])
		}
		return res(agentclient.Result{OutputTruncated: true, OutputBytes: uint64(n)}) //nolint:gosec // G115: a small test size
	case "timeout":
		return res(agentclient.Result{TimedOut: true})
	case "block": // until the run is cancelled
		<-ctx.Done()
		return clone.ExecResult{}, fmt.Errorf("agentclient: exec: %w", ctx.Err())
	case "hold": // until the test opens the gate
		select {
		case <-v.f.gate:
			return res(agentclient.Result{Exited: true})
		case <-ctx.Done():
			return clone.ExecResult{}, fmt.Errorf("agentclient: exec: %w", ctx.Err())
		}
	}
	return clone.ExecResult{}, fmt.Errorf("fake: no command %q", argv[0])
}

// newFakeStore makes a state folder with a hand-made small snapshot (no
// medium one) and opens its store.
func newFakeStore(t *testing.T) *snapshot.Store {
	t.Helper()
	dir, err := os.MkdirTemp("", "daemon-test-") // short: socket paths
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	root, err := state.Open(dir, state.Options{})
	if err != nil {
		t.Fatal(err)
	}
	class := snapshot.Classes[0]
	const id = "20261124T090000Z-aaaaaa"
	version := filepath.Join(root.SnapshotsDir(), class.Name, id)
	if err := os.MkdirAll(version, 0o700); err != nil {
		t.Fatal(err)
	}
	m := snapshot.Manifest{Format: snapshot.ManifestFormat, Class: class.Name, VCPUs: class.VCPUs,
		MemoryMiB: class.MemoryMiB, ScratchMiB: class.ScratchMiB, FirecrackerVersion: "v1.17.0"}
	for name, sum := range map[string]*string{snapshot.StateName: &m.StateSHA256, snapshot.MemName: &m.MemSHA256,
		snapshot.RootfsName: &m.RootfsSHA256, snapshot.ScratchName: &m.ScratchSHA256} {
		content := []byte("fake " + name)
		if err := os.WriteFile(filepath.Join(version, name), content, 0o444); err != nil { //nolint:gosec // G306: store files are 0444 by design
			t.Fatal(err)
		}
		h := sha256.Sum256(content)
		*sum = hex.EncodeToString(h[:])
	}
	data, _ := json.Marshal(m)
	if err := os.WriteFile(filepath.Join(version, snapshot.ManifestName), data, 0o444); err != nil { //nolint:gosec // G306: store files are 0444 by design
		t.Fatal(err)
	}
	if err := os.Symlink(id, filepath.Join(root.SnapshotsDir(), class.Name, "current")); err != nil {
		t.Fatal(err)
	}
	store, err := snapshot.Open(t.Context(), root, "v1.17.0")
	if err != nil {
		t.Fatal(err)
	}
	return store
}

// harness is a Server with a fake cloner behind a real Unix socket.
type harness struct {
	srv    *Server
	cloner *fakeCloner
	audit  *audit.Log
	client daemonv1.SandboxServiceClient
	logs   *syncWriter
}

func newHarness(t *testing.T, maxConcurrent, maxPending int) *harness {
	t.Helper()
	store := newFakeStore(t)
	h := &harness{cloner: newFakeCloner(store), logs: &syncWriter{}}
	var err error
	if h.audit, err = audit.Open(store.Root().AuditDir(), nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.audit.Close() })
	h.srv = NewServer(Config{
		Store: store, Cloner: h.cloner, Audit: h.audit, Mode: clone.Rootless,
		MaxConcurrent: maxConcurrent, MaxPending: maxPending, Version: "sandboxd test",
		Logger: log.New(h.logs, "", 0),
	})
	sock := filepath.Join(store.Root().Dir(), "d.sock")
	lis, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	gs := grpc.NewServer(grpc.Creds(peerCreds{}), grpc.MaxRecvMsgSize(MaxRecvMsgSize))
	daemonv1.RegisterSandboxServiceServer(gs, h.srv)
	go func() { _ = gs.Serve(lis) }()
	t.Cleanup(func() {
		_ = h.srv.Shutdown(context.Background())
		gs.Stop()
	})
	h.client = dial(t, sock)
	return h
}

func dial(t *testing.T, sock string) daemonv1.SandboxServiceClient {
	t.Helper()
	cc, err := grpc.NewClient("unix://"+sock, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cc.Close() })
	return daemonv1.NewSandboxServiceClient(cc)
}

// syncWriter collects the daemon's log lines, which come from many
// goroutines.
type syncWriter struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *syncWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *syncWriter) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

// collected is everything one Run stream delivered.
type collected struct {
	states         []daemonv1.RunState
	stdout, stderr bytes.Buffer
	order          []string // "out"/"err" per chunk
	result         *daemonv1.RunResult
}

// runToEnd runs req and reads the whole stream.
func runToEnd(t *testing.T, c daemonv1.SandboxServiceClient, req *daemonv1.RunRequest) (*collected, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	stream, err := c.Run(ctx, req)
	if err != nil {
		return nil, err
	}
	got := &collected{}
	for {
		msg, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return got, nil
		}
		if err != nil {
			return got, err
		}
		if got.result != nil {
			t.Fatalf("an event after the result: %v", msg)
		}
		switch ev := msg.GetEvent().(type) {
		case *daemonv1.RunResponse_State:
			got.states = append(got.states, ev.State.GetState())
		case *daemonv1.RunResponse_Stdout:
			got.stdout.Write(ev.Stdout)
			got.order = append(got.order, "out")
		case *daemonv1.RunResponse_Stderr:
			got.stderr.Write(ev.Stderr)
			got.order = append(got.order, "err")
		case *daemonv1.RunResponse_Result:
			got.result = ev.Result
		}
	}
}

var happyPath = []daemonv1.RunState{
	daemonv1.RunState_RUN_STATE_PENDING, daemonv1.RunState_RUN_STATE_RESTORING, daemonv1.RunState_RUN_STATE_MOUNTING,
	daemonv1.RunState_RUN_STATE_RUNNING, daemonv1.RunState_RUN_STATE_STOPPING, daemonv1.RunState_RUN_STATE_DONE,
}

func equalStates(a, b []daemonv1.RunState) bool {
	return fmt.Sprint(a) == fmt.Sprint(b)
}

// auditRuns reads the run records from the log and verifies the chain.
func auditRuns(t *testing.T, l *audit.Log) []*audit.Run {
	t.Helper()
	if _, err := audit.Verify(l.Path()); err != nil {
		t.Fatalf("audit chain: %v", err)
	}
	data, err := os.ReadFile(l.Path())
	if err != nil {
		t.Fatal(err)
	}
	var runs []*audit.Run
	for _, line := range bytes.Split(bytes.TrimSpace(data), []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		var env struct {
			Record audit.Record `json:"record"`
		}
		if err := json.Unmarshal(line, &env); err != nil {
			t.Fatal(err)
		}
		runs = append(runs, env.Record.Run)
	}
	return runs
}

func TestRunStreamsEverythingInOrder(t *testing.T) {
	h := newHarness(t, 2, 4)
	got, err := runToEnd(t, h.client, &daemonv1.RunRequest{Argv: []string{"both"}, Env: map[string]string{"SECRET": "hunter2"},
		Stdin: []byte("private input")})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !equalStates(got.states, happyPath) {
		t.Errorf("states %v, want %v", got.states, happyPath)
	}
	if got.stdout.String() != "out1\nout2\n" || got.stderr.String() != "err\n" || strings.Join(got.order, ",") != "out,err,out" {
		t.Errorf("output %q / %q in order %v", got.stdout.String(), got.stderr.String(), got.order)
	}
	r := got.result
	if r.GetOutcome() != daemonv1.Outcome_OUTCOME_EXITED || !r.GetExited() || r.GetExitCode() != 3 ||
		r.GetMode() != daemonv1.Mode_MODE_ROOTLESS || !state.ValidID(r.GetRunId()) {
		t.Errorf("result %v, want EXITED with 3, rootless", r)
	}
	if tm := r.GetTimings(); tm.GetTotalUs() == 0 || tm.GetRestoreUs() != 5000 || tm.GetPrepareUs() != 1000 {
		t.Errorf("timings %v", tm)
	}
	if _, _, _, d := h.cloner.stats(); d[r.GetRunId()] != 1 {
		t.Errorf("Destroy called %d times, want exactly 1", d[r.GetRunId()])
	}

	// The audit record: written, chained, the caller's real UID, no env
	// values and no stdin.
	runs := auditRuns(t, h.audit)
	if len(runs) != 1 {
		t.Fatalf("%d audit records, want 1", len(runs))
	}
	a := runs[0]
	if a.RunID != r.GetRunId() || a.Outcome != "EXITED" || a.FinalState != "DONE" || a.ExitCode == nil || *a.ExitCode != 3 ||
		a.CallerUID != uint32(os.Getuid()) || a.Mode != "rootless" || a.SnapshotVersion == "" || a.StdinBytes != 13 { //nolint:gosec // G115: a UID
		t.Errorf("audit record %+v", a)
	}
	if len(a.Transitions) != 5 || a.Transitions[4].State != "STOPPING" {
		t.Errorf("audit transitions %+v, want PENDING..STOPPING (the record is written before DONE)", a.Transitions)
	}
	data, _ := os.ReadFile(h.audit.Path())
	if bytes.Contains(data, []byte("hunter2")) || bytes.Contains(data, []byte("private input")) ||
		!bytes.Contains(data, []byte(`"env_names":["SECRET"]`)) {
		t.Errorf("the audit log holds an env value or stdin, or lacks the env name: %s", data)
	}

	// Logs, Get and List see the same run.
	ctx := t.Context()
	logs, err := h.client.Logs(ctx, &daemonv1.LogsRequest{RunId: r.GetRunId()})
	if err != nil || len(logs.GetChunks()) != 3 || logs.GetChunks()[1].GetStderr() == nil || !logs.GetFinished() ||
		logs.GetOutputBytes() != 14 || logs.GetKeptBytes() != 14 || logs.GetCut() || logs.GetDropped() {
		t.Errorf("Logs = %v, %v", logs, err)
	}
	info, err := h.client.Get(ctx, &daemonv1.GetRequest{RunId: r.GetRunId()})
	if err != nil || info.GetRun().GetState() != daemonv1.RunState_RUN_STATE_DONE || len(info.GetRun().GetTransitions()) != 6 ||
		info.GetRun().GetCallerUid() != uint32(os.Getuid()) || info.GetRun().GetResult().GetExitCode() != 3 { //nolint:gosec // G115: a UID
		t.Errorf("Get = %v, %v", info, err)
	}
	for _, all := range []bool{false, true} {
		list, err := h.client.List(ctx, &daemonv1.ListRequest{All: all})
		want := map[bool]int{false: 0, true: 1}[all]
		if err != nil || len(list.GetRuns()) != want {
			t.Errorf("List(all=%v) = %d runs, %v; want %d", all, len(list.GetRuns()), err, want)
		}
	}
	if _, err := h.client.Get(ctx, &daemonv1.GetRequest{RunId: "r-ffffffffffffffff"}); status.Code(err) != codes.NotFound {
		t.Errorf("Get(unknown) = %v, want NotFound", err)
	}
	if _, err := h.client.Logs(ctx, &daemonv1.LogsRequest{RunId: "../etc"}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("Logs(bad ID) = %v, want InvalidArgument", err)
	}
}

func TestRunOutcomes(t *testing.T) {
	h := newHarness(t, 2, 4)
	for _, tc := range []struct {
		argv      []string
		outcome   daemonv1.Outcome
		state     daemonv1.RunState
		exited    bool
		timedOut  bool
		truncated bool
	}{
		{[]string{"echo", "hi"}, daemonv1.Outcome_OUTCOME_EXITED, daemonv1.RunState_RUN_STATE_DONE, true, false, false},
		{[]string{"timeout"}, daemonv1.Outcome_OUTCOME_TIMED_OUT, daemonv1.RunState_RUN_STATE_DONE, false, true, false},
		{[]string{"flood", "1000"}, daemonv1.Outcome_OUTCOME_OUTPUT_TRUNCATED, daemonv1.RunState_RUN_STATE_DONE, false, false, true},
		{[]string{"no-such-thing"}, daemonv1.Outcome_OUTCOME_FAILED, daemonv1.RunState_RUN_STATE_FAILED, false, false, false},
	} {
		got, err := runToEnd(t, h.client, &daemonv1.RunRequest{Argv: tc.argv})
		if err != nil {
			t.Fatalf("%v: %v", tc.argv, err)
		}
		r := got.result
		if r.GetOutcome() != tc.outcome || got.states[len(got.states)-1] != tc.state || r.GetExited() != tc.exited ||
			r.GetTimedOut() != tc.timedOut || r.GetOutputTruncated() != tc.truncated {
			t.Errorf("%v: result %v, last state %v", tc.argv, r, got.states[len(got.states)-1])
		}
		if tc.outcome == daemonv1.Outcome_OUTCOME_FAILED && !strings.Contains(r.GetError(), "no command") {
			t.Errorf("%v: error %q, want the reason", tc.argv, r.GetError())
		}
	}
	// The audit record has an exit code only for the run that exited.
	for _, a := range auditRuns(t, h.audit) {
		if (a.ExitCode != nil) != (a.Outcome == "EXITED") || a.Exited != (a.Outcome == "EXITED") {
			t.Errorf("audit record %s: outcome %s, exited %v, exit code %v", a.RunID, a.Outcome, a.Exited, a.ExitCode)
		}
	}
}

func TestRunRejectsBadRequests(t *testing.T) {
	h := newHarness(t, 1, 1)
	unknown := &daemonv1.RunRequest{Argv: []string{"echo"}}
	unknown.ProtoReflect().SetUnknown([]byte{0xa0, 0x06, 0x01}) // field 100, varint 1
	for name, tc := range map[string]struct {
		req  *daemonv1.RunRequest
		code codes.Code
	}{
		"empty argv":      {&daemonv1.RunRequest{}, codes.InvalidArgument},
		"NUL in argv":     {&daemonv1.RunRequest{Argv: []string{"a\x00b"}}, codes.InvalidArgument},
		"PATH in env":     {&daemonv1.RunRequest{Argv: []string{"echo"}, Env: map[string]string{"PATH": "/x"}}, codes.InvalidArgument},
		"unknown class":   {&daemonv1.RunRequest{Argv: []string{"echo"}, SizeClass: "huge"}, codes.InvalidArgument},
		"unbuilt class":   {&daemonv1.RunRequest{Argv: []string{"echo"}, SizeClass: "medium"}, codes.FailedPrecondition},
		"long timeout":    {&daemonv1.RunRequest{Argv: []string{"echo"}, TimeoutMs: 11 * 60 * 1000}, codes.InvalidArgument},
		"big output cap":  {&daemonv1.RunRequest{Argv: []string{"echo"}, MaxOutputBytes: 17 << 20}, codes.InvalidArgument},
		"stdin too big":   {&daemonv1.RunRequest{Argv: []string{"echo"}, Stdin: make([]byte, 960<<10+1)}, codes.InvalidArgument},
		"unknown fields":  {unknown, codes.InvalidArgument},
		"message too big": {&daemonv1.RunRequest{Argv: []string{"echo", strings.Repeat("x", MaxRecvMsgSize)}}, codes.ResourceExhausted},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := runToEnd(t, h.client, tc.req)
			if status.Code(err) != tc.code {
				t.Errorf("Run = %v, want %v", err, tc.code)
			}
		})
	}
	// The largest stdin is fine.
	if got, err := runToEnd(t, h.client, &daemonv1.RunRequest{Argv: []string{"echo"}, Stdin: make([]byte, 960<<10)}); err != nil ||
		got.result.GetOutcome() != daemonv1.Outcome_OUTCOME_EXITED {
		t.Errorf("Run with 960 KiB of stdin: %v", err)
	}
	if _, _, restores, _ := h.cloner.stats(); restores != 1 {
		t.Errorf("%d restores, want 1: no rejected request may get a clone", restores)
	}
}

// waitFor polls cond for up to 5 s.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestCallerGoneDestroysTheClone(t *testing.T) {
	h := newHarness(t, 1, 1)
	ctx, cancel := context.WithCancel(t.Context())
	stream, err := h.client.Run(ctx, &daemonv1.RunRequest{Argv: []string{"block"}})
	if err != nil {
		t.Fatal(err)
	}
	for { // read until the command runs
		msg, err := stream.Recv()
		if err != nil {
			t.Fatal(err)
		}
		if msg.GetState().GetState() == daemonv1.RunState_RUN_STATE_RUNNING {
			break
		}
	}
	cancel() // the caller goes away
	waitFor(t, "the run to end", func() bool { return len(auditRuns(t, h.audit)) == 1 })
	a := auditRuns(t, h.audit)[0]
	if a.FinalState != "FAILED" || a.Outcome != "FAILED" || !strings.Contains(a.Error, "the caller went away") {
		t.Errorf("audit record %+v, want FAILED because the caller went away", a)
	}
	if active, _, _, d := h.cloner.stats(); active != 0 || d[a.RunID] != 1 {
		t.Errorf("%d clones left, Destroy called %d times; want 0 and 1", active, d[a.RunID])
	}
	// The slot is free again.
	if got, err := runToEnd(t, h.client, &daemonv1.RunRequest{Argv: []string{"echo", "next"}}); err != nil || got.stdout.String() != "next\n" {
		t.Errorf("the next run: %v, %q", err, got.stdout.String())
	}
}

func TestSlotsLimitConcurrency(t *testing.T) {
	h := newHarness(t, 2, 8)
	var wg sync.WaitGroup
	var done atomic.Int32
	for range 5 {
		wg.Go(func() {
			got, err := runToEnd(t, h.client, &daemonv1.RunRequest{Argv: []string{"hold"}})
			if err != nil || got.result.GetOutcome() != daemonv1.Outcome_OUTCOME_EXITED {
				t.Errorf("run: %v, %v", err, got)
			}
			done.Add(1)
		})
	}
	// Two runs hold a clone; three wait in PENDING.
	waitFor(t, "2 running and 3 pending", func() bool {
		list, err := h.client.List(t.Context(), &daemonv1.ListRequest{})
		if err != nil {
			return false
		}
		n := map[daemonv1.RunState]int{}
		for _, r := range list.GetRuns() {
			n[r.GetState()]++
		}
		return n[daemonv1.RunState_RUN_STATE_RUNNING] == 2 && n[daemonv1.RunState_RUN_STATE_PENDING] == 3
	})
	close(h.cloner.gate) // let every command finish
	wg.Wait()
	if _, maxActive, restores, _ := h.cloner.stats(); maxActive != 2 || restores != 5 {
		t.Errorf("at most %d clones at once over %d restores, want 2 over 5", maxActive, restores)
	}
	h.cloner.mu.Lock()
	for _, s := range h.cloner.slots {
		if s < 0 || s > 1 {
			t.Errorf("slot %d handed out, want 0 or 1", s)
		}
	}
	h.cloner.mu.Unlock()
}

func TestPendingQueueIsBounded(t *testing.T) {
	h := newHarness(t, 1, 1)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	states := func() []daemonv1.RunState {
		list, _ := h.client.List(t.Context(), &daemonv1.ListRequest{})
		var out []daemonv1.RunState
		for _, r := range list.GetRuns() {
			out = append(out, r.GetState())
		}
		return out
	}
	// One run holds the slot, then one waits for it.
	if _, err := h.client.Run(ctx, &daemonv1.RunRequest{Argv: []string{"hold"}}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "a run in RUNNING", func() bool {
		return equalStates(states(), []daemonv1.RunState{daemonv1.RunState_RUN_STATE_RUNNING})
	})
	if _, err := h.client.Run(ctx, &daemonv1.RunRequest{Argv: []string{"hold"}}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "a run in PENDING", func() bool {
		return equalStates(states(), []daemonv1.RunState{daemonv1.RunState_RUN_STATE_RUNNING, daemonv1.RunState_RUN_STATE_PENDING})
	})
	if _, err := runToEnd(t, h.client, &daemonv1.RunRequest{Argv: []string{"echo"}}); status.Code(err) != codes.ResourceExhausted {
		t.Errorf("a third run: %v, want ResourceExhausted", err)
	}
}

func TestShutdownStopsAndDestroysEverything(t *testing.T) {
	h := newHarness(t, 3, 4)
	var wg sync.WaitGroup
	results := make(chan *daemonv1.RunResult, 4)
	for range 4 { // 3 running, 1 pending
		wg.Go(func() {
			got, err := runToEnd(t, h.client, &daemonv1.RunRequest{Argv: []string{"block"}})
			if err != nil {
				t.Errorf("run: %v", err)
				return
			}
			results <- got.result
		})
	}
	waitFor(t, "3 clones", func() bool { active, _, _, _ := h.cloner.stats(); return active == 3 })
	ctx, cancel := context.WithTimeout(t.Context(), ShutdownTimeout)
	defer cancel()
	if err := h.srv.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	wg.Wait()
	close(results)
	for r := range results {
		if r.GetOutcome() != daemonv1.Outcome_OUTCOME_FAILED || !strings.Contains(r.GetError(), "shutting down") {
			t.Errorf("result %v, want FAILED because the daemon is shutting down", r)
		}
	}
	active, _, restores, d := h.cloner.stats()
	if active != 0 || restores != 3 || len(d) != 3 {
		t.Errorf("after Shutdown: %d clones left, %d restores, %d destroyed; want 0, 3, 3", active, restores, len(d))
	}
	if runs := auditRuns(t, h.audit); len(runs) != 4 {
		t.Errorf("%d audit records, want 4 (the pending run too)", len(runs))
	}
	if _, err := runToEnd(t, h.client, &daemonv1.RunRequest{Argv: []string{"echo"}}); status.Code(err) != codes.Unavailable {
		t.Errorf("a run after Shutdown: %v, want Unavailable", err)
	}
}

func TestFailedRestoreAndCleanup(t *testing.T) {
	h := newHarness(t, 1, 2)
	h.cloner.restoreErr = errors.New("clone: load snapshot: \x1b[31mguest says no\x1b[0m")
	got, err := runToEnd(t, h.client, &daemonv1.RunRequest{Argv: []string{"echo"}})
	if err != nil {
		t.Fatal(err)
	}
	want := []daemonv1.RunState{daemonv1.RunState_RUN_STATE_PENDING, daemonv1.RunState_RUN_STATE_RESTORING,
		daemonv1.RunState_RUN_STATE_STOPPING, daemonv1.RunState_RUN_STATE_FAILED}
	if !equalStates(got.states, want) || got.result.GetOutcome() != daemonv1.Outcome_OUTCOME_FAILED {
		t.Errorf("states %v, result %v", got.states, got.result)
	}
	// Guest text is cleaned of control characters.
	if strings.ContainsRune(got.result.GetError(), 0x1b) || !strings.Contains(got.result.GetError(), "guest says no") {
		t.Errorf("error %q, want the reason without escape sequences", got.result.GetError())
	}

	// A restore whose own cleanup failed retires the only slot: nothing
	// runs until the daemon restarts.
	h.cloner.restoreErr = errors.Join(errors.New("clone: load snapshot: no"), &clone.CleanupError{ID: "r-1", Err: os.ErrPermission})
	if got, err = runToEnd(t, h.client, &daemonv1.RunRequest{Argv: []string{"echo"}}); err != nil {
		t.Fatal(err)
	}
	if got.result.GetOutcome() != daemonv1.Outcome_OUTCOME_FAILED {
		t.Errorf("result %v, want FAILED", got.result)
	}
	if a := auditRuns(t, h.audit)[1]; !a.SlotRetired || a.DestroyError == "" {
		t.Errorf("audit record %+v, want the slot retired and the destroy error", a)
	}
	if _, err := runToEnd(t, h.client, &daemonv1.RunRequest{Argv: []string{"echo"}}); status.Code(err) != codes.Unavailable {
		t.Errorf("a run with every slot retired: %v, want Unavailable", err)
	}
}

func TestFailedDestroyFailsTheRunAndRetiresItsSlot(t *testing.T) {
	h := newHarness(t, 2, 2)
	h.cloner.destroyErr = &clone.CleanupError{ID: "x", Err: errors.New("cgroup busy")}
	got, err := runToEnd(t, h.client, &daemonv1.RunRequest{Argv: []string{"echo", "hi"}})
	if err != nil {
		t.Fatal(err)
	}
	r := got.result
	if r.GetOutcome() != daemonv1.Outcome_OUTCOME_FAILED || got.states[len(got.states)-1] != daemonv1.RunState_RUN_STATE_FAILED ||
		!strings.Contains(r.GetError(), "cgroup busy") || got.stdout.String() != "hi\n" {
		t.Errorf("result %v, states %v", r, got.states)
	}
	a := auditRuns(t, h.audit)[0]
	if !a.SlotRetired || !strings.Contains(a.DestroyError, "cgroup busy") || a.FinalState != "FAILED" {
		t.Errorf("audit record %+v", a)
	}
	if !strings.Contains(h.logs.String(), "slot") {
		t.Errorf("the daemon did not log the retired slot: %s", h.logs.String())
	}
	// One slot is left.
	h.cloner.destroyErr = nil
	if _, err := runToEnd(t, h.client, &daemonv1.RunRequest{Argv: []string{"echo"}}); err != nil {
		t.Errorf("a run on the remaining slot: %v", err)
	}
	h.cloner.mu.Lock()
	defer h.cloner.mu.Unlock()
	if s := h.cloner.slots; len(s) != 2 || s[0] == s[1] {
		t.Errorf("slots %v: the retired one was handed out again", s)
	}
}

func TestFailedMountStillDestroys(t *testing.T) {
	h := newHarness(t, 1, 1)
	h.cloner.mountErr = errors.New("no /dev/vdb")
	got, err := runToEnd(t, h.client, &daemonv1.RunRequest{Argv: []string{"echo"}})
	if err != nil {
		t.Fatal(err)
	}
	want := []daemonv1.RunState{daemonv1.RunState_RUN_STATE_PENDING, daemonv1.RunState_RUN_STATE_RESTORING,
		daemonv1.RunState_RUN_STATE_MOUNTING, daemonv1.RunState_RUN_STATE_STOPPING, daemonv1.RunState_RUN_STATE_FAILED}
	if !equalStates(got.states, want) || !strings.Contains(got.result.GetError(), "no /dev/vdb") {
		t.Errorf("states %v, result %v", got.states, got.result)
	}
	if active, _, _, d := h.cloner.stats(); active != 0 || d[got.result.GetRunId()] != 1 {
		t.Errorf("%d clones left, %d destroys", active, d[got.result.GetRunId()])
	}
}

func TestAuditFailureStopsAdmission(t *testing.T) {
	h := newHarness(t, 1, 1)
	if _, err := runToEnd(t, h.client, &daemonv1.RunRequest{Argv: []string{"echo"}}); err != nil {
		t.Fatal(err)
	}
	_ = h.audit.Close() // from now on, no record can be written
	if _, err := runToEnd(t, h.client, &daemonv1.RunRequest{Argv: []string{"echo"}}); status.Code(err) != codes.Unavailable {
		t.Errorf("a run without a working audit log: %v, want Unavailable", err)
	}
}

func TestAuditFailureDuringARunFailsIt(t *testing.T) {
	h := newHarness(t, 1, 2)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	stream, err := h.client.Run(ctx, &daemonv1.RunRequest{Argv: []string{"hold"}})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "a clone", func() bool { active, _, _, _ := h.cloner.stats(); return active == 1 })
	_ = h.audit.Close() // the disk "fails" while the command runs
	close(h.cloner.gate)
	var res *daemonv1.RunResult
	for res == nil {
		msg, err := stream.Recv()
		if err != nil {
			t.Fatal(err)
		}
		res = msg.GetResult()
	}
	if res.GetOutcome() != daemonv1.Outcome_OUTCOME_FAILED || !strings.Contains(res.GetError(), "audit record could not be written") {
		t.Errorf("result %v, want FAILED for the missing record", res)
	}
	if !strings.Contains(h.logs.String(), "AUDIT RECORD NOT WRITTEN") {
		t.Errorf("not logged loudly: %s", h.logs.String())
	}
}

func TestAuditFailureFailsTheWaitingRuns(t *testing.T) {
	// One run holds the only slot, one waits for it. The audit log breaks
	// while they do: the waiting run must fail without ever getting a
	// clone, since no run may start without a working log.
	h := newHarness(t, 1, 2)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	type outcome struct {
		got *collected
		err error
	}
	results := make(chan outcome, 2)
	start := func() {
		go func() {
			got, err := runToEnd(t, h.client, &daemonv1.RunRequest{Argv: []string{"hold"}})
			results <- outcome{got, err}
		}()
	}
	start()
	waitFor(t, "the first run's clone", func() bool { active, _, _, _ := h.cloner.stats(); return active == 1 })
	start()
	waitFor(t, "the second run in PENDING", func() bool {
		list, err := h.client.List(ctx, &daemonv1.ListRequest{})
		return err == nil && len(list.GetRuns()) == 2 && list.GetRuns()[1].GetState() == daemonv1.RunState_RUN_STATE_PENDING
	})
	_ = h.audit.Close()  // the log fails
	close(h.cloner.gate) // the first run's command finishes and frees the slot

	for range 2 {
		o := <-results
		if o.err != nil {
			t.Fatalf("run: %v", o.err)
		}
		if o.got.result.GetOutcome() != daemonv1.Outcome_OUTCOME_FAILED {
			t.Errorf("result %v, want FAILED", o.got.result)
		}
		if equalStates(o.got.states, []daemonv1.RunState{daemonv1.RunState_RUN_STATE_PENDING,
			daemonv1.RunState_RUN_STATE_STOPPING, daemonv1.RunState_RUN_STATE_FAILED}) &&
			!strings.Contains(o.got.result.GetError(), "no runs start") {
			t.Errorf("the waiting run failed with %q, want the audit log as the reason", o.got.result.GetError())
		}
	}
	if _, _, restores, _ := h.cloner.stats(); restores != 1 {
		t.Errorf("%d restores, want 1: the waiting run must never get a clone", restores)
	}
}

func TestRegistryKeepsOutputBounded(t *testing.T) {
	r := newRegistry(100, 250, 3)
	newEntry := func(id string) *runEntry {
		e := &runEntry{id: id}
		r.add(e)
		return e
	}
	a, b, c := newEntry("a"), newEntry("b"), newEntry("c")
	r.addOutput(a, false, bytes.Repeat([]byte("a"), 80))
	r.addOutput(a, true, bytes.Repeat([]byte("A"), 80)) // a's cap is 100: 20 kept
	if a.out.kept != 100 || !a.out.cut || a.out.total != 160 || len(a.out.chunks) != 2 {
		t.Errorf("a: %+v, want 100 kept of 160, cut", a.out)
	}
	r.addOutput(b, false, bytes.Repeat([]byte("b"), 100))
	r.addOutput(c, false, bytes.Repeat([]byte("c"), 60)) // 260 > 250: a (the oldest) loses its output
	if !a.out.dropped || a.out.kept != 0 || a.out.chunks != nil || a.out.total != 160 {
		t.Errorf("a after the budget: %+v, want its output dropped and its total kept", a.out)
	}
	if r.keptBytes() != 160 || b.out.dropped || c.out.dropped {
		t.Errorf("kept %d bytes, b dropped %v, c dropped %v; want 160 and neither", r.keptBytes(), b.out.dropped, c.out.dropped)
	}
	r.addOutput(a, false, []byte("late")) // a dropped run keeps nothing more
	if a.out.kept != 0 || a.out.total != 164 {
		t.Errorf("a: %+v", a.out)
	}

	// Only the 3 newest finished runs are remembered; active ones always.
	for _, e := range []*runEntry{a, b, c} {
		r.finish(e, &daemonv1.RunResult{RunId: e.id})
	}
	active := newEntry("active")
	d := newEntry("d")
	r.finish(d, &daemonv1.RunResult{})
	if _, ok := r.get("a"); ok {
		t.Error("the oldest finished run is still remembered")
	}
	for _, id := range []string{"b", "c", "d", "active"} {
		if _, ok := r.get(id); !ok {
			t.Errorf("%s was forgotten", id)
		}
	}
	if r.keptBytes() != 160 || len(r.list(true)) != 4 || len(r.list(false)) != 1 || r.list(false)[0] != active {
		t.Errorf("kept %d (want 160, b's and c's), %d runs listed (want 4), active %v", r.keptBytes(), len(r.list(true)), r.list(false))
	}
}

// startOptions are Start's options for a test: the fake store's state
// folder, a fake Firecracker (this binary), a fake cloner.
func startOptions(t *testing.T, store *snapshot.Store, socket string) Options {
	t.Helper()
	t.Setenv(fakeFirecrackerEnv, "1")
	return Options{
		StateDir: store.Root().Dir(), Socket: socket, Firecracker: os.Args[0], MaxConcurrent: 2,
		Cloner: newFakeCloner(store), Version: "sandboxd test", Logger: log.New(io.Discard, "", 0),
	}
}

func TestStartAndShutdown(t *testing.T) {
	store := newFakeStore(t)
	sock := filepath.Join(store.Root().Dir(), "run", "s.sock")
	d, err := Start(startOptions(t, store, sock))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	// A new socket folder is ours alone, and so is the socket.
	for path, want := range map[string]os.FileMode{filepath.Dir(sock): 0o700 | os.ModeDir, sock: 0o600 | os.ModeSocket} {
		if info, err := os.Lstat(path); err != nil || info.Mode() != want {
			t.Errorf("%s: %v, %v; want %v", path, info.Mode(), err, want)
		}
	}
	client := dial(t, sock)
	if got, err := runToEnd(t, client, &daemonv1.RunRequest{Argv: []string{"echo", "via", "Start"}}); err != nil ||
		got.stdout.String() != "via Start\n" {
		t.Fatalf("Run through a started daemon: %v, %v", err, got)
	}

	// A second daemon on the same state folder is refused, whatever its
	// socket; one on the same socket too.
	opts2 := startOptions(t, store, filepath.Join(store.Root().Dir(), "other.sock"))
	if _, err := Start(opts2); err == nil || !strings.Contains(err.Error(), "another sandboxd already uses the state folder") {
		t.Errorf("a second daemon on the same state folder: %v", err)
	}
	other := newFakeStore(t)
	if _, err := Start(startOptions(t, other, sock)); err == nil || !strings.Contains(err.Error(), "already listens") {
		t.Errorf("a second daemon on the same socket: %v", err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), ShutdownTimeout)
	defer cancel()
	if err := d.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if _, err := os.Lstat(sock); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the socket is still there after Shutdown: %v", err)
	}
	if sum, err := audit.Verify(audit.Path(store.Root().AuditDir())); err != nil || sum.Records != 1 {
		t.Errorf("audit log after Shutdown: %+v, %v; want 1 record", sum, err)
	}
	// The lock is free again: a new daemon starts.
	d2, err := Start(startOptions(t, store, sock))
	if err != nil {
		t.Fatalf("Start after Shutdown: %v", err)
	}
	_ = d2.Shutdown(ctx)
}

func TestStartRemovesAStaleSocket(t *testing.T) {
	store := newFakeStore(t)
	sock := filepath.Join(store.Root().Dir(), "s.sock")
	// A socket nobody listens on any more, as a crashed daemon leaves it.
	lis, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	lis.(*net.UnixListener).SetUnlinkOnClose(false)
	_ = lis.Close()
	d, err := Start(startOptions(t, store, sock))
	if err != nil {
		t.Fatalf("Start over a stale socket: %v", err)
	}
	_ = d.Shutdown(t.Context())

	// Anything that is not a socket stays, and Start refuses.
	if err := os.WriteFile(sock, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Start(startOptions(t, store, sock)); err == nil || !strings.Contains(err.Error(), "not a socket") {
		t.Errorf("Start over a file: %v", err)
	}
}

func TestStartRefusesABrokenAuditChain(t *testing.T) {
	store := newFakeStore(t)
	dir := store.Root().AuditDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(audit.Path(dir), []byte(`{"record":{},"prev_hash":"0","hash":"1"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Start(startOptions(t, store, filepath.Join(store.Root().Dir(), "s.sock")))
	if !errors.Is(err, audit.ErrBroken) || !strings.Contains(err.Error(), "sandboxd rotate-audit") {
		t.Errorf("Start with a broken chain: %v, want ErrBroken and the way out", err)
	}
	// Nothing was left listening, and the lock is free.
	l, err := store.Root().Lock()
	if err != nil {
		t.Errorf("the lock is still held after a failed Start: %v", err)
	}
	_ = l.Release()
}

func TestStartChecks(t *testing.T) {
	store := newFakeStore(t)
	opts := startOptions(t, store, filepath.Join(store.Root().Dir(), "s.sock"))
	opts.MaxConcurrent = 33
	if _, err := Start(opts); err == nil || !strings.Contains(err.Error(), "1 to 32") {
		t.Errorf("--max-concurrent 33: %v", err)
	}
	if os.Geteuid() != 0 {
		opts = startOptions(t, store, filepath.Join(store.Root().Dir(), "s.sock"))
		opts.Jailer = true
		if _, err := Start(opts); err == nil || !strings.Contains(err.Error(), "needs root") {
			t.Errorf("jailer mode without root: %v", err)
		}
	}
	t.Setenv("XDG_RUNTIME_DIR", "")
	if _, err := DefaultSocket(false); err == nil || !strings.Contains(err.Error(), "--socket") {
		t.Errorf("DefaultSocket without XDG_RUNTIME_DIR: %v", err)
	}
	t.Setenv("XDG_RUNTIME_DIR", "/run/user/1234")
	if got, _ := DefaultSocket(false); got != "/run/user/1234/sandboxd/sandboxd.sock" {
		t.Errorf("DefaultSocket(rootless) = %s", got)
	}
	if got, _ := DefaultSocket(true); got != "/run/sandboxd/sandboxd.sock" {
		t.Errorf("DefaultSocket(jailer) = %s", got)
	}
}

// streamWithout is a Run stream whose context has no peer credentials.
type streamWithout struct {
	grpc.ServerStream
	ctx context.Context
}

func (s streamWithout) Context() context.Context         { return s.ctx }
func (s streamWithout) Send(*daemonv1.RunResponse) error { return nil }

func TestRunRefusesACallWithoutPeerCredentials(t *testing.T) {
	h := newHarness(t, 1, 1)
	err := h.srv.Run(&daemonv1.RunRequest{Argv: []string{"echo"}}, streamWithout{ctx: t.Context()})
	if status.Code(err) != codes.Unauthenticated {
		t.Errorf("Run without peer credentials = %v, want Unauthenticated", err)
	}
	if _, _, restores, _ := h.cloner.stats(); restores != 0 {
		t.Errorf("%d restores, want none", restores)
	}
	if list, _ := h.srv.List(t.Context(), &daemonv1.ListRequest{All: true}); len(list.GetRuns()) != 0 {
		t.Errorf("the refused call was registered: %v", list.GetRuns())
	}
}

func TestSocketModeComesFromTheUmask(t *testing.T) {
	store := newFakeStore(t)
	// A folder of ours that exists already: listen leaves its mode alone,
	// so only the socket's own mode protects it here.
	sock := filepath.Join(store.Root().Dir(), "s.sock")
	before := currentUmask()
	lis, err := listen(sock, false, "", false)
	if err != nil {
		t.Fatal(err)
	}
	defer lis.Close() //nolint:errcheck // a test listener
	if info, err := os.Lstat(sock); err != nil || info.Mode() != os.ModeSocket|0o600 {
		t.Errorf("socket: %v, %v; want a 0600 socket", info.Mode(), err)
	}
	if after := currentUmask(); after != before {
		t.Errorf("the umask is %04o after listen, was %04o: not restored", after, before)
	}

	// The default folder replaced by a symlink is refused.
	target := t.TempDir()
	link := filepath.Join(store.Root().Dir(), "linkdir")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := listen(filepath.Join(link, "s.sock"), false, "", true); err == nil || !strings.Contains(err.Error(), "not a folder") {
		t.Errorf("listen in a symlinked folder: %v", err)
	}
}

// currentUmask reads the process's umask (setting it back at once).
func currentUmask() int {
	old := unix.Umask(0o022)
	unix.Umask(old)
	return old
}

func TestPeerCredsRefuseOtherConnections(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close() //nolint:errcheck // a test pipe
	defer b.Close() //nolint:errcheck // a test pipe
	if _, _, err := (peerCreds{}).ServerHandshake(a); err == nil {
		t.Error("a non-Unix connection was accepted")
	}
}
