package vsockconn_test

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/redwan-cpp/MicroVM/internal/vsockconn"
)

// fakeFirecracker listens on a Unix socket like Firecracker's vsock device
// does. For each connection it reads the CONNECT line, passes it to the
// test's channel, then lets reply do whatever the test wants.
func fakeFirecracker(t *testing.T, reply func(conn net.Conn)) (path string, connectLines <-chan string) {
	t.Helper()
	path = filepath.Join(t.TempDir(), "v.sock")
	lis, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = lis.Close() })

	lines := make(chan string, 1)
	go func() {
		conn, err := lis.Accept()
		if err != nil {
			return
		}
		defer conn.Close() //nolint:errcheck // test server
		line, err := bufio.NewReader(conn).ReadString('\n')
		if err != nil {
			return
		}
		lines <- line
		reply(conn)
	}()
	return path, lines
}

func dial(t *testing.T, path string, timeout time.Duration) (net.Conn, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), timeout)
	defer cancel()
	return vsockconn.Dial(ctx, path, 1024)
}

func TestDialSuccessKeepsBytesAfterTheReply(t *testing.T) {
	// The guest program's first bytes may arrive in the same packet as the
	// OK line. They must reach the caller, not get swallowed by Dial.
	path, lines := fakeFirecracker(t, func(conn net.Conn) {
		_, _ = io.WriteString(conn, "OK 1073741824\nhello from the guest")
	})

	conn, err := dial(t, path, 200*time.Millisecond)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer conn.Close() //nolint:errcheck // test

	if got := <-lines; got != "CONNECT 1024\n" {
		t.Errorf("Firecracker received %q, want %q", got, "CONNECT 1024\n")
	}
	// Dial must clear the handshake deadline. Wait until it would have
	// passed: if it were still set, the read below would fail with an
	// i/o timeout.
	time.Sleep(300 * time.Millisecond)
	got, err := io.ReadAll(conn)
	if err != nil {
		t.Fatalf("read after Dial: %v", err)
	}
	if string(got) != "hello from the guest" {
		t.Errorf("payload = %q, want %q", got, "hello from the guest")
	}
}

func TestDialRejectsBadReplies(t *testing.T) {
	tests := map[string]string{
		"error reply":    "FAILURE no route\n",
		"not a number":   "OK abc\n",
		"over-long line": strings.Repeat("x", 100) + "\n",
	}
	for name, reply := range tests {
		t.Run(name, func(t *testing.T) {
			path, _ := fakeFirecracker(t, func(conn net.Conn) { _, _ = io.WriteString(conn, reply) })
			conn, err := dial(t, path, 2*time.Second)
			if err == nil {
				_ = conn.Close()
				t.Fatal("Dial succeeded, want an error")
			}
			if errors.Is(err, vsockconn.ErrNotReady) {
				t.Errorf("err = %v; a bad reply is not ErrNotReady", err)
			}
		})
	}
}

func TestDialImmediateCloseIsNotReady(t *testing.T) {
	// This is what Firecracker does when nothing listens on the guest port.
	path, _ := fakeFirecracker(t, func(net.Conn) {})

	_, err := dial(t, path, 2*time.Second)
	if !errors.Is(err, vsockconn.ErrNotReady) {
		t.Errorf("err = %v, want ErrNotReady", err)
	}
}

func TestDialSilentServerHitsTheDeadline(t *testing.T) {
	path, _ := fakeFirecracker(t, func(net.Conn) { <-t.Context().Done() })

	start := time.Now()
	_, err := dial(t, path, 200*time.Millisecond)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want context.DeadlineExceeded", err)
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Errorf("Dial took %v, want about 200ms", took)
	}
}

func TestDialCancelStopsTheHandshake(t *testing.T) {
	path, _ := fakeFirecracker(t, func(net.Conn) { <-t.Context().Done() })

	ctx, cancel := context.WithCancel(t.Context())
	time.AfterFunc(100*time.Millisecond, cancel)
	_, err := vsockconn.Dial(ctx, path, 1024)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}

func TestDialMissingSocket(t *testing.T) {
	_, err := dial(t, filepath.Join(t.TempDir(), "nothing-here.sock"), time.Second)
	if !errors.Is(err, syscall.ENOENT) {
		t.Errorf("err = %v, want ENOENT", err)
	}
	if errors.Is(err, vsockconn.ErrNotReady) {
		t.Errorf("err = %v; a missing socket is not ErrNotReady", err)
	}
}

func TestDialRejectsOverlongPath(t *testing.T) {
	_, err := dial(t, "/tmp/"+strings.Repeat("a", 120), time.Second)
	if err == nil || !strings.Contains(err.Error(), "socket path") {
		t.Errorf("err = %v, want a socket path length error", err)
	}
}
