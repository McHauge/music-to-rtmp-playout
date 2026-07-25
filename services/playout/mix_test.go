package playout

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

func writeClip(t *testing.T, dir, name string, samples int, val int16) string {
	t.Helper()
	buf := make([]byte, samples*2)
	for i := 0; i < samples; i++ {
		binary.LittleEndian.PutUint16(buf[i*2:], uint16(val))
	}
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, buf, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestClipSaturates(t *testing.T) {
	tests := []struct {
		in, want int32
	}{
		{0, 0},
		{32767, 32767}, // exactly the max is untouched
		{32768, 32767}, // one past wraps to negative without the clamp
		{100000, 32767},
		{-32768, -32768}, // exactly the min is untouched
		{-32769, -32768},
		{-100000, -32768},
	}
	for _, tt := range tests {
		if got := clip(tt.in); got != tt.want {
			t.Errorf("clip(%d) = %d, want %d", tt.in, got, tt.want)
		}
	}
}

// The mix sums in int32 and hard-clips, so two loud sources must saturate
// rather than wrap around into the opposite sign — a wrap is an audible crack.
func TestMixIntoSaturatesInsteadOfWrapping(t *testing.T) {
	dir := t.TempDir()
	loud := writeClip(t, dir, "loud.pcm", chunkBytes/2, 30000)

	m := &voiceMixer{}
	if err := m.Trigger("loud", loud, 1.0); err != nil {
		t.Fatal(err)
	}
	// Program audio already near full scale; the clip pushes it past int16.
	dst := make([]byte, chunkBytes)
	for i := 0; i+1 < len(dst); i += 2 {
		binary.LittleEndian.PutUint16(dst[i:], uint16(int16(20000)))
	}
	m.mixInto(dst)

	for i := 0; i+1 < len(dst); i += 2 {
		if got := int16(binary.LittleEndian.Uint16(dst[i:])); got != 32767 {
			t.Fatalf("sample %d = %d, want 32767 (20000+30000 must clamp, not wrap)", i/2, got)
		}
	}
}

func TestMixIntoNegativeSaturation(t *testing.T) {
	dir := t.TempDir()
	quiet := writeClip(t, dir, "quiet.pcm", chunkBytes/2, -30000)

	m := &voiceMixer{}
	if err := m.Trigger("quiet", quiet, 1.0); err != nil {
		t.Fatal(err)
	}
	program := int16(-20000)
	dst := make([]byte, chunkBytes)
	for i := 0; i+1 < len(dst); i += 2 {
		binary.LittleEndian.PutUint16(dst[i:], uint16(program))
	}
	m.mixInto(dst)

	for i := 0; i+1 < len(dst); i += 2 {
		if got := int16(binary.LittleEndian.Uint16(dst[i:])); got != -32768 {
			t.Fatalf("sample %d = %d, want -32768", i/2, got)
		}
	}
}

// mixInto on an idle mixer must leave the program audio exactly as it was.
func TestMixIntoWithNoVoicesIsAPassthrough(t *testing.T) {
	m := &voiceMixer{}
	if got := m.active(); got != 0 {
		t.Fatalf("fresh mixer active=%d, want 0", got)
	}
	dst := make([]byte, chunkBytes)
	for i := 0; i+1 < len(dst); i += 2 {
		binary.LittleEndian.PutUint16(dst[i:], uint16(int16(i)))
	}
	want := append([]byte(nil), dst...)
	m.mixInto(dst)
	if !bytes.Equal(dst, want) {
		t.Error("mixInto with no voices modified the program audio")
	}
}

// A clip shorter than one chunk must drain and drop itself, not linger or read
// past its own PCM.
func TestVoiceDropsWhenExhausted(t *testing.T) {
	dir := t.TempDir()
	short := writeClip(t, dir, "short.pcm", 8, 1000) // 8 samples, far under a chunk

	m := &voiceMixer{}
	if err := m.Trigger("s", short, 1.0); err != nil {
		t.Fatal(err)
	}
	dst := silence(chunkBytes)
	m.mixInto(dst)
	if got := m.active(); got != 0 {
		t.Fatalf("active=%d after the clip drained, want 0", got)
	}
	if got := int16(binary.LittleEndian.Uint16(dst[0:])); got != 1000 {
		t.Errorf("first sample = %d, want 1000", got)
	}
	// Everything past the clip's length stays silent.
	if got := int16(binary.LittleEndian.Uint16(dst[16:])); got != 0 {
		t.Errorf("sample past the clip end = %d, want 0", got)
	}
}

func TestRetriggerCancelsPrevious(t *testing.T) {
	dir := t.TempDir()
	// Clip long enough to span many chunks.
	clipA := writeClip(t, dir, "a.pcm", chunkBytes/2*10, 1000)

	m := &voiceMixer{}
	if err := m.Trigger("a", clipA, 1.0); err != nil {
		t.Fatal(err)
	}
	dst := make([]byte, chunkBytes)
	m.mixInto(dst)
	if got := m.active(); got != 1 {
		t.Fatalf("after first trigger: active=%d, want 1", got)
	}

	// Retrigger same key: old voice fades, new voice starts.
	if err := m.Trigger("a", clipA, 1.0); err != nil {
		t.Fatal(err)
	}
	if got := m.active(); got != 2 {
		t.Fatalf("right after retrigger: active=%d, want 2 (old fading + new)", got)
	}
	// One 20ms chunk fully covers the 15ms fade — old voice must be gone.
	dst = silence(chunkBytes)
	m.mixInto(dst)
	if got := m.active(); got != 1 {
		t.Fatalf("after fade chunk: active=%d, want 1", got)
	}
	// Fade ramp: first sample of the chunk should carry old voice near full
	// gain plus the new voice; well past 15ms only the new voice remains.
	first := int16(binary.LittleEndian.Uint16(dst[0:]))
	lastIdx := (chunkBytes/2 - 1) * 2
	last := int16(binary.LittleEndian.Uint16(dst[lastIdx:]))
	if first <= 1000 {
		t.Fatalf("first sample %d: expected old(~fading)+new > 1000", first)
	}
	if last != 1000 {
		t.Fatalf("last sample %d: expected only new voice (1000)", last)
	}

	// Different keys still layer.
	clipB := writeClip(t, dir, "b.pcm", chunkBytes/2*10, 500)
	if err := m.Trigger("b", clipB, 1.0); err != nil {
		t.Fatal(err)
	}
	dst = silence(chunkBytes)
	m.mixInto(dst)
	if got := m.active(); got != 2 {
		t.Fatalf("two different keys: active=%d, want 2", got)
	}
	sum := int16(binary.LittleEndian.Uint16(dst[0:]))
	if sum != 1500 {
		t.Fatalf("layered sample=%d, want 1500", sum)
	}
}
