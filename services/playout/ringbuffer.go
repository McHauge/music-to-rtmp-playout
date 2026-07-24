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

// Write copies as much of p as fits; returns bytes written. Overflow is
// dropped (the mixer controls the true clock, so the decoder is throttled by
// this back-pressure: Write returns < len(p) when full and the caller retries).
func (rb *RingBuffer) Write(p []byte) int {
	rb.mu.Lock()
	defer rb.mu.Unlock()
	n := 0
	for n < len(p) && rb.count < rb.size {
		rb.buf[rb.w] = p[n]
		rb.w = (rb.w + 1) % rb.size
		rb.count++
		n++
	}
	return n
}

// Read pulls up to len(p) bytes; returns the count actually available.
func (rb *RingBuffer) Read(p []byte) int {
	rb.mu.Lock()
	defer rb.mu.Unlock()
	n := 0
	for n < len(p) && rb.count > 0 {
		p[n] = rb.buf[rb.r]
		rb.r = (rb.r + 1) % rb.size
		rb.count--
		n++
	}
	return n
}

// Available returns the number of readable bytes.
func (rb *RingBuffer) Available() int {
	rb.mu.Lock()
	defer rb.mu.Unlock()
	return rb.count
}

// Free returns the number of writable bytes.
func (rb *RingBuffer) Free() int {
	rb.mu.Lock()
	defer rb.mu.Unlock()
	return rb.size - rb.count
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
