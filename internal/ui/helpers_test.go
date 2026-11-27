package ui

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/cookiejar"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/redwan-cpp/MicroVM/internal/audit"
	daemonv1 "github.com/redwan-cpp/MicroVM/internal/gen/sandbox/daemon/v1"
)

// These tests run the UI server against a fake sandboxd: a real gRPC
// server on a Unix socket whose Run plays scripted runs, writes an audit
// record for each (as the real daemon does) and remembers every request.

// fakeDaemon answers like sandboxd.
type fakeDaemon struct {
	daemonv1.UnimplementedSandboxServiceServer
	audit    *audit.Log
	stateDir string

	mu   sync.Mutex
	reqs []*daemonv1.RunRequest
	runs []*daemonv1.RunInfo
	n    int
	gate chan struct{} // "block" runs wait on it (or on the call's end)
}

func (f *fakeDaemon) Info(context.Context, *daemonv1.InfoRequest) (*daemonv1.InfoResponse, error) {
	return &daemonv1.InfoResponse{
		Mode: daemonv1.Mode_MODE_ROOTLESS, DaemonVersion: "sandboxd test", FirecrackerVersion: "v1.17.0", Arch: "x86_64",
		MaxConcurrent: 4, MaxPending: 64, HostKind: daemonv1.HostKind_HOST_KIND_PHYSICAL, HostDetail: "test",
		Classes: []*daemonv1.ClassInfo{
			{Name: "small", Vcpus: 1, MemoryMib: 256, KernelVersion: "6.1.155", AgentVersion: "sandbox-agent test", ColdBootUs: 81000,
				SnapshotVersion: "20261125T090000Z-aaaaaa", SnapshotCreatedAt: timestamppb.New(time.Date(2026, 11, 25, 9, 0, 0, 0, time.UTC))},
			{Name: "medium", Vcpus: 2, MemoryMib: 512, ColdBootUs: 84000},
		},
	}, nil
}

func (f *fakeDaemon) HostStats(context.Context, *daemonv1.HostStatsRequest) (*daemonv1.HostStatsResponse, error) {
	return &daemonv1.HostStatsResponse{
		At: timestamppb.Now(), FirecrackerProcesses: &daemonv1.Count{}, RunDirs: &daemonv1.Count{},
		JailDirs: &daemonv1.Count{}, Cgroups: &daemonv1.Count{Error: "no cgroup v2: open " + f.stateDir + "/x: permission denied"},
	}, nil
}

func (f *fakeDaemon) List(context.Context, *daemonv1.ListRequest) (*daemonv1.ListResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return &daemonv1.ListResponse{Runs: append([]*daemonv1.RunInfo(nil), f.runs...)}, nil
}

// Run plays a run by the command's text: "demo1"'s secret writes, "block"
// (until released or cancelled), "evil" (hostile output), "refuse".
func (f *fakeDaemon) Run(req *daemonv1.RunRequest, stream daemonv1.SandboxService_RunServer) error {
	f.mu.Lock()
	f.reqs = append(f.reqs, req)
	f.n++
	id := fmt.Sprintf("r-%016x", f.n)
	f.mu.Unlock()
	cmd := strings.Join(req.GetArgv(), " ")
	if strings.Contains(cmd, "refuse") {
		return status.Error(codes.InvalidArgument, "argv is bad")
	}
	state := func(s daemonv1.RunState, detail string) error {
		return stream.Send(&daemonv1.RunResponse{Event: &daemonv1.RunResponse_State{State: &daemonv1.StateChange{
			State: s, At: timestamppb.Now(), RunId: id, Detail: detail}}})
	}
	out := func(stderr bool, b []byte) error {
		if stderr {
			return stream.Send(&daemonv1.RunResponse{Event: &daemonv1.RunResponse_Stderr{Stderr: b}})
		}
		return stream.Send(&daemonv1.RunResponse{Event: &daemonv1.RunResponse_Stdout{Stdout: b}})
	}
	for _, s := range []daemonv1.RunState{daemonv1.RunState_RUN_STATE_PENDING, daemonv1.RunState_RUN_STATE_RESTORING,
		daemonv1.RunState_RUN_STATE_MOUNTING, daemonv1.RunState_RUN_STATE_RUNNING} {
		if err := state(s, ""); err != nil {
			return err
		}
	}
	res := &daemonv1.RunResult{RunId: id, Outcome: daemonv1.Outcome_OUTCOME_EXITED, Exited: true, Mode: daemonv1.Mode_MODE_ROOTLESS,
		Timings: &daemonv1.Timings{PrepareUs: 100, ScratchUs: 300, ProcessStartUs: 1400, LoadSnapshotUs: 12000, AgentReadyUs: 5000,
			DeTwinUs: 1100, RestoreUs: 19900, MountUs: 4400, ExecUs: 1000, DestroyUs: 70000, TotalUs: 95000}}
	switch {
	case strings.Contains(cmd, "secret of clone"):
		n := strings.TrimPrefix(strings.Fields(strings.SplitAfter(cmd, "secret-")[1])[0], "")
		_ = out(false, []byte("lost+found\nsecret-"+n[:1]+"\n"))
		_ = out(false, []byte("secret of clone "+n[:1]+"\n"))
	case strings.Contains(cmd, "block"):
		select {
		case <-f.gate:
		case <-stream.Context().Done():
			return stream.Context().Err()
		}
	case strings.Contains(cmd, "evil"):
		for _, chunk := range evilOutput {
			_ = out(false, chunk)
		}
		_ = out(true, []byte("some stderr\n"))
		res.Error = "restore: " + f.stateDir + "/runs/" + id + ": gone"
	}
	detail := ""
	if strings.Contains(cmd, "evil") {
		detail = "clone: destroy " + f.stateDir + "/runs/" + id + ": busy"
	}
	if err := state(daemonv1.RunState_RUN_STATE_STOPPING, detail); err != nil {
		return err
	}
	code := int(res.GetExitCode())
	if err := f.audit.Append(audit.Record{Kind: audit.KindRun, Time: time.Now().UTC(), Run: &audit.Run{
		RunID: id, Argv: req.GetArgv(), Outcome: "EXITED", FinalState: "DONE", Exited: true, ExitCode: &code}}); err != nil {
		return err
	}
	f.mu.Lock()
	f.runs = append(f.runs, &daemonv1.RunInfo{RunId: id, State: daemonv1.RunState_RUN_STATE_DONE, Argv: req.GetArgv(),
		Mode: daemonv1.Mode_MODE_ROOTLESS, SizeClass: req.GetSizeClass(), CreatedAt: timestamppb.Now(), Result: res})
	f.mu.Unlock()
	if err := state(daemonv1.RunState_RUN_STATE_DONE, ""); err != nil {
		return err
	}
	return stream.Send(&daemonv1.RunResponse{Event: &daemonv1.RunResponse_Result{Result: res}})
}

// evilOutput is hostile guest output, in pieces: SSE framing, markup that
// would end a <script>, terminal escapes, bytes that are not UTF-8, and a
// character split between two chunks.
var evilOutput = [][]byte{
	[]byte("a\n\ndata: {\"type\":\"result\"}\n\nevent: x\r\n"),
	[]byte("</script><script>alert(1)</script><!-- \x1b[31mred\x1b[0m \x1b]0;title\x07"),
	[]byte("bad \xff\xfe bytes, split \xc3"),
	[]byte("\xa9 joined\n"),
}

// requests returns the requests the fake got.
func (f *fakeDaemon) requests() []*daemonv1.RunRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]*daemonv1.RunRequest(nil), f.reqs...)
}

// testEnv is a UI server on a fake daemon.
type testEnv struct {
	t      *testing.T
	fake   *fakeDaemon
	srv    *Server
	base   string // http://127.0.0.1:PORT
	client *http.Client
	logs   *syncBuf
	cancel context.CancelFunc
	done   chan error
}

// syncBuf collects log lines from many goroutines.
type syncBuf struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// newEnv starts a fake daemon and a live UI server on it, recording into
// record if it is not "". The client has a cookie jar and has opened the
// start URL already.
func newEnv(t *testing.T, record string) *testEnv {
	t.Helper()
	dir, err := os.MkdirTemp("", "ui-") // short: socket paths
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	l, err := audit.Open(filepath.Join(dir, audit.DirName), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	fake := &fakeDaemon{audit: l, stateDir: dir, gate: make(chan struct{})}
	sock := filepath.Join(dir, "d.sock")
	lis, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	gs := grpc.NewServer()
	daemonv1.RegisterSandboxServiceServer(gs, fake)
	go func() { _ = gs.Serve(lis) }()
	t.Cleanup(gs.Stop)
	cc, err := grpc.NewClient("unix://"+sock, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cc.Close() })

	httpLis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	e := &testEnv{t: t, fake: fake, logs: &syncBuf{}, done: make(chan error, 1)}
	e.srv, err = New(t.Context(), Options{
		Listener: httpLis, Daemon: daemonv1.NewSandboxServiceClient(cc), StateDir: dir, Record: record,
		Version: "sandboxctl test", Logger: log.New(e.logs, "", 0),
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	e.cancel = cancel
	go func() { e.done <- e.srv.Run(ctx) }()
	t.Cleanup(e.stop)
	e.base = "http://" + httpLis.Addr().String()
	jar, _ := cookiejar.New(nil)
	e.client = &http.Client{Jar: jar, Timeout: 10 * time.Second}
	resp, err := e.client.Get(e.srv.URL())
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.Request.URL.RawQuery != "" {
		t.Fatalf("the start URL ended at %v with %d, want / with 200", resp.Request.URL, resp.StatusCode)
	}
	return e
}

// stop shuts the server down (once) and returns Run's error.
func (e *testEnv) stop() {
	if e.cancel == nil {
		return
	}
	e.cancel()
	e.cancel = nil
	select {
	case err := <-e.done:
		if err != nil {
			e.t.Errorf("Run: %v", err)
		}
	case <-time.After(20 * time.Second):
		e.t.Error("the server did not stop")
	}
}

// post sends a JSON body the way the page does.
func (e *testEnv) post(path, body string) (*reply, string) {
	e.t.Helper()
	req, err := http.NewRequest(http.MethodPost, e.base+path, strings.NewReader(body))
	if err != nil {
		e.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", e.base)
	resp, err := e.client.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close() //nolint:errcheck // test
	data, _ := io.ReadAll(resp.Body)
	return &reply{StatusCode: resp.StatusCode, Header: resp.Header, Body: data}, string(data)
}

// stream is one open event stream.
type stream struct {
	resp  *http.Response
	lines chan string // every raw line of the stream
}

// events opens the event stream.
func (e *testEnv) events() *stream {
	e.t.Helper()
	req, err := http.NewRequestWithContext(e.t.Context(), http.MethodGet, e.base+"/api/events", nil)
	if err != nil {
		e.t.Fatal(err)
	}
	c := &http.Client{Jar: e.client.Jar} // no timeout: a stream
	resp, err := c.Do(req)               //nolint:bodyclose // closed by the test's Cleanup below
	if err != nil {
		e.t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		e.t.Fatalf("event stream: %d", resp.StatusCode)
	}
	s := &stream{resp: resp, lines: make(chan string, 4096)}
	go func() {
		defer close(s.lines)
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 0, 1<<20), 1<<20)
		for sc.Scan() {
			s.lines <- sc.Text()
		}
	}()
	e.t.Cleanup(func() { _ = resp.Body.Close() })
	return s
}

// until reads events until done says stop, and returns them all. Every
// line must be a "data:" line, a comment, or the blank line between
// events: nothing else may ever appear.
func (s *stream) until(t *testing.T, done func(*Event) bool) []*Event {
	t.Helper()
	var got []*Event
	deadline := time.After(10 * time.Second)
	for {
		select {
		case line, ok := <-s.lines:
			if !ok {
				t.Fatalf("the stream ended after %d events", len(got))
			}
			switch {
			case line == "", strings.HasPrefix(line, ": "):
				continue
			case !strings.HasPrefix(line, "data: "):
				t.Fatalf("a stream line that is not an event: %q", line)
			}
			var ev Event
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ev); err != nil {
				t.Fatalf("an event that is not one JSON object: %q: %v", line, err)
			}
			got = append(got, &ev)
			if done(&ev) {
				return got
			}
		case <-deadline:
			t.Fatalf("timed out after %d events", len(got))
		}
	}
}

// endOf is a done func for until: the end of scenario seg.
func endOf(seg int) func(*Event) bool {
	return func(ev *Event) bool { return ev.Type == TypeEnd && ev.End.Seg == seg }
}
