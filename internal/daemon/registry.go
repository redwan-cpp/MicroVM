package daemon

import (
	"slices"
	"sync"
	"time"

	daemonv1 "github.com/redwan-cpp/MicroVM/internal/gen/sandbox/daemon/v1"
	"github.com/redwan-cpp/MicroVM/internal/run"
)

// What the daemon remembers of runs, for `sandboxctl ps` and `logs`. It is
// all in memory and gone when the daemon restarts (the audit log is what
// lasts). Two kinds of limit keep it bounded (threat T6):
//
//   - the number of finished runs: the newest maxRecent stay, older ones
//     are forgotten;
//   - their output: each run keeps at most the first perRunOutput bytes
//     of what the host passed on (a run's own cap may be up to 16 MiB),
//     and all runs together at most outputBudget; past that, the oldest
//     runs lose their output first, but keep everything else.
const (
	maxRecent    = 100
	perRunOutput = 1 << 20  // 1 MiB
	outputBudget = 64 << 20 // 64 MiB
)

// runEntry is one run the daemon knows about, active or finished.
type runEntry struct {
	id        string
	argv      []string
	class     string
	mode      daemonv1.Mode
	callerUID uint32
	machine   *run.Machine
	events    *eventQueue
	cancel    func(error) // stops the run, with a reason

	// Guarded by the registry's mu.
	out       output
	result    *daemonv1.RunResult // set once the run has ended
	vm        VM                  // the restored clone, until it is destroyed
	jailedUID uint32              // jailer mode, once the run has a slot
}

// output is what the daemon kept of a run's output.
type output struct {
	chunks  []*daemonv1.OutputChunk
	kept    uint64 // bytes in chunks
	total   uint64 // bytes the host passed on in all
	cut     bool   // more than perRunOutput arrived
	dropped bool   // dropped to stay within the budget
}

// registry holds every active run and the recent finished ones.
type registry struct {
	perRun, budget uint64
	recent         int

	mu      sync.Mutex
	runs    map[string]*runEntry
	order   []*runEntry // oldest first
	kept    uint64      // output bytes kept, all runs together
	nFinish int         // finished runs in order
}

func newRegistry(perRun, budget uint64, recent int) *registry {
	return &registry{perRun: perRun, budget: budget, recent: recent, runs: make(map[string]*runEntry)}
}

// add starts tracking e.
func (r *registry) add(e *runEntry) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.runs[e.id] = e
	r.order = append(r.order, e)
}

// addOutput keeps (the part that fits of) one chunk of e's output.
func (r *registry) addOutput(e *runEntry, stderr bool, p []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	o := &e.out
	o.total += uint64(len(p))
	if o.dropped {
		return
	}
	if room := r.perRun - o.kept; uint64(len(p)) > room {
		p = p[:room]
		o.cut = true
	}
	if len(p) == 0 {
		return
	}
	chunk := &daemonv1.OutputChunk{Data: &daemonv1.OutputChunk_Stdout{Stdout: p}}
	if stderr {
		chunk = &daemonv1.OutputChunk{Data: &daemonv1.OutputChunk_Stderr{Stderr: p}}
	}
	o.chunks = append(o.chunks, chunk)
	o.kept += uint64(len(p))
	r.kept += uint64(len(p))
	// Over the budget: the oldest runs give up their output first.
	for _, old := range r.order {
		if r.kept <= r.budget {
			break
		}
		if old != e {
			r.drop(old)
		}
	}
}

// drop forgets e's output, but nothing else about it. mu is held.
func (r *registry) drop(e *runEntry) {
	if e.out.kept == 0 && e.out.chunks == nil {
		return
	}
	r.kept -= e.out.kept
	e.out.chunks, e.out.kept, e.out.dropped = nil, 0, true
}

// finish records e's result and forgets the oldest finished runs beyond
// the recent limit.
func (r *registry) finish(e *runEntry, res *daemonv1.RunResult) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e.result = res
	r.nFinish++
	for i := 0; r.nFinish > r.recent && i < len(r.order); {
		old := r.order[i]
		if old.result == nil { // still running: keep
			i++
			continue
		}
		r.kept -= old.out.kept
		delete(r.runs, old.id)
		r.order = slices.Delete(r.order, i, i+1)
		r.nFinish--
	}
}

// setVM records e's restored clone, or (nil) that it is being
// destroyed.
func (r *registry) setVM(e *runEntry, vm VM) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e.vm = vm
}

// setJailedUID records the UID e's clone runs as (jailer mode).
func (r *registry) setJailedUID(e *runEntry, uid uint32) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e.jailedUID = uid
}

// liveClone is one restored clone, for HostStats.
type liveClone struct {
	id        string
	pid       int
	jailedUID uint32
}

// live returns the runs that have a restored clone now, oldest first.
func (r *registry) live() []liveClone {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []liveClone
	for _, e := range r.order {
		if e.vm != nil {
			out = append(out, liveClone{id: e.id, pid: e.vm.Pid(), jailedUID: e.jailedUID})
		}
	}
	return out
}

// get returns the run called id.
func (r *registry) get(id string) (*runEntry, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.runs[id]
	return e, ok
}

// list returns the active runs, and with all the finished ones too,
// oldest first.
func (r *registry) list(all bool) []*runEntry {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []*runEntry
	for _, e := range r.order {
		if all || e.result == nil {
			out = append(out, e)
		}
	}
	return out
}

// info describes e for List and Get.
func (r *registry) info(e *runEntry) *daemonv1.RunInfo {
	r.mu.Lock()
	res, jailedUID := e.result, e.jailedUID
	r.mu.Unlock()
	trs := e.machine.Transitions()
	info := &daemonv1.RunInfo{
		RunId:     e.id,
		State:     stateToProto(trs[len(trs)-1].State),
		SizeClass: e.class,
		Mode:      e.mode,
		Argv:      e.argv,
		CallerUid: e.callerUID,
		CreatedAt: timestamp(e.machine.Created()),
		Result:    res,
		JailedUid: jailedUID,
	}
	for _, t := range trs {
		info.Transitions = append(info.Transitions, transitionToProto(e.id, t))
	}
	return info
}

// logs returns what the daemon kept of e's output.
func (r *registry) logs(e *runEntry) *daemonv1.LogsResponse {
	r.mu.Lock()
	defer r.mu.Unlock()
	o := e.out
	resp := &daemonv1.LogsResponse{
		RunId:       e.id,
		Chunks:      slices.Clone(o.chunks),
		OutputBytes: o.total,
		KeptBytes:   o.kept,
		Cut:         o.cut,
		Dropped:     o.dropped,
		Finished:    e.result != nil,
	}
	if e.result != nil {
		resp.OutputTruncated = e.result.GetOutputTruncated()
	}
	return resp
}

// keptBytes is how much output all runs together keep (for tests).
func (r *registry) keptBytes() uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.kept
}

// eventQueue carries a run's events from the run's goroutine to the Run
// call that streams them to the caller. Pushing never blocks: a caller
// that reads slowly can never hold up a run (its timeout still applies),
// and the queue stays bounded because the host caps a run's output.
type eventQueue struct {
	mu       sync.Mutex
	events   []*daemonv1.RunResponse
	closed   bool // the result is in: nothing more will come
	detached bool // nobody reads any more: drop new events
	wake     chan struct{}
}

func newEventQueue() *eventQueue { return &eventQueue{wake: make(chan struct{}, 1)} }

// push adds ev, unless the reader has gone.
func (q *eventQueue) push(ev *daemonv1.RunResponse) {
	q.mu.Lock()
	if !q.detached && !q.closed {
		q.events = append(q.events, ev)
	}
	q.mu.Unlock()
	q.signal()
}

// close says that the last event (the result) has been pushed.
func (q *eventQueue) close() {
	q.mu.Lock()
	q.closed = true
	q.mu.Unlock()
	q.signal()
}

// detach says that nobody will read again.
func (q *eventQueue) detach() {
	q.mu.Lock()
	q.detached, q.events = true, nil
	q.mu.Unlock()
}

func (q *eventQueue) signal() {
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

// take returns every event pushed so far, and whether that is all.
func (q *eventQueue) take() ([]*daemonv1.RunResponse, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	evs := q.events
	q.events = nil
	return evs, q.closed
}

// The conversions from internal/run to the API.

var stateProto = map[run.State]daemonv1.RunState{
	run.Pending:   daemonv1.RunState_RUN_STATE_PENDING,
	run.Restoring: daemonv1.RunState_RUN_STATE_RESTORING,
	run.Mounting:  daemonv1.RunState_RUN_STATE_MOUNTING,
	run.Running:   daemonv1.RunState_RUN_STATE_RUNNING,
	run.Stopping:  daemonv1.RunState_RUN_STATE_STOPPING,
	run.Done:      daemonv1.RunState_RUN_STATE_DONE,
	run.Failed:    daemonv1.RunState_RUN_STATE_FAILED,
}

func stateToProto(s run.State) daemonv1.RunState { return stateProto[s] }

var outcomeProto = map[run.Outcome]daemonv1.Outcome{
	run.Exited:          daemonv1.Outcome_OUTCOME_EXITED,
	run.TimedOut:        daemonv1.Outcome_OUTCOME_TIMED_OUT,
	run.OutputTruncated: daemonv1.Outcome_OUTCOME_OUTPUT_TRUNCATED,
	run.FailedOutcome:   daemonv1.Outcome_OUTCOME_FAILED,
}

func outcomeToProto(o run.Outcome) daemonv1.Outcome { return outcomeProto[o] }

func transitionToProto(id string, t run.Transition) *daemonv1.StateChange {
	return &daemonv1.StateChange{State: stateToProto(t.State), At: timestamp(t.At), Detail: t.Detail, RunId: id}
}

// micros turns a duration into whole microseconds (never negative).
func micros(d time.Duration) uint64 { return uint64(max(d.Microseconds(), 0)) }
