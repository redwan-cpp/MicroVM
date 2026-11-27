package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"os/signal"
	"syscall"

	"github.com/redwan-cpp/MicroVM/internal/state"
	"github.com/redwan-cpp/MicroVM/internal/ui"
	"github.com/redwan-cpp/MicroVM/internal/version"
)

// uiUsage is `sandboxctl ui -h`.
const uiUsage = `usage:
  sandboxctl ui [--listen 127.0.0.1:8090] [--record FILE] [--state-dir DIR]   the live page
  sandboxctl ui --replay FILE [--listen 127.0.0.1:8090]                      play a recording (no daemon needed)
  sandboxctl ui export --from FILE [--out microvm-recording-<date>.html]     one self-contained HTML file

The page is served on the loopback interface only, with a token in the printed
address. Ctrl-C stops it (and a running scenario: sandboxd stops its runs).
`

// uiCmd is `sandboxctl ui`.
func (c *client) uiCmd(args []string) int {
	if len(args) > 0 && args[0] == "export" {
		return uiExport(args[1:], c.env)
	}
	flags := flag.NewFlagSet("sandboxctl ui", flag.ContinueOnError)
	flags.SetOutput(c.env.stderr)
	flags.Usage = func() {
		c.env.errs(uiUsage)
		flags.PrintDefaults()
	}
	c.socketFlag(flags)
	listen := flags.String("listen", "127.0.0.1:8090", "where to serve the page: a loopback address and port (port 0 picks a free one)")
	record := flags.String("record", "", "record the session into this new file (it is never overwritten)")
	replay := flags.String("replay", "", "play back this recording instead of a live daemon")
	dir := state.DefaultDir
	if d := c.env.getenv(state.EnvVar); d != "" {
		dir = d
	}
	stateDir := flags.String("state-dir", dir, "sandboxd's state folder, for its audit log (default: $"+state.EnvVar+", else "+state.DefaultDir+")")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return exitUsage
	}
	if flags.NArg() > 0 || (*replay != "" && *record != "") {
		flags.Usage()
		return exitUsage
	}

	lis, err := listenLoopback(*listen)
	if err != nil {
		c.env.errln("sandboxctl:", err)
		return exitUsage
	}
	defer lis.Close() //nolint:errcheck // Run closes it; this is for the early returns

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	opts := ui.Options{
		Listener: lis,
		Version:  version.String("sandboxctl"),
		Logger:   log.New(c.env.stderr, "sandboxctl ui: ", log.LstdFlags),
	}
	if *replay != "" {
		if opts.Replay, err = ui.LoadSession(*replay); err != nil {
			c.env.errln("sandboxctl:", err)
			return 1
		}
	} else {
		api, closeConn, err := c.connect()
		if err != nil {
			c.env.errln(err)
			return 1
		}
		defer closeConn()
		opts.Daemon, opts.StateDir, opts.Record = api, *stateDir, *record
	}
	srv, err := ui.New(ctx, opts)
	if err != nil {
		return c.fail(1, err)
	}
	what := "the live page"
	switch {
	case *replay != "":
		what = "the recording " + *replay
	case *record != "":
		what += ", recording into " + *record
	}
	c.env.outf("sandboxctl ui: serving %s on this machine only. Open:\n\n    %s\n\n"+
		"(the address carries the page's token; Ctrl-C stops)\n", what, srv.URL())
	if err := srv.Run(ctx); err != nil {
		c.env.errln("sandboxctl:", err)
		return 1
	}
	return 0
}

// listenLoopback listens on addr, which must be a loopback address:
// the page is a control surface for sandboxd and is never served to the
// network (ADR-0007).
func listenLoopback(addr string) (net.Listener, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("--listen %q: %w (for example 127.0.0.1:8090)", addr, err)
	}
	if host == "localhost" || host == "" {
		host = "127.0.0.1"
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return nil, fmt.Errorf("--listen %q: the page is served on a loopback address only (127.0.0.1 or ::1), never to the network", addr)
	}
	lis, err := net.Listen("tcp", net.JoinHostPort(ip.String(), port))
	if err != nil {
		return nil, fmt.Errorf("listen on %s: %w", addr, err)
	}
	return lis, nil
}

// uiExport is `sandboxctl ui export`.
func uiExport(args []string, e env) int {
	flags := flag.NewFlagSet("sandboxctl ui export", flag.ContinueOnError)
	flags.SetOutput(e.stderr)
	from := flags.String("from", "", "the recording (sandboxctl ui --record FILE)")
	out := flags.String("out", "", "the HTML file to write (default microvm-recording-<date>.html); never overwritten")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return exitUsage
	}
	if *from == "" || flags.NArg() > 0 {
		e.errs(uiUsage)
		return exitUsage
	}
	s, err := ui.LoadSession(*from)
	if err != nil {
		e.errln("sandboxctl:", err)
		return 1
	}
	path := *out
	if path == "" {
		path = ui.DefaultExportName(s)
	}
	if err := ui.Export(s, path); err != nil {
		e.errln("sandboxctl:", err)
		return 1
	}
	e.outf("sandboxctl ui: wrote %s: one self-contained page (a recording, %d events), opens offline from file:// in any browser\n",
		path, len(s.Events))
	return 0
}
