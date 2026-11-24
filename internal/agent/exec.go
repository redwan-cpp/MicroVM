package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	agentv1 "github.com/redwan-cpp/MicroVM/internal/gen/sandbox/agent/v1"
)

// FixedPath is the PATH every command gets, and the only place a bare
// program name (argv[0] without a "/") is looked up.
const FixedPath = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

// waitDelay is how long Exec waits, after the command has exited, for its
// output pipes to close. A process that escaped the process group (with
// setsid, for example) survives the group kill and may keep the pipes open
// forever. After waitDelay we stop reading and report the exit anyway.
const waitDelay = 500 * time.Millisecond

// envName is the shape of an allowed environment variable name.
var envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// errTimeout is the "cause" of the context that ends when a command's
// timeout runs out. It tells our own timeout apart from the host going
// away (context.Canceled) or the host's deadline (context.DeadlineExceeded).
var errTimeout = errors.New("command timed out")

// command is a validated ExecRequest, ready to run.
type command struct {
	path      string   // the program file to run (argv[0] after lookup)
	argv      []string // the arguments, including argv[0] as the caller wrote it
	env       []string // the complete environment, as NAME=value
	dir       string   // the working directory
	stdin     []byte
	timeout   time.Duration
	maxOutput uint64
}

// Exec runs one command and streams its output. See ExecRequest in
// agent.proto for the rules; the steps are:
//
//  1. Validate the request (codes.InvalidArgument if it breaks a rule).
//  2. Start the command in its own process group, so that it and every
//     process it starts can be killed together.
//  3. Stream stdout and stderr in chunks of at most 32 KiB. Past the
//     output cap, keep reading but throw the bytes away, so the command is
//     never blocked on a full pipe.
//  4. On timeout, or when the host cancels the call, kill the group.
//  5. When the command's main process exits, kill the group too: nothing
//     it left running in the background may outlive it.
//  6. Send exactly one exit event, always last.
func (s *Server) Exec(req *agentv1.ExecRequest, stream agentv1.AgentService_ExecServer) error {
	c, err := parseExecRequest(req, s.workingDir())
	if err != nil {
		return err
	}

	// ctx ends when the timeout runs out (cause errTimeout) or when the
	// stream's context ends: the host cancelled the call, its deadline
	// passed, or the connection broke.
	ctx, cancel := context.WithTimeoutCause(stream.Context(), c.timeout, errTimeout)
	defer cancel()

	// If sending to the host fails, nobody is listening any more: stop the
	// command by cancelling ctx.
	out := &output{stream: stream, limit: c.maxOutput, onSendError: cancel}

	// Running the command we were asked to run is the agent's whole job.
	// The input is untrusted on purpose: the VM, not this code, is the
	// security boundary.
	cmd := exec.CommandContext(ctx, c.path) //nolint:gosec // G204: running the requested command is the agent's job; the VM is the boundary
	cmd.Args = c.argv                       // keep argv[0] as the caller wrote it
	cmd.Env = c.env
	cmd.Dir = c.dir
	if len(c.stdin) > 0 {
		cmd.Stdin = bytes.NewReader(c.stdin) // closed after the last byte
	} // else: nil means /dev/null
	cmd.Stdout = outputWriter{out: out, stderr: false}
	cmd.Stderr = outputWriter{out: out, stderr: true}
	// Setpgid puts the command in a new process group whose ID is the
	// command's own PID. Its children join that group unless they leave it.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// When ctx ends, exec calls Cancel. The default would kill only the main
	// process; we kill the whole group.
	cmd.Cancel = func() error { return killGroup(cmd.Process.Pid) }
	cmd.WaitDelay = waitDelay

	start := time.Now()
	if err := cmd.Start(); err != nil {
		if ctxErr := stream.Context().Err(); ctxErr != nil {
			return status.FromContextError(ctxErr).Err()
		}
		code := codes.FailedPrecondition
		if errors.Is(err, fs.ErrNotExist) {
			code = codes.NotFound
		}
		return status.Errorf(code, "start %q: %v", c.argv[0], err)
	}

	// Wait until the main process has exited, then kill its group at once.
	// Order matters. waitForExit does not "reap" the process: it stays a
	// zombie, so its PID (which is also the group ID) cannot be given to a
	// new, unrelated process yet. Only then do we send the group SIGKILL,
	// and only after that does cmd.Wait reap the process.
	//
	// The group kill is best effort: a process that left the group (setsid)
	// survives it, and waitDelay stops it from holding up Exec. The real
	// guarantee is that the host destroys the whole VM after the run.
	//
	// waitid on our own child should never fail. If it ever did, we would
	// skip the early kill rather than kill a command that may still run.
	if err := waitForExit(cmd.Process.Pid); err == nil {
		_ = killGroup(cmd.Process.Pid) // usually "no such process": nothing was left
	}
	waitErr := cmd.Wait()
	duration := time.Since(start)

	if out.failed() != nil {
		return out.failed()
	}
	if ctxErr := stream.Context().Err(); ctxErr != nil {
		return status.FromContextError(ctxErr).Err() // the host is gone
	}
	if cmd.ProcessState == nil {
		return status.Errorf(codes.Internal, "wait for %q: %v", c.argv[0], waitErr)
	}

	exit := exitStatus(cmd.ProcessState)
	exit.TimedOut = errors.Is(context.Cause(ctx), errTimeout)
	exit.DurationUs = uint64(max(duration.Microseconds(), 0))
	return out.finish(exit)
}

// parseExecRequest checks a request against the rules in agent.proto and
// turns it into a command; defaultDir is the working folder if the
// request names none. Every error is a gRPC status error.
func parseExecRequest(req *agentv1.ExecRequest, defaultDir string) (*command, error) {
	c, err := checkExecRequest(req, defaultDir)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	if c.path, err = lookPath(c.argv[0], filepath.SplitList(FixedPath)); err != nil {
		return nil, status.Errorf(codes.NotFound, "%v", err)
	}
	return c, nil
}

// CheckExecRequest checks req against every rule in agent.proto that does
// not depend on the guest (it cannot look for the program): argv, env,
// working_dir, stdin and the limits. sandboxd checks each run with it
// before it restores a clone, so the daemon and the agent always agree on
// which requests are valid.
func CheckExecRequest(req *agentv1.ExecRequest) error {
	_, err := checkExecRequest(req, "/")
	return err
}

// checkExecRequest is CheckExecRequest, returning the command without its
// program path.
func checkExecRequest(req *agentv1.ExecRequest, defaultDir string) (*command, error) {
	argv := req.GetArgv()
	if len(argv) == 0 || argv[0] == "" {
		return nil, errors.New("argv must not be empty")
	}
	for i, arg := range argv {
		if strings.ContainsRune(arg, 0) {
			return nil, fmt.Errorf("argv[%d] contains a NUL byte", i)
		}
	}

	timeout, maxOutput, err := ExecLimits(req.GetTimeoutMs(), req.GetMaxOutputBytes())
	if err != nil {
		return nil, err
	}

	env, err := buildEnv(req.GetEnv())
	if err != nil {
		return nil, err
	}

	dir := req.GetWorkingDir()
	if dir == "" {
		dir = defaultDir
	}
	if !filepath.IsAbs(dir) || strings.ContainsRune(dir, 0) {
		return nil, fmt.Errorf("working_dir %q must be an absolute path", dir)
	}

	if len(req.GetStdin()) > MaxStdin {
		return nil, fmt.Errorf("stdin is %d bytes; at most %d allowed", len(req.GetStdin()), MaxStdin)
	}

	return &command{
		argv:      argv,
		env:       env,
		dir:       dir,
		stdin:     req.GetStdin(),
		timeout:   timeout,
		maxOutput: maxOutput,
	}, nil
}

// buildEnv returns the command's whole environment: the fixed base
// (PATH, HOME, LANG) plus the request's extra variables, sorted by name.
// Nothing comes from the agent's own environment.
func buildEnv(extra map[string]string) ([]string, error) {
	if len(extra) > MaxEnvEntries {
		return nil, fmt.Errorf("env has %d entries; at most %d allowed", len(extra), MaxEnvEntries)
	}
	env := map[string]string{
		"PATH": FixedPath,
		"HOME": "/root",
		"LANG": "C.UTF-8",
	}
	for name, value := range extra {
		switch {
		case !envName.MatchString(name):
			return nil, fmt.Errorf("env name %q is not allowed: use letters, digits and _, not starting with a digit", name)
		case name == "PATH":
			return nil, errors.New("env may not change PATH")
		case strings.ContainsRune(value, 0):
			return nil, fmt.Errorf("env %s contains a NUL byte", name)
		}
		env[name] = value
	}

	list := make([]string, 0, len(env))
	for _, name := range slices.Sorted(maps.Keys(env)) {
		list = append(list, name+"="+env[name])
	}
	return list, nil
}

// lookPath finds the program file for argv[0], the way a shell does: a
// name that contains a "/" is used as it is; any other name is looked up
// in each folder of dirs in turn, and the first regular file with an
// execute bit wins.
//
// We cannot use exec.LookPath here: it searches the PATH of the agent's
// own process, not the fixed PATH the command runs with.
func lookPath(name string, dirs []string) (string, error) {
	if strings.Contains(name, "/") {
		return name, nil
	}
	for _, dir := range dirs {
		if dir == "" {
			continue // an empty PATH entry means "the current folder": never
		}
		path := filepath.Join(dir, name)
		if info, err := os.Stat(path); err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0o111 != 0 {
			return path, nil
		}
	}
	return "", fmt.Errorf("%q: no such program in %s", name, strings.Join(dirs, ":"))
}

// waitForExit blocks until process pid has exited, but leaves it for
// cmd.Wait to reap (WNOWAIT). Until then the process is a zombie and its
// PID stays reserved.
func waitForExit(pid int) error {
	for {
		var info unix.Siginfo
		err := unix.Waitid(unix.P_PID, pid, &info, unix.WEXITED|unix.WNOWAIT, nil)
		if errors.Is(err, unix.EINTR) {
			continue // interrupted by a signal: just wait again
		}
		return err
	}
}

// killGroup sends SIGKILL to every process in process group pgid. A
// negative PID in kill(2) means "the whole group". SIGKILL cannot be
// caught or ignored.
func killGroup(pgid int) error {
	err := unix.Kill(-pgid, unix.SIGKILL)
	if errors.Is(err, unix.ESRCH) {
		return os.ErrProcessDone // nobody left in the group
	}
	return err
}

// exitStatus describes how a finished process ended.
func exitStatus(state *os.ProcessState) *agentv1.ExitStatus {
	// ExitCode is 0..255, or -1 if a signal killed the process.
	exit := &agentv1.ExitStatus{ExitCode: int32(state.ExitCode())} //nolint:gosec // G115: always -1..255
	if ws, ok := state.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		exit.Signal = int32(ws.Signal()) //nolint:gosec // G115: signal numbers are 1..64
	}
	return exit
}

// output sends a command's stdout and stderr to the host and enforces the
// output cap. Two goroutines inside os/exec (one per pipe) write to it at
// the same time, so every field is guarded by mu.
type output struct {
	stream      agentv1.AgentService_ExecServer
	limit       uint64 // the output cap for stdout and stderr together
	onSendError func() // called once if sending to the host fails

	mu        sync.Mutex
	sent      uint64 // output bytes sent so far
	truncated bool   // some output was thrown away
	finished  bool   // the exit event is sent: nothing may follow it
	sendErr   error  // the first failed Send
}

// outputWriter is the io.Writer for one of the two pipes.
type outputWriter struct {
	out    *output
	stderr bool
}

// Write always reports that it took all of p, even when it threw some or
// all of it away. If it returned an error, os/exec would stop reading the
// pipe, and the command would block forever on its next write.
func (w outputWriter) Write(p []byte) (int, error) {
	w.out.write(w.stderr, p)
	return len(p), nil
}

func (o *output) write(stderr bool, p []byte) {
	o.mu.Lock()
	defer o.mu.Unlock()

	if o.finished || o.sendErr != nil {
		return // keep draining, send nothing
	}
	if room := o.limit - o.sent; uint64(len(p)) > room {
		p = p[:room] // the rest is over the cap: drop it
		o.truncated = true
	}
	for len(p) > 0 {
		n := min(len(p), MaxChunk)
		// os/exec reuses p for its next read as soon as Write returns, and
		// gRPC does not promise to be done with a message when Send
		// returns. So each chunk gets its own copy.
		if err := o.stream.Send(chunk(stderr, bytes.Clone(p[:n]))); err != nil {
			o.sendErr = err
			o.onSendError()
			return
		}
		o.sent += uint64(n)
		p = p[n:]
	}
}

// failed returns the error of the first failed Send, if any.
func (o *output) failed() error {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.sendErr
}

// finish sends the exit event. After it, every write is thrown away, so
// the exit event is always the last one.
func (o *output) finish(exit *agentv1.ExitStatus) error {
	o.mu.Lock()
	defer o.mu.Unlock()

	o.finished = true
	if o.sendErr != nil {
		return o.sendErr
	}
	exit.OutputTruncated = o.truncated
	return o.stream.Send(&agentv1.ExecResponse{Event: &agentv1.ExecResponse_Exit{Exit: exit}})
}

// chunk wraps one piece of output in an ExecResponse.
func chunk(stderr bool, p []byte) *agentv1.ExecResponse {
	if stderr {
		return &agentv1.ExecResponse{Event: &agentv1.ExecResponse_Stderr{Stderr: p}}
	}
	return &agentv1.ExecResponse{Event: &agentv1.ExecResponse_Stdout{Stdout: p}}
}
