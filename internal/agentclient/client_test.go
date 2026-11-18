package agentclient

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	"github.com/redwan-cpp/MicroVM/internal/agent"
	agentv1 "github.com/redwan-cpp/MicroVM/internal/gen/sandbox/agent/v1"
	"github.com/redwan-cpp/MicroVM/internal/vsockconn"
)

// Most of these tests put a hostile fake agent behind a real gRPC server
// and check that the real client (newClient, with all its limits) stays
// in control. The fakes do what a guest that replaced the agent could do.

// fakeAgent lets each test decide how Exec and Health behave.
type fakeAgent struct {
	agentv1.UnimplementedAgentServiceServer
	exec   func(*agentv1.ExecRequest, agentv1.AgentService_ExecServer) error
	health func(context.Context) (*agentv1.HealthResponse, error)
}

func (f *fakeAgent) Exec(req *agentv1.ExecRequest, stream agentv1.AgentService_ExecServer) error {
	return f.exec(req, stream)
}

func (f *fakeAgent) Health(ctx context.Context, _ *agentv1.HealthRequest) (*agentv1.HealthResponse, error) {
	return f.health(ctx)
}

// serve runs srv behind a gRPC server on an in-memory connection and
// returns a client for it built by newClient.
func serve(t *testing.T, srv agentv1.AgentServiceServer) *Client {
	t.Helper()
	lis := bufconn.Listen(1 << 20)
	gs := grpc.NewServer()
	agentv1.RegisterAgentServiceServer(gs, srv)
	go func() { _ = gs.Serve(lis) }()
	t.Cleanup(gs.Stop)

	c, err := newClient(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) })
	if err != nil {
		t.Fatalf("newClient: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func serveExec(t *testing.T, exec func(*agentv1.ExecRequest, agentv1.AgentService_ExecServer) error) *Client {
	t.Helper()
	return serve(t, &fakeAgent{exec: exec})
}

// execWithin runs Exec with a deadline of d.
func execWithin(t *testing.T, c *Client, d time.Duration, req *agentv1.ExecRequest, stdout io.Writer) (Result, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), d)
	defer cancel()
	return c.Exec(ctx, req, stdout, nil)
}

func stdoutEvent(p []byte) *agentv1.ExecResponse {
	return &agentv1.ExecResponse{Event: &agentv1.ExecResponse_Stdout{Stdout: p}}
}

func exitEvent(e *agentv1.ExitStatus) *agentv1.ExecResponse {
	return &agentv1.ExecResponse{Event: &agentv1.ExecResponse_Exit{Exit: e}}
}

func TestExecNeedsADeadline(t *testing.T) {
	c := serveExec(t, func(*agentv1.ExecRequest, agentv1.AgentService_ExecServer) error { return nil })
	if _, err := c.Exec(t.Context(), &agentv1.ExecRequest{Argv: []string{"true"}}, nil, nil); !errors.Is(err, ErrNoDeadline) {
		t.Errorf("err = %v, want ErrNoDeadline", err)
	}
}

func TestExecRejectsLimitsAboveTheCeiling(t *testing.T) {
	var called atomic.Bool
	c := serveExec(t, func(*agentv1.ExecRequest, agentv1.AgentService_ExecServer) error { called.Store(true); return nil })
	_, err := execWithin(t, c, time.Second, &agentv1.ExecRequest{Argv: []string{"true"}, MaxOutputBytes: agent.MaxOutputLimit + 1}, nil)
	if err == nil || called.Load() {
		t.Errorf("err = %v, agent called = %v; want an error before calling the agent", err, called.Load())
	}
}

func TestExecFlooderIsCutOffAtTheCap(t *testing.T) {
	// The fake never stops sending. The host must stop at the cap, long
	// before its deadline.
	c := serveExec(t, func(_ *agentv1.ExecRequest, stream agentv1.AgentService_ExecServer) error {
		chunk := bytes.Repeat([]byte("A"), agent.MaxChunk)
		for {
			if err := stream.Send(stdoutEvent(chunk)); err != nil {
				return err
			}
		}
	})

	var got bytes.Buffer
	res, err := execWithin(t, c, 10*time.Second, &agentv1.ExecRequest{Argv: []string{"yes"}, MaxOutputBytes: 100_000}, &got)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if !res.OutputTruncated || res.Exited || res.TimedOut {
		t.Errorf("result = %+v, want OutputTruncated, not Exited, not TimedOut", res)
	}
	if got.Len() != 100_000 || res.OutputBytes != 100_000 {
		t.Errorf("passed on %d bytes (OutputBytes %d), want exactly the cap, 100000", got.Len(), res.OutputBytes)
	}
	if res.Duration > 5*time.Second {
		t.Errorf("took %v; the cap, not the deadline, should end the call", res.Duration)
	}
}

func TestExecOversizedMessageIsRefused(t *testing.T) {
	c := serveExec(t, func(_ *agentv1.ExecRequest, stream agentv1.AgentService_ExecServer) error {
		return stream.Send(stdoutEvent(make([]byte, 100<<10))) // 100 KiB, over the 64 KiB limit
	})

	var got bytes.Buffer
	res, err := execWithin(t, c, 5*time.Second, &agentv1.ExecRequest{Argv: []string{"x"}}, &got)
	if status.Code(err) != codes.ResourceExhausted {
		t.Errorf("err = %v, want ResourceExhausted", err)
	}
	if res.Exited || got.Len() != 0 {
		t.Errorf("result = %+v with %d bytes passed on, want nothing", res, got.Len())
	}
}

func TestExecProtocolViolations(t *testing.T) {
	tests := map[string][]*agentv1.ExecResponse{
		"no exit, then the end": {stdoutEvent([]byte("hi"))},
		"two exits":             {exitEvent(&agentv1.ExitStatus{}), exitEvent(&agentv1.ExitStatus{})},
		"data after exit":       {exitEvent(&agentv1.ExitStatus{}), stdoutEvent([]byte("late"))},
		"empty event":           {{}},
	}
	for name, events := range tests {
		t.Run(name, func(t *testing.T) {
			c := serveExec(t, func(_ *agentv1.ExecRequest, stream agentv1.AgentService_ExecServer) error {
				for _, ev := range events {
					if err := stream.Send(ev); err != nil {
						return err
					}
				}
				return nil
			})
			res, err := execWithin(t, c, 5*time.Second, &agentv1.ExecRequest{Argv: []string{"x"}}, nil)
			var perr *ProtocolError
			if !errors.As(err, &perr) {
				t.Errorf("err = %v, want a *ProtocolError", err)
			}
			if res.Exited {
				t.Errorf("result = %+v, want Exited false", res)
			}
		})
	}
}

func TestExecHangingAgentHitsTheHostDeadline(t *testing.T) {
	c := serveExec(t, func(_ *agentv1.ExecRequest, stream agentv1.AgentService_ExecServer) error {
		<-stream.Context().Done() // never answer
		return nil
	})

	res, err := execWithin(t, c, 300*time.Millisecond, &agentv1.ExecRequest{Argv: []string{"sleep", "10"}}, nil)
	if err != nil {
		t.Fatalf("Exec: %v (hitting the deadline is a result, not an error)", err)
	}
	if !res.TimedOut || res.Exited {
		t.Errorf("result = %+v, want TimedOut, not Exited", res)
	}
	if res.Duration < 300*time.Millisecond || res.Duration > 3*time.Second {
		t.Errorf("took %v, want about 300ms", res.Duration)
	}
}

func TestExecHangAfterExitIsATimeout(t *testing.T) {
	// An exit event alone is not enough: the stream must end too.
	c := serveExec(t, func(_ *agentv1.ExecRequest, stream agentv1.AgentService_ExecServer) error {
		if err := stream.Send(exitEvent(&agentv1.ExitStatus{})); err != nil {
			return err
		}
		<-stream.Context().Done()
		return nil
	})

	res, err := execWithin(t, c, 300*time.Millisecond, &agentv1.ExecRequest{Argv: []string{"x"}}, nil)
	if err != nil || !res.TimedOut || res.Exited {
		t.Errorf("result = %+v, err = %v; want TimedOut, not Exited, no error", res, err)
	}
}

func TestExecLiesAreIgnored(t *testing.T) {
	// The fake floods past the cap, then claims a clean exit with nothing
	// truncated. The host's own count wins.
	c := serveExec(t, func(_ *agentv1.ExecRequest, stream agentv1.AgentService_ExecServer) error {
		chunk := bytes.Repeat([]byte("B"), agent.MaxChunk)
		for range 8 {
			if err := stream.Send(stdoutEvent(chunk)); err != nil {
				return err
			}
		}
		return stream.Send(exitEvent(&agentv1.ExitStatus{ExitCode: 0, OutputTruncated: false}))
	})

	res, err := execWithin(t, c, 5*time.Second, &agentv1.ExecRequest{Argv: []string{"x"}, MaxOutputBytes: 64 << 10}, nil)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if !res.OutputTruncated || res.Exited || res.OutputBytes != 64<<10 {
		t.Errorf("result = %+v, want OutputTruncated with 65536 bytes, not Exited", res)
	}
}

func TestExecOutputExactlyAtTheCapCountsAsTruncated(t *testing.T) {
	// The agent sends exactly the cap and says nothing was dropped. The
	// host cannot know whether more followed, so it reports truncation,
	// but the stream was otherwise fine, so the exit status is kept.
	c := serveExec(t, func(_ *agentv1.ExecRequest, stream agentv1.AgentService_ExecServer) error {
		if err := stream.Send(stdoutEvent(make([]byte, 4096))); err != nil {
			return err
		}
		return stream.Send(exitEvent(&agentv1.ExitStatus{ExitCode: 7}))
	})

	res, err := execWithin(t, c, 5*time.Second, &agentv1.ExecRequest{Argv: []string{"x"}, MaxOutputBytes: 4096}, nil)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if !res.OutputTruncated || !res.Exited || res.ExitCode != 7 || res.AgentReportedTruncated {
		t.Errorf("result = %+v, want OutputTruncated, Exited with code 7, AgentReportedTruncated false", res)
	}
}

func TestExecWriterErrorStopsTheCall(t *testing.T) {
	c := serveExec(t, func(_ *agentv1.ExecRequest, stream agentv1.AgentService_ExecServer) error {
		for {
			if err := stream.Send(stdoutEvent([]byte("x"))); err != nil {
				return err
			}
		}
	})

	_, err := execWithin(t, c, 5*time.Second, &agentv1.ExecRequest{Argv: []string{"x"}}, failingWriter{})
	if err == nil || !strings.Contains(err.Error(), "disk full") {
		t.Errorf("err = %v, want the writer's error", err)
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("disk full") }

func TestExecWithTheRealAgent(t *testing.T) {
	c := serve(t, agent.NewServer(agent.Config{}))

	var out bytes.Buffer
	res, err := execWithin(t, c, 10*time.Second, &agentv1.ExecRequest{Argv: []string{"echo", "hi"}}, &out)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if out.String() != "hi\n" || !res.Exited || res.ExitCode != 0 || res.TimedOut || res.OutputTruncated {
		t.Errorf("stdout = %q, result = %+v; want \"hi\\n\" and a clean exit 0", out.String(), res)
	}
}

func TestExecAgentTimeoutLandsBeforeTheHostDeadline(t *testing.T) {
	// The command's own 200ms timeout: the real agent kills it and reports
	// the exit, well inside the host's 200ms + DeadlineGrace.
	c := serve(t, agent.NewServer(agent.Config{}))

	res, err := execWithin(t, c, 10*time.Second, &agentv1.ExecRequest{Argv: []string{"sleep", "10"}, TimeoutMs: 200}, nil)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if !res.Exited || res.Signal != 9 || res.ExitCode != -1 || !res.TimedOut || !res.AgentReportedTimedOut {
		t.Errorf("result = %+v, want Exited by SIGKILL, TimedOut (host clock) and AgentReportedTimedOut", res)
	}
}

func TestHealthHasItsOwnDeadline(t *testing.T) {
	c := serve(t, &fakeAgent{health: func(ctx context.Context) (*agentv1.HealthResponse, error) {
		<-ctx.Done() // never answer
		return nil, ctx.Err()
	}})

	start := time.Now()
	_, err := c.Health(t.Context()) // no deadline from the caller
	if status.Code(err) != codes.DeadlineExceeded {
		t.Errorf("err = %v, want DeadlineExceeded", err)
	}
	if took := time.Since(start); took < CallTimeout || took > CallTimeout+2*time.Second {
		t.Errorf("Health took %v, want about %v", took, CallTimeout)
	}
}

func TestReseedChecksLengthOnTheHost(t *testing.T) {
	c := serve(t, agent.NewServer(agent.Config{}))
	for _, n := range []int{agent.MinEntropy - 1, agent.MaxEntropy + 1} {
		if err := c.Reseed(t.Context(), make([]byte, n)); err == nil {
			t.Errorf("Reseed(%d bytes) succeeded, want an error", n)
		}
	}
}

// handshakeListener plays Firecracker's part of the vsock handshake for
// every connection it accepts, then hands the connection to gRPC.
type handshakeListener struct{ net.Listener }

func (l handshakeListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	b := make([]byte, 1)
	for b[0] != '\n' { // read "CONNECT 1024\n" byte by byte
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

func TestDialOverAVsockSocket(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v.sock")
	lis, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	gs := grpc.NewServer()
	agentv1.RegisterAgentServiceServer(gs, agent.NewServer(agent.Config{Version: "via vsock"}))
	go func() { _ = gs.Serve(handshakeListener{lis}) }()
	t.Cleanup(gs.Stop)

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	c, err := Dial(ctx, path, agent.Port)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close() //nolint:errcheck // test

	resp, err := c.Health(ctx)
	if err != nil || resp.GetVersion() != "via vsock" {
		t.Errorf("Health = %v, %v; want version %q", resp, err, "via vsock")
	}
}

func TestDialPassesNotReadyThrough(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v.sock")
	lis, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lis.Close() })
	go func() { // like Firecracker when nothing listens in the guest: close at once
		for {
			conn, err := lis.Accept()
			if err != nil {
				return
			}
			b := make([]byte, 64)
			_, _ = conn.Read(b)
			_ = conn.Close()
		}
	}()

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if _, err := Dial(ctx, path, agent.Port); !errors.Is(err, vsockconn.ErrNotReady) {
		t.Errorf("err = %v, want vsockconn.ErrNotReady", err)
	}
}
