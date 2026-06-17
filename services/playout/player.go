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

	idx      int
	dec      *decoder
	breakRem int  // remaining silence bytes for a break item
	holding  bool // paused at a gate or a non-auto-next end
	finished bool
}

// loadItem prepares the source for the item at idx. Songs spawn a decoder;
// breaks set a silence countdown; gates immediately enter the holding state.
// Missing/invalid items are skipped over.
func (p *player) loadItem() {
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
	if p.finished || p.holding {
		return // silence
	}
	it := p.items[p.idx]
	switch it.Type {
	case services.ItemSong:
		if p.dec == nil {
			p.endOfItem(it)
			return
		}
		p.dec.Read(out) // short reads leave the remainder as silence (underrun pad)
		if p.dec.Finished() {
			p.endOfItem(it)
		}
	case services.ItemBreak:
		take := len(out)
		if p.breakRem < take {
			take = p.breakRem
		}
		p.breakRem -= take
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
	if it.AutoNext {
		p.advance()
	} else {
		p.holding = true
	}
}

// advance moves to the next item and loads it.
func (p *player) advance() {
	if p.dec != nil {
		p.dec.Stop()
		p.dec = nil
	}
	p.holding = false
	p.breakRem = 0
	p.idx++
	p.loadItem()
}

// skip abandons the current item immediately (works while playing or holding).
func (p *player) skip() {
	if p.finished {
		return
	}
	p.advance()
}

// release continues from a manual hold/gate. No-op when not holding.
func (p *player) release() {
	if !p.holding {
		return
	}
	p.advance()
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
	if ni >= len(p.items) {
		return "— end —"
	}
	return services.Describe(p.items[ni])
}

// overlayText is the now-playing string burned into the video. Only songs show
// text; breaks/gates/holds blank it.
func (p *player) overlayText() string {
	if p.finished || p.holding || p.idx >= len(p.items) {
		return " "
	}
	it := p.items[p.idx]
	if it.Type == services.ItemSong && it.Track != nil {
		return it.Track.Display()
	}
	return " "
}
