package ui

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"golang.org/x/sys/unix"
)

// A session file is a recording of everything one live page received:
// `sandboxctl ui --record FILE` writes it, `--replay FILE` and `ui
// export` play it back. It is JSON Lines, one object per line:
//
//	{"meta":{...}}                     first: when, which build, the daemon's Info
//	{"t":1234,"event":{...}}           every event, t milliseconds after the start
//	{"end":{...}}                      last: the final audit verify (missing if the recorder crashed)
//
// It is an allowlist: only the fields defined here exist, and a file
// with any other field is refused. It holds what the page showed (the
// scenarios, their commands, their output as captured, the timings, the
// daemon's Info, the audit records' hashes) and never the page's token,
// environment values, stdin or host paths (the UI sends no env or stdin,
// and removes host paths from the daemon's texts).

// The session format.
const (
	sessionFormat  = "microvm-sandbox-session"
	sessionVersion = 1
	// maxSession is the largest recording: past it, recording stops with
	// a notice on the page, and the file stays valid.
	maxSession = 32 << 20
	// endReserve is kept free for the end line.
	endReserve = 64 << 10
	// maxSessionLine is the longest line a session file may have (an
	// output event holds at most 64 KiB, escaped).
	maxSessionLine = 2 << 20
	// maxSessionSpan is the longest a session may last.
	maxSessionSpan = 7 * 24 * time.Hour
)

// Session is a loaded recording.
type Session struct {
	Meta   SessionMeta  `json:"meta"`
	Events []TimedEvent `json:"events"`
	// End is nil if the recording never finished (the UI crashed).
	End *SessionEnd `json:"end,omitempty"`
}

// SessionMeta is a recording's first line.
type SessionMeta struct {
	Format    string    `json:"format"`
	Version   int       `json:"version"`
	CreatedAt time.Time `json:"created_at"`
	UIVersion string    `json:"ui_version"`
	// Info is the daemon's Info when the recording began: the page's
	// header and the recording's banner come from it.
	Info *InfoEvent `json:"info"`
}

// TimedEvent is one recorded event.
type TimedEvent struct {
	// T is milliseconds after the recording began, by the UI's clock.
	T     int64  `json:"t"`
	Event *Event `json:"event"`
}

// SessionEnd is a recording's last line.
type SessionEnd struct {
	EndedAt time.Time `json:"ended_at"`
	// Verify is `sandboxctl audit verify` of the daemon's log at the end.
	Verify *VerifyEvent `json:"verify"`
	// Stopped says why recording stopped early, if it did.
	Stopped string `json:"stopped,omitempty"`
}

// sessionLine is one line of a session file: exactly one member is set.
type sessionLine struct {
	Meta  *SessionMeta `json:"meta,omitempty"`
	T     *int64       `json:"t,omitempty"`
	Event *Event       `json:"event,omitempty"`
	End   *SessionEnd  `json:"end,omitempty"`
}

// recorder writes a session file as events happen, so a crash loses at
// most the end line.
type recorder struct {
	f       *os.File
	w       *bufio.Writer
	start   time.Time
	written int64
	limit   int64
	stopped string // why recording stopped, if it did
}

// createRecorder creates path (mode 0600; never an existing file, never
// through a symlink) and writes the meta line.
func createRecorder(path string, meta SessionMeta, start time.Time) (*recorder, error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL|unix.O_NOFOLLOW, 0o600) //nolint:gosec // G304: the path the user gave
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return nil, fmt.Errorf("ui: %s exists already: a recording never overwrites a file; choose another name", path)
		}
		return nil, fmt.Errorf("ui: create the recording: %w", err)
	}
	r := &recorder{f: f, w: bufio.NewWriter(f), start: start, limit: maxSession}
	meta.Format, meta.Version = sessionFormat, sessionVersion
	if err := r.line(sessionLine{Meta: &meta}); err != nil {
		_ = f.Close()
		return nil, err
	}
	return r, nil
}

// line writes one line and flushes it.
func (r *recorder) line(l sessionLine) error {
	data, err := json.Marshal(l)
	if err != nil {
		return fmt.Errorf("ui: encode a session line: %w", err)
	}
	data = append(data, '\n')
	if _, err := r.w.Write(data); err != nil {
		return fmt.Errorf("ui: write the recording: %w", err)
	}
	if err := r.w.Flush(); err != nil {
		return fmt.Errorf("ui: write the recording: %w", err)
	}
	r.written += int64(len(data))
	return nil
}

// write records ev. If that stops the recording (it is full, or the
// file failed), it returns the notice every page must show; afterwards it
// records nothing more.
func (r *recorder) write(ev *Event) *Event {
	if r.stopped != "" {
		return nil
	}
	t := time.Since(r.start).Milliseconds()
	data, err := json.Marshal(sessionLine{T: &t, Event: ev})
	if err == nil && r.written+int64(len(data))+1 > r.limit-endReserve {
		r.stopped = fmt.Sprintf("the recording reached its limit of %d MiB, so it stopped here; the page goes on live", maxSession>>20)
	} else if err == nil {
		err = r.line(sessionLine{T: &t, Event: ev})
	}
	if err != nil {
		r.stopped = "the recording stopped: " + err.Error()
	}
	if r.stopped == "" {
		return nil
	}
	return &Event{Type: TypeNotice, Notice: &NoticeEvent{Level: "warn", Text: r.stopped}}
}

// close writes the end line and closes the file.
func (r *recorder) close(verify *VerifyEvent, now time.Time) error {
	err := r.line(sessionLine{End: &SessionEnd{EndedAt: now.UTC(), Verify: verify, Stopped: r.stopped}})
	if syncErr := r.f.Sync(); err == nil {
		err = syncErr
	}
	if closeErr := r.f.Close(); err == nil {
		err = closeErr
	}
	return err
}

// LoadSession reads and checks a session file.
func LoadSession(path string) (*Session, error) {
	f, err := os.Open(path) //nolint:gosec // G304: the path the user gave
	if err != nil {
		return nil, fmt.Errorf("ui: %w", err)
	}
	defer f.Close() //nolint:errcheck // read-only
	s, err := readSession(f)
	if err != nil {
		return nil, fmt.Errorf("ui: %s: %w", path, err)
	}
	return s, nil
}

// readSession reads a session file from r and checks every line.
func readSession(r io.Reader) (*Session, error) {
	br := bufio.NewReaderSize(io.LimitReader(r, maxSession+1), 64<<10)
	s := &Session{Events: []TimedEvent{}}
	var total int64
	lastT := int64(0)
	for n := 1; ; n++ {
		line, err := readSessionLine(br)
		if errors.Is(err, io.EOF) && len(line) == 0 {
			break
		}
		if err != nil && !errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("line %d: %w", n, err)
		}
		total += int64(len(line))
		if total > maxSession {
			return nil, fmt.Errorf("more than %d MiB: not a recording sandboxctl ui made", maxSession>>20)
		}
		var l sessionLine
		dec := json.NewDecoder(bytes.NewReader(line))
		dec.DisallowUnknownFields() // an allowlist: nothing else may be in it
		if err := dec.Decode(&l); err != nil {
			return nil, fmt.Errorf("line %d: %w", n, err)
		}
		if dec.More() {
			return nil, fmt.Errorf("line %d: more than one JSON value", n)
		}
		switch {
		case n == 1:
			if l.Meta == nil || l.T != nil || l.Event != nil || l.End != nil {
				return nil, errors.New("line 1 is not the session's meta line: not a recording sandboxctl ui made")
			}
			if l.Meta.Format != sessionFormat || l.Meta.Version != sessionVersion {
				return nil, fmt.Errorf("format %q version %d, this sandboxctl reads %q version %d",
					l.Meta.Format, l.Meta.Version, sessionFormat, sessionVersion)
			}
			if l.Meta.Info == nil {
				return nil, errors.New("the meta line has no daemon info")
			}
			s.Meta = *l.Meta
		case s.End != nil:
			return nil, fmt.Errorf("line %d comes after the end line", n)
		case l.End != nil:
			if l.Meta != nil || l.T != nil || l.Event != nil {
				return nil, fmt.Errorf("line %d: an end line with other members", n)
			}
			s.End = l.End
		case l.T != nil && l.Event != nil && l.Meta == nil:
			t := *l.T
			if t < lastT || t > maxSessionSpan.Milliseconds() {
				return nil, fmt.Errorf("line %d: time %d ms is out of order or too late", n, t)
			}
			if err := l.Event.check(); err != nil {
				return nil, fmt.Errorf("line %d: %w", n, err)
			}
			lastT = t
			s.Events = append(s.Events, TimedEvent{T: t, Event: l.Event})
		default:
			return nil, fmt.Errorf("line %d is neither an event nor the end", n)
		}
		if errors.Is(err, io.EOF) {
			break
		}
	}
	if s.Meta.Format == "" {
		return nil, errors.New("empty: not a recording sandboxctl ui made")
	}
	return s, nil
}

// readSessionLine reads one line of at most maxSessionLine bytes.
func readSessionLine(br *bufio.Reader) ([]byte, error) {
	var line []byte
	for {
		chunk, err := br.ReadSlice('\n')
		line = append(line, chunk...)
		if len(line) > maxSessionLine {
			return nil, fmt.Errorf("a line longer than %d bytes", maxSessionLine)
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		return line, err
	}
}

// check makes sure an event from a file is one the page knows: its type,
// exactly that member, and lanes and scenarios within bounds.
func (e *Event) check() error {
	members := map[string]bool{
		TypeInfo: e.Info != nil, TypeScenario: e.Scenario != nil, TypeState: e.State != nil, TypeOutput: e.Output != nil,
		TypeResult: e.Result != nil, TypeEnd: e.End != nil, TypeHost: e.Host != nil, TypeAudit: e.Audit != nil,
		TypeVerify: e.Verify != nil, TypeHistory: e.History != nil, TypeNotice: e.Notice != nil,
	}
	set, ok := members[e.Type]
	if !ok {
		return fmt.Errorf("unknown event type %q", e.Type)
	}
	if !set {
		return fmt.Errorf("a %s event without its %s member", e.Type, e.Type)
	}
	for t, has := range members {
		if has && t != e.Type {
			return fmt.Errorf("a %s event with a %s member", e.Type, t)
		}
	}
	lane := func(seg, lane int) error {
		if seg < 1 || lane < 0 || lane >= maxClones {
			return fmt.Errorf("a %s event for scenario %d lane %d", e.Type, seg, lane)
		}
		return nil
	}
	switch e.Type {
	case TypeScenario:
		sc := e.Scenario
		if sc.Seg < 1 || sc.Clones < 1 || sc.Clones > maxClones || len(sc.Argv) != sc.Clones {
			return fmt.Errorf("a scenario of %d clones with %d commands", sc.Clones, len(sc.Argv))
		}
	case TypeState:
		return lane(e.State.Seg, e.State.Lane)
	case TypeOutput:
		if e.Output.Stream != "stdout" && e.Output.Stream != "stderr" {
			return fmt.Errorf("output on stream %q", e.Output.Stream)
		}
		return lane(e.Output.Seg, e.Output.Lane)
	case TypeResult:
		return lane(e.Result.Seg, e.Result.Lane)
	case TypeEnd:
		if e.End.Seg < 1 {
			return errors.New("an end event without its scenario")
		}
	}
	return nil
}
