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
	Running      bool    `json:"running"`
	Holding      bool    `json:"holding"`  // waiting at a manual gate / hold
	Finished     bool    `json:"finished"` // reached end of flow
	ItemIndex    int     `json:"itemIndex"`
	TotalItems   int     `json:"totalItems"`
	NowPlaying   string  `json:"nowPlaying"`
	NextUp       string  `json:"nextUp"`
	ElapsedSec   float64 `json:"elapsedSec"`
	ActiveVoices int     `json:"activeVoices"`
	Error        string  `json:"error"`
}

// EngineConfig holds static dependencies for the engine.
type EngineConfig struct {
	FFmpegPath string
	NowTxtPath string
	FontFile   string
	Width      int
	Height     int
}

// Engine owns the single live show. All playback state lives in the run
// goroutine; control flows in over channels, status flows out over a snapshot
// + subscriber fan-out.
type Engine struct {
	cfg EngineConfig

	mu      sync.Mutex
	running bool
	cmd     chan command
	vmix    *voiceMixer

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
)

type command struct{ kind cmdKind }

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

// Start launches the encoder and begins playing the flow. Errors if a show is
// already live or the encoder fails to start.
func (e *Engine) Start(items []services.FlowItem, set services.Settings) error {
	e.mu.Lock()
	if e.running {
		e.mu.Unlock()
		return errAlreadyRunning
	}

	encCfg := encoderConfig{
		FFmpegPath:   e.cfg.FFmpegPath,
		RTMPURL:      set.FullRTMPURL(),
		BgImagePath:  set.BgImagePath,
		NowTxtPath:   e.cfg.NowTxtPath,
		FontFile:     e.cfg.FontFile,
		Width:        e.cfg.Width,
		Height:       e.cfg.Height,
		FPS:          set.VideoFPS,
		AudioBitrate: set.AudioBitrate,
	}
	enc, err := startEncoder(encCfg)
	if err != nil {
		e.mu.Unlock()
		return err
	}

	e.running = true
	e.cmd = make(chan command, 8)
	e.vmix = &voiceMixer{}
	e.mu.Unlock()

	go e.run(enc, items)
	return nil
}

// Stop ends the show (idempotent).
func (e *Engine) Stop() { e.send(command{cmdStop}) }

// Skip abandons the current item and advances (works while playing or holding).
func (e *Engine) Skip() { e.send(command{cmdSkip}) }

// Play releases a manual hold/gate so playout continues.
func (e *Engine) Play() { e.send(command{cmdPlay}) }

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
func (e *Engine) TriggerClip(pcmPath string, gain float64) error {
	e.mu.Lock()
	vm := e.vmix
	running := e.running
	e.mu.Unlock()
	if !running || vm == nil {
		return errNotRunning
	}
	return vm.Trigger(pcmPath, gain)
}

// run is the single owner of playback state.
func (e *Engine) run(enc *encoder, items []services.FlowItem) {
	defer func() {
		enc.Stop()
		e.mu.Lock()
		e.running = false
		e.vmix = nil
		e.mu.Unlock()
	}()

	p := &player{items: items, ffmpegPath: e.cfg.FFmpegPath}
	p.loadItem()

	start := time.Now()
	lead := int64(sampleRate * 300 / 1000) // 300ms startup lead
	written := int64(0)                     // frames written
	lastText := ""

	publish := func() {
		s := Status{
			Running:      true,
			Holding:      p.holding,
			Finished:     p.finished,
			ItemIndex:    p.idx,
			TotalItems:   len(items),
			NowPlaying:   p.nowPlayingDesc(),
			NextUp:       p.nextUpDesc(),
			ElapsedSec:   time.Since(start).Seconds(),
			ActiveVoices: e.vmix.active(),
		}
		e.setStatus(s)

		// Overlay text: the current song's display while playing, blank otherwise.
		text := p.overlayText()
		if text != lastText {
			enc.SetNowPlaying(text)
			lastText = text
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
					e.setStatus(Status{Running: false})
					e.broadcast(Status{Running: false})
					return
				case cmdSkip:
					p.skip()
				case cmdPlay:
					p.release()
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
			e.vmix.mixInto(chunk)
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
