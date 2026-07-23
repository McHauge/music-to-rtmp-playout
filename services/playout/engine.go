package playout

import (
	"log"
	"sync"
	"time"

	"music-to-rtmp-playout/services"
)

const bytesPerSec = sampleRate * frameBytes // 192000

// ringSize is the per-song look-ahead buffer (~0.5s) that decouples decode
// jitter from the mix tick.
const ringSize = bytesPerSec / 2

// Status is a snapshot of the running show, published to the UI over SSE.
type Status struct {
	Running         bool    `json:"running"`
	Holding         bool    `json:"holding"`  // waiting at a manual gate / hold
	Paused          bool    `json:"paused"`   // frozen mid-item by the operator
	Finished        bool    `json:"finished"` // reached end of flow
	PauseAfterArmed bool    `json:"pauseAfterArmed"`
	ItemIndex       int     `json:"itemIndex"`
	QueuedIndex     int     `json:"queuedIndex"` // operator-queued next item, -1 = none
	TotalItems      int     `json:"totalItems"`
	PlaylistID      int64   `json:"playlistID"`
	NowPlaying      string  `json:"nowPlaying"`
	NextUp          string  `json:"nextUp"`
	ElapsedSec      float64 `json:"elapsedSec"`      // whole-show wall time
	ItemElapsedSec  float64 `json:"itemElapsedSec"`  // position within the current item
	ItemDurationSec float64 `json:"itemDurationSec"` // length of the current item, 0 if unknown
	ActiveVoices    int     `json:"activeVoices"`
	Error           string  `json:"error"`

	// Played is the set of item indices already played (visited and departed),
	// for dimming them in the rundown. Not serialized; consumed server-side.
	Played map[int]bool `json:"-"`
}

// EngineConfig holds static dependencies for the engine.
type EngineConfig struct {
	FFmpegPath  string
	NowTxtPath  string
	ArtLivePath string
	FontFile    string
}

// Engine owns the single live show. All playback state lives in the run
// goroutine; control flows in over channels, status flows out over a snapshot
// + subscriber fan-out.
type Engine struct {
	cfg EngineConfig

	mu             sync.Mutex
	running        bool
	cmd            chan command
	vmix           *voiceMixer
	showItems      []services.FlowItem // snapshot of the current/most recent show
	showPlaylistID int64

	statusMu sync.RWMutex
	status   Status

	subsMu sync.Mutex
	subs   map[chan Status]struct{}
}

type cmdKind int

const (
	cmdSkip cmdKind = iota
	cmdPlay
	cmdStop
	cmdPause
	cmdJump
	cmdPrev
	cmdRestart
	cmdTogglePauseAfter
	cmdSetAutoNext
)

type command struct {
	kind  cmdKind
	index int  // target item for cmdJump / cmdSetAutoNext
	on    bool // new value for cmdSetAutoNext
}

// NewEngine constructs an idle engine.
func NewEngine(cfg EngineConfig) *Engine {
	return &Engine{cfg: cfg, subs: make(map[chan Status]struct{})}
}

// Running reports whether a show is live.
func (e *Engine) Running() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.running
}

// Start launches the encoder and begins playing the flow at item startAt
// (clamped to the flow bounds). Errors if a show is already live or the
// encoder fails to start.
func (e *Engine) Start(items []services.FlowItem, set services.Settings, playlistID int64, startAt int) error {
	e.mu.Lock()
	if e.running {
		e.mu.Unlock()
		return errAlreadyRunning
	}
	if startAt < 0 {
		startAt = 0
	}
	if startAt > len(items)-1 {
		startAt = len(items) - 1
	}

	encCfg := encoderConfig{
		FFmpegPath:   e.cfg.FFmpegPath,
		RTMPURL:      set.FullRTMPURL(),
		BgImagePath:  set.BgImagePath,
		NowTxtPath:   e.cfg.NowTxtPath,
		ArtLivePath:  e.cfg.ArtLivePath,
		FontFile:     e.cfg.FontFile,
		Width:        set.VideoWidth,
		Height:       set.VideoHeight,
		FPS:          set.VideoFPS,
		VideoEnabled: set.VideoEnabled,
		VideoBitrate: set.VideoBitrate,
		AudioBitrate: set.AudioBitrate,
		NowOverlay:   set.NowOverlay,
		VizStyle:     set.VizStyle,
		BannerBox:    set.BannerBox,
	}
	enc, err := startEncoder(encCfg)
	if err != nil {
		e.mu.Unlock()
		return err
	}

	vm := &voiceMixer{}
	e.running = true
	e.cmd = make(chan command, 8)
	e.vmix = vm
	// The run goroutine owns `items`; the UI reads an independent copy so
	// SetItemAutoNext can update both sides without a data race.
	e.showItems = append([]services.FlowItem(nil), items...)
	e.showPlaylistID = playlistID
	e.mu.Unlock()

	// Seed the snapshot so status reads are live-mode before run's first publish.
	// The show starts cued-and-paused; the operator presses Play to go on air.
	e.setStatus(Status{Running: true, Paused: true, ItemIndex: startAt, QueuedIndex: -1, TotalItems: len(items), PlaylistID: playlistID})

	go e.run(enc, items, vm, playlistID, startAt)
	return nil
}

// Show returns a copy of the item snapshot and the playlist ID of the current
// (or most recent) show.
func (e *Engine) Show() ([]services.FlowItem, int64) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]services.FlowItem(nil), e.showItems...), e.showPlaylistID
}

// Stop ends the show (idempotent).
func (e *Engine) Stop() { e.send(command{kind: cmdStop}) }

// Skip abandons the current item and advances (works while playing or holding).
func (e *Engine) Skip() { e.send(command{kind: cmdSkip}) }

// Play releases a manual hold/gate, or resumes a paused show.
func (e *Engine) Play() { e.send(command{kind: cmdPlay}) }

// Pause freezes playback mid-item; the stream stays live with silence.
func (e *Engine) Pause() { e.send(command{kind: cmdPause}) }

// Prev restarts the previous item (or the first item) from its beginning.
func (e *Engine) Prev() { e.send(command{kind: cmdPrev}) }

// RestartItem restarts the current item from its beginning.
func (e *Engine) RestartItem() { e.send(command{kind: cmdRestart}) }

// JumpTo targets the item at index i. While actively playing it queues the
// item as next (toggling off if already queued); while paused/holding/finished
// it jumps there immediately.
func (e *Engine) JumpTo(i int) { e.send(command{kind: cmdJump, index: i}) }

// TogglePauseAfter arms/disarms the one-shot pause-after-current-item flag.
func (e *Engine) TogglePauseAfter() { e.send(command{kind: cmdTogglePauseAfter}) }

// SetItemAutoNext updates the auto-next flag of item i in the live show's
// snapshot (both the UI copy and the player's copy).
func (e *Engine) SetItemAutoNext(i int, on bool) {
	e.mu.Lock()
	if i >= 0 && i < len(e.showItems) {
		e.showItems[i].AutoNext = on
	}
	e.mu.Unlock()
	e.send(command{kind: cmdSetAutoNext, index: i, on: on})
}

func (e *Engine) send(c command) {
	e.mu.Lock()
	ch := e.cmd
	running := e.running
	e.mu.Unlock()
	if !running || ch == nil {
		return
	}
	select {
	case ch <- c:
	default: // control buffer full — drop (next tick will catch up)
	}
}

// TriggerClip overlays a pre-decoded PCM clip on the live program audio.
// Retriggering the same key restarts the clip instead of layering a copy.
func (e *Engine) TriggerClip(key, pcmPath string, gain float64) error {
	e.mu.Lock()
	vm := e.vmix
	running := e.running
	e.mu.Unlock()
	if !running || vm == nil {
		return errNotRunning
	}
	log.Printf("playout: soundboard trigger key=%s (%d voices active)", key, vm.active())
	return vm.Trigger(key, pcmPath, gain)
}

// run is the single owner of playback state. vmix is passed in (rather than
// read from e.vmix) so the hot loop never touches the shared field.
func (e *Engine) run(enc *encoder, items []services.FlowItem, vmix *voiceMixer, playlistID int64, startAt int) {
	defer func() {
		enc.Stop()
		e.mu.Lock()
		e.running = false
		e.vmix = nil
		e.mu.Unlock()
	}()

	p := &player{items: items, ffmpegPath: e.cfg.FFmpegPath, idx: startAt, queued: -1}
	p.loadItem()
	// Begin cued-and-paused at 0:00 so nothing airs until the operator hits Play.
	if !p.finished {
		p.paused = true
	}

	start := time.Now()
	lead := int64(sampleRate * 300 / 1000) // 300ms startup lead
	written := int64(0)                    // frames written
	lastText := ""
	lastArt := "\x00" // sentinel: first publish always installs the art/placeholder

	// Banner fade state: fadeAlpha animates toward 1 (shown) / 0 (hidden) over
	// fadeDur; quantized levels are pushed to the encoder's fade mask.
	const fadeDur = 0.6 // seconds for a full fade in/out
	fadeAlpha := 0.0    // encoder starts with the mask at 0 (hidden)
	fadeWritten := 0
	lastFadeTick := time.Now()

	publish := func() {
		s := Status{
			Running:         true,
			Holding:         p.holding,
			Paused:          p.paused,
			Finished:        p.finished,
			PauseAfterArmed: p.pauseAfter,
			ItemIndex:       p.idx,
			QueuedIndex:     p.queued,
			TotalItems:      len(items),
			PlaylistID:      playlistID,
			NowPlaying:      p.nowPlayingDesc(),
			NextUp:          p.nextUpDesc(),
			ElapsedSec:      time.Since(start).Seconds(),
			ItemElapsedSec:  p.itemElapsedSec(),
			ItemDurationSec: p.itemDurationSec(),
			ActiveVoices:    vmix.active(),
			Played:          p.playedCopy(),
		}
		e.setStatus(s)

		// Banner: fade toward shown/hidden, and sync the text/art content only
		// while shown or fully hidden — never mid-fade-out, so the outgoing
		// song's banner keeps its content while it disappears.
		visible := p.bannerVisible()
		if visible || fadeAlpha == 0 {
			text := p.overlayText()
			if text != lastText {
				enc.SetNowPlaying(text)
				lastText = text
			}
			art := p.overlayArt()
			if art != lastArt && enc.SetNowArt(art) {
				lastArt = art // on swap failure, retry next tick
			}
		}
		now := time.Now()
		step := now.Sub(lastFadeTick).Seconds() / fadeDur
		lastFadeTick = now
		if visible {
			fadeAlpha = min(1, fadeAlpha+step)
		} else {
			fadeAlpha = max(0, fadeAlpha-step)
		}
		if q := int(fadeAlpha*fadeLevels + 0.5); q != fadeWritten && enc.SetBannerFade(q) {
			fadeWritten = q // on swap failure, retry next tick
		}
	}
	publish()

	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()

	for {
		// Drain any pending control commands.
		drained := false
		for !drained {
			select {
			case c := <-e.cmd:
				switch c.kind {
				case cmdStop:
					log.Printf("playout: stop — fed %.2fs of audio in %.2fs wall (ratio %.3f)",
						float64(written)/sampleRate, time.Since(start).Seconds(),
						(float64(written)/sampleRate)/time.Since(start).Seconds())
					// Keep position info so the UI can offer "continue from here".
					s := Status{ItemIndex: p.idx, TotalItems: len(items), PlaylistID: playlistID}
					e.setStatus(s)
					e.broadcast(s)
					return
				case cmdSkip:
					p.skip()
				case cmdPlay:
					if p.paused {
						p.paused = false
					} else {
						p.release()
					}
				case cmdPause:
					p.pause()
				case cmdJump:
					if p.paused || p.holding || p.finished {
						p.jumpTo(c.index)
					} else {
						p.queueNext(c.index)
					}
				case cmdPrev:
					p.jumpTo(p.idx - 1)
				case cmdRestart:
					p.jumpTo(p.idx)
				case cmdTogglePauseAfter:
					p.pauseAfter = !p.pauseAfter
				case cmdSetAutoNext:
					if c.index >= 0 && c.index < len(p.items) {
						p.items[c.index].AutoNext = c.on
					}
				}
			default:
				drained = true
			}
		}

		// Pace: write chunks until we've produced up to (elapsed + lead).
		target := int64(time.Since(start).Seconds()*sampleRate) + lead
		for written < target {
			chunk := silence(chunkBytes)
			p.fill(chunk)
			vmix.mixInto(chunk)
			if err := enc.Write(chunk); err != nil {
				log.Printf("playout: encoder write failed (ffmpeg exited?): %v", err)
				s := Status{Error: "stream encoder stopped: " + err.Error()}
				e.setStatus(s)
				e.broadcast(s)
				return
			}
			written += chunkFrames
		}

		publish()
		e.broadcast(e.Status())

		select {
		case <-enc.Done():
			log.Printf("playout: encoder process exited")
			s := Status{Error: "stream encoder exited unexpectedly"}
			e.setStatus(s)
			e.broadcast(s)
			return
		case <-ticker.C:
		}
	}
}

func (e *Engine) setStatus(s Status) {
	e.statusMu.Lock()
	e.status = s
	e.statusMu.Unlock()
}

// Status returns the latest snapshot.
func (e *Engine) Status() Status {
	e.statusMu.RLock()
	defer e.statusMu.RUnlock()
	return e.status
}

// Subscribe registers a channel for status updates. The returned func unsubscribes.
func (e *Engine) Subscribe() (<-chan Status, func()) {
	ch := make(chan Status, 4)
	e.subsMu.Lock()
	e.subs[ch] = struct{}{}
	e.subsMu.Unlock()
	return ch, func() {
		e.subsMu.Lock()
		if _, ok := e.subs[ch]; ok {
			delete(e.subs, ch)
			close(ch)
		}
		e.subsMu.Unlock()
	}
}

func (e *Engine) broadcast(s Status) {
	e.subsMu.Lock()
	for ch := range e.subs {
		select {
		case ch <- s:
		default: // slow subscriber — drop this update
		}
	}
	e.subsMu.Unlock()
}
