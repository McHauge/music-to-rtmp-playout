package playout

import (
	"encoding/binary"
	"errors"
	"log"
	"time"

	"music-to-rtmp-playout/services"
)

var (
	errAlreadyRunning = errors.New("a show is already streaming")
	errNotRunning     = errors.New("no show is streaming")
)

// prewarmResult carries a decoder spawned off the run goroutine back to run.
type prewarmResult struct {
	idx int      // the item index this decoder was spawned for
	dec *decoder // nil if err != nil
	err error
}

// Manual-cut transition: a skip/jump fades the outgoing song to silence while
// the incoming song ramps in on top of it (a crossfade). All tunable; keep
// them multiples of frameBytes.
const (
	fadeOutBytes  = int64(bytesPerSec) * 8 / 10 // 0.8s fade to silence on a manual cut
	fadeInBytes   = int64(bytesPerSec) * 3 / 10 // 0.3s fade-in of the incoming song
	minPrimeBytes = bytesPerSec / 4             // ring fill required before adopting a pending decoder
)

// player tracks position within the flow. It is owned exclusively by the
// engine's run goroutine, so it needs no locking.
type player struct {
	items      []services.FlowItem
	ffmpegPath string

	idx        int
	queued     int // operator-queued next item, -1 = none; consumed by advance
	dec        *decoder
	itemBytes  int64        // program audio consumed by the current item (elapsed clock)
	breakRem   int          // remaining silence bytes for a break item
	played     map[int]bool // items the playhead has visited and left (for UI dimming)
	holding    bool         // paused at a gate or a non-auto-next end
	paused     bool         // frozen mid-item; decoder kept alive via ring back-pressure
	pauseAfter bool         // one-shot: pause when the current item completes
	finished   bool

	// Pre-warm of the next decoder, spawned off the run goroutine so the blocking
	// exec.Start never stalls the pacing loop at a song boundary. A song change
	// then becomes an in-memory swap onto an already-primed ring buffer.
	prewarmIdx int                // item index a prewarm targets (in flight OR ready); -1 = none
	prewarmDec *decoder           // ready prewarmed decoder adopted from prewarmCh; nil until ready
	prewarmCh  chan prewarmResult // buffered(1); the helper goroutine hands the decoder back to run

	// Manual-cut transition state. The outgoing decoder renders a fade tail to
	// silence that is summed on top of the incoming program (a crossfade); the
	// incoming song ramps in as soon as its decoder is primed.
	outDec       *decoder // outgoing decoder rendering its fade tail; nil when idle
	outFadeRem   int64    // bytes of fade-out ramp remaining
	outFadeTotal int64
	inFadeRem    int64 // bytes of fade-in ramp remaining on p.dec
	inFadeTotal  int64
	armFadeIn    bool   // manual cut pending: ramp in when the next audio first plays
	tailBuf      []byte // scratch chunk the fade tail renders into before mixing

	spawnStart      time.Time // when the in-flight prewarm spawn launched (latency log)
	lastUnderrunLog time.Time // throttles the ring-underrun diagnostic
}

// applyFade scales the s16le samples in buf by a linear ramp. rem is how many
// ramp bytes remain (of total); rising selects fade-in (gain grows toward 1)
// versus fade-out (gain falls toward 0, and samples past the ramp end are
// muted). Returns the ramp bytes remaining after buf.
func applyFade(buf []byte, rem, total int64, rising bool) int64 {
	if total <= 0 {
		return 0
	}
	for i := 0; i+1 < len(buf); i += 2 {
		r := rem - int64(i)
		if r < 0 {
			r = 0
		}
		g := float64(r) / float64(total)
		if rising {
			g = 1 - g
		}
		s := int16(binary.LittleEndian.Uint16(buf[i:]))
		binary.LittleEndian.PutUint16(buf[i:], uint16(int16(float64(s)*g)))
	}
	rem -= int64(len(buf))
	if rem < 0 {
		rem = 0
	}
	return rem
}

// markPlayed records that item i has been played (visited and departed), so the
// rundown can dim it. Out-of-range indices are ignored.
func (p *player) markPlayed(i int) {
	if i < 0 || i >= len(p.items) {
		return
	}
	if p.played == nil {
		p.played = map[int]bool{}
	}
	p.played[i] = true
}

// playedCopy returns an independent snapshot of the played set for publishing,
// so status readers never touch the map the run goroutine keeps mutating.
func (p *player) playedCopy() map[int]bool {
	m := make(map[int]bool, len(p.played))
	for k := range p.played {
		m[k] = true
	}
	return m
}

// loadItem prepares the source for the item at idx. Songs spawn a decoder;
// breaks set a silence countdown; gates immediately enter the holding state.
// Missing/invalid items are skipped over.
func (p *player) loadItem() {
	p.itemBytes = 0
	if p.idx >= len(p.items) {
		p.finished = true
		return
	}
	it := p.items[p.idx]
	switch it.Type {
	case services.ItemSong:
		if it.Track == nil || it.Track.FilePath == "" {
			log.Printf("playout: item %d has no track, skipping", p.idx)
			p.advance()
			return
		}
		// Adopt a ring-primed decoder the helper prepared for exactly this item.
		// The prime gate matters: the helper delivers the decoder right after
		// exec.Start, *before* the ring fills, and starting playback on a
		// trickling ring makes the fade-in stutter (ramp, underrun, ramp again).
		if p.prewarmIdx == p.idx {
			if p.prewarmDec != nil && p.prewarmDec.Ready(minPrimeBytes) {
				p.dec = p.prewarmDec
				p.prewarmDec = nil
				p.prewarmIdx = -1
			}
			// else: the spawn is in flight or still priming — leave it as a
			// pending load; servicePrewarm adopts it once primed.
			return
		}
		// No matching prewarm (manual jump to an unpredicted item, or a failed
		// spawn): drop any stale prewarm and load asynchronously. The run
		// goroutine must never block on exec.Start — p.dec stays nil (a pending
		// load) and fill emits silence until servicePrewarm adopts the decoder.
		p.discardPrewarm()
		p.prewarmIdx = p.idx
		p.launchSpawn(p.idx)
	case services.ItemBreak:
		p.breakRem = it.BreakSec * bytesPerSec
		if p.breakRem <= 0 {
			p.advance()
		}
	case services.ItemGate:
		p.holding = true
	}
}

// nextIdx is the index advance() will select next: the operator-queued item if
// set, else idx+1. Mirrors the selection in advance().
func (p *player) nextIdx() int {
	if p.queued >= 0 {
		return p.queued
	}
	return p.idx + 1
}

// shouldPrewarmNow reports whether a decoder for the next item should be kept
// spawned and ring-primed. The next song is prewarmed for the *whole* time the
// current item plays (a standing prewarm) — not just near a natural boundary —
// so a manual skip also lands on an already-primed decoder instead of paying
// the ffmpeg spawn+probe latency as dead air. Cost: one idle, back-pressured
// ffmpeg per playing item.
func (p *player) shouldPrewarmNow() bool {
	if p.finished || p.paused {
		return false
	}
	ni := p.nextIdx()
	if ni < 0 || ni >= len(p.items) {
		return false
	}
	nit := p.items[ni]
	return nit.Type == services.ItemSong && nit.Track != nil && nit.Track.FilePath != ""
}

// launchSpawn starts the decoder for item idx on a helper goroutine, delivering
// into prewarmCh. The caller must have set prewarmIdx = idx first, so at most
// one spawn is ever in flight.
func (p *player) launchSpawn(idx int) {
	ch := p.prewarmCh
	p.spawnStart = time.Now()
	ffmpeg, path := p.ffmpegPath, p.items[idx].Track.FilePath // copy — the helper must not touch player
	go func() {
		d, err := startDecoder(ffmpeg, path, ringSize)
		ch <- prewarmResult{idx: idx, dec: d, err: err}
	}()
}

// servicePrewarm runs once per tick on the run goroutine. It adopts a decoder
// the helper goroutine has finished spawning, resolves a pending load of the
// current item, then — whenever the next item is a song with no prewarm yet —
// launches its decoder off-thread (the standing prewarm), so exec.Start never
// runs on the run goroutine.
func (p *player) servicePrewarm() {
	// (1) Adopt / discard whatever the helper handed back.
	select {
	case res := <-p.prewarmCh:
		if res.err == nil && res.dec != nil && res.idx == p.prewarmIdx && p.prewarmDec == nil {
			p.prewarmDec = res.dec // ready, still targets the live prewarm target
		} else {
			if res.dec != nil {
				reapAsync(res.dec) // stale target, or a spawn we no longer want
			}
			if res.err != nil && res.idx == p.prewarmIdx {
				p.prewarmIdx = -1 // spawn failed for the live target
				if res.idx == p.idx && p.dec == nil && !p.finished {
					// The pending load for the *current* item failed — skip it,
					// matching the old synchronous-failure behavior.
					log.Printf("playout: decoder start failed for item %d: %v", res.idx, res.err)
					p.advance()
				}
			}
		}
	default:
	}

	// (2) Pending load of the current song: adopt the delivered decoder as soon
	// as its ring is primed — the "worker says I'm ready" gate that makes the
	// swap clean. Any outgoing fade tail keeps rendering on top (crossfade).
	if p.dec == nil && !p.finished && p.idx < len(p.items) &&
		p.items[p.idx].Type == services.ItemSong &&
		p.prewarmDec != nil && p.prewarmIdx == p.idx &&
		p.prewarmDec.Ready(minPrimeBytes) {
		// This path is the cold spawn (no standing prewarm predicted this item);
		// log how long the spawn actually took so the fade tunables have data.
		log.Printf("playout: cold decoder for item %d ready %.0fms after spawn", p.idx,
			time.Since(p.spawnStart).Seconds()*1000)
		p.dec = p.prewarmDec
		p.prewarmDec = nil
		p.prewarmIdx = -1
	}

	// (3) Maybe launch a new prewarm. prewarmIdx>=0 means one is in flight or ready.
	if p.prewarmIdx >= 0 || !p.shouldPrewarmNow() {
		return
	}
	ni := p.nextIdx()
	p.prewarmIdx = ni // set before launching → at most one in-flight spawn
	p.launchSpawn(ni)
}

// discardPrewarm drops a prewarm that no longer targets the right item. A ready
// decoder is reaped off-thread; an in-flight one is left to deliver into
// prewarmCh, where servicePrewarm reaps it because res.idx won't match the reset
// prewarmIdx (or the shutdown drain reaps it).
func (p *player) discardPrewarm() {
	if p.prewarmDec != nil {
		reapAsync(p.prewarmDec)
		p.prewarmDec = nil
	}
	p.prewarmIdx = -1
}

// shutdown reaps every decoder the player owns when run exits. Called via defer
// on every run return path. run is the sole owner and has stopped touching these
// fields by the time this runs.
func (p *player) shutdown() {
	if p.dec != nil {
		reapAsync(p.dec)
		p.dec = nil
	}
	if p.outDec != nil {
		reapAsync(p.outDec)
		p.outDec = nil
	}
	if p.prewarmDec != nil {
		reapAsync(p.prewarmDec)
		p.prewarmDec = nil
	}
	if p.prewarmIdx >= 0 { // a helper spawn may still be in flight
		ch := p.prewarmCh
		go func() {
			res := <-ch // the helper always delivers exactly once
			if res.dec != nil {
				res.dec.Stop()
			}
		}()
		p.prewarmIdx = -1
	}
}

// beginTransition converts a manual cut (skip/jump) of audible audio into a
// crossfade: the current decoder becomes the outgoing fade tail instead of
// being reaped hard, and the incoming song is armed to ramp in — on top of the
// tail — as soon as it starts. When nothing is audible (pending load, cued,
// paused, break) there is nothing to fade — the caller's normal reap handles
// the decoder, and only the fade-in stays armed.
func (p *player) beginTransition() {
	if p.dec != nil && p.itemBytes > 0 && !p.paused {
		if p.outDec != nil {
			reapAsync(p.outDec) // double-cut mid-fade: drop the older tail
		}
		p.outDec = p.dec
		p.dec = nil
		p.outFadeTotal = fadeOutBytes
		p.outFadeRem = fadeOutBytes
	}
	p.armFadeIn = true
	p.inFadeRem = 0
}

// fill writes one chunk of program audio into out (already zeroed = silence).
// It advances the flow as items complete, honoring auto-next vs. hold. A
// manual-cut fade tail is summed on top of the incoming program, so a skip
// crossfades instead of leaving dead air.
func (p *player) fill(out []byte) {
	if p.paused {
		return // silence; program and fade tail freeze (ring back-pressure keeps them alive)
	}
	p.programInto(out)
	p.mixTailInto(out)
}

// programInto renders the current item's audio into out (already zeroed).
func (p *player) programInto(out []byte) {
	if p.finished || p.holding {
		return // silence
	}
	it := p.items[p.idx]
	switch it.Type {
	case services.ItemSong:
		if p.dec == nil {
			// Pending load: silence until servicePrewarm adopts the primed
			// decoder (spawn failures advance() there, so this always resolves).
			return
		}
		n := p.dec.Read(out) // short reads leave the remainder as silence (underrun pad)
		if n < len(out) && !p.dec.Finished() && time.Since(p.lastUnderrunLog) > time.Second {
			p.lastUnderrunLog = time.Now()
			log.Printf("playout: song ring underrun (read %d/%d bytes) at item %d, %.1fs in",
				n, len(out), p.idx, p.itemElapsedSec())
		}
		if n > 0 && p.armFadeIn {
			p.armFadeIn = false
			p.inFadeTotal = fadeInBytes
			p.inFadeRem = fadeInBytes
		}
		if p.inFadeRem > 0 {
			p.inFadeRem = applyFade(out[:n], p.inFadeRem, p.inFadeTotal, true)
		}
		p.itemBytes += int64(n)
		if p.dec.Finished() {
			p.endOfItem(it)
		}
	case services.ItemBreak:
		take := len(out)
		if p.breakRem < take {
			take = p.breakRem
		}
		p.breakRem -= take
		p.itemBytes += int64(take)
		if p.breakRem <= 0 {
			p.endOfItem(it)
		}
		// out stays silent
	case services.ItemGate:
		p.holding = true
	}
}

// mixTailInto renders the outgoing fade tail from a manual cut and sums it on
// top of out (which already holds the incoming program), hard-clipping like the
// soundboard mixer. Rendered even while holding or finished, so a skip into a
// gate (or off the end) still lands on the ramp instead of a hard cut.
func (p *player) mixTailInto(out []byte) {
	if p.outDec == nil {
		return
	}
	if len(p.tailBuf) < len(out) {
		p.tailBuf = make([]byte, len(out))
	}
	tail := p.tailBuf[:len(out)]
	n := p.outDec.Read(tail)
	p.outFadeRem = applyFade(tail[:n], p.outFadeRem, p.outFadeTotal, false)
	for i := 0; i+1 < n; i += 2 {
		sum := int32(int16(binary.LittleEndian.Uint16(out[i:]))) +
			int32(int16(binary.LittleEndian.Uint16(tail[i:])))
		binary.LittleEndian.PutUint16(out[i:], uint16(int16(clip(sum))))
	}
	if p.outFadeRem <= 0 || p.outDec.Finished() {
		reapAsync(p.outDec)
		p.outDec = nil
		p.outFadeRem = 0
	}
}

// endOfItem is called when the current item's audio is exhausted. It either
// auto-advances or parks in the holding state for manual release.
func (p *player) endOfItem(it services.FlowItem) {
	if p.dec != nil {
		reapAsync(p.dec)
		p.dec = nil
	}
	if p.pauseAfter {
		p.pauseAfter = false
		p.advance()
		p.paused = true
		return
	}
	if it.AutoNext {
		p.advance()
	} else {
		p.holding = true
	}
}

// advance moves to the next item (the operator-queued one, if any) and loads it.
func (p *player) advance() {
	if p.dec != nil {
		reapAsync(p.dec)
		p.dec = nil
	}
	p.markPlayed(p.idx) // leaving the current item
	p.holding = false
	p.breakRem = 0
	if p.queued >= 0 {
		p.idx = p.queued
		p.queued = -1
	} else {
		p.idx++
	}
	p.loadItem()
}

// queueNext marks item i to play after the current one; clicking the already-
// queued item cancels the queue.
func (p *player) queueNext(i int) {
	if i < 0 || i >= len(p.items) {
		return
	}
	if p.queued == i {
		p.queued = -1
	} else {
		p.queued = i
	}
	// The next target changed; servicePrewarm re-aims next tick. A prewarm
	// targeting the *current* item is an in-flight pending load — keep it, or
	// the current song would never get its decoder.
	if p.prewarmIdx != p.idx {
		p.discardPrewarm()
	}
}

// skip abandons the current item immediately (works while playing or holding).
// A song cut mid-audio fades to silence rather than hard-cutting.
func (p *player) skip() {
	if p.finished {
		return
	}
	p.beginTransition()
	p.advance()
}

// pause freezes playback mid-item. The decoder (if any) stays alive: once the
// ring buffer fills, its ffmpeg stalls on write, so resume continues in place.
func (p *player) pause() {
	if !p.finished {
		p.paused = true
	}
}

// jumpTo moves to an arbitrary item and starts it from its beginning. The
// index is clamped to the flow bounds; jumping to a gate parks at the gate.
func (p *player) jumpTo(i int) {
	if len(p.items) == 0 {
		return
	}
	if i < 0 {
		i = 0
	}
	if i > len(p.items)-1 {
		i = len(p.items) - 1
	}
	if i != p.idx {
		p.markPlayed(p.idx) // leaving the current item for a different one
	}
	p.beginTransition() // fade out a song cut mid-audio (prev/restart/jump)
	if p.dec != nil {
		reapAsync(p.dec)
		p.dec = nil
	}
	p.breakRem = 0
	p.holding = false
	p.paused = false
	p.finished = false
	p.queued = -1
	p.idx = i
	p.loadItem()
}

// release continues from a manual hold/gate. No-op when not holding.
func (p *player) release() {
	if !p.holding {
		return
	}
	p.advance()
}

// itemElapsedSec is how far into the current item we are, derived from the
// program audio consumed (which naturally freezes while paused/holding).
func (p *player) itemElapsedSec() float64 {
	return float64(p.itemBytes) / bytesPerSec
}

// itemDurationSec is the known length of the current item: a song's track
// duration or a break's silence budget. Gates/holds and missing tracks are 0
// (unknown), which the UI treats as "no progress bar".
func (p *player) itemDurationSec() float64 {
	if p.finished || p.idx >= len(p.items) {
		return 0
	}
	it := p.items[p.idx]
	switch it.Type {
	case services.ItemSong:
		if it.Track != nil {
			return it.Track.DurationSec
		}
	case services.ItemBreak:
		return float64(it.BreakSec)
	}
	return 0
}

// nowPlayingDesc returns a human label for the status panel.
func (p *player) nowPlayingDesc() string {
	if p.finished {
		return "Show finished"
	}
	if p.idx >= len(p.items) {
		return ""
	}
	it := p.items[p.idx]
	if p.paused {
		return "⏸ Paused — " + services.Describe(it)
	}
	if p.holding {
		if it.Type == services.ItemGate {
			if it.Label != "" {
				return "⏸ Hold: " + it.Label
			}
			return "⏸ Manual hold — press Play"
		}
		return "⏸ Holding — press Play to continue"
	}
	return services.Describe(it)
}

// nextUpDesc returns the next item's label, or "" at the end.
func (p *player) nextUpDesc() string {
	ni := p.idx + 1
	// When holding at a gate, the gate item is the current idx; the "next" is
	// what Play will start.
	if p.queued >= 0 {
		ni = p.queued
	}
	if ni >= len(p.items) {
		return "— end —"
	}
	return services.Describe(p.items[ni])
}

// overlayText is the now-playing string burned into the video banner. Only
// songs show text; breaks/gates/holds blank it.
func (p *player) overlayText() string {
	if t := p.overlayTrack(); t != nil {
		if t.Artist != "" {
			return t.Artist + " - " + t.Title
		}
		return t.Title
	}
	return " "
}

// overlayArt is the current song's cover-art path for the banner tile, or ""
// (→ placeholder) when nothing should show.
func (p *player) overlayArt() string {
	if t := p.overlayTrack(); t != nil {
		return t.ArtPath
	}
	return ""
}

// overlayTrack returns the track the banner should describe, or nil when the
// banner should be hidden.
func (p *player) overlayTrack() *services.Track {
	if !p.bannerVisible() {
		return nil
	}
	return p.items[p.idx].Track
}

// bannerVisible reports whether the now-playing banner should be shown: a song
// that is playing, or paused mid-play. Breaks, gates/holds, the end of the
// flow, and a song that is cued but never started (paused at 0:00) hide it.
func (p *player) bannerVisible() bool {
	if p.finished || p.holding || p.idx >= len(p.items) {
		return false
	}
	it := p.items[p.idx]
	if it.Type != services.ItemSong || it.Track == nil {
		return false
	}
	if p.dec == nil {
		return false // pending load — the banner fades back in with the audio
	}
	if p.paused && p.itemBytes == 0 {
		return false // cued, not yet started
	}
	return true
}
