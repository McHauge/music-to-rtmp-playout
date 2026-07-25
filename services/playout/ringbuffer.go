package playout

import "sync"

// RingBuffer is a simple bounded byte ring with blocking-free reads. A reader
// goroutine (fed by the per-song ffmpeg decoder) fills it; the mixer drains it.
// If the mixer outpaces the decoder, Read returns a short count and the mixer
// pads with silence rather than stalling the stream.
type RingBuffer struct {
	mu     sync.Mutex
	buf    []byte
	size   int
	r, w   int
	count  int
	closed bool // decoder finished writing
}

func NewRingBuffer(size int) *RingBuffer {
	return &RingBuffer{buf: make([]byte, size), size: size}
}

// Write copies as much of p as fits and returns the bytes written. Nothing is
// dropped: a short return is the back-pressure signal — the mixer owns the true
// clock, so the decoder waits and retries the remainder (see decoder.go).
//
// The copy runs in at most two block moves (up to the end of the ring, then the
// wrapped remainder) rather than byte by byte: this is on the audio path at
// ~200 calls/sec under the lock.
func (rb *RingBuffer) Write(p []byte) int {
	rb.mu.Lock()
	defer rb.mu.Unlock()
	n := len(p)
	if free := rb.size - rb.count; n > free {
		n = free
	}
	if n == 0 {
		return 0 // also guards the modulo below against a zero-size ring
	}
	first := copy(rb.buf[rb.w:], p[:n])
	copy(rb.buf, p[first:n]) // no-op unless the write wrapped
	rb.w = (rb.w + n) % rb.size
	rb.count += n
	return n
}

// Read pulls up to len(p) bytes; returns the count actually available.
func (rb *RingBuffer) Read(p []byte) int {
	rb.mu.Lock()
	defer rb.mu.Unlock()
	n := len(p)
	if n > rb.count {
		n = rb.count
	}
	if n == 0 {
		return 0 // also guards the modulo below against a zero-size ring
	}
	first := copy(p[:n], rb.buf[rb.r:])
	copy(p[first:n], rb.buf) // no-op unless the read wrapped
	rb.r = (rb.r + n) % rb.size
	rb.count -= n
	return n
}

// Available returns the number of readable bytes.
func (rb *RingBuffer) Available() int {
	rb.mu.Lock()
	defer rb.mu.Unlock()
	return rb.count
}

// Closed reports whether the writer has finished, regardless of unread bytes.
func (rb *RingBuffer) Closed() bool {
	rb.mu.Lock()
	defer rb.mu.Unlock()
	return rb.closed
}

// Close marks the writer as finished. Drained reads after close report EOF via Drained.
func (rb *RingBuffer) Close() {
	rb.mu.Lock()
	rb.closed = true
	rb.mu.Unlock()
}

// Drained reports true when the writer is closed and all bytes have been read.
func (rb *RingBuffer) Drained() bool {
	rb.mu.Lock()
	defer rb.mu.Unlock()
	return rb.closed && rb.count == 0
}
