package playout

import (
	"encoding/binary"
	"os"
	"sync"
)

// Audio constants — canonical pipeline format (must match SoundboardService).
const (
	sampleRate     = 48000
	channels       = 2
	bytesPerSample = 2 // s16le
	frameBytes     = channels * bytesPerSample
	// 20 ms chunk: low latency for soundboard/skip responsiveness.
	chunkFrames = sampleRate / 50
	chunkBytes  = chunkFrames * frameBytes
)

// voice is one playing soundboard clip mixed on top of the main stream.
type voice struct {
	pcm  []byte
	pos  int
	gain float64
}

// voiceMixer holds the set of currently-playing soundboard voices and mixes
// them into the main PCM chunk. Safe for concurrent Trigger + mix.
type voiceMixer struct {
	mu     sync.Mutex
	voices []*voice
}

// Trigger loads a pre-decoded PCM clip from disk and starts it as a new voice.
func (m *voiceMixer) Trigger(pcmPath string, gain float64) error {
	data, err := os.ReadFile(pcmPath)
	if err != nil {
		return err
	}
	m.mu.Lock()
	m.voices = append(m.voices, &voice{pcm: data, gain: gain})
	m.mu.Unlock()
	return nil
}

// active reports how many voices are currently playing.
func (m *voiceMixer) active() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.voices)
}

// mixInto sums all active voices into dst (an s16le chunk already containing
// the main program audio). Finished voices are dropped. Sums in int32 then
// hard-clips to int16 — with per-source headroom this rarely clips audibly.
func (m *voiceMixer) mixInto(dst []byte) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.voices) == 0 {
		return
	}
	n := len(dst) / 2 // number of int16 samples
	alive := m.voices[:0]
	for _, v := range m.voices {
		remaining := (len(v.pcm) - v.pos) / 2
		take := n
		if remaining < take {
			take = remaining
		}
		for i := 0; i < take; i++ {
			di := i * 2
			main := int32(int16(binary.LittleEndian.Uint16(dst[di:])))
			s := int16(binary.LittleEndian.Uint16(v.pcm[v.pos+di:]))
			sum := main + int32(float64(s)*v.gain)
			binary.LittleEndian.PutUint16(dst[di:], uint16(int16(clip(sum))))
		}
		v.pos += take * 2
		if v.pos < len(v.pcm) {
			alive = append(alive, v)
		}
	}
	m.voices = alive
}

func clip(v int32) int32 {
	if v > 32767 {
		return 32767
	}
	if v < -32768 {
		return -32768
	}
	return v
}

// silence returns a zeroed chunk of the given byte length.
func silence(n int) []byte { return make([]byte, n) }
