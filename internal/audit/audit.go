// Package audit is sandboxd's audit log: one record for every run, in a
// file where any change to a record, or to their order, can be detected
// afterwards (threat T11).
//
// The log is a JSONL file (one JSON value per line), <state>/audit/
// audit.jsonl, only ever appended to. Each line is
//
//	{"record":{...},"prev_hash":"<64 hex digits>","hash":"<64 hex digits>"}
//
// where hash is the SHA-256 of prev_hash (the 64 hex digits as written)
// followed by the record's bytes exactly as they appear in the line, and
// prev_hash is the hash of the line before (64 zeros for the first line).
// So every line "points at" the one before it: a hash chain. Change a
// byte of a record, delete a record, or swap two, and from there on the
// hashes no longer match. Verify walks the chain and names the first line
// that breaks it.
//
// What the chain cannot detect: someone deleting the newest records
// (what is left is still a valid chain), or rewriting the whole file with
// fresh hashes. Whoever can write the file can do both: the user in
// rootless mode, root in jailer mode. Anchoring the newest hash somewhere
// else is on the build plan's "Later" list.
//
// The log fails closed. The daemon verifies the whole chain when it
// starts and refuses to start if it is broken; and if writing or syncing
// a record ever fails, Append refuses every later record too, so the
// daemon stops accepting runs: no run happens without its record.
// docs/learn/11-the-daemon-and-a-run.md explains it all.
package audit

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const (
	// DirName is the audit folder inside the state folder.
	DirName = "audit"
	// FileName is the audit log inside it.
	FileName = "audit.jsonl"
	// maxLine is the longest line Verify reads. A record holds the run's
	// argv, which a request may make up to about 1 MiB.
	maxLine = 4 << 20
)

// ZeroHash is the prev_hash of the first record of a chain.
var ZeroHash = strings.Repeat("0", sha256.Size*2)

// ErrBroken is wrapped by every *BrokenError.
var ErrBroken = errors.New("audit: the hash chain is broken")

// BrokenError says where and why a chain is broken.
type BrokenError struct {
	// Path is the log file.
	Path string
	// Line is the first line (counting from 1) that does not fit the
	// chain. Every line before it was fine.
	Line int
	// Reason says what is wrong with it.
	Reason string
}

func (e *BrokenError) Error() string {
	return fmt.Sprintf("audit: %s is broken at line %d: %s", e.Path, e.Line, e.Reason)
}

// Unwrap makes errors.Is(err, ErrBroken) true.
func (e *BrokenError) Unwrap() error { return ErrBroken }

// Summary describes a chain that verified.
type Summary struct {
	// Records is how many records it holds.
	Records int
	// Head is the hash of the newest record, or ZeroHash for an empty log.
	Head string
}

// envelope is one line of the log. Record is kept as raw bytes: the hash
// covers exactly the bytes in the file, never a re-encoded copy.
type envelope struct {
	Record   json.RawMessage `json:"record"`
	PrevHash string          `json:"prev_hash"`
	Hash     string          `json:"hash"`
}

// chainHash is the hash of a record whose predecessor's hash is prev.
func chainHash(prev string, record []byte) string {
	h := sha256.New()
	h.Write([]byte(prev))
	h.Write(record)
	return hex.EncodeToString(h.Sum(nil))
}

// Verify walks the whole chain in the log at path. A log that does not
// exist is an empty, valid chain. A broken one gives a *BrokenError.
func Verify(path string) (Summary, error) {
	f, err := os.Open(path) //nolint:gosec // G304: the audit log in our own state folder
	if errors.Is(err, os.ErrNotExist) {
		return Summary{Head: ZeroHash}, nil
	}
	if err != nil {
		return Summary{}, fmt.Errorf("audit: %w", err)
	}
	defer f.Close() //nolint:errcheck // read-only file
	return verify(path, f)
}

// verify walks the chain in r; path is only for messages.
func verify(path string, r io.Reader) (Summary, error) {
	sum := Summary{Head: ZeroHash}
	br := bufio.NewReaderSize(r, 64<<10)
	broken := func(reason string, args ...any) (Summary, error) {
		return sum, &BrokenError{Path: path, Line: sum.Records + 1, Reason: fmt.Sprintf(reason, args...)}
	}
	for {
		line, err := readLine(br)
		if errors.Is(err, io.EOF) && len(line) == 0 {
			return sum, nil // the end, after a complete line
		}
		if errors.Is(err, errLineTooLong) {
			return broken("longer than %d bytes", maxLine)
		}
		if err != nil && !errors.Is(err, io.EOF) {
			return sum, fmt.Errorf("audit: read %s: %w", path, err)
		}
		if errors.Is(err, io.EOF) {
			// The last line has no newline: a write that never finished.
			return broken("the last line is cut off (no newline at its end); if the daemon is running, it may be writing it right now")
		}
		var env envelope
		dec := json.NewDecoder(bytes.NewReader(line))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&env); err != nil {
			return broken("not a record line: %v", err)
		}
		if dec.More() {
			return broken("more than one JSON value on the line")
		}
		if len(env.Record) == 0 || env.Record[0] != '{' {
			return broken(`"record" is missing or not an object`)
		}
		if env.PrevHash != sum.Head {
			return broken("prev_hash %s does not match the hash of the record before, %s (a record was changed, removed or moved)",
				short(env.PrevHash), short(sum.Head))
		}
		if want := chainHash(env.PrevHash, env.Record); env.Hash != want {
			return broken("hash %s does not match the record's contents, %s (the record was changed)", short(env.Hash), short(want))
		}
		sum.Records++
		sum.Head = env.Hash
	}
}

// errLineTooLong is returned by readLine for a line over maxLine.
var errLineTooLong = errors.New("line too long")

// readLine reads one line, newline included. At the end of the input it
// returns what is left with io.EOF.
func readLine(br *bufio.Reader) ([]byte, error) {
	var line []byte
	for {
		chunk, err := br.ReadSlice('\n')
		line = append(line, chunk...)
		if len(line) > maxLine {
			return nil, errLineTooLong
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		return line, err
	}
}

// short shortens a hash for messages.
func short(h string) string {
	if len(h) > 12 {
		return h[:12] + "…"
	}
	return h
}

// Owner is a numeric user and group ID.
type Owner struct {
	UID, GID int
}

// Log is an open audit log, ready to append to. It is safe for concurrent
// use.
type Log struct {
	mu      sync.Mutex
	f       *os.File
	path    string
	head    string
	records int
	failed  error // the first failed Append; every later one fails too
}

// ErrFailed is wrapped by Append's error once an earlier record could not
// be written: the log accepts nothing more until the daemon restarts.
var ErrFailed = errors.New("audit: an earlier record could not be written; restart the daemon")

// Path returns the log's path in the audit folder dir.
func Path(dir string) string { return filepath.Join(dir, FileName) }

// Open opens the audit log in dir (a state folder's audit/ folder) for
// appending, after verifying its whole chain: a broken chain is an error
// (a *BrokenError), never repaired. The folder is created mode 0700 and
// the log mode 0600 if they are missing. If owner is not nil, both are
// given to it: a daemon running as root on a user's state folder keeps
// them the user's, so that user can still read and verify them.
//
// Only one process may append at a time: the caller must hold the state
// folder's lock (state.Root.Lock).
func Open(dir string, owner *Owner) (*Log, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("audit: %w", err)
	}
	if info, err := os.Lstat(dir); err != nil {
		return nil, fmt.Errorf("audit: %w", err)
	} else if !info.IsDir() {
		return nil, fmt.Errorf("audit: %s is not a folder", dir)
	}
	path := Path(dir)
	_, statErr := os.Lstat(path)
	created := errors.Is(statErr, os.ErrNotExist)
	// O_APPEND: every write goes to the end, whatever the offset says.
	// O_NOFOLLOW: never write through a symlink someone put there.
	f, err := os.OpenFile(path, os.O_RDWR|os.O_APPEND|os.O_CREATE|unix.O_NOFOLLOW, 0o600) //nolint:gosec // G304: the audit log in our own state folder
	if err != nil {
		return nil, fmt.Errorf("audit: open %s: %w", path, err)
	}
	ok := false
	defer func() {
		if !ok {
			_ = f.Close()
		}
	}()
	if owner != nil {
		if err := os.Lchown(dir, owner.UID, owner.GID); err != nil {
			return nil, fmt.Errorf("audit: give %s to %d:%d: %w", dir, owner.UID, owner.GID, err)
		}
		if err := f.Chown(owner.UID, owner.GID); err != nil {
			return nil, fmt.Errorf("audit: give %s to %d:%d: %w", path, owner.UID, owner.GID, err)
		}
	}
	if created {
		if err := syncDir(dir); err != nil {
			return nil, err
		}
	}
	// Verify through the same open file: what we check is what we append to.
	sum, err := verify(path, io.NewSectionReader(f, 0, 1<<62))
	if err != nil {
		return nil, err
	}
	ok = true
	return &Log{f: f, path: path, head: sum.Head, records: sum.Records}, nil
}

// Append adds rec to the log and syncs it to disk before it returns. If
// that fails, the log is no longer trusted: this and every later Append
// return an error wrapping ErrFailed or the original failure.
func (l *Log) Append(rec Record) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil {
		return ErrClosed
	}
	if l.failed != nil {
		return fmt.Errorf("%w (first failure: %w)", ErrFailed, l.failed)
	}
	raw, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("audit: encode record: %w", err) // a bug, not a broken log: nothing was written
	}
	hash := chainHash(l.head, raw)
	line := make([]byte, 0, len(raw)+200)
	line = append(line, `{"record":`...)
	line = append(line, raw...)
	line = append(line, `,"prev_hash":"`...)
	line = append(line, l.head...)
	line = append(line, `","hash":"`...)
	line = append(line, hash...)
	line = append(line, "\"}\n"...)

	// One write call per line: with O_APPEND the kernel puts it at the
	// end in one piece.
	n, err := l.f.Write(line)
	if err == nil && n != len(line) {
		err = io.ErrShortWrite
	}
	if err == nil {
		err = l.f.Sync()
	}
	if err != nil {
		l.failed = err
		return fmt.Errorf("audit: write %s: %w", l.path, err)
	}
	l.head = hash
	l.records++
	return nil
}

// ErrClosed is Append's error after Close.
var ErrClosed = errors.New("audit: the log is closed")

// Failed returns why the log accepts no more records (a failed write, or
// Close), or nil while it does.
func (l *Log) Failed() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.failed == nil && l.f == nil {
		return ErrClosed
	}
	return l.failed
}

// Head returns the newest record's hash and the number of records.
func (l *Log) Head() (string, int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.head, l.records
}

// Path is the log file.
func (l *Log) Path() string { return l.path }

// Close syncs and closes the log. Append fails afterwards.
func (l *Log) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil {
		return nil
	}
	err := l.f.Sync()
	if closeErr := l.f.Close(); err == nil {
		err = closeErr
	}
	l.f = nil
	return err
}

// syncDir makes a new folder entry (a created or renamed file) durable.
func syncDir(dir string) error {
	d, err := os.Open(dir) //nolint:gosec // G304: our own audit folder
	if err != nil {
		return fmt.Errorf("audit: %w", err)
	}
	defer d.Close() //nolint:errcheck // read-only
	if err := d.Sync(); err != nil && !errors.Is(err, syscall.EINVAL) {
		return fmt.Errorf("audit: sync %s: %w", dir, err)
	}
	return nil
}

// Rotate starts a new chain in dir: it renames the current log to
// audit.jsonl.broken-<UTC time>, which stays as evidence, and opens a new
// log whose first record (Kind "rotation") names that file, says why, and
// records how far the old chain verified and its last good hash. It is
// the documented way out of a broken chain; the daemon never does it on
// its own. The caller must hold the state folder's lock. It returns the
// new log, open for appending, and the old file's new name.
func Rotate(dir string, owner *Owner, now time.Time) (*Log, string, error) {
	path := Path(dir)
	sum, verr := Verify(path)
	if verr != nil && !errors.Is(verr, ErrBroken) {
		return nil, "", verr
	}
	if _, err := os.Lstat(path); err != nil {
		return nil, "", fmt.Errorf("audit: nothing to rotate: %w", err)
	}
	aside := path + ".broken-" + now.UTC().Format("20060102T150405Z")
	if err := os.Rename(path, aside); err != nil {
		return nil, "", fmt.Errorf("audit: %w", err)
	}
	if err := syncDir(dir); err != nil {
		return nil, "", err
	}
	l, err := Open(dir, owner)
	if err != nil {
		return nil, "", err
	}
	rot := &Rotation{PreviousFile: filepath.Base(aside), VerifiedRecords: sum.Records, LastGoodHash: sum.Head}
	if verr != nil {
		rot.Reason = verr.Error()
	} else {
		rot.Reason = "rotated by hand; the old chain verified"
	}
	if err := l.Append(Record{Kind: KindRotation, Time: now.UTC(), Rotation: rot}); err != nil {
		_ = l.Close()
		return nil, "", err
	}
	return l, aside, nil
}
