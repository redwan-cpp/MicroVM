// Package vsockconn opens host-side connections to a port inside a
// Firecracker microVM.
//
// vsock is a socket family for talking between a VM and its host without a
// network card. Inside the guest, a program listens on a vsock port (our
// agent uses port 1024). On the host, Firecracker does not use the kernel's
// vsock support; it offers a plain Unix socket instead (the uds_path given
// to PUT /vsock). To reach a guest port, the host:
//
//  1. connects to that Unix socket,
//  2. writes "CONNECT <port>\n",
//  3. reads one line back: "OK <host-port>\n" on success.
//
// From then on the same connection carries the bytes to and from the guest
// program. Dial does those steps and returns the ready connection.
package vsockconn

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// maxReplyLen is the longest reply line we accept. A real one is about ten
// bytes ("OK 1073741824\n"); anything longer is not a valid reply.
const maxReplyLen = 64

// maxSocketPath is the longest Unix socket path Linux accepts: sun_path in
// struct sockaddr_un holds 108 bytes, including a closing NUL byte.
const maxSocketPath = 107

// ErrNotReady means Firecracker accepted the CONNECT but nobody in the
// guest is listening on that port yet, typically because the guest is
// still booting. Firecracker then just closes the connection. Callers may
// retry after a short pause.
var ErrNotReady = errors.New("vsockconn: nothing is listening on that port in the guest yet")

// Dial connects to port inside the guest through Firecracker's vsock Unix
// socket at udsPath. ctx bounds the whole handshake; once Dial returns, the
// connection has no deadline. Errors from connecting to the Unix socket
// (for example, it does not exist yet) are returned wrapped, as they are.
func Dial(ctx context.Context, udsPath string, port uint32) (net.Conn, error) {
	if len(udsPath) > maxSocketPath {
		return nil, fmt.Errorf("vsockconn: socket path is %d bytes, Linux allows at most %d: %s",
			len(udsPath), maxSocketPath, udsPath)
	}

	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", udsPath)
	if err != nil {
		return nil, fmt.Errorf("vsockconn: connect to %s: %w", udsPath, err)
	}

	if err := handshake(ctx, conn, port); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return conn, nil
}

// handshake sends CONNECT and checks the reply, all within ctx.
func handshake(ctx context.Context, conn net.Conn, port uint32) error {
	// Reads and writes on a net.Conn ignore contexts, but they honour a
	// deadline. So when ctx ends (its deadline passes or it is cancelled),
	// we move the connection's deadline into the past, which wakes up a
	// blocked read or write at once.
	stop := context.AfterFunc(ctx, func() { _ = conn.SetDeadline(time.Unix(1, 0)) })

	err := exchange(conn, port)

	// stop returns false if that function has already started: ctx ended
	// during the handshake and the connection is unusable. Report ctx's
	// error (context.DeadlineExceeded or context.Canceled), not the "i/o
	// timeout" it caused.
	if !stop() {
		return fmt.Errorf("vsockconn: CONNECT %d: %w", port, ctx.Err())
	}
	// If stop returned true, the function never ran and never will, so the
	// connection has no deadline: from here on it belongs to the caller.
	return err
}

// exchange writes the CONNECT line and reads and checks the reply.
func exchange(conn net.Conn, port uint32) error {
	if _, err := fmt.Fprintf(conn, "CONNECT %d\n", port); err != nil {
		if isClosedByPeer(err) {
			return fmt.Errorf("%w (port %d): %w", ErrNotReady, port, err)
		}
		return fmt.Errorf("vsockconn: send CONNECT %d: %w", port, err)
	}

	line, err := readLine(conn)
	if err != nil {
		if len(line) == 0 && (errors.Is(err, io.EOF) || isClosedByPeer(err)) {
			return fmt.Errorf("%w (port %d)", ErrNotReady, port)
		}
		return fmt.Errorf("vsockconn: read reply to CONNECT %d: %w", port, err)
	}

	// The reply is "OK <n>", where n is the host-side port number
	// Firecracker gave this connection. We only check that it is a number.
	rest, ok := strings.CutPrefix(line, "OK ")
	if !ok {
		return fmt.Errorf("vsockconn: CONNECT %d refused: unexpected reply %q", port, line)
	}
	if _, err := strconv.ParseUint(rest, 10, 32); err != nil {
		return fmt.Errorf("vsockconn: CONNECT %d: malformed reply %q", port, line)
	}
	return nil
}

// readLine reads one "\n"-terminated line, one byte at a time, and returns
// it without the "\n". Reading byte by byte is slow, but the line is short
// and it never takes a byte past the "\n": those bytes already belong to
// the guest program's conversation (for gRPC, the first bytes of HTTP/2),
// and a buffered reader would swallow them.
func readLine(r io.Reader) (string, error) {
	var line []byte
	b := make([]byte, 1)
	for len(line) < maxReplyLen {
		if _, err := io.ReadFull(r, b); err != nil {
			return string(line), err
		}
		if b[0] == '\n' {
			return string(line), nil
		}
		line = append(line, b[0])
	}
	return string(line), fmt.Errorf("reply is longer than %d bytes", maxReplyLen)
}

// isClosedByPeer reports whether err means the other side closed or reset
// the connection.
func isClosedByPeer(err error) bool {
	return errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.EPIPE)
}
