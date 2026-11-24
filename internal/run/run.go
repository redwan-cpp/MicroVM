// Package run is the life of one run, as a state machine: which states a
// run may be in, which steps between them are allowed, when each step
// happened, and how the host judges the way a run ended.
//
//	PENDING → RESTORING → MOUNTING → RUNNING → STOPPING → DONE
//	   └──────────┴───────────┴──────────┴──────────┴────→ FAILED
//
// A run only moves forward. From any state before the end it may go to
// STOPPING (something went wrong, or the caller went away) or straight to
// FAILED, and every run that sandboxd carries out ends with STOPPING (the
// clone is destroyed and the audit record written) and then DONE or
// FAILED. Any other step is a bug in the caller, and To refuses it.
//
// Nothing here starts a VM: internal/daemon drives the machine, and this
// package only records and checks. docs/learn/11-the-daemon-and-a-run.md
// explains the whole run.
package run

import (
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"
)

// State is where a run is in its life.
type State int

// The states, in the order a run goes through them. The zero value is not
// a state.
const (
	// Pending means the run is accepted and waits for a free slot.
	Pending State = iota + 1
	// Restoring means a clone is being restored from the base snapshot.
	Restoring
	// Mounting means the clone's scratch disk is being mounted at /work.
	Mounting
	// Running means the command runs.
	Running
	// Stopping means the clone is being destroyed and the audit record
	// written.
	Stopping
	// Done means the run is over: the clone is gone and the record
	// written. The command may still have failed (see Outcome).
	Done
	// Failed means the run could not be carried out.
	Failed
)

var stateNames = map[State]string{
	Pending:   "PENDING",
	Restoring: "RESTORING",
	Mounting:  "MOUNTING",
	Running:   "RUNNING",
	Stopping:  "STOPPING",
	Done:      "DONE",
	Failed:    "FAILED",
}

// String returns the state's name in capitals, for example "PENDING", as
// in the API and the audit log.
func (s State) String() string {
	if name, ok := stateNames[s]; ok {
		return name
	}
	return fmt.Sprintf("State(%d)", int(s))
}

// Terminal reports whether s is an end state: nothing follows it.
func (s State) Terminal() bool { return s == Done || s == Failed }

// next lists the allowed steps from each state. DONE and FAILED have none.
var next = map[State][]State{
	Pending:   {Restoring, Stopping, Failed},
	Restoring: {Mounting, Stopping, Failed},
	Mounting:  {Running, Stopping, Failed},
	Running:   {Stopping, Failed},
	Stopping:  {Done, Failed},
}

// Allowed reports whether a run may go from one state straight to the
// other.
func Allowed(from, to State) bool { return slices.Contains(next[from], to) }

// ErrIllegal is the error To returns for a step the state machine does
// not allow. It is always a bug in the caller.
var ErrIllegal = errors.New("run: illegal state change")

// Transition is one step of a run.
type Transition struct {
	// State is the state the run entered.
	State State
	// At is when, by the host's clock. It is a time.Now() value, so it
	// also carries Go's monotonic clock reading.
	At time.Time
	// Since is how long after the run was created, from the monotonic
	// clock: it never jumps when someone sets the wall clock.
	Since time.Duration
	// Detail says why, for example an error.
	Detail string
}

// Machine is the state of one run. It is safe for concurrent use: the
// daemon's run goroutine moves it on while `sandboxctl ps` reads it.
type Machine struct {
	mu          sync.Mutex
	created     time.Time
	transitions []Transition // the first is always Pending
	onChange    func(Transition)
}

// NewMachine starts a run in PENDING, now. onChange, if not nil, is called
// with every transition, the first one included, while the machine is
// locked: it must be quick and must not call back into the machine. The
// daemon uses it to stream state changes to the caller.
func NewMachine(onChange func(Transition)) *Machine {
	now := time.Now()
	m := &Machine{created: now, onChange: onChange}
	m.record(Transition{State: Pending, At: now})
	return m
}

// To moves the run to state s, now, and records why. It refuses (with
// ErrIllegal, changing nothing) any step that Allowed does not allow.
func (m *Machine) To(s State, detail string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	from := m.transitions[len(m.transitions)-1].State
	if !Allowed(from, s) {
		return fmt.Errorf("%w: %s → %s", ErrIllegal, from, s)
	}
	now := time.Now()
	m.record(Transition{State: s, At: now, Since: now.Sub(m.created), Detail: detail})
	return nil
}

// record appends t and tells onChange. The caller holds mu (or, in
// NewMachine, is the only one who can see m).
func (m *Machine) record(t Transition) {
	m.transitions = append(m.transitions, t)
	if m.onChange != nil {
		m.onChange(t)
	}
}

// State is the run's current state.
func (m *Machine) State() State {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.transitions[len(m.transitions)-1].State
}

// Created is when the run was created (its PENDING transition).
func (m *Machine) Created() time.Time { return m.created }

// Transitions returns a copy of every transition so far, oldest first.
func (m *Machine) Transitions() []Transition {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.transitions)
}

// Entered returns when the run entered state s, and whether it did.
func (m *Machine) Entered(s State) (time.Time, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, t := range m.transitions {
		if t.State == s {
			return t.At, true
		}
	}
	return time.Time{}, false
}
