package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	daemonv1 "github.com/redwan-cpp/MicroVM/internal/gen/sandbox/daemon/v1"
)

const (
	// maxTimeout is the longest --timeout (the agent's ceiling).
	maxTimeout = 10 * time.Minute
	// maxStdin is the most --stdin may send (the daemon's ceiling).
	maxStdin = 1<<20 - 64<<10
)

// runOptions are the parsed flags of `sandboxctl run`.
type runOptions struct {
	class     string
	timeout   time.Duration
	maxOutput uint64
	env       map[string]string
	stdin     bool
	verbose   bool
	argv      []string
}

// envFlag collects repeated --env NAME=VALUE flags.
type envFlag map[string]string

func (f envFlag) String() string { return fmt.Sprint(map[string]string(f)) }

func (f envFlag) Set(s string) error {
	name, value, ok := strings.Cut(s, "=")
	if !ok || name == "" {
		return fmt.Errorf("%q is not NAME=VALUE", s)
	}
	f[name] = value
	return nil
}

// sizeFlag is a byte count: a number with an optional unit, B, KiB, MiB
// or GiB (also K, M, G: always powers of 1024).
type sizeFlag struct{ n *uint64 }

func (f sizeFlag) String() string {
	if f.n == nil {
		return ""
	}
	return strconv.FormatUint(*f.n, 10)
}

func (f sizeFlag) Set(s string) error {
	n, err := parseSize(s)
	if err == nil {
		*f.n = n
	}
	return err
}

// parseSize reads a byte count such as "1MiB", "64KiB" or "4096".
func parseSize(s string) (uint64, error) {
	units := []struct {
		suffix string
		shift  uint
	}{{"GiB", 30}, {"MiB", 20}, {"KiB", 10}, {"G", 30}, {"M", 20}, {"K", 10}, {"B", 0}}
	num, shift := strings.TrimSpace(s), uint(0)
	for _, u := range units {
		if rest, ok := strings.CutSuffix(num, u.suffix); ok {
			num, shift = strings.TrimSpace(rest), u.shift
			break
		}
	}
	n, err := strconv.ParseUint(num, 10, 64)
	if err != nil || n > (1<<63)>>shift {
		return 0, fmt.Errorf("%q is not a size (use for example 4096, 64KiB or 1MiB)", s)
	}
	return n << shift, nil
}

// parseRunFlags reads `sandboxctl run`'s flags and command.
func (c *client) parseRunFlags(args []string) (*runOptions, error) {
	o := &runOptions{env: map[string]string{}}
	flags := flag.NewFlagSet("sandboxctl run", flag.ContinueOnError)
	flags.SetOutput(c.env.stderr)
	flags.Usage = func() {
		c.env.errln("usage: sandboxctl run [flags] -- CMD ARGS...")
		flags.PrintDefaults()
	}
	c.socketFlag(flags)
	flags.StringVar(&o.class, "class", "small", "the size class: small (1 vCPU, 256 MiB) or medium (2 vCPUs, 512 MiB)")
	flags.DurationVar(&o.timeout, "timeout", 30*time.Second, "the command's time limit (at most 10m)")
	o.maxOutput = 1 << 20
	flags.Var(sizeFlag{&o.maxOutput}, "max-output", "the cap on stdout and stderr together, for example 64KiB (at most 16MiB; default 1MiB)")
	flags.Var(envFlag(o.env), "env", "an extra environment variable, NAME=VALUE (repeat it for more)")
	flags.BoolVar(&o.stdin, "stdin", false, "send this program's standard input to the command (at most 960 KiB)")
	flags.BoolVar(&o.verbose, "v", false, "show every state change, the outcome and the timings on stderr")
	if err := flags.Parse(args); err != nil {
		return nil, err
	}
	o.argv = flags.Args()
	switch {
	case len(o.argv) == 0:
		return nil, errors.New("no command given: sandboxctl run [flags] -- CMD ARGS")
	case o.timeout <= 0 || o.timeout > maxTimeout:
		return nil, fmt.Errorf("--timeout %v: it must be more than 0 and at most %v", o.timeout, maxTimeout)
	case o.timeout%time.Millisecond != 0:
		return nil, fmt.Errorf("--timeout %v: use whole milliseconds", o.timeout)
	case o.maxOutput == 0:
		return nil, errors.New("--max-output must be more than 0")
	}
	return o, nil
}

// request turns the options into a RunRequest, reading stdin if asked.
func (o *runOptions) request(stdin io.Reader) (*daemonv1.RunRequest, error) {
	req := &daemonv1.RunRequest{
		Argv:           o.argv,
		Env:            o.env,
		SizeClass:      o.class,
		TimeoutMs:      uint32(o.timeout.Milliseconds()), //nolint:gosec // G115: at most 10 minutes, checked above
		MaxOutputBytes: o.maxOutput,
	}
	if o.stdin {
		data, err := io.ReadAll(io.LimitReader(stdin, maxStdin+1))
		if err != nil {
			return nil, fmt.Errorf("read stdin: %w", err)
		}
		if len(data) > maxStdin {
			return nil, fmt.Errorf("stdin is more than %d bytes (960 KiB), the most a run may get", maxStdin)
		}
		req.Stdin = data
	}
	return req, nil
}

// run is `sandboxctl run`: it streams the command's output and exits with
// its exit code (see the package comment for the others).
func (c *client) run(args []string) int {
	o, err := c.parseRunFlags(args)
	if errors.Is(err, flag.ErrHelp) {
		return 0
	}
	if err != nil {
		c.env.errln("sandboxctl:", err)
		return exitUsage
	}
	req, err := o.request(c.env.stdin)
	if err != nil {
		c.env.errln("sandboxctl:", err)
		return exitUsage
	}
	api, closeConn, err := c.connect()
	if err != nil {
		c.env.errln(err)
		return exitFailed
	}
	defer closeConn()

	// Ctrl-C ends the call: the daemon then stops the run and destroys
	// its clone.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	stream, err := api.Run(ctx, req)
	if err != nil {
		return c.fail(exitFailed, err)
	}
	var start time.Time
	for {
		msg, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			c.env.errln("sandboxctl: the daemon ended the run without a result")
			return exitFailed
		}
		if err != nil {
			return c.fail(exitFailed, err)
		}
		switch ev := msg.GetEvent().(type) {
		case *daemonv1.RunResponse_Stdout:
			_, _ = c.env.stdout.Write(ev.Stdout)
		case *daemonv1.RunResponse_Stderr:
			_, _ = c.env.stderr.Write(ev.Stderr)
		case *daemonv1.RunResponse_State:
			if start.IsZero() {
				start = ev.State.GetAt().AsTime()
			}
			if o.verbose {
				c.printState(ev.State, start)
			}
		case *daemonv1.RunResponse_Result:
			if o.verbose || ev.Result.GetOutcome() == daemonv1.Outcome_OUTCOME_FAILED {
				c.printResult(ev.Result)
			}
			return exitCode(ev.Result)
		}
	}
}

// exitCode is what `sandboxctl run` exits with for a result.
func exitCode(r *daemonv1.RunResult) int {
	switch r.GetOutcome() {
	case daemonv1.Outcome_OUTCOME_EXITED:
		if sig := r.GetSignal(); r.GetExitCode() < 0 && sig > 0 {
			return 128 + int(sig) // as a shell reports a command killed by a signal
		}
		return int(r.GetExitCode())
	case daemonv1.Outcome_OUTCOME_TIMED_OUT:
		return exitTimedOut
	case daemonv1.Outcome_OUTCOME_OUTPUT_TRUNCATED:
		return exitTruncated
	}
	return exitFailed
}

// printState shows one state change on stderr (-v).
func (c *client) printState(s *daemonv1.StateChange, start time.Time) {
	line := fmt.Sprintf("sandboxctl: %-9s +%s", stateName(s.GetState()), ms(s.GetAt().AsTime().Sub(start)))
	if d := s.GetDetail(); d != "" {
		line += "  " + firstLine(d)
	}
	c.env.errln(line)
}

// printResult shows the outcome and the timings on stderr.
func (c *client) printResult(r *daemonv1.RunResult) {
	c.env.errf("sandboxctl: run %s: %s (%s mode)\n", r.GetRunId(), outcomeText(r), modeName(r.GetMode()))
	if e := r.GetError(); e != "" {
		c.env.errf("sandboxctl: error: %s\n", e)
	}
	t := r.GetTimings()
	us := func(v uint64) string { return ms(microseconds(v)) }
	c.env.errf("sandboxctl: timings: queued %s, restore %s (prepare %s, scratch %s, process start %s, "+
		"LoadSnapshot %s, agent ready %s, de-twin %s), mount %s, exec %s, destroy %s, total %s\n",
		us(t.GetQueuedUs()), us(t.GetRestoreUs()), us(t.GetPrepareUs()), us(t.GetScratchUs()), us(t.GetProcessStartUs()),
		us(t.GetLoadSnapshotUs()), us(t.GetAgentReadyUs()), us(t.GetDeTwinUs()), us(t.GetMountUs()), us(t.GetExecUs()),
		us(t.GetDestroyUs()), us(t.GetTotalUs()))
}

// outcomeText describes a result in a few words, for example "EXITED 0".
func outcomeText(r *daemonv1.RunResult) string {
	if r == nil {
		return "-"
	}
	name := strings.TrimPrefix(r.GetOutcome().String(), "OUTCOME_")
	if r.GetOutcome() == daemonv1.Outcome_OUTCOME_EXITED {
		if r.GetExitCode() < 0 {
			return fmt.Sprintf("%s by signal %d", name, r.GetSignal())
		}
		return fmt.Sprintf("%s %d", name, r.GetExitCode())
	}
	return name
}

func stateName(s daemonv1.RunState) string { return strings.TrimPrefix(s.String(), "RUN_STATE_") }

func modeName(m daemonv1.Mode) string {
	return strings.ToLower(strings.TrimPrefix(m.String(), "MODE_"))
}

// microseconds turns a count of microseconds from the daemon into a
// Duration, capped instead of overflowing.
func microseconds(us uint64) time.Duration {
	const maxUs = uint64(math.MaxInt64 / int64(time.Microsecond))
	return time.Duration(min(us, maxUs)) * time.Microsecond //nolint:gosec // G115: capped to fit in an int64 above
}

// ms formats d in milliseconds with one decimal.
func ms(d time.Duration) string { return fmt.Sprintf("%.1fms", float64(d)/float64(time.Millisecond)) }

func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return line
}
