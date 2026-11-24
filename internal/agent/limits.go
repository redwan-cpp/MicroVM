package agent

import (
	"fmt"
	"time"
)

// The limits of one Exec. The agent enforces them inside the guest, and the
// host (internal/agentclient) enforces the same numbers again on its side,
// because it cannot trust the agent to do so. Both call ExecLimits, so a
// request means the same thing on both sides.
const (
	// DefaultTimeout applies when ExecRequest.timeout_ms is 0.
	DefaultTimeout = 30 * time.Second
	// MaxTimeout is the longest timeout a request may ask for.
	MaxTimeout = 10 * time.Minute

	// DefaultOutputLimit applies when ExecRequest.max_output_bytes is 0.
	// It covers stdout and stderr together.
	DefaultOutputLimit uint64 = 1 << 20 // 1 MiB
	// MaxOutputLimit is the largest output cap a request may ask for.
	MaxOutputLimit uint64 = 16 << 20 // 16 MiB

	// MaxChunk is the largest piece of stdout or stderr in one
	// ExecResponse. The host refuses messages above 64 KiB, so a chunk
	// plus its few bytes of framing always fits.
	MaxChunk = 32 << 10 // 32 KiB

	// MaxRequestSize is the largest gRPC message the agent accepts.
	// It bounds argv, env and stdin together.
	MaxRequestSize = 1 << 20 // 1 MiB
	// MaxStdin is the most stdin one request may carry: 64 KiB less than
	// the message cap, so a request with the largest stdin still has room
	// for its argv and env. sandboxd checks the same ceiling, so a run
	// that the daemon accepts never fails at the agent for its size.
	MaxStdin = MaxRequestSize - 64<<10 // 960 KiB
	// MaxEnvEntries is the most environment variables one request may add.
	MaxEnvEntries = 64

	// MinEntropy and MaxEntropy bound ReseedRequest.entropy, in bytes.
	MinEntropy = 32
	MaxEntropy = 512
)

// ExecLimits turns the timeout_ms and max_output_bytes fields of an
// ExecRequest into the limits that apply: 0 picks the default, and a value
// above the ceiling is an error (never silently lowered, so the caller
// always knows which limit is in force).
func ExecLimits(timeoutMS uint32, maxOutputBytes uint64) (time.Duration, uint64, error) {
	timeout := DefaultTimeout
	if timeoutMS != 0 {
		timeout = time.Duration(timeoutMS) * time.Millisecond
	}
	if timeout > MaxTimeout {
		return 0, 0, fmt.Errorf("timeout_ms %d is above the maximum of %d (%s)",
			timeoutMS, MaxTimeout.Milliseconds(), MaxTimeout)
	}

	maxOutput := DefaultOutputLimit
	if maxOutputBytes != 0 {
		maxOutput = maxOutputBytes
	}
	if maxOutput > MaxOutputLimit {
		return 0, 0, fmt.Errorf("max_output_bytes %d is above the maximum of %d (16 MiB)",
			maxOutputBytes, MaxOutputLimit)
	}
	return timeout, maxOutput, nil
}
