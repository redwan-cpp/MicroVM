package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/redwan-cpp/MicroVM/internal/audit"
	daemonv1 "github.com/redwan-cpp/MicroVM/internal/gen/sandbox/daemon/v1"
	"github.com/redwan-cpp/MicroVM/internal/state"
	"github.com/redwan-cpp/MicroVM/internal/vmm"
)

// callTimeout bounds ps and logs.
const callTimeout = 10 * time.Second

// ps is `sandboxctl ps [--all] [RUN_ID]`: a table of runs, or one run in
// detail.
func (c *client) ps(args []string) int {
	flags := flag.NewFlagSet("sandboxctl ps", flag.ContinueOnError)
	flags.SetOutput(c.env.stderr)
	c.socketFlag(flags)
	all := flags.Bool("all", false, "list the recent finished runs too")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return exitUsage
	}
	if flags.NArg() > 1 {
		c.env.errln("usage: sandboxctl ps [--all] [RUN_ID]")
		return exitUsage
	}
	api, closeConn, err := c.connect()
	if err != nil {
		c.env.errln(err)
		return 1
	}
	defer closeConn()
	ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
	defer cancel()

	if flags.NArg() == 1 {
		resp, err := api.Get(ctx, &daemonv1.GetRequest{RunId: flags.Arg(0)})
		if err != nil {
			return c.fail(1, err)
		}
		c.printRun(resp.GetRun())
		return 0
	}
	resp, err := api.List(ctx, &daemonv1.ListRequest{All: *all})
	if err != nil {
		return c.fail(1, err)
	}
	w := tabwriter.NewWriter(c.env.stdout, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "RUN ID\tSTATE\tCLASS\tMODE\tUID\tAGE\tOUTCOME\tCOMMAND")
	now := time.Now()
	for _, r := range resp.GetRuns() {
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%d\t%s\t%s\t%s\n", r.GetRunId(), stateName(r.GetState()), r.GetSizeClass(),
			modeName(r.GetMode()), r.GetCallerUid(), age(now.Sub(r.GetCreatedAt().AsTime())), outcomeText(r.GetResult()),
			command(r.GetArgv(), 40))
	}
	_ = w.Flush()
	if len(resp.GetRuns()) == 0 && !*all {
		c.env.errln("(no active runs; sandboxctl ps --all also lists the recent finished ones)")
	}
	return 0
}

// printRun shows one run: its command, every state change and the result.
func (c *client) printRun(r *daemonv1.RunInfo) {
	c.env.outf("run      %s\nstate    %s\nclass    %s\nmode     %s\ncaller   uid %d\ncommand  %s\n",
		r.GetRunId(), stateName(r.GetState()), r.GetSizeClass(), modeName(r.GetMode()), r.GetCallerUid(), command(r.GetArgv(), 200))
	if uid := r.GetJailedUid(); uid != 0 {
		c.env.outf("jailed   uid %d\n", uid)
	}
	start := r.GetCreatedAt().AsTime()
	c.env.outf("created  %s\n", start.Local().Format(time.RFC3339Nano))
	for _, t := range r.GetTransitions() {
		line := fmt.Sprintf("  %-9s +%s", stateName(t.GetState()), ms(t.GetAt().AsTime().Sub(start)))
		if d := t.GetDetail(); d != "" {
			line += "  " + firstLine(d)
		}
		c.env.outln(line)
	}
	if res := r.GetResult(); res != nil {
		c.env.outf("outcome  %s\noutput   %d bytes", outcomeText(res), res.GetOutputBytes())
		if res.GetOutputTruncated() {
			c.env.outs(" (hit its cap)")
		}
		c.env.outln()
		if e := res.GetError(); e != "" {
			c.env.outf("error    %s\n", e)
		}
		t := res.GetTimings()
		us := func(v uint64) string { return ms(microseconds(v)) }
		c.env.outf("timings  queued %s, restore %s (prepare %s, scratch %s, process start %s, LoadSnapshot %s, "+
			"agent ready %s, de-twin %s), mount %s, exec %s, destroy %s, total %s\n",
			us(t.GetQueuedUs()), us(t.GetRestoreUs()), us(t.GetPrepareUs()), us(t.GetScratchUs()), us(t.GetProcessStartUs()),
			us(t.GetLoadSnapshotUs()), us(t.GetAgentReadyUs()), us(t.GetDeTwinUs()), us(t.GetMountUs()), us(t.GetExecUs()),
			us(t.GetDestroyUs()), us(t.GetTotalUs()))
	}
}

// logs is `sandboxctl logs RUN_ID`: the kept output, stdout to stdout and
// stderr to stderr, in the order it came, and a note when some is missing.
func (c *client) logs(args []string) int {
	flags := flag.NewFlagSet("sandboxctl logs", flag.ContinueOnError)
	flags.SetOutput(c.env.stderr)
	c.socketFlag(flags)
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return exitUsage
	}
	if flags.NArg() != 1 {
		c.env.errln("usage: sandboxctl logs RUN_ID")
		return exitUsage
	}
	api, closeConn, err := c.connect()
	if err != nil {
		c.env.errln(err)
		return 1
	}
	defer closeConn()
	ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
	defer cancel()
	resp, err := api.Logs(ctx, &daemonv1.LogsRequest{RunId: flags.Arg(0)})
	if err != nil {
		return c.fail(1, err)
	}
	for _, chunk := range resp.GetChunks() {
		if b := chunk.GetStderr(); b != nil {
			_, _ = c.env.stderr.Write(b)
		} else {
			_, _ = c.env.stdout.Write(chunk.GetStdout())
		}
	}
	for _, note := range logNotes(resp) {
		c.env.errln("sandboxctl: " + note)
	}
	return 0
}

// logNotes says what is missing from a run's kept output, and why.
func logNotes(r *daemonv1.LogsResponse) []string {
	var notes []string
	if !r.GetFinished() {
		notes = append(notes, "the run is still going: this is its output so far")
	}
	if r.GetOutputTruncated() {
		notes = append(notes, "the run's output reached its cap (--max-output); the rest was never read")
	}
	if r.GetDropped() {
		notes = append(notes, fmt.Sprintf("the output (%d bytes) was dropped: the daemon keeps 64 MiB of output in all, oldest runs first",
			r.GetOutputBytes()))
	} else if r.GetCut() {
		notes = append(notes, fmt.Sprintf("output cut: the daemon keeps the first %d of its %d bytes", r.GetKeptBytes(), r.GetOutputBytes()))
	}
	return notes
}

// auditCmd is `sandboxctl audit verify [--state-dir DIR]`: it reads the
// audit log itself, so it works without a daemon.
func auditCmd(args []string, e env) int {
	if len(args) == 0 || args[0] != "verify" {
		e.errln("usage: sandboxctl audit verify [--state-dir DIR]")
		return exitUsage
	}
	flags := flag.NewFlagSet("sandboxctl audit verify", flag.ContinueOnError)
	flags.SetOutput(e.stderr)
	dir := state.DefaultDir
	if d := e.getenv(state.EnvVar); d != "" {
		dir = d
	}
	stateDir := flags.String("state-dir", dir, "the state folder (default: $"+state.EnvVar+", else "+state.DefaultDir+")")
	if err := flags.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return exitUsage
	}
	path := audit.Path(filepath.Join(*stateDir, audit.DirName))
	sum, err := audit.Verify(path)
	if err != nil {
		e.errf("sandboxctl: BROKEN: %v\n", err)
		e.errf("sandboxctl: keep the file as evidence; stop sandboxd and start a new chain with: sandboxd rotate-audit --state-dir %s\n", *stateDir)
		return 1
	}
	e.outf("audit log %s: %d records, hash chain OK, newest hash %s\n", path, sum.Records, sum.Head)
	return 0
}

// command shows argv on one line, at most n characters, without control
// characters (another user's argv could hold terminal escapes).
func command(argv []string, n int) string {
	s := vmm.CleanConsole([]byte(strings.Join(argv, " ")), 1<<20)
	s = strings.NewReplacer("\n", " ", "\t", " ").Replace(s)
	if len(s) > n {
		s = s[:n-3] + "..."
	}
	return s
}

// age shows a duration the way ps does: 12s, 3m, 2h, 4d.
func age(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}
