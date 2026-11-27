package ui

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/redwan-cpp/MicroVM/internal/audit"
	daemonv1 "github.com/redwan-cpp/MicroVM/internal/gen/sandbox/daemon/v1"
)

// The panels that are not about one scenario are kept up to date by
// asking sandboxd at a fixed pace, each call with a 2 s deadline. An
// event is published only when its panel changed, so an idle page costs
// nothing and a recording stays small. Runs started elsewhere (from the
// CLI) show up in the history this way.
const (
	hostEvery    = 500 * time.Millisecond
	historyEvery = time.Second
	infoEvery    = 10 * time.Second
	// auditShown is how many of the newest audit records the page shows.
	auditShown = 8
)

// poll runs the pollers until ctx ends.
func (s *Server) poll(ctx context.Context) {
	s.publishAudit(audit.Tail(s.auditPath, auditShown))
	s.pollHistory(ctx)
	s.pollHost(ctx)
	host := time.NewTicker(hostEvery)
	defer host.Stop()
	history := time.NewTicker(historyEvery)
	defer history.Stop()
	info := time.NewTicker(infoEvery)
	defer info.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-host.C:
			s.pollHost(ctx)
		case <-history.C:
			s.pollHistory(ctx)
		case <-info.C:
			s.pollInfo(ctx)
		}
	}
}

// publishChanged publishes ev if its panel changed since the last time,
// and reports whether it did.
func (s *Server) publishChanged(ev *Event) bool {
	data, err := json.Marshal(ev)
	if err != nil {
		return false
	}
	s.pollMu.Lock()
	same := s.lastPub[ev.Type] == string(data)
	s.lastPub[ev.Type] = string(data)
	s.pollMu.Unlock()
	if !same {
		s.hub.publish(ev)
	}
	return !same
}

// unavailable says, in a few words, why sandboxd did not answer.
func (s *Server) unavailable(err error) string {
	if st, ok := status.FromError(err); ok {
		if st.Code() == codes.Unavailable || st.Code() == codes.DeadlineExceeded {
			return "sandboxd is not answering (" + s.scrub.scrub(st.Message()) + ")"
		}
		return "sandboxd: " + s.scrub.scrub(st.Message())
	}
	return s.scrub.scrub(err.Error())
}

// pollHost asks sandboxd what its clones use now.
func (s *Server) pollHost(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	r, err := s.daemon.HostStats(ctx, &daemonv1.HostStatsRequest{})
	if ctx.Err() != nil && errors.Is(ctx.Err(), context.Canceled) {
		return // shutting down
	}
	ev := &HostEvent{Clones: []CloneStat{}}
	if err != nil {
		ev.Error = s.unavailable(err)
	} else {
		ev = hostEvent(r, s.scrub.scrub)
	}
	s.publishChanged(&Event{Type: TypeHost, Host: ev})
}

// pollHistory asks sandboxd for every run it remembers; when that
// changed, the audit log has new records too.
func (s *Server) pollHistory(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	r, err := s.daemon.List(ctx, &daemonv1.ListRequest{All: true})
	if ctx.Err() != nil && errors.Is(ctx.Err(), context.Canceled) {
		return
	}
	ev := &HistoryEvent{Runs: []HistoryRun{}}
	if err != nil {
		ev.Error = s.unavailable(err)
	} else {
		ev = historyEvent(r)
	}
	if s.publishChanged(&Event{Type: TypeHistory, History: ev}) {
		s.publishAudit(audit.Tail(s.auditPath, auditShown))
	}
}

// pollInfo asks sandboxd for its Info again (it may have restarted, in
// another mode or with other snapshots).
func (s *Server) pollInfo(ctx context.Context) {
	info, err := s.fetchInfo(ctx)
	if err != nil {
		return // the host panel says sandboxd is not answering
	}
	s.mu.Lock()
	s.info = info
	s.mu.Unlock()
	s.publishChanged(&Event{Type: TypeInfo, Info: info})
}

// publishAudit shows the newest audit records (the newest auditShown of
// entries).
func (s *Server) publishAudit(entries []audit.Entry, err error) {
	if len(entries) > auditShown {
		entries = entries[len(entries)-auditShown:]
	}
	ev := auditEvent(entries)
	if err != nil {
		ev.Error = "the audit log could not be read: " + s.scrub.scrub(err.Error())
	}
	s.publishChanged(&Event{Type: TypeAudit, Audit: ev})
}

// verify walks the whole audit chain, as `sandboxctl audit verify` does,
// and shows the result.
func (s *Server) verify() *VerifyEvent {
	sum, err := audit.Verify(s.auditPath)
	ev := &VerifyEvent{OK: err == nil, Records: sum.Records, Head: sum.Head}
	var broken *audit.BrokenError
	switch {
	case errors.As(err, &broken):
		ev.Line, ev.Reason = broken.Line, s.scrub.scrub(broken.Reason)
		ev.Head = ""
	case err != nil:
		ev.Reason, ev.Head = "the audit log could not be read: "+s.scrub.scrub(err.Error()), ""
	}
	s.hub.publish(&Event{Type: TypeVerify, Verify: ev})
	return ev
}
