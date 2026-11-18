package agentclient

import (
	"context"
	"errors"
	"net"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"

	"github.com/redwan-cpp/MicroVM/internal/agent"
	agentv1 "github.com/redwan-cpp/MicroVM/internal/gen/sandbox/agent/v1"
)

// notReadyListener is Firecracker's vsock socket while the guest boots:
// the first `refuse` connections are closed at once (nothing listens in
// the guest yet), later ones get the handshake.
type notReadyListener struct {
	net.Listener
	refuse atomic.Int32
}

func (l *notReadyListener) Accept() (net.Conn, error) {
	for {
		conn, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		if l.refuse.Add(-1) >= 0 {
			b := make([]byte, 64)
			_, _ = conn.Read(b)
			_ = conn.Close()
			continue
		}
		return conn, answerConnect(conn)
	}
}

func TestWaitReadyRetriesWhileTheVMStarts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v.sock")
	gs := grpc.NewServer()
	agentv1.RegisterAgentServiceServer(gs, agent.NewServer(agent.Config{Version: "late agent"}))
	t.Cleanup(gs.Stop)

	// For the first 50ms the socket does not exist at all; then the first
	// three connections find nobody listening in the guest.
	go func() {
		time.Sleep(50 * time.Millisecond)
		lis, err := net.Listen("unix", path)
		if err != nil {
			t.Error(err)
			return
		}
		nr := &notReadyListener{Listener: lis}
		nr.refuse.Store(3)
		_ = gs.Serve(nr)
	}()

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	c, health, err := WaitReady(ctx, path, nil)
	if err != nil {
		t.Fatalf("WaitReady: %v", err)
	}
	defer c.Close() //nolint:errcheck // test
	if health.GetVersion() != "late agent" {
		t.Errorf("Health version = %q, want %q", health.GetVersion(), "late agent")
	}
}

func TestWaitReadyGivesUpAtTheDeadline(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	_, _, err := WaitReady(ctx, filepath.Join(t.TempDir(), "never.sock"), nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want DeadlineExceeded", err)
	}
}

func TestWaitReadyStopsWhenTheVMIsGone(t *testing.T) {
	done := make(chan struct{})
	close(done)
	_, _, err := WaitReady(t.Context(), filepath.Join(t.TempDir(), "never.sock"), done)
	if !errors.Is(err, ErrVMGone) {
		t.Errorf("err = %v, want ErrVMGone", err)
	}
}
