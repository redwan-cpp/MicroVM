package ui

import (
	"fmt"
	"strings"
	"time"

	"github.com/redwan-cpp/MicroVM/internal/audit"
	daemonv1 "github.com/redwan-cpp/MicroVM/internal/gen/sandbox/daemon/v1"
)

// The events are the one format between the UI server and the page: each
// one is a JSON object on one SSE "data:" line, and the recorder writes
// exactly these objects into a session file, so the replay player feeds
// the page the same events the live page got. Only the member that Type
// names is set. The field names are a file format (session files):
// never rename them.

// The event types.
const (
	TypeInfo     = "info"     // Event.Info: the daemon described itself
	TypeScenario = "scenario" // Event.Scenario: a scenario starts
	TypeState    = "state"    // Event.State: one clone's run changed state
	TypeOutput   = "output"   // Event.Output: a clone's command wrote output
	TypeResult   = "result"   // Event.Result: one clone's run ended
	TypeEnd      = "end"      // Event.End: the scenario ended
	TypeHost     = "host"     // Event.Host: what sandboxd's clones use now
	TypeAudit    = "audit"    // Event.Audit: the newest audit records
	TypeVerify   = "verify"   // Event.Verify: the audit chain was verified
	TypeHistory  = "history"  // Event.History: the runs sandboxd remembers
	TypeNotice   = "notice"   // Event.Notice: a message for the presenter
)

// Event is one thing the page learns.
type Event struct {
	Type     string         `json:"type"`
	Info     *InfoEvent     `json:"info,omitempty"`
	Scenario *ScenarioEvent `json:"scenario,omitempty"`
	State    *StateEvent    `json:"state,omitempty"`
	Output   *OutputEvent   `json:"output,omitempty"`
	Result   *ResultEvent   `json:"result,omitempty"`
	End      *EndEvent      `json:"end,omitempty"`
	Host     *HostEvent     `json:"host,omitempty"`
	Audit    *AuditEvent    `json:"audit,omitempty"`
	Verify   *VerifyEvent   `json:"verify,omitempty"`
	History  *HistoryEvent  `json:"history,omitempty"`
	Notice   *NoticeEvent   `json:"notice,omitempty"`
}

// InfoEvent describes the daemon (its Info call) and the UI.
type InfoEvent struct {
	// Mode is "rootless" or "jailer".
	Mode               string      `json:"mode"`
	DaemonVersion      string      `json:"daemon_version"`
	FirecrackerVersion string      `json:"firecracker_version"`
	Arch               string      `json:"arch"`
	MaxConcurrent      int         `json:"max_concurrent"`
	MaxPending         int         `json:"max_pending"`
	HostKind           string      `json:"host_kind"` // "physical", "virtual" or "unknown"
	HostDetail         string      `json:"host_detail"`
	Classes            []ClassInfo `json:"classes"`
	// MaxClones is how many clones one scenario may ask for here: the
	// daemon's max_concurrent, at most 8.
	MaxClones int `json:"max_clones"`
	// UIVersion is the sandboxctl build that served the page.
	UIVersion string `json:"ui_version"`
}

// ClassInfo is one size class with a snapshot.
type ClassInfo struct {
	Name            string    `json:"name"`
	VCPUs           int       `json:"vcpus"`
	MemoryMiB       int       `json:"memory_mib"`
	SnapshotVersion string    `json:"snapshot_version"`
	SnapshotCreated time.Time `json:"snapshot_created"`
	KernelVersion   string    `json:"kernel_version"`
	AgentVersion    string    `json:"agent_version"`
	VMGenID         bool      `json:"vmgenid"`
	// ColdBootUS is the snapshot's measured cold boot (manifest).
	ColdBootUS uint64 `json:"cold_boot_us"`
}

// ScenarioEvent starts one scenario: Clones runs at once, lane 0 to
// Clones-1.
type ScenarioEvent struct {
	// Seg numbers the scenarios of a session from 1.
	Seg       int    `json:"seg"`
	ID        string `json:"id"`
	Title     string `json:"title"`
	Caption   string `json:"caption"`
	Clones    int    `json:"clones"`
	Class     string `json:"class"`
	TimeoutMS uint32 `json:"timeout_ms"`
	MaxOutput uint64 `json:"max_output"`
	// Argv is each lane's command.
	Argv [][]string `json:"argv"`
}

// StateEvent is one state change of one lane's run.
type StateEvent struct {
	Seg   int    `json:"seg"`
	Lane  int    `json:"lane"`
	RunID string `json:"run_id"`
	State string `json:"state"` // "PENDING" ... "DONE", "FAILED"
	// MS is when, in milliseconds after the scenario started, by the
	// daemon's timestamp (the host's clock, as the UI's).
	MS     float64 `json:"ms"`
	Detail string  `json:"detail,omitempty"`
}

// OutputEvent is output of one lane's command, as the guest wrote it
// (made valid UTF-8; the page removes control characters when it shows
// it).
type OutputEvent struct {
	Seg    int    `json:"seg"`
	Lane   int    `json:"lane"`
	Stream string `json:"stream"` // "stdout" or "stderr"
	Data   string `json:"data"`
	// Gap is true when output before this was left out (a page that
	// connected late gets only the newest output of each lane).
	Gap bool `json:"gap,omitempty"`
}

// ResultEvent is how one lane's run ended, as sandboxd judged it.
type ResultEvent struct {
	Seg         int       `json:"seg"`
	Lane        int       `json:"lane"`
	RunID       string    `json:"run_id"`
	Outcome     string    `json:"outcome"` // "EXITED", "TIMED_OUT", "OUTPUT_TRUNCATED" or "FAILED"
	Exited      bool      `json:"exited"`
	ExitCode    int32     `json:"exit_code"`
	Signal      int32     `json:"signal"`
	TimedOut    bool      `json:"timed_out"`
	Truncated   bool      `json:"truncated"`
	OutputBytes uint64    `json:"output_bytes"`
	Mode        string    `json:"mode"`
	Error       string    `json:"error,omitempty"`
	TimingsUS   TimingsUS `json:"timings_us"`
}

// TimingsUS are a run's phases in microseconds (the daemon's Timings).
type TimingsUS struct {
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

// EndEvent ends a scenario.
type EndEvent struct {
	Seg int `json:"seg"`
	// Audit is each lane's audit record, found in the log after the run:
	// its hash can be checked with sandboxctl audit verify on the
	// machine that made the recording.
	Audit []AuditRef `json:"audit"`
	// Error says why the scenario ended early, if it did.
	Error string `json:"error,omitempty"`
}

// AuditRef is one lane's audit record.
type AuditRef struct {
	Lane     int    `json:"lane"`
	RunID    string `json:"run_id"`
	Hash     string `json:"hash,omitempty"`
	PrevHash string `json:"prev_hash,omitempty"`
	// Missing says why the record was not found (Hash is then empty).
	Missing string `json:"missing,omitempty"`
}

// HostEvent is what sandboxd says its clones use now (HostStats).
type HostEvent struct {
	Clones    []CloneStat `json:"clones"`
	Processes Count       `json:"processes"`
	RunDirs   Count       `json:"run_dirs"`
	JailDirs  Count       `json:"jail_dirs"`
	Cgroups   Count       `json:"cgroups"`
	// Error says why sandboxd could not be asked; the rest is then empty.
	Error string `json:"error,omitempty"`
}

// CloneStat is one live clone's Firecracker.
type CloneStat struct {
	RunID       string `json:"run_id"`
	PID         int32  `json:"pid"`
	JailedUID   uint32 `json:"jailed_uid,omitempty"`
	RSS         uint64 `json:"rss"`
	PSS         uint64 `json:"pss"`
	MemoryError string `json:"memory_error,omitempty"`
}

// Count is a number sandboxd counted, or why it could not.
type Count struct {
	N     uint32 `json:"n"`
	Error string `json:"error,omitempty"`
}

// AuditEvent is the newest records of the audit log, oldest first.
type AuditEvent struct {
	Records []AuditRecord `json:"records"`
	Error   string        `json:"error,omitempty"`
}

// AuditRecord is one line of the audit log.
type AuditRecord struct {
	Kind       string    `json:"kind"`
	Time       time.Time `json:"time"`
	RunID      string    `json:"run_id,omitempty"`
	Outcome    string    `json:"outcome,omitempty"`
	FinalState string    `json:"final_state,omitempty"`
	PrevHash   string    `json:"prev_hash"`
	Hash       string    `json:"hash"`
}

// VerifyEvent is the result of verifying the whole audit chain.
type VerifyEvent struct {
	OK      bool   `json:"ok"`
	Records int    `json:"records"`
	Head    string `json:"head,omitempty"`
	// Line and Reason name the first broken line, if it is broken.
	Line   int    `json:"line,omitempty"`
	Reason string `json:"reason,omitempty"`
}

// HistoryEvent lists the runs sandboxd remembers, oldest first.
type HistoryEvent struct {
	Runs  []HistoryRun `json:"runs"`
	Error string       `json:"error,omitempty"`
}

// HistoryRun is one run, as `sandboxctl ps` shows it.
type HistoryRun struct {
	RunID     string    `json:"run_id"`
	State     string    `json:"state"`
	Class     string    `json:"class"`
	Mode      string    `json:"mode"`
	CallerUID uint32    `json:"caller_uid"`
	JailedUID uint32    `json:"jailed_uid,omitempty"`
	Created   time.Time `json:"created"`
	Outcome   string    `json:"outcome"`
	Command   string    `json:"command"`
}

// NoticeEvent is a message for the presenter.
type NoticeEvent struct {
	Level string `json:"level"` // "info" or "warn"
	Text  string `json:"text"`
}

// The conversions from the daemon's API.

func stateName(s daemonv1.RunState) string { return strings.TrimPrefix(s.String(), "RUN_STATE_") }

func modeName(m daemonv1.Mode) string {
	return strings.ToLower(strings.TrimPrefix(m.String(), "MODE_"))
}

func outcomeName(o daemonv1.Outcome) string { return strings.TrimPrefix(o.String(), "OUTCOME_") }

func hostKindName(k daemonv1.HostKind) string {
	switch k {
	case daemonv1.HostKind_HOST_KIND_PHYSICAL:
		return "physical"
	case daemonv1.HostKind_HOST_KIND_VIRTUAL:
		return "virtual"
	}
	return "unknown"
}

// maxClones is the most clones one scenario may ask for.
const maxClones = 8

// infoEvent converts the daemon's Info.
func infoEvent(r *daemonv1.InfoResponse, uiVersion string) *InfoEvent {
	e := &InfoEvent{
		Mode:               modeName(r.GetMode()),
		DaemonVersion:      clean(r.GetDaemonVersion(), 200),
		FirecrackerVersion: clean(r.GetFirecrackerVersion(), 64),
		Arch:               clean(r.GetArch(), 32),
		MaxConcurrent:      int(r.GetMaxConcurrent()),
		MaxPending:         int(r.GetMaxPending()),
		HostKind:           hostKindName(r.GetHostKind()),
		HostDetail:         clean(r.GetHostDetail(), 200),
		MaxClones:          min(int(r.GetMaxConcurrent()), maxClones),
		UIVersion:          uiVersion,
		Classes:            []ClassInfo{},
	}
	for _, c := range r.GetClasses() {
		e.Classes = append(e.Classes, ClassInfo{
			Name:            clean(c.GetName(), 32),
			VCPUs:           int(c.GetVcpus()),
			MemoryMiB:       int(c.GetMemoryMib()),
			SnapshotVersion: clean(c.GetSnapshotVersion(), 64),
			SnapshotCreated: c.GetSnapshotCreatedAt().AsTime().UTC(),
			KernelVersion:   clean(c.GetKernelVersion(), 64),
			AgentVersion:    clean(c.GetAgentVersion(), 200),
			VMGenID:         c.GetVmgenid(),
			ColdBootUS:      c.GetColdBootUs(),
		})
	}
	return e
}

// resultEvent converts a run's result.
func resultEvent(seg, lane int, r *daemonv1.RunResult, scrub func(string) string) *ResultEvent {
	t := r.GetTimings()
	return &ResultEvent{
		Seg: seg, Lane: lane,
		RunID:       r.GetRunId(),
		Outcome:     outcomeName(r.GetOutcome()),
		Exited:      r.GetExited(),
		ExitCode:    r.GetExitCode(),
		Signal:      r.GetSignal(),
		TimedOut:    r.GetTimedOut(),
		Truncated:   r.GetOutputTruncated(),
		OutputBytes: r.GetOutputBytes(),
		Mode:        modeName(r.GetMode()),
		Error:       scrub(r.GetError()),
		TimingsUS: TimingsUS{
			Queued: t.GetQueuedUs(), Prepare: t.GetPrepareUs(), Scratch: t.GetScratchUs(),
			ProcessStart: t.GetProcessStartUs(), LoadSnapshot: t.GetLoadSnapshotUs(), AgentReady: t.GetAgentReadyUs(),
			DeTwin: t.GetDeTwinUs(), Restore: t.GetRestoreUs(), Mount: t.GetMountUs(), Exec: t.GetExecUs(),
			Destroy: t.GetDestroyUs(), Total: t.GetTotalUs(),
		},
	}
}

// hostEvent converts HostStats.
func hostEvent(r *daemonv1.HostStatsResponse, scrub func(string) string) *HostEvent {
	count := func(c *daemonv1.Count) Count { return Count{N: c.GetN(), Error: scrub(c.GetError())} }
	e := &HostEvent{
		Clones:    []CloneStat{},
		Processes: count(r.GetFirecrackerProcesses()),
		RunDirs:   count(r.GetRunDirs()),
		JailDirs:  count(r.GetJailDirs()),
		Cgroups:   count(r.GetCgroups()),
	}
	for _, c := range r.GetClones() {
		e.Clones = append(e.Clones, CloneStat{
			RunID: c.GetRunId(), PID: c.GetPid(), JailedUID: c.GetJailedUid(),
			RSS: c.GetRssBytes(), PSS: c.GetPssBytes(), MemoryError: scrub(c.GetMemoryError()),
		})
	}
	return e
}

// historyEvent converts List.
func historyEvent(r *daemonv1.ListResponse) *HistoryEvent {
	e := &HistoryEvent{Runs: []HistoryRun{}}
	for _, run := range r.GetRuns() {
		outcome := "-"
		if res := run.GetResult(); res != nil {
			outcome = outcomeName(res.GetOutcome())
			if res.GetOutcome() == daemonv1.Outcome_OUTCOME_EXITED {
				outcome = fmt.Sprintf("EXITED %d", res.GetExitCode())
			}
		}
		e.Runs = append(e.Runs, HistoryRun{
			RunID: run.GetRunId(), State: stateName(run.GetState()), Class: clean(run.GetSizeClass(), 32),
			Mode: modeName(run.GetMode()), CallerUID: run.GetCallerUid(), JailedUID: run.GetJailedUid(),
			Created: run.GetCreatedAt().AsTime().UTC(), Outcome: outcome,
			Command: clean(strings.Join(run.GetArgv(), " "), 160),
		})
	}
	return e
}

// auditEvent converts the newest audit lines.
func auditEvent(entries []audit.Entry) *AuditEvent {
	e := &AuditEvent{Records: []AuditRecord{}}
	for _, a := range entries {
		e.Records = append(e.Records, AuditRecord{
			Kind: clean(a.Kind, 32), Time: a.Time.UTC(), RunID: clean(a.RunID, 32), Outcome: clean(a.Outcome, 32),
			FinalState: clean(a.FinalState, 32), PrevHash: clean(a.PrevHash, 64), Hash: clean(a.Hash, 64),
		})
	}
	return e
}
