package vmm

import (
	"bytes"
	"sync"
)

// RingBuffer is an io.Writer that keeps only the last few bytes written
// to it (how many is fixed when it is made). It is safe for concurrent use.
//
// We use it for the guest's serial console. The guest controls what is
// printed there, so it could print without end: an unbounded buffer (or
// file) would let it fill the host's memory (or disk), and a pipe that
// nobody reads would block Firecracker. A ring buffer is always read and
// never grows.
//
// It works like a circular track: writing goes round and round, and each
// new byte overwrites the oldest one.
type RingBuffer struct {
	mu   sync.Mutex
	buf  []byte // fixed size; the "track"
	next int    // where the next byte goes
	full bool   // the track has gone all the way round at least once
}

// NewRingBuffer returns a RingBuffer that keeps the last size bytes.
func NewRingBuffer(size int) *RingBuffer {
	return &RingBuffer{buf: make([]byte, size)}
}

// Write keeps p (or its last bytes, if p alone is too long). It never
// fails and never blocks for long.
func (r *RingBuffer) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	n := len(p)
	if len(r.buf) == 0 {
		return n, nil
	}
	if len(p) > len(r.buf) {
		p = p[len(p)-len(r.buf):] // older bytes would be overwritten anyway
	}
	for len(p) > 0 {
		k := copy(r.buf[r.next:], p)
		p = p[k:]
		r.next += k
		if r.next == len(r.buf) { // reached the end of the track: go round
			r.next = 0
			r.full = true
		}
	}
	return n, nil
}

// Bytes returns a copy of the bytes kept, oldest first.
func (r *RingBuffer) Bytes() []byte {
	r.mu.Lock()
	defer r.mu.Unlock()

	if !r.full {
		return bytes.Clone(r.buf[:r.next])
	}
	out := make([]byte, 0, len(r.buf))
	out = append(out, r.buf[r.next:]...) // the oldest part
	return append(out, r.buf[:r.next]...)
}
