// Package agentclient is the host side of the agent API: it talks to
// sandbox-agent inside a microVM over gRPC on vsock.
//
// The most important rule in this package: the agent is not trusted. Code
// in the guest runs as root, so it can kill the agent and put its own
// program on the agent's port. Whatever answers there may send too much
// output, never finish, send malformed messages or lie about how a command
// ended. So the client:
//
//   - requires a deadline for Exec and puts one on every other call,
//   - counts the output itself and stops reading at the cap,
//   - refuses messages over 64 KiB and headers over 16 KiB,
//   - checks the order of events (exactly one exit event, last),
//   - decides "timed out" and "output truncated" from its own clock and
//     its own byte count. What the agent says about those is kept in the
//     AgentReported fields, for logs only.
//
// See docs/threat-model.md (T7, trust boundary 3).
package agentclient

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync/atomic"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/redwan-cpp/MicroVM/internal/agent"
	agentv1 "github.com/redwan-cpp/MicroVM/internal/gen/sandbox/agent/v1"
	"github.com/redwan-cpp/MicroVM/internal/vsockconn"
)

const (
	// CallTimeout is the longest Health, SyncTime and Reseed may take
	// (less if the caller's context has an earlier deadline).
	CallTimeout = 2 * time.Second

	// DeadlineGrace is how much longer than the command's own timeout the
	// host waits for Exec. The agent kills the command when the timeout
	// runs out and normally reports the exit within this grace; the host's
	// deadline is only the backstop for an agent that doesn't.
	DeadlineGrace = 1 * time.Second

	// maxRecvMsgSize is the largest message the host accepts from the
	// agent. An output chunk is at most 32 KiB (agent.MaxChunk).
	maxRecvMsgSize = 64 << 10
	// maxHeaderListSize caps the HTTP/2 headers (and trailers) of a reply.
	maxHeaderListSize = 16 << 10
)

// Client is a connection to one agent. It is safe for concurrent use.
type Client struct {
	conn *grpc.ClientConn
	api  agentv1.AgentServiceClient
	// unused holds the connection Dial made, until gRPC takes it.
	unused *atomic.Pointer[net.Conn]
}

// Dial connects to the agent listening on port inside the VM whose vsock
// Unix socket is vsockUDS (see vmm.Process.VsockPath).
//
// Dial does the vsock handshake itself, before gRPC is involved, so its
// errors are the ones from vsockconn.Dial: errors.Is(err,
// vsockconn.ErrNotReady) means the guest is up but the agent is not
// listening yet, and the caller may retry (the e2e test does, while the
// VM boots). On success, the gRPC connection is built on top of that
// already-open connection.
//
// There is no TLS: vsock connects this VM to this host only, through
// Firecracker, so there is no network in between to protect against
// (docs/learn/08-guest-vsock-agent.md explains this).
func Dial(ctx context.Context, vsockUDS string, port uint32) (*Client, error) {
	first, err := vsockconn.Dial(ctx, vsockUDS, port)
	if err != nil {
		return nil, err
	}

	// gRPC opens connections by calling a "dialer" function. The first
	// call gets the connection we just made; if gRPC ever has to reconnect,
	// later calls do a fresh handshake.
	unused := new(atomic.Pointer[net.Conn])
	unused.Store(&first)
	dialer := func(ctx context.Context, _ string) (net.Conn, error) {
		if conn := unused.Swap(nil); conn != nil {
			return *conn, nil
		}
		return vsockconn.Dial(ctx, vsockUDS, port)
	}

	c, err := newClient(dialer)
	if err != nil {
		_ = first.Close()
		return nil, err
	}
	c.unused = unused
	return c, nil
}

// newClient builds a Client that opens its connections with dialer. Dial
// uses vsock; the tests use an in-memory connection. Every safety option
// lives here, so the tests check the same client that production uses.
func newClient(dialer func(context.Context, string) (net.Conn, error)) (*Client, error) {
	conn, err := grpc.NewClient(
		// "passthrough" hands the name to our dialer unchanged; the dialer
		// ignores it, since it already knows where the agent is.
		"passthrough:///vsock",
		grpc.WithContextDialer(dialer),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(maxRecvMsgSize)),
		grpc.WithMaxHeaderListSize(maxHeaderListSize),
		// A retried Exec could run a command twice. Never retry.
		grpc.WithDisableRetry(),
	)
	if err != nil {
		return nil, fmt.Errorf("agentclient: %w", err)
	}
	conn.Connect() // start the HTTP/2 handshake now, not at the first call
	return &Client{conn: conn, api: agentv1.NewAgentServiceClient(conn)}, nil
}

// Close closes the connection. Calls still running fail.
func (c *Client) Close() error {
	err := c.conn.Close()
	if c.unused != nil {
		if conn := c.unused.Swap(nil); conn != nil {
			_ = (*conn).Close() // gRPC never took it
		}
	}
	return err
}

// Health asks the agent for its version and uptime. Like everything from
// the agent, the answer is untrusted: it is fine for logs, not for
// decisions.
func (c *Client) Health(ctx context.Context) (*agentv1.HealthResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, CallTimeout)
	defer cancel()
	resp, err := c.api.Health(ctx, &agentv1.HealthRequest{})
	if err != nil {
		return nil, fmt.Errorf("agentclient: health: %w", err)
	}
	return resp, nil
}

// SyncTime sets the guest clock to t.
func (c *Client) SyncTime(ctx context.Context, t time.Time) error {
	ctx, cancel := context.WithTimeout(ctx, CallTimeout)
	defer cancel()
	if _, err := c.api.SyncTime(ctx, &agentv1.SyncTimeRequest{UnixNanos: t.UnixNano()}); err != nil {
		return fmt.Errorf("agentclient: sync time: %w", err)
	}
	return nil
}

// Reseed mixes entropy (32 to 512 bytes, for example from crypto/rand)
// into the guest's random number generator.
func (c *Client) Reseed(ctx context.Context, entropy []byte) error {
	if n := len(entropy); n < agent.MinEntropy || n > agent.MaxEntropy {
		return fmt.Errorf("agentclient: reseed: entropy is %d bytes; it must be %d to %d", n, agent.MinEntropy, agent.MaxEntropy)
	}
	ctx, cancel := context.WithTimeout(ctx, CallTimeout)
	defer cancel()
	if _, err := c.api.Reseed(ctx, &agentv1.ReseedRequest{Entropy: entropy}); err != nil {
		return fmt.Errorf("agentclient: reseed: %w", err)
	}
	return nil
}

// ErrNoDeadline is returned by Exec when its context has no deadline.
var ErrNoDeadline = errors.New("agentclient: Exec needs a context with a deadline")

// ProtocolError means the agent broke the Exec protocol: the stream ended
// without an exit event, had a second one, had data after it, or had an
// empty event. A real agent never does this, so it points to a bug or to
// a hostile guest.
type ProtocolError struct {
	Reason string
}

func (e *ProtocolError) Error() string {
	return "agentclient: agent broke the exec protocol: " + e.Reason
}
