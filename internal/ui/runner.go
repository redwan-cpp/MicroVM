package ui

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"google.golang.org/grpc/status"

	"github.com/redwan-cpp/MicroVM/internal/audit"
	daemonv1 "github.com/redwan-cpp/MicroVM/internal/gen/sandbox/daemon/v1"
)

// Running a scenario: one Run call per lane, all at once, each relayed
// to the hub as it happens. Output is gathered for up to outputFlush and
// sent in pieces of at most maxOutputEvent, so an output flood is a few
// events a second per lane, not thousands.
const (
	outputFlush    = 100 * time.Millisecond
	maxOutputEvent = 64 << 10
	// auditLook is how many of the newest audit records are searched for
	// a scenario's runs.
	auditLook = 64
)

// The reasons a scenario ends early.
var (
	// errBusy is startScenario's error while a scenario runs.
	errBusy = errors.New("a scenario is running: wait for it to end, or stop it")
	// errStoppedByPage: the page's Stop button.
	errStoppedByPage = errors.New("stopped from the page")
	// errUIStopping: sandboxctl ui is shutting down.
	errUIStopping = errors.New("sandboxctl ui is stopping")
)

// startScenario checks req and starts it, unless one is running.
func (s *Server) startScenario(req runRequest) (*ScenarioEvent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.active {
		return nil, errBusy
	}
	p, err := expand(req, s.info)
	if err != nil {
		return nil, err
	}
	s.seg++
	p.event.Seg = s.seg
	timeout := time.Duration(p.event.TimeoutMS)*time.Millisecond + runSlack
	ctx, stop := context.WithCancelCause(s.runCtx)
	ctx, cancel := context.WithTimeoutCause(ctx, timeout, errors.New("the scenario took longer than its timeout plus 2 minutes"))
	s.active = true
	s.cancelActive = func() { stop(errStoppedByPage) }
	started := time.Now()
	s.hub.publish(&Event{Type: TypeScenario, Scenario: p.event})
	s.scenarios.Add(1)
	go s.runScenario(ctx, func() { cancel(); stop(nil) }, p, started)
	return p.event, nil
}

// stopScenario ends the running scenario, if there is one.
func (s *Server) stopScenario() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.active || s.cancelActive == nil {
		return false
	}
	s.cancelActive()
	return true
}

// runScenario runs every lane, then finds their audit records and ends
// the scenario.
func (s *Server) runScenario(ctx context.Context, cancel context.CancelFunc, p *plan, started time.Time) {
	defer s.scenarios.Done()
	defer cancel()
	seg := p.event.Seg
	ids := make([]string, len(p.reqs))
	errs := make([]error, len(p.reqs))
	var wg sync.WaitGroup
	for i, req := range p.reqs {
		wg.Go(func() { ids[i], errs[i] = s.runLane(ctx, seg, i, req, started) })
	}
	wg.Wait()

	end := &EndEvent{Seg: seg, Audit: []AuditRef{}}
	if err := errors.Join(errs...); err != nil {
		end.Error = s.scrub.scrub(firstError(errs).Error())
	}
	entries, auditErr := audit.Tail(s.auditPath, auditLook)
	for i, id := range ids {
		ref := AuditRef{Lane: i, RunID: id}
		switch {
		case id == "":
			ref.Missing = "the run never started"
		case auditErr != nil:
			ref.Missing = "the audit log could not be read: " + s.scrub.scrub(auditErr.Error())
		default:
			ref.Missing = fmt.Sprintf("not among the newest %d audit records", auditLook)
			for _, e := range entries {
				if e.RunID == id && e.Kind == audit.KindRun {
					ref.Hash, ref.PrevHash, ref.Missing = e.Hash, e.PrevHash, ""
				}
			}
		}
		end.Audit = append(end.Audit, ref)
	}
	s.publishAudit(entries, auditErr)
	s.pollHistory(context.Background()) //nolint:contextcheck // its own short deadline; the scenario's context may have ended
	s.hub.publish(&Event{Type: TypeEnd, End: end})

	s.mu.Lock()
	s.active, s.cancelActive = false, nil
	s.mu.Unlock()
}

func firstError(errs []error) error {
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}

// laneOut gathers one stream of a lane's output between flushes.
type laneOut struct {
	stream string
	buf    []byte
	carry  utf8Carry
}

// recvd is one message (or the end) of a Run stream.
type recvd struct {
	msg *daemonv1.RunResponse
	err error
}

// runLane runs one lane's request and relays its events. It returns the
// run's ID (once sandboxd named it) and why the lane failed, if it did:
// a run that sandboxd carried out to its result has not failed here,
// whatever its outcome.
func (s *Server) runLane(ctx context.Context, seg, lane int, req *daemonv1.RunRequest, started time.Time) (string, error) {
	fail := func(runID string, err error) (string, error) {
		msg := "sandboxd: " + err.Error()
		if st, ok := status.FromError(err); ok {
			msg = "sandboxd: " + st.Message()
		}
		if cause := context.Cause(ctx); cause != nil {
			// The UI ended the call: sandboxd stops the run, destroys its
			// clone and records it ("the caller went away").
			msg = cause.Error() + "; sandboxd stops the run and destroys its clone"
			err = cause
		}
		s.hub.publish(&Event{Type: TypeResult, Result: &ResultEvent{
			Seg: seg, Lane: lane, RunID: runID, Outcome: "FAILED", Mode: s.currentInfo().Mode,
			Error: s.scrub.scrub(msg),
		}})
		return runID, err
	}
	stream, err := s.daemon.Run(ctx, req)
	if err != nil {
		return fail("", err)
	}
	msgs := make(chan recvd, 64)
	go func() {
		for {
			m, err := stream.Recv()
			msgs <- recvd{m, err}
			if err != nil {
				return
			}
		}
	}()

	out := []*laneOut{{stream: "stdout"}, {stream: "stderr"}}
	flush := func(final bool) {
		for _, o := range out {
			text := o.carry.text(o.buf)
			if final {
				text += o.carry.flush()
			}
			o.buf = o.buf[:0]
			for len(text) > 0 {
				piece := prefix(text, maxOutputEvent)
				text = text[len(piece):]
				s.hub.publish(&Event{Type: TypeOutput, Output: &OutputEvent{Seg: seg, Lane: lane, Stream: o.stream, Data: piece}})
			}
		}
	}
	tick := time.NewTicker(outputFlush)
	defer tick.Stop()
	runID, gotResult := "", false
	for {
		select {
		case r := <-msgs:
			if r.err != nil {
				flush(true)
				switch {
				case gotResult && errors.Is(r.err, io.EOF):
					return runID, nil
				case errors.Is(r.err, io.EOF):
					return fail(runID, errors.New("the daemon ended the run without a result"))
				}
				return fail(runID, r.err)
			}
			switch ev := r.msg.GetEvent().(type) {
			case *daemonv1.RunResponse_State:
				flush(false)
				st := ev.State
				if runID == "" {
					runID = st.GetRunId()
				}
				s.hub.publish(&Event{Type: TypeState, State: &StateEvent{
					Seg: seg, Lane: lane, RunID: runID, State: stateName(st.GetState()),
					MS:     float64(st.GetAt().AsTime().Sub(started).Microseconds()) / 1000,
					Detail: s.scrub.scrub(st.GetDetail()),
				}})
			case *daemonv1.RunResponse_Stdout:
				out[0].buf = append(out[0].buf, ev.Stdout...)
				if len(out[0].buf) >= maxOutputEvent {
					flush(false)
				}
			case *daemonv1.RunResponse_Stderr:
				out[1].buf = append(out[1].buf, ev.Stderr...)
				if len(out[1].buf) >= maxOutputEvent {
					flush(false)
				}
			case *daemonv1.RunResponse_Result:
				flush(true)
				gotResult = true
				if runID == "" {
					runID = ev.Result.GetRunId()
				}
				s.hub.publish(&Event{Type: TypeResult, Result: resultEvent(seg, lane, ev.Result, s.scrub.scrub)})
			}
		case <-tick.C:
			flush(false)
		}
	}
}
