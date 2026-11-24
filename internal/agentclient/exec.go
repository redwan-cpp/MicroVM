package agentclient

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"time"

	"github.com/redwan-cpp/MicroVM/internal/agent"
	agentv1 "github.com/redwan-cpp/MicroVM/internal/gen/sandbox/agent/v1"
)

// Result is how a command ended. The first group of fields is decided by
// the host; the AgentReported fields are only what the agent claims.
type Result struct {
	// Exited is true when the agent reported an exit status and then ended
	// the stream properly. Only then do ExitCode and Signal mean anything.
	// It is false when the host stopped the run itself (deadline, output
	// cap), so a run the host cut short can never look like "exit 0".
	Exited bool
	// ExitCode is the exit status (0 to 255), or -1 if a signal killed the
	// command. Only meaningful when Exited.
	ExitCode int
	// Signal is the signal that killed the command, or 0. Only meaningful
	// when Exited.
	Signal int

	// TimedOut is true if, by the host's own clock, the command ran out of
	// time: either the host's deadline ended the call, or the exit came
	// after the command's timeout had passed.
	TimedOut bool
	// OutputTruncated is true if the output reached the host's cap.
	// Everything past the cap was dropped unread. Output that exactly fills
	// the cap counts too: only the agent could tell whether more followed,
	// and the agent is not trusted.
	OutputTruncated bool
	// OutputBytes is how many bytes of stdout and stderr together were
	// passed on to the caller's writers.
	OutputBytes uint64
	// Duration is the time from starting the call to its end, measured by
	// the host.
	Duration time.Duration

	// AgentReportedTimedOut, AgentReportedTruncated and
	// AgentReportedDuration are the agent's own claims from its exit event.
	// They are for logs only: never base a decision on them.
	AgentReportedTimedOut  bool
	AgentReportedTruncated bool
	AgentReportedDuration  time.Duration
}

// Exec runs a command in the guest and copies its stdout and stderr to the
// given writers (nil means discard) while it runs.
//
// ctx must have a deadline (ErrNoDeadline otherwise). The host waits at
// most until that deadline, or until the command's timeout plus
// DeadlineGrace, whichever is sooner. Hitting it is not an error: Exec
// returns a Result with TimedOut set and Exited false.
//
// Stdout and stderr together are capped at req.MaxOutputBytes (default
// 1 MiB, the same rules as the agent). If the agent sends more, Exec stops
// reading, cancels the call (the agent then kills the command) and returns
// a Result with OutputTruncated set and Exited false.
//
// Errors: ErrNoDeadline; an invalid request; a *ProtocolError; a write
// error from stdout or stderr; the context's error if the caller cancelled
// ctx; or the gRPC error the call failed with (for example the agent
// rejected the request, or sent a message over 64 KiB).
func (c *Client) Exec(ctx context.Context, req *agentv1.ExecRequest, stdout, stderr io.Writer) (Result, error) {
	if _, ok := ctx.Deadline(); !ok {
		return Result{}, ErrNoDeadline
	}
	// The same defaults and ceilings as the agent, so both sides agree on
	// what the request means.
	timeout, limit, err := agent.ExecLimits(req.GetTimeoutMs(), req.GetMaxOutputBytes())
	if err != nil {
		return Result{}, fmt.Errorf("agentclient: exec: %w", err)
	}

	start := time.Now()
	// WithDeadline keeps ctx's deadline if it is sooner than ours.
	ctx, cancel := context.WithDeadline(ctx, start.Add(timeout+DeadlineGrace))
	defer cancel()
	deadline, _ := ctx.Deadline() // the host's deadline for this call
	// A second context, so that stopping the stream at the output cap is
	// not mistaken for the deadline or for the caller's cancel.
	streamCtx, stopStream := context.WithCancel(ctx)
	defer stopStream()

	stream, err := c.api.Exec(streamCtx, req)
	if err != nil {
		return Result{}, fmt.Errorf("agentclient: exec: %w", err)
	}

	out := &capped{limit: limit, stdout: orDiscard(stdout), stderr: orDiscard(stderr)}
	var exit *agentv1.ExitStatus
	result := func() Result {
		return Result{
			OutputTruncated: out.truncated,
			OutputBytes:     out.written,
			Duration:        time.Since(start),
		}
	}

	for {
		msg, err := stream.Recv()
		if err != nil {
			res := result()
			// Was the host's deadline hit? The host's own clock decides, not
			// the status code in err: the agent could send "deadline
			// exceeded" whenever it likes. We look at the clock as well as
			// at ctx, because ctx's timer may fire a moment after the
			// deadline, while the agent (which got the same deadline) may
			// already have ended the stream.
			hitDeadline := errors.Is(ctx.Err(), context.DeadlineExceeded) || !time.Now().Before(deadline)
			switch {
			case errors.Is(err, io.EOF) && exit != nil:
				// The normal end: one exit event, then the end of the stream.
				res.Exited = true
				res.ExitCode = int(exit.GetExitCode())
				res.Signal = int(exit.GetSignal())
				res.TimedOut = res.Duration >= timeout
				res.AgentReportedTimedOut = exit.GetTimedOut()
				res.AgentReportedTruncated = exit.GetOutputTruncated()
				res.AgentReportedDuration = microseconds(exit.GetDurationUs())
				return res, nil
			case hitDeadline:
				res.TimedOut = true
				return res, nil
			case ctx.Err() != nil:
				return res, fmt.Errorf("agentclient: exec: %w", ctx.Err()) // the caller cancelled
			case errors.Is(err, io.EOF):
				return res, &ProtocolError{Reason: "the stream ended without an exit event"}
			default:
				return res, fmt.Errorf("agentclient: exec: %w", err)
			}
		}

		if exit != nil {
			stopStream()
			if msg.GetExit() != nil {
				return result(), &ProtocolError{Reason: "a second exit event"}
			}
			return result(), &ProtocolError{Reason: "an event after the exit event"}
		}

		switch ev := msg.GetEvent().(type) {
		case *agentv1.ExecResponse_Stdout:
			err = out.write(out.stdout, ev.Stdout)
		case *agentv1.ExecResponse_Stderr:
			err = out.write(out.stderr, ev.Stderr)
		case *agentv1.ExecResponse_Exit:
			exit = ev.Exit
			if exit == nil {
				exit = &agentv1.ExitStatus{}
			}
			if err := checkExit(exit); err != nil {
				stopStream()
				return result(), err
			}
			continue // wait for the end of the stream
		default:
			stopStream()
			return result(), &ProtocolError{Reason: "an empty event"}
		}

		if errors.Is(err, errOverCap) {
			// More output than the cap: stop reading. Cancelling the call
			// tells a real agent to kill the command.
			stopStream()
			return result(), nil
		}
		if err != nil {
			stopStream()
			return result(), err
		}
	}
}

// The ranges a real exit status can have on Linux. A number outside them
// can only come from a broken or hostile agent; refusing it keeps records
// of runs (the audit log, from week 4) clean.
const (
	minExitCode = -1  // killed by a signal
	maxExitCode = 255 // exit statuses are one byte
	maxSignal   = 64  // SIGRTMAX on Linux
)

// checkExit refuses an exit status no real process can have.
func checkExit(exit *agentv1.ExitStatus) error {
	if code := exit.GetExitCode(); code < minExitCode || code > maxExitCode {
		return &ProtocolError{Reason: fmt.Sprintf("exit code %d is outside %d..%d", code, minExitCode, maxExitCode)}
	}
	if sig := exit.GetSignal(); sig < 0 || sig > maxSignal {
		return &ProtocolError{Reason: fmt.Sprintf("signal %d is outside 0..%d", sig, maxSignal)}
	}
	return nil
}

// errOverCap is returned by capped.write when output went past the cap.
var errOverCap = errors.New("output is over the cap")

// capped passes output on to the caller's writers and counts it against
// the cap (stdout and stderr together).
type capped struct {
	limit          uint64
	stdout, stderr io.Writer
	written        uint64
	truncated      bool
}

// write passes p on to w, but never more than the cap allows. It returns
// errOverCap if some of p did not fit.
func (c *capped) write(w io.Writer, p []byte) error {
	room := c.limit - c.written
	over := uint64(len(p)) > room
	if over {
		p = p[:room]
	}
	if len(p) > 0 {
		if _, err := w.Write(p); err != nil {
			return fmt.Errorf("agentclient: exec: write output: %w", err)
		}
		c.written += uint64(len(p))
	}
	if c.written >= c.limit {
		c.truncated = true
	}
	if over {
		return errOverCap
	}
	return nil
}

// microseconds turns the agent's duration_us into a Duration. The value
// is untrusted, so an absurdly large one is capped instead of overflowing.
func microseconds(us uint64) time.Duration {
	const maxUs = uint64(math.MaxInt64 / int64(time.Microsecond))
	return time.Duration(min(us, maxUs)) * time.Microsecond //nolint:gosec // G115: capped to fit in an int64 above
}

func orDiscard(w io.Writer) io.Writer {
	if w == nil {
		return io.Discard
	}
	return w
}
