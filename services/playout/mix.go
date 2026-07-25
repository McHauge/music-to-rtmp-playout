package playout

import (
	"encoding/binary"
	"os"
	"sync"

	"music-to-rtmp-playout/services"
)

// Audio constants — the canonical pipeline format, 48 kHz / stereo / s16le end
// to end. Aliased from the services package rather than restated, so the
// soundboard's decoded PCM and the mix math cannot drift apart: any mismatch
// would corrupt the sum and the encoder's stdin framing.
const (
	sampleRate     = services.SampleRate
	channels       = services.Channels
	bytesPerSample = services.BytesPerSample
	frameBytes     = channels * bytesPerSample
	bytesPerSec    = sampleRate * frameBytes // 192000
	// 20 ms chunk: low latency for soundboard/skip responsiveness.
	chunkFrames = sampleRate / 50
	chunkBytes  = chunkFrames * frameBytes
	// 15 ms fade applied to a voice canceled by a retrigger, so the cut
	// lands on a ramp instead of an audible mid-waveform pop.
	retriggerFadeSamples = sampleRate * 15 / 1000 * channels
)

// voice is one playing soundboard clip mixed on top of the main stream.
type voice struct {
	key  string
	pcm  []byte
	pos  int
	gain float64
	// fadeTotal > 0 marks the voice as fading out; fadeRemain counts down
	// the s16 samples left before it is dropped.
	fadeTotal  int
	fadeRemain int
}

// voiceMixer holds the set of currently-playing soundboard voices and mixes
// them into the main PCM chunk. Safe for concurrent Trigger + mix.
type voiceMixer struct {
	mu     sync.Mutex
	voices []*voice
}

// Trigger loads a pre-decoded PCM clip from disk and starts it as a new voice.
// Any voice already playing under the same key is faded out first, so
// retriggering a clip restarts it from the top (hot-cue behavior) instead of
// stacking a second copy.
func (m *voiceMixer) Trigger(key, pcmPath string, gain float64) error {
	data, err := os.ReadFile(pcmPath)
	if err != nil {
		return err
	}
	m.mu.Lock()
	for _, v := range m.voices {
		if v.key == key && v.fadeTotal == 0 {
			v.fadeTotal = retriggerFadeSamples
			v.fadeRemain = retriggerFadeSamples
		}
	}
	m.voices = append(m.voices, &voice{key: key, pcm: data, gain: gain})
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
		if v.fadeTotal > 0 && v.fadeRemain < take {
			take = v.fadeRemain
		}
		for i := 0; i < take; i++ {
			di := i * 2
			g := v.gain
			if v.fadeTotal > 0 {
				g *= float64(v.fadeRemain-i) / float64(v.fadeTotal)
			}
			main := int32(int16(binary.LittleEndian.Uint16(dst[di:])))
			s := int16(binary.LittleEndian.Uint16(v.pcm[v.pos+di:]))
			sum := main + int32(float64(s)*g)
			binary.LittleEndian.PutUint16(dst[di:], uint16(int16(clip(sum))))
		}
		v.pos += take * 2
		if v.fadeTotal > 0 {
			v.fadeRemain -= take
			if v.fadeRemain <= 0 {
				continue
			}
		}
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
