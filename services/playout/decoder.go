package playout

import (
	"io"
	"os/exec"
	"time"
)

// decoder runs a short-lived ffmpeg that normalizes one song file to canonical
// 48k/stereo/s16le PCM, feeding a ring buffer. Decoupling decode from the mix
// tick (via the ring) means decode jitter never stalls the stream.
type decoder struct {
	cmd   *exec.Cmd
	ring  *RingBuffer
	done  chan struct{}
	abort chan struct{} // closed by Stop; unblocks a reader stuck on a full ring
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
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
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

// Stop kills the ffmpeg process (used on skip/stop) and reaps it.
func (d *decoder) Stop() {
	close(d.abort)
	if d.cmd != nil && d.cmd.Process != nil {
		_ = d.cmd.Process.Kill()
	}
	<-d.done
	_ = d.cmd.Wait() // reap zombie
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
