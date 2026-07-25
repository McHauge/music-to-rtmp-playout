package playout

import (
	"fmt"
	"io"
	"log"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// decoderStderrLimit caps the retained ffmpeg diagnostics per decoder; the
// first few lines are the useful ones.
const decoderStderrLimit = 4 << 10

// boundedBuffer collects up to limit bytes and discards the rest, so a chatty
// or looping subprocess cannot grow it without bound. Safe for concurrent use
// because exec writes to it from its own goroutine while Stop may read it.
type boundedBuffer struct {
	mu    sync.Mutex
	buf   []byte
	limit int
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	b.mu.Lock()
	defer b.mu.Unlock()
	if room := b.limit - len(b.buf); room > 0 {
		if len(p) > room {
			p = p[:room]
		}
		b.buf = append(b.buf, p...)
	}
	return n, nil // report a full write even when truncating; dropping is intentional
}

func (b *boundedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return strings.TrimSpace(string(b.buf))
}

// decoder runs a short-lived ffmpeg that normalizes one song file to canonical
// 48k/stereo/s16le PCM, feeding a ring buffer. Decoupling decode from the mix
// tick (via the ring) means decode jitter never stalls the stream.
type decoder struct {
	cmd      *exec.Cmd
	ring     *RingBuffer
	done     chan struct{}
	abort    chan struct{} // closed by Stop; unblocks a reader stuck on a full ring
	stopOnce sync.Once     // Stop is idempotent: close(abort) must not panic on a second call
}

// startDecoder spawns the ffmpeg decoder for filePath. ringSize bytes of
// look-ahead are buffered (≈ a few hundred ms).
func startDecoder(ffmpegPath, filePath string, ringSize int) (*decoder, error) {
	cmd := exec.Command(ffmpegPath, "-hide_banner", "-loglevel", "error",
		"-i", filePath,
		"-ar", "48000", "-ac", "2", "-f", "s16le",
		"pipe:1")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("decoder stdout pipe: %w", err)
	}
	// Capture ffmpeg's diagnostics instead of discarding them into a nil writer:
	// without this a corrupt or unsupported file gives no clue why a song came
	// out short. Bounded so a pathological file can't grow it without limit.
	stderr := &boundedBuffer{limit: decoderStderrLimit}
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("decoder ffmpeg start: %w", err)
	}

	d := &decoder{cmd: cmd, ring: NewRingBuffer(ringSize), done: make(chan struct{}), abort: make(chan struct{})}

	go func() {
		defer close(d.done)
		defer d.ring.Close()
		buf := make([]byte, 16*1024)
		for {
			n, err := stdout.Read(buf)
			if n > 0 {
				// Push into the ring, back-pressuring if the mixer is behind.
				written := 0
				for written < n {
					w := d.ring.Write(buf[written:n])
					written += w
					if written < n {
						select {
						case <-d.abort:
							return // Stop called; no one will drain the ring
						case <-time.After(5 * time.Millisecond):
						}
					}
				}
			}
			if err == io.EOF {
				return
			}
			if err != nil {
				// Closing the ring here is indistinguishable from a clean
				// end-of-song to the engine, which just advances — so say what
				// actually happened, with whatever ffmpeg reported.
				if msg := stderr.String(); msg != "" {
					log.Printf("playout: decode of %s ended early: %v — ffmpeg said: %s", filePath, err, msg)
				} else {
					log.Printf("playout: decode of %s ended early: %v", filePath, err)
				}
				return
			}
		}
	}()

	return d, nil
}

// Read pulls decoded PCM. Returns bytes available (may be short).
func (d *decoder) Read(p []byte) int { return d.ring.Read(p) }

// Finished reports true once ffmpeg has produced everything and the ring is empty.
func (d *decoder) Finished() bool { return d.ring.Drained() }

// Ready reports whether at least min bytes of PCM are primed in the ring (or
// the whole file has been decoded, for clips shorter than min). Used to gate
// the swap onto a freshly spawned decoder so playback starts clean.
func (d *decoder) Ready(min int) bool { return d.ring.Available() >= min || d.ring.Closed() }

// Stop kills the ffmpeg process (used on skip/stop) and reaps it. Idempotent —
// the shutdown drain and a normal reap can both land on the same decoder.
func (d *decoder) Stop() {
	d.stopOnce.Do(func() {
		close(d.abort)
		if d.cmd != nil && d.cmd.Process != nil {
			_ = d.cmd.Process.Kill()
		}
		<-d.done
		_ = d.cmd.Wait() // reap zombie
	})
}

// reapAsync stops a decoder on a background goroutine so its blocking
// close(abort)+Kill()+Wait() never stalls the run goroutine at a song boundary.
// The goroutine owns d exclusively — the caller must have already dropped its
// reference (nil'd the field) — and exits once Stop returns, so it cannot leak.
func reapAsync(d *decoder) {
	if d == nil {
		return
	}
	go d.Stop()
}
