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
		p.dec.Stop()
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
		p.dec.Stop()
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
		p.dec.Stop()
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

// overlayText is the now-playing string burned into the video. Only songs show
// text; breaks/gates/holds blank it.
func (p *player) overlayText() string {
	if p.finished || p.holding || p.paused || p.idx >= len(p.items) {
		return " "
	}
	it := p.items[p.idx]
	if it.Type == services.ItemSong && it.Track != nil {
		return it.Track.Display()
	}
	return " "
}
