package playout

import (
	"bytes"
	"testing"
)

func TestRingBufferWriteReadRoundTrip(t *testing.T) {
	rb := NewRingBuffer(8)
	src := []byte{1, 2, 3, 4}
	if n := rb.Write(src); n != 4 {
		t.Fatalf("Write=%d, want 4", n)
	}
	if got := rb.Available(); got != 4 {
		t.Fatalf("Available=%d, want 4", got)
	}
	dst := make([]byte, 4)
	if n := rb.Read(dst); n != 4 {
		t.Fatalf("Read=%d, want 4", n)
	}
	if !bytes.Equal(dst, src) {
		t.Fatalf("Read=%v, want %v", dst, src)
	}
	if got := rb.Available(); got != 0 {
		t.Fatalf("Available after drain=%d, want 0", got)
	}
}

func TestRingBufferOverflowBackPressure(t *testing.T) {
	rb := NewRingBuffer(4)
	// Only 4 of 6 bytes fit; overflow is dropped and reported via short count.
	if n := rb.Write([]byte{1, 2, 3, 4, 5, 6}); n != 4 {
		t.Fatalf("Write=%d, want 4 (buffer full)", n)
	}
	if got := rb.Available(); got != 4 {
		t.Fatalf("Available=%d, want 4", got)
	}
	// Full buffer accepts nothing more.
	if n := rb.Write([]byte{7}); n != 0 {
		t.Fatalf("Write into full buffer=%d, want 0", n)
	}
}

func TestRingBufferWrapAround(t *testing.T) {
	rb := NewRingBuffer(4)
	rb.Write([]byte{1, 2, 3, 4})
	// Drain 2, freeing space at the front; the next write must wrap.
	dst := make([]byte, 2)
	rb.Read(dst)
	if n := rb.Write([]byte{5, 6}); n != 2 {
		t.Fatalf("Write after partial drain=%d, want 2", n)
	}
	// Remaining reads should come out in FIFO order across the wrap point.
	out := make([]byte, 4)
	if n := rb.Read(out); n != 4 {
		t.Fatalf("Read=%d, want 4", n)
	}
	if !bytes.Equal(out, []byte{3, 4, 5, 6}) {
		t.Fatalf("Read=%v, want [3 4 5 6]", out)
	}
}

func TestRingBufferShortRead(t *testing.T) {
	rb := NewRingBuffer(8)
	rb.Write([]byte{1, 2})
	dst := make([]byte, 8)
	// Read returns only what is available, not len(dst).
	if n := rb.Read(dst); n != 2 {
		t.Fatalf("Read=%d, want 2 (short)", n)
	}
}

func TestRingBufferCloseAndDrained(t *testing.T) {
	rb := NewRingBuffer(8)
	rb.Write([]byte{1, 2, 3})
	if rb.Closed() {
		t.Fatal("Closed=true before Close")
	}
	if rb.Drained() {
		t.Fatal("Drained=true with unread bytes")
	}
	rb.Close()
	if !rb.Closed() {
		t.Fatal("Closed=false after Close")
	}
	// Closed but bytes remain: not drained yet.
	if rb.Drained() {
		t.Fatal("Drained=true with unread bytes after Close")
	}
	rb.Read(make([]byte, 3))
	if !rb.Drained() {
		t.Fatal("Drained=false after closing and reading all bytes")
	}
}
