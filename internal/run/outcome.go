package run

import (
	"fmt"

	"github.com/redwan-cpp/MicroVM/internal/agentclient"
)

// Outcome is how a run ended, as the host judges it.
type Outcome int

// The outcomes. The zero value means "not ended yet".
const (
	// Exited means the command exited by itself, or a signal inside the
	// guest killed it; Verdict.ExitCode says how.
	Exited Outcome = iota + 1
	// TimedOut means the command ran out of time and was stopped.
	TimedOut
	// OutputTruncated means the output reached its cap and the command
	// was stopped.
	OutputTruncated
	// FailedOutcome means the run could not be carried out (Verdict.Err
	// says why). It is named so as not to clash with the state Failed.
	FailedOutcome
)

var outcomeNames = map[Outcome]string{
	Exited:          "EXITED",
	TimedOut:        "TIMED_OUT",
	OutputTruncated: "OUTPUT_TRUNCATED",
	FailedOutcome:   "FAILED",
}

// String returns the outcome's name, for example "TIMED_OUT", as in the
// API and the audit log.
func (o Outcome) String() string {
	if name, ok := outcomeNames[o]; ok {
		return name
	}
	return fmt.Sprintf("Outcome(%d)", int(o))
}

// Verdict is the host's judgement of how a command ended.
type Verdict struct {
	// Outcome is the verdict itself.
	Outcome Outcome
	// Exited is true only for the outcome Exited. Only then do ExitCode
	// and Signal mean anything.
	Exited bool
	// ExitCode is 0 to 255, or -1 if a signal killed the command.
	ExitCode int
	// Signal is the signal that killed the command, or 0.
	Signal int
	// TimedOut and OutputTruncated are the host's own findings
	// (agentclient.Result), whatever the outcome.
	TimedOut        bool
	OutputTruncated bool
	// OutputBytes is how much output the host passed on.
	OutputBytes uint64
	// Err is why the outcome is FailedOutcome, or nil.
	Err error
}

// Judge turns what agentclient.Client.Exec returned into a verdict. It
// uses only what the host decided itself (agentclient.Result's first
// fields), never what the agent claims (the AgentReported fields).
//
// The order matters. A command that ran out of time is TimedOut, even if
// an exit status arrived late; one whose output reached the cap is
// OutputTruncated, even if it then exited: in both cases the host stopped
// listening, so its exit status says nothing reliable, and the verdict
// carries no exit code. Only a command that ended within both limits is
// Exited.
func Judge(res agentclient.Result, err error) Verdict {
	v := Verdict{TimedOut: res.TimedOut, OutputTruncated: res.OutputTruncated, OutputBytes: res.OutputBytes}
	switch {
	case err != nil:
		v.Outcome, v.Err = FailedOutcome, err
	case res.TimedOut:
		v.Outcome = TimedOut
	case res.OutputTruncated:
		v.Outcome = OutputTruncated
	case res.Exited:
		v.Outcome, v.Exited, v.ExitCode, v.Signal = Exited, true, res.ExitCode, res.Signal
	default:
		// agentclient never returns this; a verdict must not be "fine" by
		// accident.
		v.Outcome, v.Err = FailedOutcome, fmt.Errorf("run: the command neither exited nor hit a limit: %+v", res)
	}
	return v
}

// Failure is the verdict for a run that failed before (or instead of)
// running its command.
func Failure(err error) Verdict { return Verdict{Outcome: FailedOutcome, Err: err} }
