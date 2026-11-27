package ui

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

// clean makes a short text from the daemon (a version, a detail that may
// quote the guest) safe for one line on the page: valid UTF-8, no control
// or bidirectional-override characters, at most n bytes. Command output
// is not cleaned here: it is relayed and recorded as captured, and the
// page removes control characters when it shows it.
func clean(s string, n int) string {
	s = strings.ToValidUTF8(s, "�")
	s = strings.Map(func(r rune) rune {
		switch {
		case r == '\n' || r == '\t':
			return ' '
		case unicode.IsControl(r), isBidiControl(r):
			return -1
		}
		return r
	}, s)
	return cut(s, n)
}

// cleanText is clean for longer texts that keep their newlines (an
// error from the daemon).
func cleanText(s string, n int) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = clean(l, n)
	}
	return cut(strings.Join(lines, "\n"), n)
}

// cut cuts s to at most n bytes at a character boundary, and marks the
// cut with "…".
func cut(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return prefix(s, n) + "…"
}

// prefix returns the longest start of s of at most n bytes that ends at
// a character boundary.
func prefix(s string, n int) string {
	if len(s) <= n {
		return s
	}
	i := n
	for i > 0 && !utf8.RuneStart(s[i]) {
		i--
	}
	return s[:i]
}

// isBidiControl reports the characters that reorder text on screen
// (U+202A-U+202E, U+2066-U+2069, U+200E, U+200F): guest text could
// use them to make one thing look like another.
func isBidiControl(r rune) bool {
	return (r >= 0x202A && r <= 0x202E) || (r >= 0x2066 && r <= 0x2069) || r == 0x200E || r == 0x200F
}

// scrubber removes host paths from texts the daemon writes (an error
// that names a run folder, say), so that neither the page nor a recorded
// session holds an absolute host path: the state folder becomes
// <state>, the repository <repo>, the home folder ~.
type scrubber struct {
	pairs [][2]string // longest first
}

// newScrubber builds a scrubber for the state folder; the working folder
// and the home folder come from the process.
func newScrubber(stateDir string) *scrubber {
	s := &scrubber{}
	add := func(path, name string) {
		if path == "" || path == "/" {
			return
		}
		if abs, err := filepath.Abs(path); err == nil {
			s.pairs = append(s.pairs, [2]string{abs, name})
			if resolved, err := filepath.EvalSymlinks(abs); err == nil && resolved != abs {
				s.pairs = append(s.pairs, [2]string{resolved, name})
			}
		}
	}
	add(stateDir, "<state>")
	if wd, err := os.Getwd(); err == nil {
		add(wd, "<repo>")
	}
	if home, err := os.UserHomeDir(); err == nil {
		add(home, "~")
	}
	add(os.Getenv("XDG_RUNTIME_DIR"), "<runtime>")
	slices.SortStableFunc(s.pairs, func(a, b [2]string) int { return len(b[0]) - len(a[0]) })
	return s
}

// scrub returns text without the scrubber's paths, cleaned of control
// characters, at most 4 KiB.
func (s *scrubber) scrub(text string) string {
	for _, p := range s.pairs {
		text = strings.ReplaceAll(text, p[0], p[1])
	}
	return cleanText(text, 4<<10)
}

// utf8Carry turns a stream of output chunks into valid UTF-8 text. A
// character split between two chunks is kept whole (its first bytes wait
// for the next chunk); each run of bytes that are not UTF-8 becomes one U+FFFD.
type utf8Carry struct {
	rest []byte // the start of a character cut off at the end of a chunk
}

// text returns the text of p (after what was carried), keeping back an
// incomplete character at its end.
func (c *utf8Carry) text(p []byte) string {
	b := append(c.rest, p...)
	c.rest = nil
	// Look back at most utf8.UTFMax-1 bytes for the start of the last
	// character; if it is incomplete, carry it.
	for i := len(b) - 1; i >= 0 && i >= len(b)-(utf8.UTFMax-1); i-- {
		if utf8.RuneStart(b[i]) {
			if !utf8.FullRune(b[i:]) {
				c.rest = slices.Clone(b[i:])
				b = b[:i]
			}
			break
		}
	}
	return strings.ToValidUTF8(string(b), "�")
}

// flush returns what is still carried (an incomplete character at the
// very end of the output), as U+FFFD.
func (c *utf8Carry) flush() string {
	if len(c.rest) == 0 {
		return ""
	}
	c.rest = nil
	return "�"
}
