package playout

import (
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
