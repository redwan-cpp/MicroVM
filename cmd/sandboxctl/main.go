// Command sandboxctl is the request form: the tool you type commands into.
// It never touches a VM itself. It only sends requests to sandboxd, over
// the daemon's Unix socket.
//
//	sandboxctl run [flags] -- CMD ARGS...   run CMD in a fresh microVM
//	sandboxctl ps [--all] [RUN_ID]          active (and recent) runs
//	sandboxctl logs RUN_ID                  the output kept of a run
//	sandboxctl audit verify                 check the audit log's hash chain
//	sandboxctl version
//
// `sandboxctl run` exits with the command's own exit code; with 124 if it
// timed out, 123 if its output hit the cap, and 125 if the run itself
// failed (no daemon, no clone, ...). A command may exit with those numbers
// itself: -v, or `sandboxctl ps RUN_ID`, shows the real outcome.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	daemonv1 "github.com/redwan-cpp/MicroVM/internal/gen/sandbox/daemon/v1"
	"github.com/redwan-cpp/MicroVM/internal/version"
)

// SocketEnv names the environment variable that picks the socket.
const SocketEnv = "SANDBOXD_SOCKET"

// jailerSocket is the jailer-mode daemon's socket (ADR-0006).
const jailerSocket = "/run/sandboxd/sandboxd.sock"

// The exit codes of `sandboxctl run` that are not the command's own.
const (
	exitTruncated = 123 // the output reached its cap
	exitTimedOut  = 124 // the command ran out of time (as timeout(1))
	exitFailed    = 125 // the run itself failed, or sandboxd could not be reached
	exitUsage     = 2   // the usual "you used the command wrong"
)

// env is how the CLI reads its environment; tests replace it.
type env struct {
	stdin          io.Reader
	stdout, stderr io.Writer
	getenv         func(string) string
}

// Small printers: the CLI has nothing useful to do when writing to its
// own stdout or stderr fails.
func (e env) outf(format string, args ...any) { _, _ = fmt.Fprintf(e.stdout, format, args...) }
func (e env) outln(args ...any)               { _, _ = fmt.Fprintln(e.stdout, args...) }
func (e env) outs(s string)                   { _, _ = io.WriteString(e.stdout, s) }
func (e env) errf(format string, args ...any) { _, _ = fmt.Fprintf(e.stderr, format, args...) }
func (e env) errln(args ...any)               { _, _ = fmt.Fprintln(e.stderr, args...) }
func (e env) errs(s string)                   { _, _ = io.WriteString(e.stderr, s) }

func main() {
	os.Exit(cli(os.Args[1:], env{stdin: os.Stdin, stdout: os.Stdout, stderr: os.Stderr, getenv: os.Getenv}))
}

// usage is the help text.
const usage = `usage: sandboxctl [--socket PATH] COMMAND ...

  run [flags] -- CMD ARGS...   run CMD in a fresh microVM (run -h for the flags)
  ps [--all] [RUN_ID]          active runs (--all: recent ones too), or one run in detail
  logs RUN_ID                  the output the daemon kept of a run
  audit verify [--state-dir D] check the audit log's hash chain (reads the file itself)
  version                      print the version

The socket: --socket, else $SANDBOXD_SOCKET, else $XDG_RUNTIME_DIR/sandboxd/sandboxd.sock
if it exists (rootless sandboxd), else /run/sandboxd/sandboxd.sock (jailer mode).
`

// cli runs one sandboxctl command and returns its exit code.
func cli(args []string, e env) int {
	global := flag.NewFlagSet("sandboxctl", flag.ContinueOnError)
	global.SetOutput(e.stderr)
	global.Usage = func() { e.errs(usage) }
	socket := global.String("socket", "", "the daemon's socket")
	if err := global.Parse(args); err != nil {
		return exitUsage
	}
	if global.NArg() == 0 {
		global.Usage()
		return exitUsage
	}
	cmd, rest := global.Arg(0), global.Args()[1:]
	c := &client{env: e, socket: *socket}
	switch cmd {
	case "run":
		return c.run(rest)
	case "ps":
		return c.ps(rest)
	case "logs":
		return c.logs(rest)
	case "audit":
		return auditCmd(rest, e)
	case "version":
		e.outln(version.String("sandboxctl"))
		return 0
	case "help", "-h", "--help":
		e.outs(usage)
		return 0
	}
	e.errf("sandboxctl: unknown command %q\n", cmd)
	global.Usage()
	return exitUsage
}

// client talks to sandboxd.
type client struct {
	env    env
	socket string // from --socket, before socketPath decides
}

// socketFlag adds --socket to a subcommand's flags too, so it may come
// before or after the subcommand's name.
func (c *client) socketFlag(flags *flag.FlagSet) {
	flags.StringVar(&c.socket, "socket", c.socket, "the daemon's socket")
}

// socketPath picks the socket: --socket, $SANDBOXD_SOCKET, the rootless
// default if it exists, else the jailer-mode one.
func socketPath(flagValue string, getenv func(string) string) string {
	if flagValue != "" {
		return flagValue
	}
	if s := getenv(SocketEnv); s != "" {
		return s
	}
	if dir := getenv("XDG_RUNTIME_DIR"); dir != "" {
		rootless := filepath.Join(dir, "sandboxd", "sandboxd.sock")
		if _, err := os.Stat(rootless); err == nil {
			return rootless
		}
	}
	return jailerSocket
}

// connect opens a connection to sandboxd, or explains why there is none.
func (c *client) connect() (daemonv1.SandboxServiceClient, func(), error) {
	path := socketPath(c.socket, c.env.getenv)
	if _, err := os.Stat(path); err != nil {
		return nil, nil, fmt.Errorf("sandboxd is not running: no socket at %s (%w)\n"+
			"start it (bin/sandboxd serve, or make demo1), or point at it with --socket or $%s", path, err, SocketEnv)
	}
	cc, err := grpc.NewClient("unix://"+path,
		grpc.WithTransportCredentials(insecure.NewCredentials()),    // a local socket: see ADR-0006
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(8<<20)), // Logs: up to 1 MiB of output
		grpc.WithDisableRetry(), // a retried Run could run a command twice
	)
	if err != nil {
		return nil, nil, fmt.Errorf("sandboxctl: %w", err)
	}
	c.socket = path
	return daemonv1.NewSandboxServiceClient(cc), func() { _ = cc.Close() }, nil
}

// fail prints err and returns code. A gRPC error is shown as its message,
// with a hint when the daemon could not be reached.
func (c *client) fail(code int, err error) int {
	if st, ok := status.FromError(err); ok {
		msg := st.Message()
		if st.Code() == codes.Unavailable {
			msg = fmt.Sprintf("sandboxd at %s is unavailable: %s", c.socket, msg)
		}
		c.env.errf("sandboxctl: %s (%s)\n", msg, st.Code())
		return code
	}
	c.env.errf("sandboxctl: %v\n", err)
	return code
}
