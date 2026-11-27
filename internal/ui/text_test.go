package ui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestClean(t *testing.T) {
	rlo := string(rune(0x202E)) // right-to-left override
	for in, want := range map[string]string{
		"plain":                   "plain",
		"two\nlines\tand tab":     "two lines and tab",
		"esc\x1b[31mred\x07\x00!": "esc[31mred!",
		"bad \xff byte":           "bad \U0000FFFD byte",
		"evil" + rlo + "txt.exe":  "eviltxt.exe",
		"del\x7f and c1\u0085":    "del and c1",
	} {
		if got := clean(in, 100); got != want {
			t.Errorf("clean(%q) = %q, want %q", in, got, want)
		}
	}
	if got := clean(strings.Repeat("é", 10), 5); got != "éé…" {
		t.Errorf("clean cuts at a character boundary: %q", got)
	}
	if got := cleanText("line one\x1b\nline two", 100); got != "line one\nline two" {
		t.Errorf("cleanText = %q", got)
	}
}

func TestScrubber(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home folder")
	}
	state := filepath.Join(t.TempDir(), "state")
	s := newScrubber(state)
	got := s.scrub("restore: " + state + "/runs/r-1/api.sock: gone; see " + home + "/notes and " + state + "-old")
	if strings.Contains(got, state) || strings.Contains(got, home+"/") || !strings.Contains(got, "<state>/runs/r-1/api.sock") {
		t.Errorf("scrub = %q", got)
	}
}

func TestUTF8Carry(t *testing.T) {
	var c utf8Carry
	parts := []string{c.text([]byte("abc \xe2\x82")), c.text([]byte("\xac euro, \xf0\x9f")), c.text([]byte("\x98\x80 smile \xff"))}
	if strings.Join(parts, "") != "abc € euro, 😀 smile \U0000FFFD" || parts[0] != "abc " {
		t.Errorf("parts %q", parts)
	}
	// An incomplete character at the very end becomes U+FFFD.
	c.text([]byte("end \xe2\x82"))
	if got := c.flush(); got != "\U0000FFFD" {
		t.Errorf("flush = %q", got)
	}
	if got := c.flush(); got != "" {
		t.Errorf("a second flush = %q", got)
	}
}

func TestPrefixAndCut(t *testing.T) {
	if got := prefix("aéb", 2); got != "a" {
		t.Errorf("prefix = %q", got)
	}
	if got := prefix("abc", 5); got != "abc" {
		t.Errorf("prefix = %q", got)
	}
	if got := cut("abcdef", 3); got != "abc…" {
		t.Errorf("cut = %q", got)
	}
}
