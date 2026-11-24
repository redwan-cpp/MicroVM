package run

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/redwan-cpp/MicroVM/internal/agentclient"
)

var allStates = []State{Pending, Restoring, Mounting, Running, Stopping, Done, Failed}

// TestAllowedTable spells out every pair of states: the whole state
// machine in one table. Anything not listed as allowed is a bug.
func TestAllowedTable(t *testing.T) {
	allowed := map[[2]State]bool{
		{Pending, Restoring}: true, {Pending, Stopping}: true, {Pending, Failed}: true,
		{Restoring, Mounting}: true, {Restoring, Stopping}: true, {Restoring, Failed}: true,
		{Mounting, Running}: true, {Mounting, Stopping}: true, {Mounting, Failed}: true,
		{Running, Stopping}: true, {Running, Failed}: true,
		{Stopping, Done}: true, {Stopping, Failed}: true,
	}
	for _, from := range allStates {
		for _, to := range allStates {
			if got, want := Allowed(from, to), allowed[[2]State{from, to}]; got != want {
				t.Errorf("Allowed(%s, %s) = %v, want %v", from, to, got, want)
			}
		}
	}
	for _, s := range allStates {
		if s.Terminal() != (s == Done || s == Failed) {
			t.Errorf("%s.Terminal() = %v", s, s.Terminal())
		}
		if s.Terminal() && len(next[s]) != 0 {
			t.Errorf("terminal state %s has next states %v", s, next[s])
		}
	}
	// The zero value is not a state, and nothing leads to or from it.
	if Allowed(0, Pending) || Allowed(Pending, 0) {
		t.Error("the zero State takes part in a transition")
	}
}

func TestStateNames(t *testing.T) {
	want := []string{"PENDING", "RESTORING", "MOUNTING", "RUNNING", "STOPPING", "DONE", "FAILED"}
	for i, s := range allStates {
		if s.String() != want[i] {
			t.Errorf("%d.String() = %q, want %q", int(s), s.String(), want[i])
		}
	}
	if got := State(42).String(); got != "State(42)" {
		t.Errorf("State(42).String() = %q", got)
	}
}

func TestMachineRecordsEveryStepInOrder(t *testing.T) {
	var seen []State
	m := NewMachine(func(tr Transition) { seen = append(seen, tr.State) })
	if m.State() != Pending {
		t.Fatalf("a new run is %s, want PENDING", m.State())
	}
	path := []State{Restoring, Mounting, Running, Stopping, Done}
	for _, s := range path {
		time.Sleep(time.Millisecond)
		if err := m.To(s, "step "+s.String()); err != nil {
			t.Fatalf("To(%s): %v", s, err)
		}
	}
	trs := m.Transitions()
	if len(trs) != len(path)+1 || len(seen) != len(trs) {
		t.Fatalf("%d transitions, %d seen, want %d", len(trs), len(seen), len(path)+1)
	}
	for i, tr := range trs {
		if seen[i] != tr.State {
			t.Errorf("onChange saw %s at %d, the record says %s", seen[i], i, tr.State)
		}
		if i == 0 {
			if tr.Since != 0 || !tr.At.Equal(m.Created()) {
				t.Errorf("first transition %+v, want PENDING at the creation time", tr)
			}
			continue
		}
		prev := trs[i-1]
		if tr.Since <= prev.Since || tr.At.Before(prev.At) {
			t.Errorf("transition %d (%s) is not after %s: %v vs %v", i, tr.State, prev.State, tr.Since, prev.Since)
		}
		if tr.Detail != "step "+tr.State.String() {
			t.Errorf("transition %d detail %q", i, tr.Detail)
		}
	}
	if at, ok := m.Entered(Running); !ok || !at.Equal(trs[3].At) {
		t.Errorf("Entered(RUNNING) = %v, %v", at, ok)
	}
	if _, ok := m.Entered(Failed); ok {
		t.Error("Entered(FAILED) is true for a run that never failed")
	}
}

func TestMachineRefusesIllegalSteps(t *testing.T) {
	for _, tc := range []struct {
		name string
		path []State // legal steps first
		bad  State
	}{
		{"skip restoring", nil, Mounting},
		{"backwards", []State{Restoring, Mounting}, Restoring},
		{"to pending", nil, Pending},
		{"done without stopping", []State{Restoring, Mounting, Running}, Done},
		{"after done", []State{Stopping, Done}, Failed},
		{"after failed", []State{Failed}, Stopping},
		{"twice", []State{Restoring}, Restoring},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := NewMachine(nil)
			for _, s := range tc.path {
				if err := m.To(s, ""); err != nil {
					t.Fatalf("To(%s): %v", s, err)
				}
			}
			before := m.Transitions()
			if err := m.To(tc.bad, ""); !errors.Is(err, ErrIllegal) {
				t.Errorf("To(%s) from %s = %v, want ErrIllegal", tc.bad, m.State(), err)
			}
			if after := m.Transitions(); len(after) != len(before) {
				t.Errorf("a refused step changed the record: %d → %d transitions", len(before), len(after))
			}
		})
	}
}

func TestMachineIsSafeForConcurrentReaders(t *testing.T) {
	m := NewMachine(nil)
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			for range 1000 {
				_ = m.State()
				_ = m.Transitions()
			}
		})
	}
	for _, s := range []State{Restoring, Mounting, Running, Stopping, Done} {
		if err := m.To(s, ""); err != nil {
			t.Error(err)
		}
	}
	wg.Wait()
}

func TestJudge(t *testing.T) {
	boom := errors.New("boom")
	for _, tc := range []struct {
		name string
		res  agentclient.Result
		err  error
		want Verdict
	}{
		{
			name: "exit 0",
			res:  agentclient.Result{Exited: true, OutputBytes: 3},
			want: Verdict{Outcome: Exited, Exited: true, OutputBytes: 3},
		},
		{
			name: "exit 7",
			res:  agentclient.Result{Exited: true, ExitCode: 7},
			want: Verdict{Outcome: Exited, Exited: true, ExitCode: 7},
		},
		{
			name: "killed by a signal in the guest",
			res:  agentclient.Result{Exited: true, ExitCode: -1, Signal: 9},
			want: Verdict{Outcome: Exited, Exited: true, ExitCode: -1, Signal: 9},
		},
		{
			name: "the host's deadline",
			res:  agentclient.Result{TimedOut: true},
			want: Verdict{Outcome: TimedOut, TimedOut: true},
		},
		{
			// The exit status came after the timeout: it is not believed.
			name: "a late exit is still a timeout",
			res:  agentclient.Result{Exited: true, ExitCode: 0, TimedOut: true},
			want: Verdict{Outcome: TimedOut, TimedOut: true},
		},
		{
			name: "the output cap",
			res:  agentclient.Result{OutputTruncated: true, OutputBytes: 100},
			want: Verdict{Outcome: OutputTruncated, OutputTruncated: true, OutputBytes: 100},
		},
		{
			// Output exactly at the cap counts as truncated, exit or not.
			name: "an exit at the cap is still truncated",
			res:  agentclient.Result{Exited: true, OutputTruncated: true, OutputBytes: 100},
			want: Verdict{Outcome: OutputTruncated, OutputTruncated: true, OutputBytes: 100},
		},
		{
			name: "both limits: the timeout wins",
			res:  agentclient.Result{TimedOut: true, OutputTruncated: true},
			want: Verdict{Outcome: TimedOut, TimedOut: true, OutputTruncated: true},
		},
		{
			// What the agent claims never counts.
			name: "the agent's claims are ignored",
			res:  agentclient.Result{Exited: true, AgentReportedTimedOut: true, AgentReportedTruncated: true},
			want: Verdict{Outcome: Exited, Exited: true},
		},
		{
			name: "an error",
			res:  agentclient.Result{Exited: true},
			err:  boom,
			want: Verdict{Outcome: FailedOutcome, Err: boom},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := Judge(tc.res, tc.err)
			if got != tc.want {
				t.Errorf("Judge = %+v, want %+v", got, tc.want)
			}
			if got.Exited != (got.Outcome == Exited) {
				t.Errorf("Exited is %v for %s", got.Exited, got.Outcome)
			}
		})
	}

	// A result that says nothing is not a success.
	if v := Judge(agentclient.Result{}, nil); v.Outcome != FailedOutcome || v.Err == nil {
		t.Errorf("Judge(empty result) = %+v, want FAILED with an error", v)
	}
}

func TestOutcomeNames(t *testing.T) {
	for o, want := range map[Outcome]string{
		Exited: "EXITED", TimedOut: "TIMED_OUT", OutputTruncated: "OUTPUT_TRUNCATED", FailedOutcome: "FAILED", 0: "Outcome(0)",
	} {
		if o.String() != want {
			t.Errorf("%d.String() = %q, want %q", int(o), o.String(), want)
		}
	}
}

func TestFailure(t *testing.T) {
	boom := errors.New("boom")
	if v := Failure(boom); v.Outcome != FailedOutcome || !errors.Is(v.Err, boom) || v.Exited {
		t.Errorf("Failure = %+v", v)
	}
}
