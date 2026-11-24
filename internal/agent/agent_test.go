package agent_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	"github.com/redwan-cpp/MicroVM/internal/agent"
	agentv1 "github.com/redwan-cpp/MicroVM/internal/gen/sandbox/agent/v1"
)

// These tests run a real agent.Server, in this test process, behind a real
// gRPC server. Instead of vsock they use bufconn, an in-memory connection
// from the gRPC project, so they need neither a VM nor root. The commands
// (echo, sh, sleep, ...) run on the machine running the tests.

// newClient serves srv over bufconn and returns a client connected to it.
func newClient(t *testing.T, srv agentv1.AgentServiceServer) agentv1.AgentServiceClient {
	t.Helper()
	lis := bufconn.Listen(1 << 20)
	gs := grpc.NewServer(grpc.MaxRecvMsgSize(agent.MaxRequestSize))
	agentv1.RegisterAgentServiceServer(gs, srv)
	go func() { _ = gs.Serve(lis) }()
	t.Cleanup(gs.Stop)

	cc, err := grpc.NewClient("passthrough:///bufconn",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return lis.DialContext(ctx)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("grpc.NewClient: %v", err)
	}
	t.Cleanup(func() { _ = cc.Close() })
	return agentv1.NewAgentServiceClient(cc)
}

// newAgent returns a client for a fresh Server with fake clock and RNG.
func newAgent(t *testing.T) agentv1.AgentServiceClient {
	t.Helper()
	return newClient(t, agent.NewServer(agent.Config{
		Version: "sandbox-agent test",
		Clock:   &fakeClock{},
		Entropy: &fakeEntropy{},
	}))
}

// result is everything one Exec stream delivered.
type result struct {
	stdout, stderr bytes.Buffer
	exit           *agentv1.ExitStatus
	maxChunk       int
}

// execute runs req to the end. It fails the test if the stream breaks the
// protocol: anything after the exit event, or no exit event at all.
func execute(t *testing.T, client agentv1.AgentServiceClient, req *agentv1.ExecRequest) (*result, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	stream, err := client.Exec(ctx, req)
	if err != nil {
		return nil, err
	}
	res := &result{}
	for {
		msg, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			if res.exit == nil {
				t.Fatal("stream ended without an exit event")
			}
			return res, nil
		}
		if err != nil {
			return res, err
		}
		if res.exit != nil {
			t.Fatalf("event after the exit event: %v", msg)
		}
		switch ev := msg.GetEvent().(type) {
		case *agentv1.ExecResponse_Stdout:
			res.stdout.Write(ev.Stdout)
			res.maxChunk = max(res.maxChunk, len(ev.Stdout))
		case *agentv1.ExecResponse_Stderr:
			res.stderr.Write(ev.Stderr)
			res.maxChunk = max(res.maxChunk, len(ev.Stderr))
		case *agentv1.ExecResponse_Exit:
			res.exit = ev.Exit
		default:
			t.Fatalf("empty event: %v", msg)
		}
	}
}

func mustExecute(t *testing.T, client agentv1.AgentServiceClient, req *agentv1.ExecRequest) *result {
	t.Helper()
	res, err := execute(t, client, req)
	if err != nil {
		t.Fatalf("Exec(%q): %v", req.GetArgv(), err)
	}
	return res
}

func TestExecEcho(t *testing.T) {
	res := mustExecute(t, newAgent(t), &agentv1.ExecRequest{Argv: []string{"echo", "hi"}})

	if got := res.stdout.String(); got != "hi\n" {
		t.Errorf("stdout = %q, want %q", got, "hi\n")
	}
	if res.stderr.Len() != 0 {
		t.Errorf("stderr = %q, want nothing", res.stderr.String())
	}
	if e := res.exit; e.GetExitCode() != 0 || e.GetSignal() != 0 || e.GetTimedOut() || e.GetOutputTruncated() {
		t.Errorf("exit = %v, want a clean exit 0", e)
	}
}

func TestExecStderrAndExitCode(t *testing.T) {
	res := mustExecute(t, newAgent(t), &agentv1.ExecRequest{
		Argv: []string{"sh", "-c", "echo out; echo oops >&2; exit 3"},
	})

	if got := res.stdout.String(); got != "out\n" {
		t.Errorf("stdout = %q, want %q", got, "out\n")
	}
	if got := res.stderr.String(); got != "oops\n" {
		t.Errorf("stderr = %q, want %q", got, "oops\n")
	}
	if got := res.exit.GetExitCode(); got != 3 {
		t.Errorf("exit code = %d, want 3", got)
	}
}

func TestExecSignalExit(t *testing.T) {
	res := mustExecute(t, newAgent(t), &agentv1.ExecRequest{
		Argv: []string{"sh", "-c", "kill -TERM $$"},
	})

	if e := res.exit; e.GetExitCode() != -1 || e.GetSignal() != int32(unix.SIGTERM) || e.GetTimedOut() {
		t.Errorf("exit = %v, want exit_code -1, signal %d", e, unix.SIGTERM)
	}
}

func TestExecStdinAndWorkingDir(t *testing.T) {
	dir := t.TempDir()
	res := mustExecute(t, newAgent(t), &agentv1.ExecRequest{
		Argv:       []string{"sh", "-c", "pwd; cat"},
		Stdin:      []byte("from stdin"),
		WorkingDir: dir,
	})

	if want := dir + "\nfrom stdin"; res.stdout.String() != want {
		t.Errorf("stdout = %q, want %q", res.stdout.String(), want)
	}
}

func TestExecTimeoutKillsTheGroup(t *testing.T) {
	// The script starts a background sleep, prints its PID, then waits.
	// The timeout must kill both, not just the shell.
	start := time.Now()
	res := mustExecute(t, newAgent(t), &agentv1.ExecRequest{
		Argv:      []string{"sh", "-c", "sleep 30 & echo $!; sleep 30"},
		TimeoutMs: 300,
	})

	if took := time.Since(start); took > 5*time.Second {
		t.Errorf("Exec took %v, want about 300ms", took)
	}
	if e := res.exit; !e.GetTimedOut() || e.GetSignal() != int32(unix.SIGKILL) || e.GetExitCode() != -1 {
		t.Errorf("exit = %v, want timed_out with SIGKILL", e)
	}
	if us := res.exit.GetDurationUs(); us < 300_000 {
		t.Errorf("duration = %d µs, want at least the 300ms timeout", us)
	}
	waitUntilGone(t, parsePID(t, res.stdout.String()))
}

func TestExecHostCancelKillsTheGroup(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	stream, err := newAgent(t).Exec(ctx, &agentv1.ExecRequest{
		Argv: []string{"sh", "-c", "sleep 30 & echo $!; sleep 30"},
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	msg, err := stream.Recv() // the PID line
	if err != nil {
		t.Fatalf("Recv: %v", err)
	}
	pid := parsePID(t, string(msg.GetStdout()))

	cancel() // the host gives up on the call
	waitUntilGone(t, pid)
}

func TestExecBackgroundChildDiesWithTheCommand(t *testing.T) {
	// The shell exits at once and leaves a sleep running in the background,
	// holding stdout open. Exec must kill it and return right away.
	start := time.Now()
	res := mustExecute(t, newAgent(t), &agentv1.ExecRequest{
		Argv: []string{"sh", "-c", "sleep 30 & echo $!"},
	})

	if took := time.Since(start); took > 2*time.Second {
		t.Errorf("Exec took %v, want it to return as soon as the shell exits", took)
	}
	if e := res.exit; e.GetExitCode() != 0 || e.GetTimedOut() {
		t.Errorf("exit = %v, want exit 0", e)
	}
	waitUntilGone(t, parsePID(t, res.stdout.String()))
}

func TestExecEscapedGrandchildCannotHangExec(t *testing.T) {
	if _, err := exec.LookPath("setsid"); err != nil {
		t.Skip("setsid is not installed")
	}
	// setsid puts the inner shell in a new session, outside the process
	// group, so the group kill misses it, and it keeps stdout open for 30 s.
	// Exec must still return, after waitDelay (500ms).
	//
	// The FIFO makes the outer shell wait until the inner one has really
	// left the group (it writes its PID only after setsid). Without it, the
	// outer shell could exit first and the group kill would catch the child
	// before it escaped, which tests something else.
	fifo := filepath.Join(t.TempDir(), "escaped")
	if err := unix.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	res := mustExecute(t, newAgent(t), &agentv1.ExecRequest{
		Argv: []string{"sh", "-c", `setsid sh -c 'echo $$ > "$FIFO"; exec sleep 30' & read pid < "$FIFO"; echo "$pid"`},
		Env:  map[string]string{"FIFO": fifo},
	})
	took := time.Since(start)
	pid := parsePID(t, res.stdout.String())
	t.Cleanup(func() { _ = unix.Kill(pid, unix.SIGKILL) })

	if took < 400*time.Millisecond || took > 3*time.Second {
		t.Errorf("Exec took %v, want about 500ms (the wait delay)", took)
	}
	if err := unix.Kill(pid, 0); err != nil {
		t.Errorf("the escaped process %d is gone (%v); it should have survived the group kill", pid, err)
	}
	if e := res.exit; e.GetExitCode() != 0 || e.GetTimedOut() {
		t.Errorf("exit = %v, want exit 0", e)
	}
}

func TestExecOutputCapKeepsDraining(t *testing.T) {
	// 5 MiB of output against a 64 KiB cap. The agent must keep reading
	// (and dropping) the rest, so head runs to its end and exits 0 instead
	// of blocking on a full pipe until the timeout.
	const capBytes = 64 << 10
	res := mustExecute(t, newAgent(t), &agentv1.ExecRequest{
		Argv:           []string{"head", "-c", strconv.Itoa(5 << 20), "/dev/zero"},
		MaxOutputBytes: capBytes,
		TimeoutMs:      20_000,
	})

	if got := res.stdout.Len(); got != capBytes {
		t.Errorf("got %d bytes of stdout, want exactly the cap, %d", got, capBytes)
	}
	if res.maxChunk > agent.MaxChunk {
		t.Errorf("largest chunk = %d bytes, want at most %d", res.maxChunk, agent.MaxChunk)
	}
	if e := res.exit; !e.GetOutputTruncated() || e.GetTimedOut() || e.GetExitCode() != 0 {
		t.Errorf("exit = %v, want exit 0, output_truncated, not timed_out", e)
	}
}

func TestExecCleanEnvironment(t *testing.T) {
	t.Setenv("AGENT_SECRET", "must not leak into commands")
	res := mustExecute(t, newAgent(t), &agentv1.ExecRequest{
		Argv: []string{"env"},
		Env:  map[string]string{"FOO": "bar", "HOME": "/tmp"},
	})

	got := strings.Split(strings.TrimSpace(res.stdout.String()), "\n")
	slices.Sort(got)
	want := []string{"FOO=bar", "HOME=/tmp", "LANG=C.UTF-8", "PATH=" + agent.FixedPath}
	if !slices.Equal(got, want) {
		t.Errorf("environment = %q, want exactly %q", got, want)
	}
}

func TestExecUsesTheFixedPath(t *testing.T) {
	// A program that exists only on the agent's own PATH must not be found,
	// and a program on the fixed PATH must be found whatever the agent's
	// own PATH says.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "only-on-agent-path"), []byte("#!/bin/sh\necho no\n"), 0o755); err != nil { //nolint:gosec // G306: a test script must be executable
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	client := newAgent(t)

	_, err := execute(t, client, &agentv1.ExecRequest{Argv: []string{"only-on-agent-path"}})
	if status.Code(err) != codes.NotFound {
		t.Errorf("program on the agent's PATH: err = %v, want NotFound", err)
	}
	res := mustExecute(t, client, &agentv1.ExecRequest{Argv: []string{"echo", "found"}})
	if got := res.stdout.String(); got != "found\n" {
		t.Errorf("stdout = %q, want %q", got, "found\n")
	}
}

func TestExecRejectsBadRequests(t *testing.T) {
	tooManyEnv := map[string]string{}
	for i := range agent.MaxEnvEntries + 1 {
		tooManyEnv[fmt.Sprintf("V%d", i)] = "x"
	}
	tests := []struct {
		name string
		req  *agentv1.ExecRequest
		want codes.Code
	}{
		{"empty argv", &agentv1.ExecRequest{}, codes.InvalidArgument},
		{"empty argv[0]", &agentv1.ExecRequest{Argv: []string{""}}, codes.InvalidArgument},
		{"NUL in argv", &agentv1.ExecRequest{Argv: []string{"echo", "a\x00b"}}, codes.InvalidArgument},
		{"env name starts with a digit", &agentv1.ExecRequest{Argv: []string{"true"}, Env: map[string]string{"1X": "y"}}, codes.InvalidArgument},
		{"env name with a dash", &agentv1.ExecRequest{Argv: []string{"true"}, Env: map[string]string{"A-B": "y"}}, codes.InvalidArgument},
		{"env overrides PATH", &agentv1.ExecRequest{Argv: []string{"true"}, Env: map[string]string{"PATH": "/tmp"}}, codes.InvalidArgument},
		{"too many env entries", &agentv1.ExecRequest{Argv: []string{"true"}, Env: tooManyEnv}, codes.InvalidArgument},
		{"timeout above 10 minutes", &agentv1.ExecRequest{Argv: []string{"true"}, TimeoutMs: 10*60*1000 + 1}, codes.InvalidArgument},
		{"output cap above 16 MiB", &agentv1.ExecRequest{Argv: []string{"true"}, MaxOutputBytes: 16<<20 + 1}, codes.InvalidArgument},
		{"relative working_dir", &agentv1.ExecRequest{Argv: []string{"true"}, WorkingDir: "tmp"}, codes.InvalidArgument},
		{"unknown program", &agentv1.ExecRequest{Argv: []string{"no-such-program-anywhere"}}, codes.NotFound},
		{"missing working_dir", &agentv1.ExecRequest{Argv: []string{"true"}, WorkingDir: "/no/such/dir"}, codes.NotFound},
	}
	client := newAgent(t)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := execute(t, client, tt.req)
			if status.Code(err) != tt.want {
				t.Errorf("err = %v, want code %v", err, tt.want)
			}
		})
	}
}

func TestHealth(t *testing.T) {
	resp, err := newAgent(t).Health(t.Context(), &agentv1.HealthRequest{})
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	if resp.GetVersion() != "sandbox-agent test" {
		t.Errorf("version = %q, want %q", resp.GetVersion(), "sandbox-agent test")
	}
}

type fakeClock struct {
	set []time.Time
	err error
}

func (c *fakeClock) SetTime(t time.Time) error {
	c.set = append(c.set, t)
	return c.err
}

type fakeEntropy struct {
	got [][]byte
	err error
}

func (e *fakeEntropy) AddEntropy(b []byte) error {
	e.got = append(e.got, bytes.Clone(b))
	return e.err
}

func TestSyncTime(t *testing.T) {
	clock := &fakeClock{}
	client := newClient(t, agent.NewServer(agent.Config{Clock: clock}))

	want := time.Date(2026, 11, 18, 12, 0, 0, 123, time.UTC)
	if _, err := client.SyncTime(t.Context(), &agentv1.SyncTimeRequest{UnixNanos: want.UnixNano()}); err != nil {
		t.Fatalf("SyncTime: %v", err)
	}
	if len(clock.set) != 1 || !clock.set[0].Equal(want) {
		t.Errorf("clock set to %v, want [%v]", clock.set, want)
	}

	if _, err := client.SyncTime(t.Context(), &agentv1.SyncTimeRequest{UnixNanos: 0}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("SyncTime(0): err = %v, want InvalidArgument", err)
	}

	clock.err = errors.New("operation not permitted")
	if _, err := client.SyncTime(t.Context(), &agentv1.SyncTimeRequest{UnixNanos: want.UnixNano()}); status.Code(err) != codes.Internal {
		t.Errorf("SyncTime with a failing clock: err = %v, want Internal", err)
	}
}

func TestReseed(t *testing.T) {
	rng := &fakeEntropy{}
	client := newClient(t, agent.NewServer(agent.Config{Entropy: rng}))

	for _, n := range []int{agent.MinEntropy, agent.MaxEntropy} {
		if _, err := client.Reseed(t.Context(), &agentv1.ReseedRequest{Entropy: bytes.Repeat([]byte{7}, n)}); err != nil {
			t.Errorf("Reseed(%d bytes): %v", n, err)
		}
	}
	if len(rng.got) != 2 || len(rng.got[0]) != agent.MinEntropy || len(rng.got[1]) != agent.MaxEntropy {
		t.Errorf("entropy received = %d calls, want 2 with %d and %d bytes", len(rng.got), agent.MinEntropy, agent.MaxEntropy)
	}

	for _, n := range []int{0, agent.MinEntropy - 1, agent.MaxEntropy + 1} {
		_, err := client.Reseed(t.Context(), &agentv1.ReseedRequest{Entropy: make([]byte, n)})
		if status.Code(err) != codes.InvalidArgument {
			t.Errorf("Reseed(%d bytes): err = %v, want InvalidArgument", n, err)
		}
	}
}

// parsePID reads the PID that a test script printed on its first line.
func parsePID(t *testing.T, out string) int {
	t.Helper()
	first, _, _ := strings.Cut(out, "\n")
	pid, err := strconv.Atoi(strings.TrimSpace(first))
	if err != nil {
		t.Fatalf("no PID in the output %q", out)
	}
	return pid
}

// waitUntilGone waits until process pid no longer runs. A process that was
// killed may stay a zombie (dead, waiting for its parent to collect it);
// that counts as gone.
func waitUntilGone(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
		if err != nil {
			return // no such process
		}
		// /proc/PID/stat is "PID (name) STATE ...": the state letter comes
		// after the ")" that ends the name.
		if i := bytes.LastIndexByte(stat, ')'); i >= 0 && i+2 < len(stat) && stat[i+2] == 'Z' {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Errorf("process %d is still running", pid)
	_ = unix.Kill(pid, unix.SIGKILL)
}

// fakeScratch "mounts" by remembering the size; its folder is a test
// folder on the host.
type fakeScratch struct {
	dir    string
	mounts []uint64
	err    error
}

func (s *fakeScratch) Mount(size uint64) error {
	s.mounts = append(s.mounts, size)
	return s.err
}

func (s *fakeScratch) Dir() string { return s.dir }

func TestMountScratch(t *testing.T) {
	scratch := &fakeScratch{dir: t.TempDir()}
	client := newClient(t, agent.NewServer(agent.Config{Clock: &fakeClock{}, Entropy: &fakeEntropy{}, Scratch: scratch}))

	// Before MountScratch, a command runs in "/".
	if got := mustExecute(t, client, &agentv1.ExecRequest{Argv: []string{"pwd"}}).stdout.String(); got != "/\n" {
		t.Errorf("pwd before MountScratch = %q, want /", got)
	}

	if _, err := client.MountScratch(t.Context(), &agentv1.MountScratchRequest{SizeBytes: 0}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("MountScratch(0 bytes): %v, want InvalidArgument", err)
	}
	for range 2 { // the second call changes nothing
		if _, err := client.MountScratch(t.Context(), &agentv1.MountScratchRequest{SizeBytes: 64 << 20}); err != nil {
			t.Fatalf("MountScratch: %v", err)
		}
	}
	if !slices.Equal(scratch.mounts, []uint64{64 << 20}) {
		t.Errorf("mounted %v, want exactly once with 64 MiB", scratch.mounts)
	}
	if _, err := client.MountScratch(t.Context(), &agentv1.MountScratchRequest{SizeBytes: 128 << 20}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("MountScratch with another size: %v, want FailedPrecondition", err)
	}

	// Now the scratch disk is the default working folder; an explicit one
	// still wins.
	if got := mustExecute(t, client, &agentv1.ExecRequest{Argv: []string{"pwd"}}).stdout.String(); got != scratch.dir+"\n" {
		t.Errorf("pwd after MountScratch = %q, want %s", got, scratch.dir)
	}
	if got := mustExecute(t, client, &agentv1.ExecRequest{Argv: []string{"pwd"}, WorkingDir: "/"}).stdout.String(); got != "/\n" {
		t.Errorf("pwd with working_dir / = %q, want /", got)
	}
}

func TestMountScratchFailureIsReported(t *testing.T) {
	scratch := &fakeScratch{dir: t.TempDir(), err: errors.New("no /dev/vdb")}
	client := newClient(t, agent.NewServer(agent.Config{Scratch: scratch}))
	_, err := client.MountScratch(t.Context(), &agentv1.MountScratchRequest{SizeBytes: 1 << 20})
	if status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), "no /dev/vdb") {
		t.Errorf("MountScratch = %v, want FailedPrecondition with the reason", err)
	}
	// A failed mount leaves the default folder alone, and may be retried.
	if got := mustExecute(t, client, &agentv1.ExecRequest{Argv: []string{"pwd"}}).stdout.String(); got != "/\n" {
		t.Errorf("pwd after a failed MountScratch = %q, want /", got)
	}
	scratch.err = nil
	if _, err := client.MountScratch(t.Context(), &agentv1.MountScratchRequest{SizeBytes: 1 << 20}); err != nil {
		t.Errorf("MountScratch after a failure: %v", err)
	}
}

func TestCheckExecRequest(t *testing.T) {
	if err := agent.CheckExecRequest(&agentv1.ExecRequest{Argv: []string{"echo", "hi"}, Env: map[string]string{"A": "b"},
		Stdin: make([]byte, agent.MaxStdin)}); err != nil {
		t.Errorf("a valid request: %v", err)
	}
	// The host cannot look for programs in the guest, so an unknown one
	// passes here; the agent then answers NotFound.
	if err := agent.CheckExecRequest(&agentv1.ExecRequest{Argv: []string{"no-such-program-anywhere"}}); err != nil {
		t.Errorf("an unknown program: %v, want nil", err)
	}
	for name, req := range map[string]*agentv1.ExecRequest{
		"empty argv":    {},
		"NUL in argv":   {Argv: []string{"a\x00"}},
		"PATH in env":   {Argv: []string{"true"}, Env: map[string]string{"PATH": "/"}},
		"bad env name":  {Argv: []string{"true"}, Env: map[string]string{"A B": "c"}},
		"stdin too big": {Argv: []string{"true"}, Stdin: make([]byte, agent.MaxStdin+1)},
		"long timeout":  {Argv: []string{"true"}, TimeoutMs: 10*60*1000 + 1},
	} {
		if err := agent.CheckExecRequest(req); err == nil {
			t.Errorf("%s: CheckExecRequest accepted it", name)
		}
	}
	// The largest stdin plus argv and env fits in one request message.
	if agent.MaxStdin+64<<10 != agent.MaxRequestSize {
		t.Errorf("MaxStdin %d leaves %d bytes of the %d-byte message for the rest, want 64 KiB",
			agent.MaxStdin, agent.MaxRequestSize-agent.MaxStdin, agent.MaxRequestSize)
	}
}

func TestExecAcceptsTheLargestStdin(t *testing.T) {
	stdin := bytes.Repeat([]byte("x"), agent.MaxStdin)
	res := mustExecute(t, newAgent(t), &agentv1.ExecRequest{
		Argv: []string{"wc", "-c"}, Stdin: stdin, Env: map[string]string{"A": strings.Repeat("y", 1000)},
	})
	if got := strings.TrimSpace(res.stdout.String()); got != strconv.Itoa(agent.MaxStdin) {
		t.Errorf("wc -c = %q, want %d", got, agent.MaxStdin)
	}
}
