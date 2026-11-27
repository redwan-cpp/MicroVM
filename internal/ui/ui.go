// Package ui is `sandboxctl ui`: a web page for showing MicroVM Sandbox
// to people, served on the loopback interface only. The page has
// buttons for the demo scenarios (Demo 1's three clones, hostile code, a
// fork bomb, an output flood, a custom command), one lane per clone with
// its phases and output, and panels for what sandboxd says about the
// host, the audit log, the timings and the recent runs.
//
// The server is an ordinary client of sandboxd, on the same socket as
// every sandboxctl command: it starts runs with Run and reads Info, List,
// HostStats; it never runs anything itself, and every daemon limit
// applies to it. The browser talks to the server over HTTP: JSON for the
// buttons, Server-Sent Events for everything that happens (ADR-0007).
//
// Live and replay are two separate code paths on purpose:
//
//   - live: the page (live.html, app.js, live.js) gets events from
//     sandboxd as they happen, and only this page has the LIVE badge;
//     --record FILE writes every event the page gets into a session file
//     (session.go);
//   - replay: --replay FILE serves, and `ui export` writes, the replay
//     page (replay.html, app.js, player.js) with the session embedded,
//     which always says it is a recording, whatever the file says
//     (page.go).
//
// The page is a control surface of its own (threat model T13), so the
// server checks a random token (a cookie), the Host header and, on every
// call that changes something, the Origin header, and it sends a strict
// Content-Security-Policy. Guest output is untrusted everywhere: the page
// shows it only as text.
package ui

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/redwan-cpp/MicroVM/internal/audit"
	daemonv1 "github.com/redwan-cpp/MicroVM/internal/gen/sandbox/daemon/v1"
)

const (
	// callTimeout bounds every unary call to sandboxd.
	callTimeout = 2 * time.Second
	// runSlack is how much longer than its timeout a run may take in all:
	// waiting for a slot, the restore and the destroy.
	runSlack = 2 * time.Minute
	// stopWait is how long shutting down waits for running scenarios to
	// end (sandboxd stops their runs when their calls end).
	stopWait = 15 * time.Second
)

// Options configure New.
type Options struct {
	// Listener is where to serve: a loopback address (the caller checks
	// that), already listening.
	Listener net.Listener
	// Daemon is sandboxd (live mode). nil means replay mode.
	Daemon daemonv1.SandboxServiceClient
	// StateDir is sandboxd's state folder, where the UI reads the audit
	// log (as `sandboxctl audit verify` does). Live mode only.
	StateDir string
	// Record, if set, is a new file to record the session into (live
	// mode only).
	Record string
	// Replay is the session to play back (replay mode).
	Replay *Session
	// Version is sandboxctl's build, shown on the page.
	Version string
	// Logger gets the server's messages. nil means the standard logger.
	Logger *log.Logger
	// Token replaces the random token (tests only).
	Token string
}

// Server is a started UI server. Create it with New, then call Run.
type Server struct {
	opts   Options
	logger *log.Logger
	live   bool

	token      string
	port       int
	hostPort   string // the address we listen on, as a Host header: 127.0.0.1:8090
	cookieName string
	hosts      map[string]bool // the Host headers we answer
	handler    http.Handler

	// Replay mode: the page, built once.
	replayPage []byte

	// Live mode.
	daemon    daemonv1.SandboxServiceClient
	hub       *hub
	rec       *recorder
	scrub     *scrubber
	auditPath string
	start     time.Time

	runCtx    context.Context // every scenario's parent; Run's shutdown cancels it
	stopRuns  context.CancelCauseFunc
	scenarios sync.WaitGroup

	mu           sync.Mutex
	info         *InfoEvent
	active       bool               // a scenario is running: one at a time
	cancelActive context.CancelFunc // ends it
	seg          int

	pollMu  sync.Mutex
	lastPub map[string]string // the last published host/history/audit event, as JSON
}

// New prepares a server: in live mode it asks sandboxd for its Info (so a
// daemon that is not there fails now, with a clear message) and creates
// the recording; in replay mode it builds the page.
func New(ctx context.Context, opts Options) (*Server, error) {
	if opts.Listener == nil {
		return nil, errors.New("ui: no listener")
	}
	s := &Server{opts: opts, logger: opts.Logger, live: opts.Daemon != nil, lastPub: make(map[string]string)}
	if s.logger == nil {
		s.logger = log.Default()
	}
	addr, ok := opts.Listener.Addr().(*net.TCPAddr)
	if !ok || !addr.IP.IsLoopback() {
		return nil, fmt.Errorf("ui: listening on %v, not on a loopback address", opts.Listener.Addr())
	}
	s.port = addr.Port
	s.cookieName = "microvm_ui_" + strconv.Itoa(s.port) // one cookie per port: two servers never mix
	// The Host headers a request may carry: the address we listen on,
	// or localhost with our port. Anything else (a name that resolves
	// to 127.0.0.1 through DNS rebinding, say) is refused.
	s.hostPort = net.JoinHostPort(addr.IP.String(), strconv.Itoa(s.port))
	s.hosts = map[string]bool{s.hostPort: true, "localhost:" + strconv.Itoa(s.port): true}
	s.token = opts.Token
	if s.token == "" {
		b := make([]byte, 16) // 128 bits
		_, _ = rand.Read(b)   // never fails (crypto/rand documents this)
		s.token = hex.EncodeToString(b)
	}

	if s.live {
		if err := s.startLive(ctx); err != nil {
			return nil, err
		}
	} else {
		if opts.Replay == nil {
			return nil, errors.New("ui: neither a daemon nor a session to replay")
		}
		page, err := replayPage(opts.Replay, false)
		if err != nil {
			return nil, err
		}
		s.replayPage = page
	}
	s.handler = s.routes()
	return s, nil
}

// startLive asks sandboxd for its Info and opens the recording.
func (s *Server) startLive(ctx context.Context) error {
	s.daemon = s.opts.Daemon
	s.scrub = newScrubber(s.opts.StateDir)
	s.auditPath = audit.Path(filepath.Join(s.opts.StateDir, audit.DirName))
	s.start = time.Now()
	info, err := s.fetchInfo(ctx)
	if err != nil {
		return err
	}
	s.info = info
	if s.opts.Record != "" {
		meta := SessionMeta{CreatedAt: s.start.UTC().Truncate(time.Second), UIVersion: s.opts.Version, Info: info}
		if s.rec, err = createRecorder(s.opts.Record, meta, s.start); err != nil {
			return err
		}
	}
	s.hub = newHub(s.logger, s.rec)
	s.runCtx, s.stopRuns = context.WithCancelCause(context.Background())
	s.publishChanged(&Event{Type: TypeInfo, Info: info})
	return nil
}

// fetchInfo asks sandboxd for its Info.
func (s *Server) fetchInfo(ctx context.Context) (*InfoEvent, error) {
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	r, err := s.daemon.Info(ctx, &daemonv1.InfoRequest{})
	if err != nil {
		return nil, fmt.Errorf("ui: ask sandboxd for its Info: %w", err)
	}
	return infoEvent(r, s.opts.Version), nil
}

// URL is the address to open: it carries the token once; the page then
// keeps it in a cookie and drops it from the address bar.
func (s *Server) URL() string {
	return fmt.Sprintf("http://%s/?token=%s", s.hostPort, s.token)
}

// Handler is the server's HTTP handler (tests).
func (s *Server) Handler() http.Handler { return s.handler }

// Run serves until ctx ends, then shuts down: in live mode it stops the
// pollers, ends a running scenario (sandboxd stops its runs and destroys
// their clones), verifies the audit log once more and closes the
// recording with that result.
func (s *Server) Run(ctx context.Context) error {
	srv := &http.Server{
		Handler:           s.handler,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    16 << 10,
		ErrorLog:          s.logger,
	}
	served := make(chan error, 1)
	go func() { served <- srv.Serve(s.opts.Listener) }()

	pollCtx, stopPolls := context.WithCancel(ctx)
	var polls sync.WaitGroup
	if s.live {
		polls.Go(func() { s.poll(pollCtx) })
	}

	var err error
	select {
	case <-ctx.Done():
	case err = <-served:
		err = fmt.Errorf("ui: serve: %w", err)
	}
	stopPolls()
	polls.Wait()
	if s.live {
		err = errors.Join(err, s.shutdownLive())
	}
	shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if e := srv.Shutdown(shutCtx); e != nil && !errors.Is(e, http.ErrServerClosed) {
		err = errors.Join(err, fmt.Errorf("ui: shut down: %w", e))
	}
	return err
}

// shutdownLive ends the live side: scenarios, the final verify, the
// recording and every event stream.
func (s *Server) shutdownLive() error {
	s.stopRuns(errUIStopping)
	done := make(chan struct{})
	go func() {
		s.scenarios.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(stopWait):
		s.logger.Printf("a scenario's runs did not end within %v; sandboxd still stops them and destroys their clones", stopWait)
	}
	v := s.verify()
	s.hub.close()
	if s.rec == nil {
		return nil
	}
	if err := s.rec.close(v, time.Now()); err != nil {
		return fmt.Errorf("ui: close the recording: %w", err)
	}
	s.logger.Printf("recording written to %s", s.opts.Record)
	return nil
}

// currentInfo is the daemon's newest Info.
func (s *Server) currentInfo() *InfoEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.info
}
