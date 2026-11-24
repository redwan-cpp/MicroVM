package audit

import "time"

// The kinds of record.
const (
	// KindRun is one finished run (Record.Run).
	KindRun = "run"
	// KindRotation starts a new chain after an old one was moved aside
	// (Record.Rotation).
	KindRotation = "rotation"
)

// Record is one line's record. Its JSON field names are a file format:
// never rename them.
type Record struct {
	// Kind is KindRun or KindRotation; the field of that name is set.
	Kind string `json:"kind"`
	// Time is when the record was written (UTC).
	Time time.Time `json:"time"`
	// Run is set for KindRun.
	Run *Run `json:"run,omitempty"`
	// Rotation is set for KindRotation.
	Rotation *Rotation `json:"rotation,omitempty"`
}

// Run is the record of one run. It is written in the run's STOPPING
// state, after its clone was destroyed and before DONE (or FAILED) is
// reported to anyone. It holds the command line, but never the values of
// the environment variables or the stdin the run was given.
type Run struct {
	// RunID names the run.
	RunID string `json:"run_id"`
	// Argv is the command.
	Argv []string `json:"argv"`
	// EnvNames are the names of the extra environment variables (their
	// values are not recorded).
	EnvNames []string `json:"env_names,omitempty"`
	// StdinBytes is how much stdin the run was given (its contents are
	// not recorded).
	StdinBytes int `json:"stdin_bytes"`
	// SizeClass is the snapshot's class, SnapshotVersion its version.
	SizeClass       string `json:"size_class"`
	SnapshotVersion string `json:"snapshot_version,omitempty"`
	// Mode is "rootless" or "jailer".
	Mode string `json:"mode"`
	// JailedUID is the UID the clone's Firecracker ran as, in jailer
	// mode only.
	JailedUID int `json:"jailed_uid,omitempty"`
	// CallerUID is the UID of the process that asked for the run, from
	// the socket (SO_PEERCRED).
	CallerUID uint32 `json:"caller_uid"`
	// DaemonVersion is the sandboxd build that ran it.
	DaemonVersion string `json:"daemon_version"`

	// Outcome is the host's verdict: EXITED, TIMED_OUT, OUTPUT_TRUNCATED
	// or FAILED.
	Outcome string `json:"outcome"`
	// FinalState is the state reported right after this record was
	// written: DONE or FAILED.
	FinalState string `json:"final_state"`
	// Exited is true only for EXITED; only then is ExitCode present.
	Exited   bool `json:"exited"`
	ExitCode *int `json:"exit_code,omitempty"`
	// Signal is the signal that killed the command, if one did.
	Signal int `json:"signal,omitempty"`
	// TimedOut and OutputTruncated are the host's own findings.
	TimedOut        bool `json:"timed_out"`
	OutputTruncated bool `json:"output_truncated"`
	// OutputBytes is how much output the host passed on.
	OutputBytes uint64 `json:"output_bytes"`
	// Error says why the run failed, if it did.
	Error string `json:"error,omitempty"`
	// DestroyError says what Destroy could not clean up, if anything.
	DestroyError string `json:"destroy_error,omitempty"`
	// SlotRetired is true if, because of that, the run's slot (and in
	// jailer mode its UID) is not used again until the daemon restarts.
	SlotRetired bool `json:"slot_retired,omitempty"`

	// Transitions are every state change up to STOPPING.
	Transitions []Transition `json:"transitions"`
	// TimingsUS are the run's phases, in microseconds.
	TimingsUS Timings `json:"timings_us"`
}

// Transition is one state change of a run.
type Transition struct {
	// State is the state entered, for example "RUNNING".
	State string `json:"state"`
	// At is when, by the host's wall clock (UTC).
	At time.Time `json:"at"`
	// SinceUS is how long after the run was accepted, in microseconds,
	// by the host's monotonic clock.
	SinceUS int64 `json:"since_us"`
	// Detail says why, for example an error.
	Detail string `json:"detail,omitempty"`
}

// Timings are a run's phases in microseconds; see the daemon API's
// Timings message for what each one covers.
type Timings struct {
	Queued       uint64 `json:"queued"`
	Prepare      uint64 `json:"prepare"`
	Scratch      uint64 `json:"scratch"`
	ProcessStart uint64 `json:"process_start"`
	LoadSnapshot uint64 `json:"load_snapshot"`
	AgentReady   uint64 `json:"agent_ready"`
	DeTwin       uint64 `json:"de_twin"`
	Restore      uint64 `json:"restore"`
	Mount        uint64 `json:"mount"`
	Exec         uint64 `json:"exec"`
	Destroy      uint64 `json:"destroy"`
	Total        uint64 `json:"total"`
}

// Rotation is the first record of a chain that replaced a broken (or
// retired) one.
type Rotation struct {
	// PreviousFile is the old log's new name, in the same folder.
	PreviousFile string `json:"previous_file"`
	// Reason says why: the old chain's verification error, or that it was
	// rotated by hand.
	Reason string `json:"reason"`
	// VerifiedRecords is how many of the old log's records verified.
	VerifiedRecords int `json:"verified_records"`
	// LastGoodHash is the hash of the old log's last good record (64
	// zeros if there was none): it ties the new chain to the old one.
	LastGoodHash string `json:"last_good_hash"`
}
