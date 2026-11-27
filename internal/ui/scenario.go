package ui

import (
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	daemonv1 "github.com/redwan-cpp/MicroVM/internal/gen/sandbox/daemon/v1"
)

// A scenario is what one button on the page runs: one command in one or
// more fresh clones at once. The presenter clicks; sandboxd does the
// work. The UI only ever sends RunRequests, never env or stdin, and the
// daemon checks every one of them as it checks sandboxctl's.

// scenario is one of the page's buttons.
type scenario struct {
	id, title, caption string
	clones             int
	timeout            time.Duration
	maxOutput          uint64
	// argv is lane i's command.
	argv func(i int) []string
}

// demo1Command is Demo 1's command for clone n (as in scripts/demo1.sh):
// write a secret of its own to /work, its own scratch disk, list /work,
// hold on a second so that all three run at once, then show every
// secret it can read there: only its own.
func demo1Command(n int) string {
	return fmt.Sprintf(`echo "secret of clone %d" > /work/secret-%d; ls /work; sleep 1; cat /work/secret-*`, n, n)
}

// scenarios are the preset buttons, in the page's order.
var scenarios = []scenario{
	{
		id: "demo1", title: "Three clones, private /work",
		caption: "Demo 1: three clones of one snapshot run at once; each writes a secret to its own scratch disk and sees only its own.",
		clones:  3, timeout: 30 * time.Second,
		argv: func(i int) []string { return []string{"sh", "-c", demo1Command(i + 1)} },
	},
	{
		id: "hostile", title: "Hostile code stays inside",
		caption: "It sees only the guest's own empty folders, has no network card to reach the internet with, and cannot change the read-only root disk.",
		clones:  1, timeout: 15 * time.Second,
		argv: func(int) []string { return []string{"sh", "-c", "ls -A /home /root; ping -c1 -W1 8.8.8.8; rm /bin/sh"} },
	},
	{
		// Each f starts two more and waits for them, so the processes
		// double without end, and fill the guest's task table and memory
		// within a second or two. Mostly (9 runs in 10 on the dev
		// machine) the 1 s timeout comes first: TIMED_OUT. Sometimes the
		// guest runs out first and the agent dies with it (out of memory,
		// or unable to start a thread): the run FAILED, as the host
		// judges it. Either way the clone is destroyed.
		id: "forkbomb", title: "Fork bomb is stopped",
		caption: "Each process starts two more, without end. The host's own clock stops it at its 1 s timeout (timed out); if the guest runs out of processes or memory first, its agent dies with it (failed). Either way the host destroys the clone: the bomb only ever had the guest's own vCPU and memory.",
		clones:  1, timeout: time.Second,
		argv: func(int) []string { return []string{"sh", "-c", "f() { f | f; }; f"} },
	},
	{
		// A finite flood: with an honest agent, a command that never
		// stops writing runs until its timeout (the agent drops what is
		// past the cap; see the build plan's "Later" list), so this one
		// writes 50 MB and ends.
		id: "flood", title: "Output flood hits the cap",
		caption: "The command writes 50 MB of output. The host passes on only the first 256 KiB, the run's output cap; the rest is thrown away unread.",
		clones:  1, timeout: 30 * time.Second, maxOutput: 256 << 10,
		argv: func(int) []string {
			return []string{"sh", "-c", "yes 'the guest writes and writes and writes' | head -c 50000000"}
		},
	},
}

// customID is the custom command box's scenario.
const customID = "custom"

// The custom command box's limits (the daemon has its own, and checks
// them again).
const (
	maxLine        = 4 << 10
	maxTimeout     = 10 * time.Minute // the daemon's ceiling
	defaultTimeout = 30 * time.Second
	maxBody        = 64 << 10
	// defaultMaxOutput is the daemon's default output cap, sent
	// explicitly so the page can show it.
	defaultMaxOutput = 1 << 20
)

// runRequest is the body of POST /api/run.
type runRequest struct {
	Scenario  string `json:"scenario"`
	Line      string `json:"line,omitempty"`
	Clones    int    `json:"clones,omitempty"`
	Class     string `json:"class,omitempty"`
	TimeoutMS uint32 `json:"timeout_ms,omitempty"`
}

// plan is a checked scenario: what the page shows, and one RunRequest
// per lane.
type plan struct {
	event *ScenarioEvent
	reqs  []*daemonv1.RunRequest
}

// errBadRequest marks a request the page should not have sent.
var errBadRequest = errors.New("bad request")

func badRequest(format string, args ...any) error {
	return fmt.Errorf("%w: %s", errBadRequest, fmt.Sprintf(format, args...))
}

// expand checks req against the daemon's limits (info) and turns it into
// a plan.
func expand(req runRequest, info *InfoEvent) (*plan, error) {
	class := req.Class
	if class == "" {
		class = "small"
	}
	if !hasClass(info, class) {
		return nil, badRequest("the daemon has no snapshot of class %q", class)
	}
	var sc scenario
	if req.Scenario == customID {
		line := req.Line
		switch {
		case strings.TrimSpace(line) == "":
			return nil, badRequest("the command line is empty")
		case len(line) > maxLine:
			return nil, badRequest("the command line is longer than %d bytes", maxLine)
		case !utf8.ValidString(line) || strings.ContainsRune(line, 0):
			return nil, badRequest("the command line is not text")
		}
		timeout := defaultTimeout
		if req.TimeoutMS != 0 {
			timeout = time.Duration(req.TimeoutMS) * time.Millisecond
		}
		if timeout > maxTimeout {
			return nil, badRequest("the timeout is above %v", maxTimeout)
		}
		clones := req.Clones
		if clones == 0 {
			clones = 1
		}
		sc = scenario{
			id: customID, title: "Custom command", clones: clones, timeout: timeout,
			caption: "Your own command line, run by sh -c in each clone.",
			argv:    func(int) []string { return []string{"sh", "-c", line} },
		}
	} else {
		found := false
		for _, s := range scenarios {
			if s.id == req.Scenario {
				sc, found = s, true
			}
		}
		if !found {
			return nil, badRequest("no scenario %q", req.Scenario)
		}
		if req.Line != "" || req.Clones != 0 || req.TimeoutMS != 0 {
			return nil, badRequest("scenario %q takes no command line, clone count or timeout", req.Scenario)
		}
	}
	if sc.maxOutput == 0 {
		sc.maxOutput = defaultMaxOutput
	}
	if sc.clones < 1 || sc.clones > info.MaxClones {
		return nil, badRequest("%d clones: this daemon runs 1 to %d at once (sandboxd serve --max-concurrent)", sc.clones, info.MaxClones)
	}

	p := &plan{event: &ScenarioEvent{
		ID: sc.id, Title: sc.title, Caption: sc.caption, Clones: sc.clones, Class: class,
		TimeoutMS: uint32(sc.timeout.Milliseconds()), MaxOutput: sc.maxOutput, //nolint:gosec // G115: at most 10 minutes
	}}
	for i := range sc.clones {
		argv := sc.argv(i)
		p.event.Argv = append(p.event.Argv, argv)
		p.reqs = append(p.reqs, &daemonv1.RunRequest{
			Argv: argv, SizeClass: class, TimeoutMs: p.event.TimeoutMS, MaxOutputBytes: sc.maxOutput,
		})
	}
	return p, nil
}

func hasClass(info *InfoEvent, name string) bool {
	for _, c := range info.Classes {
		if c.Name == name {
			return true
		}
	}
	return false
}
