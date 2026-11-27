package ui

import (
	"bytes"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fixtureSession is a small session made by hand (no real recording),
// with guest output that tries everything.
func fixtureSession(t *testing.T) *Session {
	t.Helper()
	info := infoEvent(must(new(fakeDaemon).Info(t.Context(), nil)), "sandboxctl test")
	sc := &ScenarioEvent{Seg: 1, ID: "custom", Title: "Custom command", Clones: 1, Class: "small", TimeoutMS: 1000,
		MaxOutput: 1 << 20, Argv: [][]string{{"sh", "-c", "evil"}}}
	return &Session{
		Meta: SessionMeta{Format: sessionFormat, Version: sessionVersion, CreatedAt: time.Date(2026, 11, 25, 9, 30, 0, 0, time.UTC),
			UIVersion: "sandboxctl test", Info: info},
		Events: []TimedEvent{
			{T: 0, Event: &Event{Type: TypeInfo, Info: info}},
			{T: 10, Event: &Event{Type: TypeScenario, Scenario: sc}},
			{T: 30, Event: &Event{Type: TypeState, State: &StateEvent{Seg: 1, Lane: 0, RunID: "r-0000000000000001", State: "PENDING"}}},
			{T: 60, Event: &Event{Type: TypeOutput, Output: &OutputEvent{Seg: 1, Lane: 0, Stream: "stdout", Data: hostileText}}},
			{T: 90, Event: &Event{Type: TypeResult, Result: &ResultEvent{Seg: 1, Lane: 0, RunID: "r-0000000000000001", Outcome: "EXITED"}}},
			{T: 95, Event: &Event{Type: TypeEnd, End: &EndEvent{Seg: 1, Audit: []AuditRef{{RunID: "r-0000000000000001", Hash: strings.Repeat("ab", 32)}}}}},
		},
		End: &SessionEnd{EndedAt: time.Date(2026, 11, 25, 9, 31, 0, 0, time.UTC), Verify: &VerifyEvent{OK: true, Records: 1, Head: strings.Repeat("ab", 32)}},
	}
}

// hostileText is guest output that would end a <script> element, open a
// comment, carry terminal escapes and claim to be live.
const hostileText = "</script><script>alert(1)</script></SCRIPT ><!-- \x1b[2J\x1b[31mLIVE\x1b[0m \u2028\u2029 <img src=x onerror=alert(2)> & done\n"

func TestRecorderRoundTripKeepsTiming(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	start := time.Now()
	info := &InfoEvent{Mode: "rootless", Arch: "x86_64", Classes: []ClassInfo{}}
	r, err := createRecorder(path, SessionMeta{CreatedAt: start.UTC(), UIVersion: "test", Info: info}, start)
	if err != nil {
		t.Fatal(err)
	}
	sent := []*Event{
		{Type: TypeInfo, Info: info},
		{Type: TypeNotice, Notice: &NoticeEvent{Level: "info", Text: "one"}},
		{Type: TypeOutput, Output: &OutputEvent{Seg: 1, Lane: 2, Stream: "stderr", Data: hostileText}},
	}
	var at []time.Duration
	for i, ev := range sent {
		if i > 0 {
			time.Sleep(150 * time.Millisecond)
		}
		at = append(at, time.Since(start))
		if n := r.write(ev); n != nil {
			t.Fatalf("write: %+v", n.Notice)
		}
	}
	if err := r.close(&VerifyEvent{OK: true, Records: 7, Head: "h"}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Lstat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("the recording: %v, %v; want mode 0600", info, err)
	}
	s, err := LoadSession(path)
	if err != nil {
		t.Fatalf("LoadSession: %v", err)
	}
	if len(s.Events) != len(sent) || s.End == nil || !s.End.Verify.OK || s.End.Verify.Records != 7 || s.Meta.Info.Arch != "x86_64" {
		t.Fatalf("loaded %+v", s)
	}
	for i, te := range s.Events {
		a, _ := json.Marshal(sent[i])
		b, _ := json.Marshal(te.Event)
		if !bytes.Equal(a, b) {
			t.Errorf("event %d: %s, want %s", i, b, a)
		}
		// The original timing, within a few milliseconds.
		if diff := math.Abs(float64(te.T) - float64(at[i].Milliseconds())); diff > 20 {
			t.Errorf("event %d at %d ms, sent at %d ms", i, te.T, at[i].Milliseconds())
		}
	}
	if s.Events[2].Event.Output.Data != hostileText {
		t.Errorf("the hostile output came back as %q", s.Events[2].Event.Output.Data)
	}

	// Never over an existing file, never through a symlink.
	if _, err := createRecorder(path, SessionMeta{Info: info}, start); err == nil || !strings.Contains(err.Error(), "exists already") {
		t.Errorf("a second recording into the same file: %v", err)
	}
	link := filepath.Join(t.TempDir(), "link.jsonl")
	if err := os.Symlink(filepath.Join(t.TempDir(), "target"), link); err != nil {
		t.Fatal(err)
	}
	if _, err := createRecorder(link, SessionMeta{Info: info}, start); err == nil {
		t.Error("a recording through a symlink")
	}
}

func TestRecorderStopsAtItsLimit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	info := &InfoEvent{Mode: "rootless", Classes: []ClassInfo{}}
	r, err := createRecorder(path, SessionMeta{Info: info}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	r.limit = endReserve + 8<<10 // a small limit for the test
	var notices int
	chunk := strings.Repeat("x", 1000)
	for range 50 {
		if n := r.write(&Event{Type: TypeOutput, Output: &OutputEvent{Seg: 1, Stream: "stdout", Data: chunk}}); n != nil {
			notices++
			if !strings.Contains(n.Notice.Text, "limit of 32 MiB") {
				t.Errorf("notice %q", n.Notice.Text)
			}
		}
	}
	if notices != 1 {
		t.Errorf("%d notices, want exactly 1", notices)
	}
	if err := r.close(&VerifyEvent{OK: true}, time.Now()); err != nil {
		t.Fatal(err)
	}
	s, err := LoadSession(path)
	if err != nil || s.End == nil || s.End.Stopped == "" || len(s.Events) == 0 || len(s.Events) > 8 {
		t.Errorf("a stopped recording loads as %d events, end %+v, %v", len(s.Events), s.End, err)
	}
	if info, _ := os.Stat(path); info.Size() > r.limit {
		t.Errorf("the file is %d bytes, over its limit %d", info.Size(), r.limit)
	}
}

func TestLoadSessionIsAnAllowlist(t *testing.T) {
	good := fixtureLines(t)
	if _, err := readSession(strings.NewReader(strings.Join(good, "\n") + "\n")); err != nil {
		t.Fatalf("the fixture does not load: %v", err)
	}
	// A crash leaves no end line: still a recording.
	if s, err := readSession(strings.NewReader(strings.Join(good[:len(good)-1], "\n") + "\n")); err != nil || s.End != nil {
		t.Errorf("without its end line: %v, %v", s, err)
	}
	for name, lines := range map[string][]string{
		"claims to be live":   replace(good, 0, `{"meta":{`, `{"live":true,"meta":{`),
		"unknown event field": replace(good, 2, `"type":"scenario"`, `"type":"scenario","live":true`),
		"unknown member":      replace(good, 3, `"state":{`, `"token":"x","state":{`),
		"env in a scenario":   replace(good, 2, `"clones":1`, `"clones":1,"env":{"A":"b"}`),
		"another format":      replace(good, 0, sessionFormat, "something-else"),
		"time goes back":      replace(good, 4, `"t":60`, `"t":1`),
		"type without member": replace(good, 3, `"type":"state"`, `"type":"result"`),
		"lane out of range":   replace(good, 3, `"lane":0`, `"lane":8`),
		"unknown type":        replace(good, 3, `"type":"state"`, `"type":"exec"`),
		"no meta":             good[1:],
		"after the end":       append(append([]string{}, good...), good[1]),
		"too long a line":     append([]string{good[0]}, `{"t":1,"event":{"type":"notice","notice":{"level":"info","text":"`+strings.Repeat("x", maxSessionLine)+`"}}}`),
		"two values":          append([]string{good[0], good[1] + " {}"}, good[2:]...),
	} {
		if _, err := readSession(strings.NewReader(strings.Join(lines, "\n") + "\n")); err == nil {
			t.Errorf("%s: loaded", name)
		}
	}
}

// fixtureLines is the fixture session as the lines of a session file.
func fixtureLines(t *testing.T) []string {
	t.Helper()
	s := fixtureSession(t)
	enc := func(l sessionLine) string {
		b, err := json.Marshal(l)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	lines := []string{enc(sessionLine{Meta: &s.Meta})}
	for _, te := range s.Events {
		tt := te.T
		lines = append(lines, enc(sessionLine{T: &tt, Event: te.Event}))
	}
	return append(lines, enc(sessionLine{End: s.End}))
}

func replace(lines []string, i int, from, to string) []string {
	out := append([]string{}, lines...)
	out[i] = strings.Replace(out[i], from, to, 1)
	return out
}
