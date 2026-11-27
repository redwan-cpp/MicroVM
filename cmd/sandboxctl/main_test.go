package main

import (
	"bytes"
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/redwan-cpp/MicroVM/internal/audit"
	daemonv1 "github.com/redwan-cpp/MicroVM/internal/gen/sandbox/daemon/v1"
)

// fakeDaemon answers like sandboxd. Run plays the result named by
// argv[0]; it remembers the last request.
type fakeDaemon struct {
	daemonv1.UnimplementedSandboxServiceServer
	last *daemonv1.RunRequest
}

func (f *fakeDaemon) Run(req *daemonv1.RunRequest, stream daemonv1.SandboxService_RunServer) error {
	f.last = req
	now := timestamppb.New(time.Now())
	for _, s := range []daemonv1.RunState{daemonv1.RunState_RUN_STATE_PENDING, daemonv1.RunState_RUN_STATE_RUNNING} {
		_ = stream.Send(&daemonv1.RunResponse{Event: &daemonv1.RunResponse_State{State: &daemonv1.StateChange{State: s, At: now}}})
	}
	_ = stream.Send(&daemonv1.RunResponse{Event: &daemonv1.RunResponse_Stdout{Stdout: []byte("to stdout\n")}})
	_ = stream.Send(&daemonv1.RunResponse{Event: &daemonv1.RunResponse_Stderr{Stderr: []byte("to stderr\n")}})
	res := &daemonv1.RunResult{RunId: "r-0123456789abcdef", Mode: daemonv1.Mode_MODE_ROOTLESS, Timings: &daemonv1.Timings{TotalUs: 1500}}
	switch req.GetArgv()[0] {
	case "exit7":
		res.Outcome, res.Exited, res.ExitCode = daemonv1.Outcome_OUTCOME_EXITED, true, 7
	case "killed":
		res.Outcome, res.Exited, res.ExitCode, res.Signal = daemonv1.Outcome_OUTCOME_EXITED, true, -1, 9
	case "timeout":
		res.Outcome, res.TimedOut = daemonv1.Outcome_OUTCOME_TIMED_OUT, true
	case "flood":
		res.Outcome, res.OutputTruncated = daemonv1.Outcome_OUTCOME_OUTPUT_TRUNCATED, true
	case "refuse":
		return status.Error(codes.InvalidArgument, "argv is bad")
	default:
		res.Outcome, res.Error = daemonv1.Outcome_OUTCOME_FAILED, "restore: no"
	}
	return stream.Send(&daemonv1.RunResponse{Event: &daemonv1.RunResponse_Result{Result: res}})
}

func (f *fakeDaemon) List(_ context.Context, req *daemonv1.ListRequest) (*daemonv1.ListResponse, error) {
	runs := []*daemonv1.RunInfo{{RunId: "r-00000000000000aa", State: daemonv1.RunState_RUN_STATE_RUNNING, SizeClass: "small",
		Mode: daemonv1.Mode_MODE_JAILER, Argv: []string{"sleep", "9"}, CallerUid: 1000, CreatedAt: timestamppb.Now()}}
	if req.GetAll() {
		runs = append(runs, &daemonv1.RunInfo{RunId: "r-00000000000000bb", State: daemonv1.RunState_RUN_STATE_DONE,
			SizeClass: "medium", Mode: daemonv1.Mode_MODE_JAILER, Argv: []string{"true"}, CreatedAt: timestamppb.Now(),
			Result: &daemonv1.RunResult{Outcome: daemonv1.Outcome_OUTCOME_EXITED, Exited: true}})
	}
	return &daemonv1.ListResponse{Runs: runs}, nil
}

func (f *fakeDaemon) Logs(_ context.Context, req *daemonv1.LogsRequest) (*daemonv1.LogsResponse, error) {
	if req.GetRunId() != "r-00000000000000bb" {
		return nil, status.Error(codes.NotFound, "no run "+req.GetRunId())
	}
	return &daemonv1.LogsResponse{RunId: req.GetRunId(), Finished: true, Cut: true, KeptBytes: 4, OutputBytes: 9, Chunks: []*daemonv1.OutputChunk{
		{Data: &daemonv1.OutputChunk_Stdout{Stdout: []byte("out\n")}},
		{Data: &daemonv1.OutputChunk_Stderr{Stderr: []byte("e\n")}},
	}}, nil
}

// serveFake starts a fake daemon on a Unix socket and returns its path.
func serveFake(t *testing.T, srv daemonv1.SandboxServiceServer) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "ctl-") // short: socket paths
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "s.sock")
	lis, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	gs := grpc.NewServer()
	daemonv1.RegisterSandboxServiceServer(gs, srv)
	go func() { _ = gs.Serve(lis) }()
	t.Cleanup(gs.Stop)
	return sock
}

// sandboxctl runs the CLI with args and returns its exit code and output.
func sandboxctl(t *testing.T, stdin string, environ map[string]string, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := cli(args, env{
		stdin: strings.NewReader(stdin), stdout: &stdout, stderr: &stderr,
		getenv: func(k string) string { return environ[k] },
	})
	return code, stdout.String(), stderr.String()
}

func TestRunExitCodesAndStreams(t *testing.T) {
	fake := &fakeDaemon{}
	sock := serveFake(t, fake)
	for argv0, want := range map[string]int{"exit7": 7, "killed": 128 + 9, "timeout": 124, "flood": 123, "fail": 125, "refuse": 125} {
		code, stdout, stderr := sandboxctl(t, "", nil, "--socket", sock, "run", "--", argv0, "x")
		if code != want {
			t.Errorf("run %s: exit %d, want %d (stderr %q)", argv0, code, want, stderr)
		}
		if argv0 == "refuse" {
			if !strings.Contains(stderr, "argv is bad") {
				t.Errorf("run refuse: stderr %q, want the daemon's reason", stderr)
			}
			continue
		}
		if stdout != "to stdout\n" || !strings.HasPrefix(stderr, "to stderr\n") {
			t.Errorf("run %s: stdout %q, stderr %q", argv0, stdout, stderr)
		}
		// A failed run always explains itself; others only with -v.
		if (argv0 == "fail") != strings.Contains(stderr, "restore: no") {
			t.Errorf("run %s: stderr %q", argv0, stderr)
		}
	}

	// -v shows the states, the outcome and the timings; --socket may come
	// after the subcommand too, and the flags reach the request.
	code, _, stderr := sandboxctl(t, "hello", nil, "run", "--socket", sock, "-v", "--class", "medium", "--timeout", "1.5s",
		"--max-output", "64KiB", "--env", "A=b", "--env", "C=d=e", "--stdin", "--", "exit7")
	if code != 7 || !strings.Contains(stderr, "RUNNING") || !strings.Contains(stderr, "EXITED 7 (rootless mode)") ||
		!strings.Contains(stderr, "total 1.5ms") {
		t.Errorf("run -v: exit %d, stderr %q", code, stderr)
	}
	r := fake.last
	if r.GetSizeClass() != "medium" || r.GetTimeoutMs() != 1500 || r.GetMaxOutputBytes() != 64<<10 ||
		r.GetEnv()["A"] != "b" || r.GetEnv()["C"] != "d=e" || string(r.GetStdin()) != "hello" {
		t.Errorf("request %v", r)
	}
}

func TestRunFlagErrors(t *testing.T) {
	for _, args := range [][]string{
		{"run"},                               // no command
		{"run", "--timeout", "0s", "--", "x"}, // no time at all
		{"run", "--timeout", "11m", "--", "x"},
		{"run", "--timeout", "1500us", "--", "x"},
		{"run", "--max-output", "lots", "--", "x"},
		{"run", "--env", "NOEQUALS", "--", "x"},
		{"nonsense"},
		{},
	} {
		if code, _, stderr := sandboxctl(t, "", nil, args...); code != exitUsage || stderr == "" {
			t.Errorf("sandboxctl %q: exit %d, stderr %q; want %d and a message", args, code, stderr, exitUsage)
		}
	}
	// Too much stdin is refused before anything is sent.
	big := strings.Repeat("x", maxStdin+1)
	if code, _, stderr := sandboxctl(t, big, nil, "--socket", "/nonexistent", "run", "--stdin", "--", "cat"); code != exitUsage ||
		!strings.Contains(stderr, "960 KiB") {
		t.Errorf("too much stdin: exit %d, %q", code, stderr)
	}
}

func TestNoDaemon(t *testing.T) {
	code, _, stderr := sandboxctl(t, "", map[string]string{SocketEnv: "/nonexistent/s.sock"}, "run", "--", "true")
	if code != exitFailed || !strings.Contains(stderr, "sandboxd is not running") || !strings.Contains(stderr, "/nonexistent/s.sock") {
		t.Errorf("run without a daemon: exit %d, %q", code, stderr)
	}
	if code, _, _ := sandboxctl(t, "", map[string]string{SocketEnv: "/nonexistent/s.sock"}, "ps"); code != 1 {
		t.Errorf("ps without a daemon: exit %d, want 1", code)
	}
}

func TestSocketPath(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "sandboxd"), 0o700); err != nil {
		t.Fatal(err)
	}
	getenv := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	if got := socketPath("/flag.sock", getenv(map[string]string{SocketEnv: "/env.sock"})); got != "/flag.sock" {
		t.Errorf("the flag wins: %s", got)
	}
	if got := socketPath("", getenv(map[string]string{SocketEnv: "/env.sock", "XDG_RUNTIME_DIR": dir})); got != "/env.sock" {
		t.Errorf("then the environment: %s", got)
	}
	// No rootless socket yet: the jailer-mode one.
	if got := socketPath("", getenv(map[string]string{"XDG_RUNTIME_DIR": dir})); got != jailerSocket {
		t.Errorf("without a rootless socket: %s", got)
	}
	if err := os.WriteFile(filepath.Join(dir, "sandboxd", "sandboxd.sock"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if got := socketPath("", getenv(map[string]string{"XDG_RUNTIME_DIR": dir})); got != filepath.Join(dir, "sandboxd", "sandboxd.sock") {
		t.Errorf("with a rootless socket: %s", got)
	}
}

func TestParseSize(t *testing.T) {
	for in, want := range map[string]uint64{"4096": 4096, "64KiB": 64 << 10, "1MiB": 1 << 20, "2M": 2 << 20, "16 MiB": 16 << 20,
		"1GiB": 1 << 30, "10B": 10, "3K": 3 << 10} {
		if got, err := parseSize(in); err != nil || got != want {
			t.Errorf("parseSize(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, in := range []string{"", "MiB", "-1", "1.5MiB", "1TiB", "99999999999999999999", "9000000000GiB"} {
		if _, err := parseSize(in); err == nil {
			t.Errorf("parseSize(%q) succeeded", in)
		}
	}
}

func TestExitCode(t *testing.T) {
	for _, tc := range []struct {
		res  *daemonv1.RunResult
		want int
	}{
		{&daemonv1.RunResult{Outcome: daemonv1.Outcome_OUTCOME_EXITED, Exited: true}, 0},
		{&daemonv1.RunResult{Outcome: daemonv1.Outcome_OUTCOME_EXITED, Exited: true, ExitCode: 255}, 255},
		{&daemonv1.RunResult{Outcome: daemonv1.Outcome_OUTCOME_EXITED, Exited: true, ExitCode: -1, Signal: 15}, 143},
		{&daemonv1.RunResult{Outcome: daemonv1.Outcome_OUTCOME_TIMED_OUT}, 124},
		{&daemonv1.RunResult{Outcome: daemonv1.Outcome_OUTCOME_OUTPUT_TRUNCATED}, 123},
		{&daemonv1.RunResult{Outcome: daemonv1.Outcome_OUTCOME_FAILED}, 125},
		{&daemonv1.RunResult{}, 125}, // no outcome is never success
	} {
		if got := exitCode(tc.res); got != tc.want {
			t.Errorf("exitCode(%v) = %d, want %d", tc.res, got, tc.want)
		}
	}
}

func TestLogNotes(t *testing.T) {
	notes := strings.Join(logNotes(&daemonv1.LogsResponse{Cut: true, KeptBytes: 1 << 20, OutputBytes: 5 << 20, OutputTruncated: true}), "\n")
	if !strings.Contains(notes, "still going") || !strings.Contains(notes, "first 1048576 of its 5242880") || !strings.Contains(notes, "cap") {
		t.Errorf("notes %q", notes)
	}
	if notes := logNotes(&daemonv1.LogsResponse{Finished: true, Dropped: true, Cut: true}); len(notes) != 1 || !strings.Contains(notes[0], "dropped") {
		t.Errorf("notes %q, want only that it was dropped", notes)
	}
	if notes := logNotes(&daemonv1.LogsResponse{Finished: true}); len(notes) != 0 {
		t.Errorf("notes %q for a complete log", notes)
	}
}

func TestCommandIsCleanAndShort(t *testing.T) {
	if got := command([]string{"sh", "-c", "echo \x1b[31mred\x1b[0m\nnext"}, 100); got != "sh -c echo [31mred[0m next" {
		t.Errorf("command = %q", got)
	}
	if got := command([]string{strings.Repeat("x", 50)}, 10); got != "xxxxxxx..." {
		t.Errorf("command = %q", got)
	}
}

func TestAuditVerify(t *testing.T) {
	dir := t.TempDir()
	l, err := audit.Open(filepath.Join(dir, audit.DirName), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Append(audit.Record{Kind: audit.KindRun, Run: &audit.Run{RunID: "r-1"}}); err != nil {
		t.Fatal(err)
	}
	_ = l.Close()
	code, stdout, _ := sandboxctl(t, "", nil, "audit", "verify", "--state-dir", dir)
	if code != 0 || !strings.Contains(stdout, "1 records, hash chain OK") {
		t.Errorf("verify: exit %d, %q", code, stdout)
	}
	// $MICROVM_STATE_DIR is the default.
	if code, _, _ := sandboxctl(t, "", map[string]string{"MICROVM_STATE_DIR": dir}, "audit", "verify"); code != 0 {
		t.Errorf("verify with the environment: exit %d", code)
	}
	path := audit.Path(filepath.Join(dir, audit.DirName))
	data, _ := os.ReadFile(path)                                                                            //nolint:gosec // G304: the test's own audit log
	if err := os.WriteFile(path, bytes.Replace(data, []byte("r-1"), []byte("r-2"), 1), 0o600); err != nil { //nolint:gosec // G703: the test's own audit log
		t.Fatal(err)
	}
	code, _, stderr := sandboxctl(t, "", nil, "audit", "verify", "--state-dir", dir)
	if code != 1 || !strings.Contains(stderr, "BROKEN") || !strings.Contains(stderr, "line 1") || !strings.Contains(stderr, "rotate-audit") {
		t.Errorf("verify a tampered log: exit %d, %q", code, stderr)
	}
}

func TestPsAndLogs(t *testing.T) {
	sock := serveFake(t, &fakeDaemon{})
	code, stdout, _ := sandboxctl(t, "", nil, "--socket", sock, "ps")
	if code != 0 || !strings.Contains(stdout, "RUN ID") || !strings.Contains(stdout, "r-00000000000000aa  RUNNING") ||
		!strings.Contains(stdout, "jailer") || strings.Contains(stdout, "r-00000000000000bb") {
		t.Errorf("ps: exit %d\n%s", code, stdout)
	}
	if _, stdout, _ = sandboxctl(t, "", nil, "--socket", sock, "ps", "--all"); !strings.Contains(stdout, "EXITED 0") {
		t.Errorf("ps --all:\n%s", stdout)
	}
	code, stdout, stderr := sandboxctl(t, "", nil, "--socket", sock, "logs", "r-00000000000000bb")
	if code != 0 || stdout != "out\n" || !strings.HasPrefix(stderr, "e\n") || !strings.Contains(stderr, "output cut") {
		t.Errorf("logs: exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	if code, _, stderr = sandboxctl(t, "", nil, "--socket", sock, "logs", "r-00000000000000cc"); code != 1 || !strings.Contains(stderr, "no run") {
		t.Errorf("logs of an unknown run: exit %d, %q", code, stderr)
	}
	if code, _, _ = sandboxctl(t, "", nil, "--socket", sock, "logs"); code != exitUsage {
		t.Errorf("logs without an ID: exit %d", code)
	}
}

func TestVersion(t *testing.T) {
	if code, stdout, _ := sandboxctl(t, "", nil, "version"); code != 0 || !strings.HasPrefix(stdout, "sandboxctl ") {
		t.Errorf("version: exit %d, %q", code, stdout)
	}
}

func TestListenLoopbackOnly(t *testing.T) {
	for _, addr := range []string{"0.0.0.0:0", "192.168.1.10:0", "example.com:0", ":::0", "127.0.0.1", "nonsense"} {
		if lis, err := listenLoopback(addr); err == nil {
			_ = lis.Close()
			t.Errorf("listenLoopback(%q) listened", addr)
		}
	}
	for _, addr := range []string{"127.0.0.1:0", "localhost:0", ":0"} {
		lis, err := listenLoopback(addr)
		if err != nil {
			t.Errorf("listenLoopback(%q): %v", addr, err)
			continue
		}
		if !strings.HasPrefix(lis.Addr().String(), "127.0.0.1:") {
			t.Errorf("listenLoopback(%q) listens on %v", addr, lis.Addr())
		}
		_ = lis.Close()
	}
}

func TestUICommand(t *testing.T) {
	// Without a daemon it says so, before it serves anything.
	code, _, stderr := sandboxctl(t, "", map[string]string{SocketEnv: "/nonexistent/s.sock"}, "ui", "--listen", "127.0.0.1:0")
	if code != 1 || !strings.Contains(stderr, "sandboxd is not running") {
		t.Errorf("ui without a daemon: exit %d, %q", code, stderr)
	}
	if code, _, stderr := sandboxctl(t, "", nil, "ui", "--listen", "0.0.0.0:8090"); code != exitUsage || !strings.Contains(stderr, "loopback") {
		t.Errorf("ui on all interfaces: exit %d, %q", code, stderr)
	}
	if code, _, _ := sandboxctl(t, "", nil, "ui", "--replay", "a", "--record", "b"); code != exitUsage {
		t.Errorf("ui --replay with --record: exit %d", code)
	}

	// export: a recording becomes one HTML file, never over another.
	dir := t.TempDir()
	session := filepath.Join(dir, "s.jsonl")
	if err := os.WriteFile(session, []byte(`{"meta":{"format":"microvm-sandbox-session","version":1,"created_at":"2026-11-25T09:30:00Z",`+
		`"ui_version":"test","info":{"mode":"rootless","daemon_version":"d","firecracker_version":"v1.17.0","arch":"x86_64",`+
		`"max_concurrent":4,"max_pending":64,"host_kind":"physical","host_detail":"","classes":[],"max_clones":4,"ui_version":"test"}}}`+"\n"+
		`{"end":{"ended_at":"2026-11-25T09:31:00Z","verify":{"ok":true,"records":0}}}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "demo.html")
	code, stdout, stderr := sandboxctl(t, "", nil, "ui", "export", "--from", session, "--out", out)
	if code != 0 || !strings.Contains(stdout, "self-contained") {
		t.Fatalf("export: exit %d, %q %q", code, stdout, stderr)
	}
	page, err := os.ReadFile(out) //nolint:gosec // G304: the test's own file
	if err != nil || !bytes.Contains(page, []byte("RECORDING, not live.")) {
		t.Errorf("the export: %v", err)
	}
	if code, _, stderr := sandboxctl(t, "", nil, "ui", "export", "--from", session, "--out", out); code != 1 || !strings.Contains(stderr, "exists already") {
		t.Errorf("a second export: exit %d, %q", code, stderr)
	}
	if code, _, _ := sandboxctl(t, "", nil, "ui", "export"); code != exitUsage {
		t.Errorf("export without --from: exit %d", code)
	}
	bad := filepath.Join(dir, "bad.jsonl")
	if err := os.WriteFile(bad, []byte(`{"live":true}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, _, stderr := sandboxctl(t, "", nil, "ui", "export", "--from", bad); code != 1 || !strings.Contains(stderr, "unknown field") {
		t.Errorf("export of a file that is not a recording: exit %d, %q", code, stderr)
	}
}
