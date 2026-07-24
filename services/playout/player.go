package playout

import (
	"errors"
	"log"

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

// prewarmLeadBytes is how much audio (or break silence) must remain before we
// spawn the next song's decoder off-thread. 1.5s covers ffmpeg spawn plus the
// 0.5s ring prime with margin, so the decoder is adopted already-primed at the
// boundary. Tunable; raise if spawns are slow on the target box.
const prewarmLeadBytes = int64(bytesPerSec) * 3 / 2 // 1.5s

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
		// The primed 0.5s ring makes the first post-boundary Read a full chunk,
		// so there is no silence gap and no pacer catch-up burst.
		if p.prewarmDec != nil && p.prewarmIdx == p.idx {
			p.dec = p.prewarmDec
			p.prewarmDec = nil
			p.prewarmIdx = -1
			return
		}
		// No matching prewarm (manual jump to an unpredicted item, a too-fast
		// transition, or a failed spawn): drop any stale prewarm and spawn
		// synchronously. Rare, so the occasional inline stall is acceptable.
		p.discardPrewarm()
		d, err := startDecoder(p.ffmpegPath, it.Track.FilePath, ringSize)
		if err != nil {
			log.Printf("playout: decoder start failed for %q: %v", it.Track.FilePath, err)
			p.advance()
			return
		}
		p.dec = d
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

// shouldPrewarmNow reports whether the current item is close enough to its end
// (and will auto-advance to a song) to justify spawning the next decoder now.
func (p *player) shouldPrewarmNow() bool {
	if p.finished || p.holding || p.paused {
		return false
	}
	ni := p.nextIdx()
	if ni < 0 || ni >= len(p.items) {
		return false
	}
	nit := p.items[ni]
	if nit.Type != services.ItemSong || nit.Track == nil || nit.Track.FilePath == "" {
		return false // next is a break/gate/empty song — nothing to prewarm
	}
	it := p.items[p.idx]
	switch it.Type {
	case services.ItemSong:
		if p.dec == nil || it.Track == nil || it.Track.DurationSec <= 0 {
			return false // unknown length → can't time it; sync-spawn fallback
		}
		if !(it.AutoNext || p.pauseAfter) {
			return false // will hold at end, not auto-advance
		}
		remaining := int64(it.Track.DurationSec*float64(bytesPerSec)) - p.itemBytes
		return remaining <= prewarmLeadBytes
	case services.ItemBreak:
		if !it.AutoNext {
			return false
		}
		return int64(p.breakRem) <= prewarmLeadBytes // covers break→song
	}
	return false
}

// servicePrewarm runs once per tick on the run goroutine. It adopts a decoder
// the helper goroutine has finished spawning, then — when the current item is
// within prewarmLeadBytes of its end and the next item is a song — launches the
// next decoder off-thread so exec.Start never runs on the run goroutine.
func (p *player) servicePrewarm() {
	// (1) Adopt / discard whatever the helper handed back.
	select {
	case res := <-p.prewarmCh:
		if res.err == nil && res.dec != nil && res.idx == p.prewarmIdx {
			p.prewarmDec = res.dec // ready, still targets the current next item
		} else {
			if res.dec != nil {
				reapAsync(res.dec) // stale target, or a spawn we no longer want
			}
			if res.idx == p.prewarmIdx {
				p.prewarmIdx = -1 // spawn failed for the live target → sync fallback later
			}
		}
	default:
	}

	// (2) Maybe launch a new prewarm. prewarmIdx>=0 means one is in flight or ready.
	if p.prewarmIdx >= 0 || !p.shouldPrewarmNow() {
		return
	}
	ni := p.nextIdx()
	it := p.items[ni]
	p.prewarmIdx = ni // set before launching → at most one in-flight spawn
	ch := p.prewarmCh
	ffmpeg, path := p.ffmpegPath, it.Track.FilePath // copy — the helper must not touch player
	go func() {
		d, err := startDecoder(ffmpeg, path, ringSize)
		ch <- prewarmResult{idx: ni, dec: d, err: err}
	}()
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

// fill writes one chunk of program audio into out (already zeroed = silence).
// It advances the flow as items complete, honoring auto-next vs. hold.
func (p *player) fill(out []byte) {
	if p.finished || p.holding || p.paused {
		return // silence
	}
	it := p.items[p.idx]
	switch it.Type {
	case services.ItemSong:
		if p.dec == nil {
			p.endOfItem(it)
			return
		}
		n := p.dec.Read(out) // short reads leave the remainder as silence (underrun pad)
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
	p.discardPrewarm() // the next target changed; servicePrewarm re-aims next tick
}

// skip abandons the current item immediately (works while playing or holding).
func (p *player) skip() {
	if p.finished {
		return
	}
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
	if p.paused && p.itemBytes == 0 {
		return false // cued, not yet started
	}
	return true
}
