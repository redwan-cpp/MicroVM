package agent

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLookPath(t *testing.T) {
	first, second := t.TempDir(), t.TempDir()
	write := func(path string, mode os.FileMode) {
		t.Helper()
		if err := os.WriteFile(path, []byte("#!/bin/sh\n"), mode); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(first, "not-executable"), 0o644)
	write(filepath.Join(second, "not-executable"), 0o755)
	write(filepath.Join(first, "tool"), 0o755)
	write(filepath.Join(second, "tool"), 0o755)
	if err := os.Mkdir(filepath.Join(first, "a-folder"), 0o755); err != nil { //nolint:gosec // G301: an ordinary test folder
		t.Fatal(err)
	}
	dirs := []string{"", first, second}

	// The first match wins; files without an execute bit and folders are
	// skipped; a name with a "/" is used as it is.
	tests := []struct {
		name    string
		want    string
		wantErr bool
	}{
		{"tool", filepath.Join(first, "tool"), false},
		{"not-executable", filepath.Join(second, "not-executable"), false},
		{"a-folder", "", true},
		{"missing", "", true},
		{"./relative", "./relative", false},
		{"/bin/sh", "/bin/sh", false},
	}
	for _, tt := range tests {
		got, err := lookPath(tt.name, dirs)
		if (err != nil) != tt.wantErr || got != tt.want {
			t.Errorf("lookPath(%q) = %q, %v; want %q, error %v", tt.name, got, err, tt.want, tt.wantErr)
		}
	}
}

func TestExecLimits(t *testing.T) {
	tests := []struct {
		timeoutMS   uint32
		maxOutput   uint64
		wantTimeout time.Duration
		wantOutput  uint64
		wantErr     bool
	}{
		{0, 0, DefaultTimeout, DefaultOutputLimit, false},
		{250, 4096, 250 * time.Millisecond, 4096, false},
		{uint32(MaxTimeout / time.Millisecond), MaxOutputLimit, MaxTimeout, MaxOutputLimit, false},
		{uint32(MaxTimeout/time.Millisecond) + 1, 0, 0, 0, true},
		{0, MaxOutputLimit + 1, 0, 0, true},
	}
	for _, tt := range tests {
		timeout, maxOutput, err := ExecLimits(tt.timeoutMS, tt.maxOutput)
		if (err != nil) != tt.wantErr || timeout != tt.wantTimeout || maxOutput != tt.wantOutput {
			t.Errorf("ExecLimits(%d, %d) = %v, %d, %v; want %v, %d, error %v",
				tt.timeoutMS, tt.maxOutput, timeout, maxOutput, err, tt.wantTimeout, tt.wantOutput, tt.wantErr)
		}
	}
}
