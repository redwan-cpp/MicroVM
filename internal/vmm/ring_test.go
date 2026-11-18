package vmm

import (
	"strings"
	"testing"
)

func TestRingBuffer(t *testing.T) {
	tests := []struct {
		name   string
		size   int
		writes []string
		want   string
	}{
		{"empty", 8, nil, ""},
		{"less than the size", 8, []string{"abc"}, "abc"},
		{"exactly the size", 8, []string{"abcdefgh"}, "abcdefgh"},
		{"one write longer than the size", 8, []string{"0123456789"}, "23456789"},
		{"wrapping in small writes", 8, []string{"abcde", "fghij", "kl"}, "efghijkl"},
		{"filling exactly, then more", 4, []string{"abcd", "e"}, "bcde"},
		{"many rounds", 3, []string{strings.Repeat("xy", 50), "z"}, "xyz"},
		{"size zero keeps nothing", 0, []string{"abc"}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := NewRingBuffer(tt.size)
			for _, w := range tt.writes {
				if n, err := r.Write([]byte(w)); n != len(w) || err != nil {
					t.Fatalf("Write(%q) = %d, %v; want %d, nil", w, n, err, len(w))
				}
			}
			if got := string(r.Bytes()); got != tt.want {
				t.Errorf("Bytes() = %q, want %q", got, tt.want)
			}
		})
	}
}
