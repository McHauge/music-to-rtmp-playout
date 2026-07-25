package playout

import (
	"encoding/binary"
	"errors"
	"testing"
	"time"

	"music-to-rtmp-playout/services"
)

// newTestPlayer builds a player over the given items with a live prewarm
// channel. Tests here use only break/gate items so no decoder (ffmpeg) is ever
// spawned — the flow state machine is exercised without external processes.
func newTestPlayer(items []services.FlowItem) *player {
	p := &player{
		items:      items,
		queued:     -1,
		prewarmIdx: -1,
		prewarmCh:  make(chan prewarmResult, 1),
	}
	p.loadItem()
	return p
}

// drainBreak pumps program chunks until the current break item completes —
// i.e. the playhead advances, parks in a hold, or the show finishes. Returns
// the number of chunks it took, or -1 if it never resolved (guards a bad loop).
func drainBreak(p *player) int {
	out := make([]byte, chunkBytes)
	startIdx := p.idx
	for i := 0; i < 100000; i++ {
		for j := range out {
			out[j] = 0
		}
		p.fill(out)
		if p.idx != startIdx || p.holding || p.finished {
			return i + 1
		}
	}
	return -1
}

func TestApplyFadeIn(t *testing.T) {
	// A rising ramp: the first sample of the ramp is near zero gain, the last
	// approaches full gain.
	buf := make([]byte, 8) // 4 samples
	for i := 0; i < 4; i++ {
		binary.LittleEndian.PutUint16(buf[i*2:], uint16(int16(1000)))
	}
	total := int64(8)
	rem := applyFade(buf, total, total, true)
	if rem != 0 {
		t.Fatalf("rem=%d, want 0 after consuming the whole ramp", rem)
	}
	first := int16(binary.LittleEndian.Uint16(buf[0:]))
	last := int16(binary.LittleEndian.Uint16(buf[6:]))
	if first >= last {
		t.Fatalf("fade-in should rise: first=%d last=%d", first, last)
	}
}

func TestApplyFadeOutMutesPastRamp(t *testing.T) {
	buf := make([]byte, 8)
	for i := 0; i < 4; i++ {
		binary.LittleEndian.PutUint16(buf[i*2:], uint16(int16(1000)))
	}
	// Ramp shorter than the buffer: samples past the ramp end are muted.
	rem := applyFade(buf, 4, 4, false)
	if rem != 0 {
		t.Fatalf("rem=%d, want 0", rem)
	}
	last := int16(binary.LittleEndian.Uint16(buf[6:]))
	if last != 0 {
		t.Fatalf("sample past fade-out ramp should be muted, got %d", last)
	}
}

func TestBreakThenAutoNextAdvances(t *testing.T) {
	items := []services.FlowItem{
		{Type: services.ItemBreak, BreakSec: 1, AutoNext: true},
		{Type: services.ItemBreak, BreakSec: 1, AutoNext: false},
	}
	p := newTestPlayer(items)
	if p.idx != 0 || p.breakRem != bytesPerSec {
		t.Fatalf("initial: idx=%d breakRem=%d, want 0/%d", p.idx, p.breakRem, bytesPerSec)
	}
	drainBreak(p)
	// AutoNext break auto-advances to item 1.
	if p.idx != 1 {
		t.Fatalf("after auto-next break: idx=%d, want 1", p.idx)
	}
	if !p.played[0] {
		t.Fatalf("item 0 should be marked played")
	}
}

func TestBreakWithoutAutoNextHolds(t *testing.T) {
	items := []services.FlowItem{
		{Type: services.ItemBreak, BreakSec: 1, AutoNext: false},
		{Type: services.ItemBreak, BreakSec: 1},
	}
	p := newTestPlayer(items)
	drainBreak(p)
	if !p.holding {
		t.Fatalf("non-auto-next break should park in holding")
	}
	if p.idx != 0 {
		t.Fatalf("holding break should stay on idx 0, got %d", p.idx)
	}
	// Release advances to the next item.
	p.release()
	if p.holding || p.idx != 1 {
		t.Fatalf("after release: holding=%v idx=%d, want false/1", p.holding, p.idx)
	}
}

func TestGateHolds(t *testing.T) {
	items := []services.FlowItem{
		{Type: services.ItemGate, Label: "Top of hour"},
		{Type: services.ItemBreak, BreakSec: 1},
	}
	p := newTestPlayer(items)
	if !p.holding {
		t.Fatalf("gate should enter holding immediately on load")
	}
	if got := p.nowPlayingDesc(); got != "⏸ Hold: Top of hour" {
		t.Fatalf("gate desc=%q", got)
	}
	if p.bannerVisible() {
		t.Fatalf("banner must be hidden at a gate")
	}
	p.release()
	if p.holding || p.idx != 1 {
		t.Fatalf("after release: holding=%v idx=%d", p.holding, p.idx)
	}
}

func TestJumpToClampsBounds(t *testing.T) {
	items := []services.FlowItem{
		{Type: services.ItemBreak, BreakSec: 1},
		{Type: services.ItemBreak, BreakSec: 1},
		{Type: services.ItemGate},
	}
	p := newTestPlayer(items)
	p.jumpTo(99) // past the end → clamp to last
	if p.idx != 2 {
		t.Fatalf("jumpTo(99) idx=%d, want 2 (clamped)", p.idx)
	}
	p.jumpTo(-5) // before the start → clamp to 0
	if p.idx != 0 {
		t.Fatalf("jumpTo(-5) idx=%d, want 0 (clamped)", p.idx)
	}
}

func TestQueueNextToggleAndNextIdx(t *testing.T) {
	items := []services.FlowItem{
		{Type: services.ItemBreak, BreakSec: 1},
		{Type: services.ItemBreak, BreakSec: 1},
		{Type: services.ItemGate},
	}
	p := newTestPlayer(items)
	if p.nextIdx() != 1 {
		t.Fatalf("default nextIdx=%d, want 1", p.nextIdx())
	}
	p.queueNext(2)
	if p.queued != 2 || p.nextIdx() != 2 {
		t.Fatalf("after queueNext(2): queued=%d nextIdx=%d", p.queued, p.nextIdx())
	}
	// Clicking the queued item again cancels the queue.
	p.queueNext(2)
	if p.queued != -1 || p.nextIdx() != 1 {
		t.Fatalf("after cancel: queued=%d nextIdx=%d", p.queued, p.nextIdx())
	}
}

func TestFinishedAfterLastItem(t *testing.T) {
	items := []services.FlowItem{
		{Type: services.ItemBreak, BreakSec: 1, AutoNext: true},
	}
	p := newTestPlayer(items)
	drainBreak(p)
	// Auto-next off the end finishes the show.
	if !p.finished {
		t.Fatalf("expected finished after the last auto-next item")
	}
	if got := p.nowPlayingDesc(); got != "Show finished" {
		t.Fatalf("desc=%q, want 'Show finished'", got)
	}
}

// A queueNext that discards an in-flight prewarm cannot cancel the helper
// goroutine, and servicePrewarm then aims a second spawn at the new target — so
// more than one delivery can be outstanding when the operator stops the show.
// shutdown must drain every one of them: an undrained result holds a decoder
// nobody will ever Stop, whose ffmpeg then outlives the show.
func TestShutdownDrainsEveryInFlightSpawn(t *testing.T) {
	p := &player{queued: -1, prewarmIdx: -1, prewarmCh: make(chan prewarmResult, 1)}
	p.spawnsInFlight = 2

	const spawns = 2
	delivered := make(chan int, spawns)
	for i := 0; i < spawns; i++ {
		go func(i int) {
			p.prewarmCh <- prewarmResult{idx: i} // blocks until the drain consumes it
			delivered <- i
		}(i)
	}

	p.shutdown()

	for i := 0; i < spawns; i++ {
		select {
		case <-delivered:
		case <-time.After(5 * time.Second):
			t.Fatalf("shutdown drained %d of %d in-flight spawns — the rest leak an ffmpeg each", i, spawns)
		}
	}
	if p.spawnsInFlight != 0 {
		t.Errorf("spawnsInFlight=%d after shutdown, want 0", p.spawnsInFlight)
	}
}

// Every delivery servicePrewarm consumes must decrement the in-flight count,
// including one for a target that has already been discarded — otherwise
// shutdown over-drains and blocks a goroutine forever.
func TestServicePrewarmDecrementsSpawnCount(t *testing.T) {
	p := &player{
		items:      []services.FlowItem{{Type: services.ItemBreak, BreakSec: 1}},
		queued:     -1,
		prewarmIdx: -1, // the prewarm this result targets was discarded
		prewarmCh:  make(chan prewarmResult, 1),
	}
	p.spawnsInFlight = 1
	p.prewarmCh <- prewarmResult{idx: 7, err: errors.New("spawn failed")}

	p.servicePrewarm()

	if p.spawnsInFlight != 0 {
		t.Errorf("spawnsInFlight=%d after consuming one delivery, want 0", p.spawnsInFlight)
	}
}
