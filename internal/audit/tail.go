package audit

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"time"
)

// Entry is one line of the log as Tail reads it: the line's two chain
// hashes and a few fields of its record, for showing the newest records
// (sandboxctl ui). Tail does not check the chain; Verify does.
type Entry struct {
	// PrevHash and Hash are the line's chain hashes, as written.
	PrevHash string `json:"prev_hash"`
	Hash     string `json:"hash"`
	// Kind is the record's kind (KindRun or KindRotation), or "" if the
	// line could not be read as a record.
	Kind string `json:"kind"`
	// Time is when the record was written.
	Time time.Time `json:"time"`
	// RunID, Outcome and FinalState are a run record's.
	RunID      string `json:"run_id,omitempty"`
	Outcome    string `json:"outcome,omitempty"`
	FinalState string `json:"final_state,omitempty"`
}

// tailBlock is how much Tail reads at a time, from the end backwards.
const tailBlock = 64 << 10

// Tail returns the newest n complete lines of the log at path, oldest
// first. It reads the file from the end, at most maxLine plus a block
// (a record may be up to maxLine long), so it costs the same for a log
// of any length. A log that does not exist has no lines; a last line
// without its newline (a write in progress) is left out.
func Tail(path string, n int) ([]Entry, error) {
	if n <= 0 {
		return nil, nil
	}
	f, err := os.Open(path) //nolint:gosec // G304: the audit log in our own state folder
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("audit: %w", err)
	}
	defer f.Close() //nolint:errcheck // read-only file
	info, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("audit: %w", err)
	}

	// Read blocks from the end until n+1 newlines are in hand (the
	// first of them ends the line before the n we want), or the start.
	var chunks [][]byte
	newlines, off := 0, info.Size()
	for off > 0 && newlines <= n && info.Size()-off < maxLine+tailBlock {
		step := min(int64(tailBlock), off)
		off -= step
		chunk := make([]byte, step)
		if _, err := f.ReadAt(chunk, off); err != nil && !errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("audit: read %s: %w", path, err)
		}
		newlines += bytes.Count(chunk, []byte("\n"))
		chunks = append(chunks, chunk)
	}
	var buf []byte
	for i := len(chunks) - 1; i >= 0; i-- {
		buf = append(buf, chunks[i]...)
	}
	last := bytes.LastIndexByte(buf, '\n')
	if last < 0 {
		return nil, nil // not even one complete line
	}
	lines := bytes.SplitAfter(buf[:last+1], []byte("\n"))
	lines = lines[:len(lines)-1] // SplitAfter's empty piece after the last newline
	if off > 0 && len(lines) > 0 {
		lines = lines[1:] // it started mid-line
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	entries := make([]Entry, 0, len(lines))
	for _, line := range lines {
		entries = append(entries, parseEntry(line))
	}
	return entries, nil
}

// parseEntry reads one line for Tail. A line it cannot read keeps
// Kind "".
func parseEntry(line []byte) Entry {
	var env envelope
	if err := json.Unmarshal(line, &env); err != nil {
		return Entry{}
	}
	e := Entry{PrevHash: env.PrevHash, Hash: env.Hash}
	var rec struct {
		Kind string    `json:"kind"`
		Time time.Time `json:"time"`
		Run  *struct {
			RunID      string `json:"run_id"`
			Outcome    string `json:"outcome"`
			FinalState string `json:"final_state"`
		} `json:"run"`
	}
	if err := json.Unmarshal(env.Record, &rec); err != nil {
		return e
	}
	e.Kind, e.Time = rec.Kind, rec.Time
	if rec.Run != nil {
		e.RunID, e.Outcome, e.FinalState = rec.Run.RunID, rec.Run.Outcome, rec.Run.FinalState
	}
	return e
}
