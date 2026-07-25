package playout

import (
	"log"
	"os"
	"sync"
	"time"

	"music-to-rtmp-playout/services"
)

const bytesPerSec = sampleRate * frameBytes // 192000

// ringSize is the per-song look-ahead buffer (~0.5s) that decouples decode
// jitter from the mix tick.
const ringSize = bytesPerSec / 2

// Encoder reconnect policy. When the persistent ffmpeg dies (e.g. the RTMP
// relay drops the connection), the show does not end — the run loop brings up a
// fresh encoder and keeps going. Backoff grows while reconnects keep failing
// fast and resets once the stream has stayed healthy for encHealthyResetAfter.
const (
	encReconnectBackoffMin = 200 * time.Millisecond
	encReconnectBackoffMax = 30 * time.Second
	encHealthyResetAfter   = 15 * time.Second
)

// encReconnectBackoff is the delay before reconnect attempt n (n starts at 1),
// doubling from the min and capped at the max (also guarding shift overflow).
func encReconnectBackoff(n int) time.Duration {
	if n < 1 {
		n = 1
	}
	d := encReconnectBackoffMin << (n - 1)
	if d <= 0 || d > encReconnectBackoffMax {
		return encReconnectBackoffMax
	}
	return d
}

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

// DiagSnapshot is the per-second real-time health of the run loop, exposed for
// the debug endpoint (PLAYOUT_DIAG). Clean output PTS can hide bursty delivery;
// these numbers show the loop cadence directly.
type DiagSnapshot struct {
	WritesPerSec  int   `json:"writesPerSec"`
	MaxWriteGapMs int64 `json:"maxWriteGapMs"`
	MaxPrewarmMs  int64 `json:"maxPrewarmMs"`
	MaxPublishMs  int64 `json:"maxPublishMs"`
	StdinQ        int   `json:"stdinQ"`
	StdinCap      int   `json:"stdinCap"`
	VizQ          int   `json:"vizQ"`
	VizCap        int   `json:"vizCap"`
	VizDropTotal  int64 `json:"vizDropTotal"`
}

// EngineConfig holds static dependencies for the engine.
type EngineConfig struct {
	FFmpegPath     string
	NowTxtPath     string
	ArtLivePath    string
	FontFile       string
	NVENCAvailable bool // GPU (h264_nvenc) usable — probed once at startup
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

	diagMu   sync.Mutex
	diagSnap DiagSnapshot
}

// setDiag / Diag publish the latest run-loop health snapshot for the debug endpoint.
func (e *Engine) setDiag(s DiagSnapshot) { e.diagMu.Lock(); e.diagSnap = s; e.diagMu.Unlock() }

// Diag returns the latest run-loop health snapshot (zero value when idle).
func (e *Engine) Diag() DiagSnapshot { e.diagMu.Lock(); defer e.diagMu.Unlock(); return e.diagSnap }

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
		VideoCodec:   resolveVideoCodec(set.VideoEncoder, e.cfg.NVENCAvailable),
		AudioBitrate: set.AudioBitrate,
		NowOverlay:   set.NowOverlay,
		VizStyle:     set.VizStyle,
		BannerBox:    set.BannerBox,
		LowLatency:   set.LowLatency,
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

	go e.run(enc, encCfg, items, vm, playlistID, startAt)
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
func (e *Engine) Stop() { e.sendCritical(command{kind: cmdStop}) }

// Skip abandons the current item and advances (works while playing or holding).
func (e *Engine) Skip() { e.send(command{kind: cmdSkip}) }

// Play releases a manual hold/gate, or resumes a paused show.
func (e *Engine) Play() { e.send(command{kind: cmdPlay}) }

// Pause freezes playback mid-item; the stream stays live with silence.
func (e *Engine) Pause() { e.sendCritical(command{kind: cmdPause}) }

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

// sendCritical enqueues a one-shot command that must not be lost under a full
// control buffer (Stop/Pause — unlike idempotent skip/jump, a dropped one leaves
// the stream running). It blocks until the run loop drains a slot (every 5ms
// tick) and gives up only after a timeout, so it can never deadlock even if run
// has already exited on the TOCTOU boundary of the running check above.
func (e *Engine) sendCritical(c command) {
	e.mu.Lock()
	ch := e.cmd
	running := e.running
	e.mu.Unlock()
	if !running || ch == nil {
		return
	}
	select {
	case ch <- c:
	case <-time.After(2 * time.Second):
		log.Printf("playout: critical control command (kind %d) dropped; run loop unresponsive", c.kind)
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
func (e *Engine) run(enc *encoder, encCfg encoderConfig, items []services.FlowItem, vmix *voiceMixer, playlistID int64, startAt int) {
	defer func() {
		enc.Stop()
		e.mu.Lock()
		e.running = false
		e.vmix = nil
		e.mu.Unlock()
	}()

	p := &player{
		items: items, ffmpegPath: e.cfg.FFmpegPath, idx: startAt, queued: -1,
		prewarmIdx: -1, prewarmCh: make(chan prewarmResult, 1),
	}
	p.loadItem()
	// Reap every decoder the player owns on any run return path (LIFO: before the
	// encoder Stop defer). Declared here because the top-level defer can't see p.
	defer p.shutdown()
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
	// fadeDur; quantized levels are pushed to the encoder over ZMQ (SetBannerFade).
	const fadeDur = 0.6 // seconds for a full fade in/out
	fadeAlpha := 0.0    // encoder starts with the mask at 0 (hidden)
	fadeWritten := 0
	lastFadeTick := time.Now()

	// snapshot is the single place a live Status is built. Every live publisher
	// (the pacing loop and the reconnect wait) goes through it: the status panel
	// renders all of these fields, so a hand-rolled literal that omits some makes
	// the show clock and progress bar reset on screen.
	snapshot := func(errMsg string) Status {
		return Status{
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
			Error:           errMsg,
		}
	}

	// stoppedSnapshot is the final publish on every stop path: not running, but
	// keeping the position so the UI can offer "continue from here".
	stoppedSnapshot := func() Status {
		return Status{ItemIndex: p.idx, TotalItems: len(items), PlaylistID: playlistID}
	}

	// publish refreshes the status snapshot and drives the banner fade. It returns
	// the snapshot so the caller can broadcast it without re-reading under the lock.
	publish := func() Status {
		s := snapshot("")
		e.setStatus(s)

		// Banner: track the *audible* song. On a manual cut the old song's fade
		// tail is still heard, so the banner fades out with it (old content
		// held), swaps while hidden, and fades back in as the new song ramps.
		// At a natural boundary the audio switches instantly, so the banner
		// hard-swaps in place instead of blinking out and back.
		wantText, wantArt := p.overlayText(), p.overlayArt()
		swap := func() {
			if wantText != lastText {
				enc.SetNowPlaying(wantText)
				lastText = wantText
			}
			if wantArt != lastArt {
				enc.SetNowArt(wantArt)
				lastArt = wantArt
			}
		}
		changed := wantText != lastText || wantArt != lastArt
		if changed && fadeAlpha > 0 && !p.tailActive() && p.bannerVisible() {
			swap()
			changed = false
		}
		visible := p.bannerVisible() && !changed
		if fadeAlpha == 0 && changed {
			swap()
			visible = p.bannerVisible() // swapped while hidden: fade back in
		}
		now := time.Now()
		step := now.Sub(lastFadeTick).Seconds() / fadeDur
		lastFadeTick = now
		if visible {
			fadeAlpha = min(1, fadeAlpha+step)
		} else {
			fadeAlpha = max(0, fadeAlpha-step)
		}
		if q := int(fadeAlpha*fadeLevels + 0.5); q != fadeWritten {
			enc.SetBannerFade(q)
			fadeWritten = q
		}
		return s
	}
	publish()

	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()

	// Real-time loop diagnostics (opt-in via PLAYOUT_DIAG=1). Clean output PTS can
	// still ride on bursty *delivery*; this measures the loop cadence directly —
	// the max wall-clock gap between encoder writes, time in servicePrewarm/publish,
	// and the encoder buffer depths — so a stall that jitters delivery is visible.
	diag := os.Getenv("PLAYOUT_DIAG") != ""
	diagStart := time.Now()
	diagLastWr := time.Now()
	var diagWrites int
	var diagMaxGap, diagMaxPrew, diagMaxPub time.Duration

	// Encoder reconnect state (see encReconnectBackoff). encFails grows while
	// reconnects keep failing fast; lastReconnect gates the reset back to zero.
	encFails := 0
	lastReconnect := time.Time{}

	// reconnectEncoder tears down the dead ffmpeg and brings up a fresh one so a
	// dropped RTMP link (or an encoder crash) does not end a 24/7 show. It stays
	// responsive to Stop while down and backs off between attempts. Returns false
	// only when the operator asked to Stop — the caller then returns. On success
	// it re-baselines the pacing clock (excluding the downtime, so the stream
	// resumes live instead of fast-forwarding a buffered backlog) and re-syncs the
	// banner. now.txt / art_live persist on disk across encoders, but the in-memory
	// fade level does not, so it is re-sent.
	reconnectEncoder := func() bool {
		downStart := time.Now()
		enc.Stop()
		if !lastReconnect.IsZero() && time.Since(lastReconnect) > encHealthyResetAfter {
			encFails = 0 // the stream had been healthy for a while — start backoff fresh
		}
		for {
			encFails++
			backoff := encReconnectBackoff(encFails)
			s := snapshot("stream output dropped — reconnecting…")
			e.setStatus(s)
			e.broadcast(s)
			// Wait out the backoff, but honor the critical commands while we are down.
			select {
			case c := <-e.cmd:
				switch c.kind {
				case cmdStop:
					st := stoppedSnapshot()
					e.setStatus(st)
					e.broadcast(st)
					return false
				case cmdPause:
					// Pause goes through sendCritical precisely because dropping one
					// leaves the show running against the operator's intent — so it
					// must be honored here too, not swallowed with the rest.
					p.pause()
				}
				// The remaining commands need a live output; they are dropped.
			case <-time.After(backoff):
			}
			ne, err := startEncoder(encCfg)
			if err != nil {
				log.Printf("playout: encoder reconnect attempt %d failed to start: %v", encFails, err)
				continue
			}
			// startEncoder returns as soon as ffmpeg launches; if the RTMP target is
			// still unreachable ffmpeg exits a few seconds later, which the run loop
			// now notices immediately (Write returns errEncoderExited on the closed
			// done channel) and comes straight back here with a grown backoff.
			enc = ne
			start = start.Add(time.Since(downStart)) // exclude downtime from the pacing clock
			lastReconnect = time.Now()
			enc.SetNowPlaying(lastText)
			if lastArt != "\x00" {
				enc.SetNowArt(lastArt)
			}
			enc.SetBannerFade(fadeWritten)
			log.Printf("playout: encoder reconnected after %s (attempt %d)",
				time.Since(downStart).Round(time.Millisecond), encFails)
			return true
		}
	}

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
					s := stoppedSnapshot()
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

		// Adopt any ready prewarmed decoder (before fill can consume it) and, when
		// the current item nears its end, spawn the next song's decoder off-thread.
		tPrew := time.Now()
		p.servicePrewarm()
		if diag {
			if d := time.Since(tPrew); d > diagMaxPrew {
				diagMaxPrew = d
			}
		}

		// Pace: write chunks until we've produced up to (elapsed + lead).
		target := int64(time.Since(start).Seconds()*sampleRate) + lead
		for written < target {
			chunk := silence(chunkBytes)
			p.fill(chunk)
			vmix.mixInto(chunk)
			if err := enc.Write(chunk); err != nil {
				log.Printf("playout: encoder write failed (ffmpeg exited?): %v — reconnecting", err)
				if !reconnectEncoder() {
					return // operator asked to Stop while the output was down
				}
				// Fresh encoder up; the lost chunk is dropped (silence bounds any
				// gap). Recompute the pacing target next tick against the rebased
				// clock instead of bursting to catch up on this one.
				break
			}
			if diag {
				if g := time.Since(diagLastWr); g > diagMaxGap {
					diagMaxGap = g
				}
				diagLastWr = time.Now()
				diagWrites++
			}
			written += chunkFrames
		}

		tPub := time.Now()
		e.broadcast(publish())
		if diag {
			if d := time.Since(tPub); d > diagMaxPub {
				diagMaxPub = d
			}
			if time.Since(diagStart) >= time.Second {
				snap := DiagSnapshot{
					WritesPerSec:  diagWrites,
					MaxWriteGapMs: diagMaxGap.Milliseconds(),
					MaxPrewarmMs:  diagMaxPrew.Milliseconds(),
					MaxPublishMs:  diagMaxPub.Milliseconds(),
					StdinQ:        len(enc.stdinCh), StdinCap: cap(enc.stdinCh),
					VizQ: len(enc.vizCh), VizCap: cap(enc.vizCh),
					VizDropTotal: enc.vizDropped.Load(),
				}
				e.setDiag(snap)
				log.Printf("playout DIAG: writes/s=%d maxWriteGap=%dms maxPrewarm=%dms maxPublish=%dms stdinQ=%d/%d vizQ=%d/%d vizDrop=%d",
					snap.WritesPerSec, snap.MaxWriteGapMs, snap.MaxPrewarmMs, snap.MaxPublishMs,
					snap.StdinQ, snap.StdinCap, snap.VizQ, snap.VizCap, snap.VizDropTotal)
				diagStart = time.Now()
				diagWrites = 0
				diagMaxGap, diagMaxPrew, diagMaxPub = 0, 0, 0
			}
		}

		select {
		case <-enc.Done():
			// ffmpeg exited on its own (e.g. the RTMP relay dropped the
			// connection — WSAECONNABORTED). This fires before the stdin write
			// error would, so it is the primary reconnect trigger; don't end the
			// show, bring a fresh encoder up and keep going.
			log.Printf("playout: encoder process exited unexpectedly — reconnecting")
			if !reconnectEncoder() {
				return // operator asked to Stop while the output was down
			}
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
