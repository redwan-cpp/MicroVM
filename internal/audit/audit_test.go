package audit

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// runRecord is a small run record for the tests.
func runRecord(id string) Record {
	code := 0
	return Record{Kind: KindRun, Time: time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC), Run: &Run{
		RunID: id, Argv: []string{"echo", id}, SizeClass: "small", Mode: "rootless",
		Outcome: "EXITED", FinalState: "DONE", Exited: true, ExitCode: &code,
		Transitions: []Transition{{State: "PENDING"}, {State: "STOPPING", SinceUS: 20_000}},
	}}
}

// writeChain opens a log in a fresh folder and appends n records.
func writeChain(t *testing.T, n int) (dir string) {
	t.Helper()
	dir = filepath.Join(t.TempDir(), DirName)
	l, err := Open(dir, nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	for i := range n {
		if err := l.Append(runRecord(fmt.Sprintf("r-%016d", i))); err != nil {
			t.Fatalf("Append %d: %v", i, err)
		}
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	return dir
}

func readLines(t *testing.T, dir string) [][]byte {
	t.Helper()
	data, err := os.ReadFile(Path(dir))
	if err != nil {
		t.Fatal(err)
	}
	return bytes.SplitAfter(data, []byte("\n"))[:bytes.Count(data, []byte("\n"))]
}

func writeLines(t *testing.T, dir string, lines [][]byte) {
	t.Helper()
	if err := os.WriteFile(Path(dir), bytes.Join(lines, nil), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestChainVerifies(t *testing.T) {
	dir := writeChain(t, 3)
	sum, err := Verify(Path(dir))
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if sum.Records != 3 || len(sum.Head) != 64 || sum.Head == ZeroHash {
		t.Errorf("Verify = %+v, want 3 records and a real head", sum)
	}

	// Every line is the documented envelope, and the hash is exactly
	// SHA-256(prev_hash + the record's raw bytes).
	prev := ZeroHash
	for i, line := range readLines(t, dir) {
		var env struct {
			Record   json.RawMessage `json:"record"`
			PrevHash string          `json:"prev_hash"`
			Hash     string          `json:"hash"`
		}
		if err := json.Unmarshal(line, &env); err != nil {
			t.Fatalf("line %d: %v", i+1, err)
		}
		if !bytes.HasPrefix(line, []byte(`{"record":{"kind":"run"`)) || !bytes.HasSuffix(line, []byte("\"}\n")) {
			t.Errorf("line %d has an unexpected shape: %s", i+1, line)
		}
		if env.PrevHash != prev || env.Hash != chainHash(prev, env.Record) {
			t.Errorf("line %d: prev %s, hash %s; want prev %s and the chained hash", i+1, env.PrevHash, env.Hash, prev)
		}
		prev = env.Hash
	}

	// The modes: folder 0700, file 0600.
	for path, want := range map[string]os.FileMode{dir: 0o700, Path(dir): 0o600} {
		if info, err := os.Stat(path); err != nil || info.Mode().Perm() != want {
			t.Errorf("%s: %v, %v; want mode %04o", path, info, err, want)
		}
	}
}

func TestReopenContinuesTheChain(t *testing.T) {
	dir := writeChain(t, 2)
	l, err := Open(dir, nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if head, n := l.Head(); n != 2 || head == ZeroHash {
		t.Errorf("Head = %s, %d; want the second record's hash", head, n)
	}
	if err := l.Append(runRecord("r-0000000000000009")); err != nil {
		t.Fatal(err)
	}
	_ = l.Close()
	if sum, err := Verify(Path(dir)); err != nil || sum.Records != 3 {
		t.Errorf("Verify after reopening = %+v, %v; want 3 records", sum, err)
	}
	if err := l.Append(runRecord("r-0000000000000010")); err == nil {
		t.Error("Append after Close succeeded")
	}
}

func TestMissingLogIsAnEmptyChain(t *testing.T) {
	sum, err := Verify(filepath.Join(t.TempDir(), "nothing.jsonl"))
	if err != nil || sum.Records != 0 || sum.Head != ZeroHash {
		t.Errorf("Verify(missing) = %+v, %v; want an empty chain", sum, err)
	}
}

func TestBrokenChains(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(lines [][]byte) [][]byte
		line   int    // the first broken line
		reason string // part of the reason
	}{
		{
			name: "tampered middle record",
			change: func(lines [][]byte) [][]byte {
				lines[1] = bytes.Replace(lines[1], []byte(`"EXITED"`), []byte(`"FAILED"`), 1)
				return lines
			},
			line: 2, reason: "does not match the record's contents",
		},
		{
			// Fixing the hash of a changed record does not help: the next
			// record's prev_hash still points at the old one.
			name: "tampered record with a recomputed hash",
			change: func(lines [][]byte) [][]byte {
				var env envelope
				_ = json.Unmarshal(lines[1], &env)
				rec := bytes.Replace(env.Record, []byte(`"EXITED"`), []byte(`"FAILED"`), 1)
				lines[1] = []byte(fmt.Sprintf(`{"record":%s,"prev_hash":%q,"hash":%q}`+"\n", rec, env.PrevHash, chainHash(env.PrevHash, rec)))
				return lines
			},
			line: 3, reason: "prev_hash",
		},
		{
			name:   "truncated last line",
			change: func(lines [][]byte) [][]byte { lines[2] = lines[2][:len(lines[2])/2]; return lines },
			line:   3, reason: "cut off",
		},
		{
			name:   "last newline missing",
			change: func(lines [][]byte) [][]byte { lines[2] = bytes.TrimSuffix(lines[2], []byte("\n")); return lines },
			line:   3, reason: "cut off",
		},
		{
			name:   "reordered records",
			change: func(lines [][]byte) [][]byte { lines[1], lines[2] = lines[2], lines[1]; return lines },
			line:   2, reason: "prev_hash",
		},
		{
			name:   "deleted middle record",
			change: func(lines [][]byte) [][]byte { return append(lines[:1], lines[2:]...) },
			line:   2, reason: "prev_hash",
		},
		{
			name:   "deleted first record",
			change: func(lines [][]byte) [][]byte { return lines[1:] },
			line:   1, reason: "prev_hash",
		},
		{
			name:   "an empty line",
			change: func(lines [][]byte) [][]byte { return append(lines, []byte("\n")) },
			line:   4, reason: "not a record line",
		},
		{
			name: "an extra field",
			change: func(lines [][]byte) [][]byte {
				lines[0] = bytes.Replace(lines[0], []byte(`{"record":`), []byte(`{"note":1,"record":`), 1)
				return lines
			},
			line: 1, reason: "not a record line",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := writeChain(t, 3)
			writeLines(t, dir, tc.change(readLines(t, dir)))
			sum, err := Verify(Path(dir))
			var broken *BrokenError
			if !errors.As(err, &broken) || !errors.Is(err, ErrBroken) {
				t.Fatalf("Verify = %+v, %v; want a *BrokenError", sum, err)
			}
			if broken.Line != tc.line || !strings.Contains(broken.Reason, tc.reason) {
				t.Errorf("broken at line %d (%s), want line %d (%s)", broken.Line, broken.Reason, tc.line, tc.reason)
			}
			if sum.Records != tc.line-1 {
				t.Errorf("%d records verified, want %d", sum.Records, tc.line-1)
			}
			// The daemon refuses to append to a broken chain.
			if _, err := Open(dir, nil); !errors.Is(err, ErrBroken) {
				t.Errorf("Open of a broken chain = %v, want ErrBroken", err)
			}
		})
	}
}

func TestRemovingTheNewestRecordsIsNotDetected(t *testing.T) {
	// The documented limit (threat model T11): a shorter chain is still a
	// valid chain.
	dir := writeChain(t, 3)
	writeLines(t, dir, readLines(t, dir)[:2])
	if sum, err := Verify(Path(dir)); err != nil || sum.Records != 2 {
		t.Errorf("Verify = %+v, %v; want 2 valid records", sum, err)
	}
}

func TestAppendFailureStopsTheLog(t *testing.T) {
	dir := writeChain(t, 1)
	l, err := Open(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Break the file under the log: closing the descriptor makes every
	// write fail, as a full disk or an I/O error would.
	_ = l.f.Close()
	if err := l.Append(runRecord("r-0000000000000002")); err == nil {
		t.Fatal("Append to a closed file succeeded")
	}
	if l.Failed() == nil {
		t.Error("Failed() is nil after a failed Append")
	}
	if err := l.Append(runRecord("r-0000000000000003")); !errors.Is(err, ErrFailed) {
		t.Errorf("Append after a failure = %v, want ErrFailed", err)
	}
	if sum, err := Verify(Path(dir)); err != nil || sum.Records != 1 {
		t.Errorf("Verify = %+v, %v; want the 1 good record", sum, err)
	}
}

func TestOpenRefusesASymlink(t *testing.T) {
	dir := filepath.Join(t.TempDir(), DirName)
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "elsewhere")
	if err := os.Symlink(target, Path(dir)); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dir, nil); err == nil {
		t.Error("Open followed a symlink")
	}
	if _, err := os.Stat(target); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("Open created the symlink's target: %v", err)
	}
}

func TestRotateStartsANewChain(t *testing.T) {
	dir := writeChain(t, 3)
	lines := readLines(t, dir)
	writeLines(t, dir, append(lines[:1], lines[2:]...)) // break it at line 2
	brokenSum, _ := Verify(Path(dir))

	now := time.Date(2026, 11, 24, 13, 0, 0, 0, time.UTC)
	l, aside, err := Rotate(dir, nil, now)
	if err != nil {
		t.Fatalf("Rotate: %v", err)
	}
	if err := l.Append(runRecord("r-0000000000000004")); err != nil {
		t.Fatal(err)
	}
	_ = l.Close()

	if filepath.Base(aside) != "audit.jsonl.broken-20261124T130000Z" {
		t.Errorf("moved aside to %s", aside)
	}
	if _, err := Verify(aside); !errors.Is(err, ErrBroken) {
		t.Errorf("the evidence file changed: Verify = %v, want still broken", err)
	}
	sum, err := Verify(Path(dir))
	if err != nil || sum.Records != 2 {
		t.Fatalf("new chain: %+v, %v; want 2 records", sum, err)
	}
	var env envelope
	if err := json.Unmarshal(readLines(t, dir)[0], &env); err != nil || env.PrevHash != ZeroHash {
		t.Fatalf("first line of the new chain: %v, prev %s", err, env.PrevHash)
	}
	var rec Record
	if err := json.Unmarshal(env.Record, &rec); err != nil || rec.Kind != KindRotation || rec.Rotation == nil {
		t.Fatalf("first record %s, want a rotation", env.Record)
	}
	r := rec.Rotation
	if r.PreviousFile != filepath.Base(aside) || r.VerifiedRecords != 1 || r.LastGoodHash != brokenSum.Head ||
		!strings.Contains(r.Reason, "line 2") {
		t.Errorf("rotation record %+v", r)
	}
}

func TestRecordJSONNames(t *testing.T) {
	// The field names are a file format; pin them.
	code := 3
	data, err := json.Marshal(Record{Kind: KindRun, Run: &Run{ExitCode: &code, JailedUID: 70000, SlotRetired: true,
		Error: "e", DestroyError: "d", Signal: 9, EnvNames: []string{"A"}, SnapshotVersion: "v"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"kind", "time", "run", "run_id", "argv", "env_names", "stdin_bytes", "size_class",
		"snapshot_version", "mode", "jailed_uid", "caller_uid", "daemon_version", "outcome", "final_state", "exited",
		"exit_code", "signal", "timed_out", "output_truncated", "output_bytes", "error", "destroy_error", "slot_retired",
		"transitions", "timings_us", "queued", "restore", "scratch", "mount", "exec", "destroy", "total"} {
		if !strings.Contains(string(data), `"`+key+`"`) {
			t.Errorf("record JSON has no %q: %s", key, data)
		}
	}
	// No exit code unless the command exited.
	data, _ = json.Marshal(Run{})
	if strings.Contains(string(data), "exit_code") {
		t.Errorf("a run without an exit has exit_code: %s", data)
	}
}
