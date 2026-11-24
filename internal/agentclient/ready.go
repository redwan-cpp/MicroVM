package agentclient

import (
	"context"
	"errors"
	"fmt"
	"syscall"
	"time"

	"github.com/redwan-cpp/MicroVM/internal/agent"
	agentv1 "github.com/redwan-cpp/MicroVM/internal/gen/sandbox/agent/v1"
	"github.com/redwan-cpp/MicroVM/internal/vsockconn"
)

// retryInterval is how long WaitReady pauses between two tries.
const retryInterval = 10 * time.Millisecond

// ErrVMGone is returned by WaitReady when the VM stopped before the agent
// answered.
var ErrVMGone = errors.New("agentclient: the VM stopped before the agent answered")

// WaitReady connects to the agent of a VM that is starting (a cold boot or
// a snapshot restore) and waits until it answers Health. It returns the
// connected client and that answer.
//
// While the VM starts, two errors are normal and mean "try again in 10
// ms": the vsock socket vsockUDS does not exist yet or does not accept
// connections yet (ENOENT, ECONNREFUSED), or the guest is up but nothing
// listens on the agent's port yet (vsockconn.ErrNotReady). Any other
// error ends the wait. So does ctx (give it a deadline), and so does
// vmDone being closed, which means the VM's Firecracker exited (nil means
// "never").
func WaitReady(ctx context.Context, vsockUDS string, vmDone <-chan struct{}) (*Client, *agentv1.HealthResponse, error) {
	for {
		client, err := Dial(ctx, vsockUDS, agent.Port)
		if err == nil {
			health, err := client.Health(ctx)
			if err != nil {
				_ = client.Close()
				return nil, nil, fmt.Errorf("agentclient: connected, but Health failed: %w", err)
			}
			return client, health, nil
		}
		retry := errors.Is(err, vsockconn.ErrNotReady) ||
			errors.Is(err, syscall.ENOENT) || errors.Is(err, syscall.ECONNREFUSED)
		if !retry {
			return nil, nil, err
		}

		select {
		case <-vmDone:
			return nil, nil, ErrVMGone
		case <-ctx.Done():
			return nil, nil, fmt.Errorf("agentclient: the agent did not answer (last error: %w): %w", err, ctx.Err())
		case <-time.After(retryInterval):
		}
	}
}
