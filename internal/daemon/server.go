// Package daemon is sandboxd: the gRPC service that carries out runs (a
// command in a fresh clone of a base snapshot) and everything around it,
// the socket, the state folder's lock, the audit log, the clone slots and
// shutdown. cmd/sandboxd only reads its flags.
//
// One run, from the daemon's side (docs/learn/11-the-daemon-and-a-run.md
// tells it in full):
//
//	PENDING    accepted; wait for a free slot (at most MaxConcurrent run)
//	RESTORING  clone.Restore: a new clone of the class's base snapshot
//	MOUNTING   its own scratch disk at /work (2 s deadline)
//	RUNNING    the command; its output streams to the caller as it comes
//	STOPPING   Destroy the clone, then write the audit record
//	DONE       (or FAILED) reported to the caller, last
//
// Whatever happens (the command fails, the restore fails, the caller goes
// away, the daemon shuts down), the run goes through STOPPING, its clone
// is destroyed once, and its audit record is written before anyone hears
// that it ended.
package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/redwan-cpp/MicroVM/internal/agent"
	"github.com/redwan-cpp/MicroVM/internal/agentclient"
	"github.com/redwan-cpp/MicroVM/internal/audit"
	"github.com/redwan-cpp/MicroVM/internal/clone"
	agentv1 "github.com/redwan-cpp/MicroVM/internal/gen/sandbox/agent/v1"
	daemonv1 "github.com/redwan-cpp/MicroVM/internal/gen/sandbox/daemon/v1"
	"github.com/redwan-cpp/MicroVM/internal/run"
	"github.com/redwan-cpp/MicroVM/internal/snapshot"
	"github.com/redwan-cpp/MicroVM/internal/state"
	"github.com/redwan-cpp/MicroVM/internal/vmm"
)

const (
	// mountTimeout bounds MountScratch, in the MOUNTING state.
	mountTimeout = 2 * time.Second
	// maxDetail is the longest detail or error text the daemon reports.
	maxDetail = 4 << 10
)

// Errors that end a run early, as the run's cause.
var (
	errCallerGone   = errors.New("the caller went away")
	errShuttingDown = errors.New("the daemon is shutting down")
)

// VM is one restored clone, as the daemon uses it. *clone.Clone is one.
type VM interface {
	MountScratch(ctx context.Context) error
	Exec(ctx context.Context, req *agentv1.ExecRequest, stdout, stderr io.Writer) (clone.ExecResult, error)
	Timings() clone.Timings
	Version() *snapshot.Version
	// Pid is the clone's Firecracker process (HostStats reads its memory).
	Pid() int
	Destroy() error
}

// Cloner restores clones. The daemon uses clone.Restore (NewCloner); its
// unit tests use a fake.
type Cloner interface {
	// Restore makes a clone of the current snapshot of class, named id,
	// in the given slot (0 to MaxConcurrent-1; in jailer mode the slot
	// picks the clone's UID). On error it has destroyed what it made, and
	// a failure to do so is a *clone.CleanupError inside the error.
	Restore(ctx context.Context, class, id string, slot int) (VM, error)
}

// cloneFactory is the real Cloner: clone.Restore from one store.
type cloneFactory struct {
	store       *snapshot.Store
	firecracker string
	jailer      *clone.JailerOptions // a template: the UID and GID come from the slot
}

// NewCloner returns a Cloner that restores from store with clone.Restore.
// With jailer set, clones run in jailer mode, the one in slot i as UID and
// GID jailer.DefaultUID + i and jailer.DefaultGID + i.
func NewCloner(store *snapshot.Store, firecracker string, jailer *clone.JailerOptions) Cloner {
	return &cloneFactory{store: store, firecracker: firecracker, jailer: jailer}
}

func (f *cloneFactory) Restore(ctx context.Context, class, id string, slot int) (VM, error) {
	opts := clone.Options{Firecracker: f.firecracker, ID: id}
	if f.jailer != nil {
		j := *f.jailer
		j.UID += slot
		j.GID += slot
		opts.Jailer = &j
	}
	c, err := clone.Restore(ctx, f.store, class, opts)
	if err != nil {
		return nil, err
	}
	return c, nil
}

// Config is what a Server needs.
type Config struct {
	// Store is the opened snapshot store; Validate checks classes in it.
	Store *snapshot.Store
	// Cloner restores the clones.
	Cloner Cloner
	// Audit is the opened audit log.
	Audit *audit.Log
	// Mode is the mode every clone runs in.
	Mode clone.Mode
	// FirstUID is the jailed UID of slot 0 (jailer mode only), recorded
	// in the audit log.
	FirstUID int
	// MaxConcurrent is how many runs may have a clone at once.
	MaxConcurrent int
	// MaxPending is how many more may wait for a slot; beyond that, Run
	// says ResourceExhausted.
	MaxPending int
	// Version is the daemon's build, recorded in the audit log.
	Version string
	// FirecrackerVersion is the Firecracker the clones run (Info).
	FirecrackerVersion string
	// Host is what DetectHost found (Info). The zero value says unknown.
	Host HostKind
	// Logger gets one line per finished run and every problem. nil means
	// the standard logger.
	Logger *log.Logger
}

// Server implements the SandboxService API. Create it with NewServer.
type Server struct {
	daemonv1.UnimplementedSandboxServiceServer

	cfg   Config
	runs  *registry
	pool  *slotPool
	stats *hostStats

	base     context.Context // every run's parent; Shutdown cancels it
	stopRuns context.CancelCauseFunc
	wg       sync.WaitGroup // one per run goroutine

	mu      sync.Mutex
	pending int  // runs waiting in PENDING
	closing bool // Shutdown was called: no new runs
}

// NewServer returns a Server for cfg.
func NewServer(cfg Config) *Server {
	if cfg.Logger == nil {
		cfg.Logger = log.Default()
	}
	base, stop := context.WithCancelCause(context.Background())
	return &Server{
		cfg:      cfg,
		runs:     newRegistry(perRunOutput, outputBudget, maxRecent),
		pool:     newSlotPool(cfg.MaxConcurrent),
		stats:    &hostStats{procDir: "/proc"},
		base:     base,
		stopRuns: stop,
	}
}

// Run carries out one run and streams its events (see the package
// comment). The run itself happens in a goroutine of its own, so a caller
// that reads slowly never holds it up; if the caller goes away, the run is
// stopped (and still destroyed and recorded).
func (s *Server) Run(req *daemonv1.RunRequest, stream daemonv1.SandboxService_RunServer) error {
	ctx := stream.Context()
	plan, err := s.validate(req)
	if err != nil {
		return err
	}
	caller, ok := callerOf(ctx)
	if !ok {
		// Every connection through peerCreds has them; a call without
		// them came some other way, and its caller would be recorded as
		// UID 0 (root). Refuse it.
		return status.Error(codes.Unauthenticated, "the call has no peer credentials (SO_PEERCRED): sandboxd only serves its Unix socket")
	}
	e, err := s.admit(req, plan, caller.UID)
	if err != nil {
		return err
	}

	// Stream the run's events until its result has been sent.
	for {
		evs, closed := e.events.take()
		for _, ev := range evs {
			if err := stream.Send(ev); err != nil {
				e.cancel(errCallerGone)
				e.events.detach()
				return err
			}
		}
		if closed {
			return nil // that was the result: nothing comes after it
		}
		select {
		case <-e.events.wake:
		case <-ctx.Done():
			// The caller cancelled or disconnected: stop the run. Its
			// goroutine still destroys the clone and writes the record.
			e.cancel(errCallerGone)
			e.events.detach()
			return status.FromContextError(ctx.Err()).Err()
		}
	}
}

// runPlan is a validated request.
type runPlan struct {
	class   snapshot.Class
	timeout time.Duration
	exec    *agentv1.ExecRequest
}

// validate checks req and returns what to run. Every error is a gRPC
// status: InvalidArgument for a bad request, FailedPrecondition for a
// class without a snapshot.
func (s *Server) validate(req *daemonv1.RunRequest) (*runPlan, error) {
	if len(req.ProtoReflect().GetUnknown()) > 0 {
		return nil, status.Error(codes.InvalidArgument,
			"the request has fields this daemon does not know (is sandboxctl newer than sandboxd?)")
	}
	name := req.GetSizeClass()
	if name == "" {
		name = snapshot.Classes[0].Name
	}
	class, err := snapshot.LookupClass(name)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	if _, err := s.cfg.Store.Current(class.Name); err != nil {
		return nil, status.Error(codes.FailedPrecondition, err.Error())
	}
	exec := &agentv1.ExecRequest{
		Argv:           req.GetArgv(),
		Env:            req.GetEnv(),
		Stdin:          req.GetStdin(),
		TimeoutMs:      req.GetTimeoutMs(),
		MaxOutputBytes: req.GetMaxOutputBytes(),
	}
	// The agent's own rules, checked here first: a request the daemon
	// accepts never fails at the agent for its shape.
	if err := agent.CheckExecRequest(exec); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	timeout, _, err := agent.ExecLimits(req.GetTimeoutMs(), req.GetMaxOutputBytes())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	return &runPlan{class: class, timeout: timeout, exec: exec}, nil
}

// admit accepts a run (or says why not), registers it in PENDING and
// starts its goroutine.
func (s *Server) admit(req *daemonv1.RunRequest, plan *runPlan, callerUID uint32) (*runEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case s.closing:
		return nil, status.Error(codes.Unavailable, errShuttingDown.Error())
	case s.cfg.Audit.Failed() != nil:
		// No run may happen without its record.
		return nil, status.Errorf(codes.Unavailable, "the audit log could not be written, so no runs are accepted until the daemon restarts: %v",
			s.cfg.Audit.Failed())
	case s.pool.allRetired():
		return nil, status.Error(codes.Unavailable, "every slot is retired after failed cleanups; check the daemon's log and restart it")
	case s.pending >= s.cfg.MaxPending:
		return nil, status.Errorf(codes.ResourceExhausted, "%d runs are already waiting for a slot; try again later", s.pending)
	}
	s.pending++

	ctx, cancel := context.WithCancelCause(s.base)
	events := newEventQueue()
	e := &runEntry{
		id:        state.NewID(),
		argv:      slices.Clone(req.GetArgv()),
		class:     plan.class.Name,
		mode:      modeToProto(s.cfg.Mode),
		callerUID: callerUID,
		events:    events,
		cancel:    cancel,
	}
	e.machine = run.NewMachine(func(t run.Transition) {
		events.push(&daemonv1.RunResponse{Event: &daemonv1.RunResponse_State{State: transitionToProto(e.id, t)}})
	})
	s.runs.add(e)
	s.wg.Add(1)
	go s.execute(ctx, e, req, plan)
	return e, nil
}

// span is what execute learns about one run, for its result and record.
type span struct {
	verdict     run.Verdict
	restore     clone.Timings
	restored    bool
	queued      time.Duration
	mount       time.Duration
	exec        time.Duration
	destroy     time.Duration
	destroyErr  error
	slotRetired bool
	jailedUID   int
	snapshot    string
}

// execute is the life of one run (see the package comment).
func (s *Server) execute(ctx context.Context, e *runEntry, req *daemonv1.RunRequest, plan *runPlan) {
	defer s.wg.Done()
	defer e.cancel(nil)
	var sp span

	// PENDING: wait for a slot.
	slot, err := s.pool.acquire(ctx)
	s.mu.Lock()
	s.pending--
	s.mu.Unlock()
	sp.queued = time.Since(e.machine.Created())
	if err == nil && ctx.Err() != nil {
		// A slot came free just as the run was cancelled: give it back.
		s.pool.release(slot)
		err = context.Cause(ctx)
	}
	if err == nil && s.cfg.Audit.Failed() != nil {
		// The audit log broke while this run waited: admit's check is
		// not enough, since no run may start without a working log.
		s.pool.release(slot)
		err = fmt.Errorf("the audit log could not be written, so no runs start until the daemon restarts: %w", s.cfg.Audit.Failed())
	}
	if err != nil {
		sp.verdict = run.Failure(fmt.Errorf("while waiting for a slot: %w", err))
		s.to(e, run.Stopping, detailOf(sp.verdict.Err))
		s.finish(e, req, &sp)
		return
	}
	if s.cfg.Mode == clone.Jailer {
		sp.jailedUID = s.cfg.FirstUID + slot
		s.runs.setJailedUID(e, uint32(sp.jailedUID)) //nolint:gosec // G115: 70000 + a slot below 32
	}

	// RESTORING.
	s.to(e, run.Restoring, "")
	vm, err := s.cfg.Cloner.Restore(ctx, plan.class.Name, e.id, slot)
	if err != nil {
		sp.verdict = run.Failure(s.because(ctx, fmt.Errorf("restore: %w", err)))
		// Restore cleaned up after itself; if that failed, something of
		// the clone may be left, so its slot must not be used again.
		var cleanup *clone.CleanupError
		if errors.As(err, &cleanup) {
			sp.destroyErr = cleanup
		}
		s.to(e, run.Stopping, detailOf(sp.verdict.Err))
		s.releaseSlot(e, slot, &sp)
		s.finish(e, req, &sp)
		return
	}
	sp.restored, sp.restore, sp.snapshot = true, vm.Timings(), vm.Version().ID
	s.runs.setVM(e, vm) // HostStats sees it from now on

	// MOUNTING, then RUNNING.
	s.to(e, run.Mounting, "")
	began := time.Now()
	mountCtx, cancel := context.WithTimeout(ctx, mountTimeout)
	err = vm.MountScratch(mountCtx)
	cancel()
	sp.mount = time.Since(began)
	if err != nil {
		sp.verdict = run.Failure(s.because(ctx, fmt.Errorf("mount the scratch disk: %w", err)))
	} else {
		s.to(e, run.Running, "")
		began = time.Now()
		// agentclient enforces the timeout (plus its grace) itself; this
		// deadline is only the outer bound it requires.
		execCtx, cancel := context.WithTimeout(ctx, plan.timeout+2*agentclient.DeadlineGrace)
		res, err := vm.Exec(execCtx, plan.exec, outputWriter{s, e, false}, outputWriter{s, e, true})
		cancel()
		sp.exec = time.Since(began)
		if err != nil {
			err = s.because(ctx, err)
		}
		sp.verdict = run.Judge(res.Result, err)
	}

	// STOPPING: destroy the clone. Destroy takes no context: it must run
	// to its end even when the run was cancelled, and bounds itself.
	s.to(e, run.Stopping, detailOf(sp.verdict.Err))
	s.runs.setVM(e, nil)
	began = time.Now()
	sp.destroyErr = vm.Destroy()
	sp.destroy = time.Since(began)
	s.releaseSlot(e, slot, &sp)
	s.finish(e, req, &sp)
}

// because adds the reason a run was cancelled to err.
func (s *Server) because(ctx context.Context, err error) error {
	if cause := context.Cause(ctx); cause != nil {
		return fmt.Errorf("%w: %w", cause, err)
	}
	return err
}

// releaseSlot gives the slot back, unless Destroy failed: then something
// of the clone (a process, a folder, a jail, a cgroup) may be left on the
// host, and in jailer mode its UID may still own a process, so the slot
// is retired until the daemon restarts.
func (s *Server) releaseSlot(e *runEntry, slot int, sp *span) {
	if sp.destroyErr == nil {
		s.pool.release(slot)
		return
	}
	s.pool.retire(slot)
	sp.slotRetired = true
	s.cfg.Logger.Printf("run %s: cleanup failed, slot %d retired until restart: %s", e.id, slot, clean(sp.destroyErr.Error()))
}

// finish writes the audit record (still in STOPPING), then reports the
// end: DONE or FAILED, and the result.
func (s *Server) finish(e *runEntry, req *daemonv1.RunRequest, sp *span) {
	v := sp.verdict
	if sp.destroyErr != nil && v.Outcome != run.FailedOutcome {
		v = run.Failure(fmt.Errorf("destroy: %w", sp.destroyErr))
	}
	final := run.Done
	if v.Outcome == run.FailedOutcome {
		final = run.Failed
	}
	total := time.Since(e.machine.Created())
	timings := s.timings(sp, total)

	rec := s.record(e, req, sp, v, final, timings)
	if err := s.cfg.Audit.Append(rec); err != nil {
		// The run happened, but its record is not on disk: say so, and
		// accept no more runs (admit checks Audit.Failed).
		s.cfg.Logger.Printf("run %s: AUDIT RECORD NOT WRITTEN, accepting no more runs: %v", e.id, err)
		if v.Outcome == run.FailedOutcome && v.Err != nil {
			v = run.Failure(fmt.Errorf("%w; and the audit record could not be written: %w", v.Err, err))
		} else {
			v = run.Failure(fmt.Errorf("the audit record could not be written: %w", err))
		}
		final = run.Failed
	}

	res := &daemonv1.RunResult{
		RunId:           e.id,
		Outcome:         outcomeToProto(v.Outcome),
		Exited:          v.Exited,
		TimedOut:        v.TimedOut,
		OutputTruncated: v.OutputTruncated,
		OutputBytes:     v.OutputBytes,
		Mode:            e.mode,
		Timings:         timings,
	}
	if v.Exited {
		res.ExitCode, res.Signal = int32(v.ExitCode), int32(v.Signal) //nolint:gosec // G115: agentclient checked -1..255 and 0..64
	}
	res.Error = errorText(v.Err, sp.destroyErr)

	s.to(e, final, res.GetError())
	s.runs.finish(e, res)
	e.events.push(&daemonv1.RunResponse{Event: &daemonv1.RunResponse_Result{Result: res}})
	e.events.close()

	summary := v.Outcome.String()
	if v.Exited {
		summary = fmt.Sprintf("exit %d", v.ExitCode)
	}
	s.cfg.Logger.Printf("run %s: %s, %s in %v (%s, class %s, caller uid %d)",
		e.id, final, summary, total.Round(100*time.Microsecond), s.cfg.Mode, e.class, e.callerUID)
}

// to moves e's state machine on. A refused step is a bug in this file:
// it is logged loudly, and the run goes on.
func (s *Server) to(e *runEntry, st run.State, detail string) {
	if err := e.machine.To(st, detail); err != nil {
		s.cfg.Logger.Printf("BUG: run %s: %v", e.id, err)
	}
}

// timings builds the API's Timings from what execute measured.
func (s *Server) timings(sp *span, total time.Duration) *daemonv1.Timings {
	r := sp.restore
	return &daemonv1.Timings{
		QueuedUs:       micros(sp.queued),
		PrepareUs:      micros(r.Prepare),
		ScratchUs:      micros(r.Scratch),
		ProcessStartUs: micros(r.ProcessStart),
		LoadSnapshotUs: micros(r.LoadSnapshot),
		AgentReadyUs:   micros(r.AgentReady),
		DeTwinUs:       micros(r.DeTwin),
		RestoreUs:      micros(r.Total),
		MountUs:        micros(sp.mount),
		ExecUs:         micros(sp.exec),
		DestroyUs:      micros(sp.destroy),
		TotalUs:        micros(total),
	}
}

// record builds the run's audit record.
func (s *Server) record(e *runEntry, req *daemonv1.RunRequest, sp *span, v run.Verdict, final run.State,
	t *daemonv1.Timings) audit.Record {
	r := &audit.Run{
		RunID:           e.id,
		Argv:            e.argv,
		StdinBytes:      len(req.GetStdin()),
		SizeClass:       e.class,
		SnapshotVersion: sp.snapshot,
		Mode:            string(s.cfg.Mode),
		JailedUID:       sp.jailedUID,
		CallerUID:       e.callerUID,
		DaemonVersion:   s.cfg.Version,
		Outcome:         v.Outcome.String(),
		FinalState:      final.String(),
		Exited:          v.Exited,
		TimedOut:        v.TimedOut,
		OutputTruncated: v.OutputTruncated,
		OutputBytes:     v.OutputBytes,
		Error:           errorText(v.Err, nil),
		SlotRetired:     sp.slotRetired,
		TimingsUS: audit.Timings{
			Queued: t.GetQueuedUs(), Prepare: t.GetPrepareUs(), Scratch: t.GetScratchUs(),
			ProcessStart: t.GetProcessStartUs(), LoadSnapshot: t.GetLoadSnapshotUs(), AgentReady: t.GetAgentReadyUs(),
			DeTwin: t.GetDeTwinUs(), Restore: t.GetRestoreUs(), Mount: t.GetMountUs(), Exec: t.GetExecUs(),
			Destroy: t.GetDestroyUs(), Total: t.GetTotalUs(),
		},
	}
	for name := range req.GetEnv() {
		r.EnvNames = append(r.EnvNames, name)
	}
	slices.Sort(r.EnvNames)
	if v.Exited {
		code := v.ExitCode
		r.ExitCode, r.Signal = &code, v.Signal
	}
	if sp.destroyErr != nil {
		r.DestroyError = clean(sp.destroyErr.Error())
	}
	for _, tr := range e.machine.Transitions() {
		r.Transitions = append(r.Transitions, audit.Transition{
			State: tr.State.String(), At: tr.At.UTC(), SinceUS: tr.Since.Microseconds(), Detail: tr.Detail,
		})
	}
	return audit.Record{Kind: audit.KindRun, Time: time.Now().UTC(), Run: r}
}

// outputWriter passes one of a run's output streams to the registry (for
// Logs) and to the caller's event queue. It never fails and never blocks.
type outputWriter struct {
	s      *Server
	e      *runEntry
	stderr bool
}

func (w outputWriter) Write(p []byte) (int, error) {
	b := slices.Clone(p) // agentclient may reuse p
	w.s.runs.addOutput(w.e, w.stderr, b)
	ev := &daemonv1.RunResponse{Event: &daemonv1.RunResponse_Stdout{Stdout: b}}
	if w.stderr {
		ev = &daemonv1.RunResponse{Event: &daemonv1.RunResponse_Stderr{Stderr: b}}
	}
	w.e.events.push(ev)
	return len(p), nil
}

// List returns the active runs, and with all the recent finished ones.
func (s *Server) List(_ context.Context, req *daemonv1.ListRequest) (*daemonv1.ListResponse, error) {
	if len(req.ProtoReflect().GetUnknown()) > 0 {
		return nil, status.Error(codes.InvalidArgument, "the request has fields this daemon does not know")
	}
	resp := &daemonv1.ListResponse{}
	for _, e := range s.runs.list(req.GetAll()) {
		resp.Runs = append(resp.Runs, s.runs.info(e))
	}
	return resp, nil
}

// Logs returns the output kept of one run.
func (s *Server) Logs(_ context.Context, req *daemonv1.LogsRequest) (*daemonv1.LogsResponse, error) {
	e, err := s.lookup(req.ProtoReflect().GetUnknown(), req.GetRunId())
	if err != nil {
		return nil, err
	}
	return s.runs.logs(e), nil
}

// Get returns one run.
func (s *Server) Get(_ context.Context, req *daemonv1.GetRequest) (*daemonv1.GetResponse, error) {
	e, err := s.lookup(req.ProtoReflect().GetUnknown(), req.GetRunId())
	if err != nil {
		return nil, err
	}
	return &daemonv1.GetResponse{Run: s.runs.info(e)}, nil
}

// Info describes the daemon (see the API).
func (s *Server) Info(_ context.Context, req *daemonv1.InfoRequest) (*daemonv1.InfoResponse, error) {
	if len(req.ProtoReflect().GetUnknown()) > 0 {
		return nil, status.Error(codes.InvalidArgument, "the request has fields this daemon does not know")
	}
	host := s.cfg.Host
	if host.Kind == daemonv1.HostKind_HOST_KIND_UNSPECIFIED {
		host = HostKind{daemonv1.HostKind_HOST_KIND_UNKNOWN, "not looked at"}
	}
	arch, err := snapshot.HostArch()
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	resp := &daemonv1.InfoResponse{
		Mode:               modeToProto(s.cfg.Mode),
		DaemonVersion:      s.cfg.Version,
		FirecrackerVersion: s.cfg.FirecrackerVersion,
		Arch:               arch,
		MaxConcurrent:      uint32(max(s.cfg.MaxConcurrent, 0)), //nolint:gosec // G115: 1 to 32
		MaxPending:         uint32(max(s.cfg.MaxPending, 0)),    //nolint:gosec // G115: 64
		HostKind:           host.Kind,
		HostDetail:         host.Detail,
	}
	for _, v := range s.cfg.Store.Classes() {
		m := v.Manifest
		resp.Classes = append(resp.Classes, &daemonv1.ClassInfo{
			Name:              m.Class,
			Vcpus:             uint32(max(m.VCPUs, 0)),      //nolint:gosec // G115: a class's shape
			MemoryMib:         uint32(max(m.MemoryMiB, 0)),  //nolint:gosec // G115: a class's shape
			ScratchMib:        uint32(max(m.ScratchMiB, 0)), //nolint:gosec // G115: a class's shape
			SnapshotVersion:   v.ID,
			SnapshotCreatedAt: timestamp(m.CreatedAt),
			KernelVersion:     m.KernelVersion,
			AgentVersion:      m.AgentVersion,
			Vmgenid:           m.VMGenID,
			ColdBootUs:        m.ColdBootUS,
		})
	}
	return resp, nil
}

// HostStats reports what the daemon's clones use now (see the API). It
// reads /proc and the state folder; one call runs at a time.
func (s *Server) HostStats(ctx context.Context, req *daemonv1.HostStatsRequest) (*daemonv1.HostStatsResponse, error) {
	if len(req.ProtoReflect().GetUnknown()) > 0 {
		return nil, status.Error(codes.InvalidArgument, "the request has fields this daemon does not know")
	}
	h := s.stats
	h.mu.Lock()
	defer h.mu.Unlock()
	root := s.cfg.Store.Root()
	resp := &daemonv1.HostStatsResponse{
		At:                   timestamp(time.Now()),
		FirecrackerProcesses: h.processesUnder(ctx, root.RunsDir(), root.JailDir()),
		RunDirs:              countDir(root.RunsDir()),
		JailDirs:             countDir(filepath.Join(root.JailDir(), "firecracker")),
	}
	if dir, err := h.cgroupParent(); err != nil {
		resp.Cgroups = &daemonv1.Count{Error: "no cgroup v2: " + reason(err)}
	} else {
		resp.Cgroups = countSubdirs(dir)
	}
	for _, c := range s.runs.live() {
		cs := &daemonv1.CloneStats{RunId: c.id, Pid: int32(c.pid), JailedUid: c.jailedUID} //nolint:gosec // G115: a PID
		var err error
		if cs.RssBytes, cs.PssBytes, err = h.memory(c.pid); err != nil {
			cs.MemoryError = err.Error()
		}
		resp.Clones = append(resp.Clones, cs)
	}
	return resp, nil
}

// lookup finds the run a Logs or Get request names.
func (s *Server) lookup(unknown []byte, id string) (*runEntry, error) {
	if len(unknown) > 0 {
		return nil, status.Error(codes.InvalidArgument, "the request has fields this daemon does not know")
	}
	if !state.ValidID(id) {
		return nil, status.Errorf(codes.InvalidArgument, "%q is not a run ID (they look like r-0123456789abcdef)", clean(id))
	}
	e, ok := s.runs.get(id)
	if !ok {
		return nil, status.Errorf(codes.NotFound, "no run %s: the daemon keeps the last %d finished runs, until it restarts", id, maxRecent)
	}
	return e, nil
}

// Shutdown stops accepting runs, stops the running ones (each is still
// destroyed and recorded) and waits for them, at most until ctx ends.
func (s *Server) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	s.closing = true
	s.mu.Unlock()
	s.stopRuns(errShuttingDown)
	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("daemon: runs still stopping: %w", ctx.Err())
	}
}

// slotPool hands out the slots 0 to n-1, one per running clone. A slot
// whose clone could not be cleaned up is retired: never handed out again.
type slotPool struct {
	free chan int
	dead chan struct{} // closed when every slot is retired

	mu      sync.Mutex
	retired int
	n       int
}

func newSlotPool(n int) *slotPool {
	p := &slotPool{free: make(chan int, n), dead: make(chan struct{}), n: n}
	for i := range n {
		p.free <- i
	}
	return p
}

// acquire waits for a free slot, or until ctx ends.
func (p *slotPool) acquire(ctx context.Context) (int, error) {
	select {
	case slot := <-p.free:
		return slot, nil
	case <-ctx.Done():
		return -1, context.Cause(ctx)
	case <-p.dead:
		return -1, errors.New("every slot is retired after failed cleanups")
	}
}

func (p *slotPool) release(slot int) { p.free <- slot }

func (p *slotPool) retire(int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.retired++
	if p.retired == p.n {
		close(p.dead)
	}
}

func (p *slotPool) allRetired() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.retired == p.n
}

// clean makes text that may quote the guest (an agent's error, a console
// tail) safe to show: control characters other than newline and tab go,
// and it is cut to maxDetail bytes.
func clean(text string) string {
	return vmm.CleanConsole([]byte(text), maxDetail)
}

// detailOf is the detail text for err, or "".
func detailOf(err error) string {
	if err == nil {
		return ""
	}
	return clean(err.Error())
}

// errorText is the error text of a result: the run's error, and what
// Destroy could not clean up.
func errorText(err, destroyErr error) string {
	var parts []string
	if err != nil {
		parts = append(parts, err.Error())
	}
	if destroyErr != nil && !errors.Is(err, destroyErr) {
		parts = append(parts, "destroy: "+destroyErr.Error())
	}
	return clean(strings.Join(parts, "; "))
}

func modeToProto(m clone.Mode) daemonv1.Mode {
	if m == clone.Jailer {
		return daemonv1.Mode_MODE_JAILER
	}
	return daemonv1.Mode_MODE_ROOTLESS
}

func timestamp(t time.Time) *timestamppb.Timestamp { return timestamppb.New(t) }
