package playout

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/go-zeromq/zmq4"
)

// encoder is the persistent ffmpeg process that muxes the canonical PCM stream
// (received on stdin) with a static/looping background + now-playing overlay,
// and pushes FLV to the RTMP target. It must stay up for the whole show; the
// mixer keeps feeding it PCM (silence during gaps) so it never starves.
type encoder struct {
	cmd      *exec.Cmd
	stdin    io.WriteCloser
	nowTxt   string
	artLive  string // live banner cover art; "" when the banner is off
	bannerOn bool
	done     chan struct{}

	// Banner fade is driven at runtime over ZMQ instead of a fade-mask PNG. The
	// colorchannelmixer@fade filter's alpha gain (aa) is set by commands sent to
	// ffmpeg's zmq REP socket; the mask PNG approach re-read the file every frame
	// (-loop 1 image2) while the engine rewrote it ~15x per fade, and those
	// re-reads colliding with the rewrites stalled the muxer — a brief audio+video
	// freeze on every fade-in. zmqSender owns the REQ socket and the send timeline.
	zmqAddr    string        // "tcp://127.0.0.1:PORT"; "" when the banner is off
	fadeLevel  atomic.Int32  // latest desired fade level (0..fadeLevels)
	fadeTickle chan struct{} // cap 1; SetBannerFade pokes it, zmqSender coalesces

	// stdinCh + stdinPump decouple the pacing loop from the pipe:0 write. The FLV
	// muxer only drains stdin as fast as it can interleave video, so a momentarily
	// starved video branch (e.g. the viz input below waiting on its TCP feed)
	// back-pressures pipe:0. If Write blocked on that directly it would freeze the
	// one loop that also feeds the viz, starving the video branch further — a
	// self-sustaining stall (observed as a flood of "stdin write stalled" +
	// "viz feed dropped"). The pump absorbs ~5s of that back-pressure so the loop
	// keeps producing and the video branch never starves for lack of new audio.
	stdinCh  chan []byte  // buffered ~5s; drained into pipe:0 by stdinPump
	stdinErr atomic.Value // first pipe:0 write error (ffmpeg died); surfaced by Write

	// Viz side-channel: with a visualization on, the filtergraph gets its own copy
	// of the program PCM over loopback TCP instead of tapping [0:a]. The broadcast
	// audio path is stdin → AAC only; the pump above keeps a slow video branch from
	// back-pressuring it, so the viz can lag but the program audio cannot.
	vizLn      net.Listener
	vizCh      chan []byte  // buffered ~5s; Write drops (viz lags) instead of blocking
	vizDropped atomic.Int64 // bytes dropped from vizCh; repaid as silence by vizFeed
}

// overwriteInPlace replaces path's content without the file ever disappearing:
// new bytes over the old, then truncate any leftover tail. ffmpeg re-opens
// these files continuously (drawtext reload=1 every frame, image2 -loop 1 every
// loop iteration), and on Windows a rename replace has a delete-pending window
// in which that re-open fails — fatal to the filter/input and the stream. A
// reader can see a torn write instead, which at worst glitches a single frame.
func overwriteInPlace(path string, data []byte) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE, 0o644)
	if err != nil {
		return
	}
	if _, err := f.Write(data); err == nil {
		_ = f.Truncate(int64(len(data)))
	}
	_ = f.Close()
}

// encoderConfig captures everything the encoder ffmpeg needs.
type encoderConfig struct {
	FFmpegPath   string
	RTMPURL      string
	BgImagePath  string
	NowTxtPath   string
	ArtLivePath  string // anchor for generated banner assets (viz mask); the live art itself is served from memory
	FontFile     string
	Width        int
	Height       int
	FPS          int
	VideoEnabled bool   // false = audio-only stream, no video track
	VideoBitrate string // CBR rate; empty = auto (CRF)
	VideoCodec   string // resolved ffmpeg video encoder: "libx264" | "h264_nvenc"
	AudioBitrate string
	NowOverlay   bool   // draw the "now playing" lower-third banner
	VizStyle     string // banner visualization: "bars" | "wave" | "none"
	BannerBox    bool   // translucent box behind the banner
	LowLatency   bool   // x264 low-latency tuning (no lookahead/B-frames, tight VBV)
}

// startEncoder launches the persistent ffmpeg and returns it with stdin open.
func startEncoder(c encoderConfig) (*encoder, error) {
	// Diagnostics override: force a specific video encoder ("libx264"|"h264_nvenc")
	// without touching Settings, for A/B testing (e.g. isolating NVENC's startup
	// warm-up). Only honored when PLAYOUT_DIAG is on.
	if v := os.Getenv("PLAYOUT_DIAG_CODEC"); v != "" && os.Getenv("PLAYOUT_DIAG") != "" {
		c.VideoCodec = v
	}
	if c.FPS <= 0 {
		c.FPS = 10
	}
	if c.Width <= 0 {
		c.Width = 1280
	}
	if c.Height <= 0 {
		c.Height = 720
	}
	if c.AudioBitrate == "" {
		c.AudioBitrate = "160k"
	}
	if c.FontFile == "" {
		c.FontFile = findBoldFont()
	}

	banner := c.VideoEnabled && c.NowOverlay
	// audioInGraph: the visualization taps a *copy* of the program audio inside
	// the filtergraph (showfreqs/showwaves). The output audio is still mapped
	// directly from 0:a and never routed through the graph — routing it here
	// (via asplit) coupled it to the video branch's framesync, which reordered
	// the audio into the AAC encoder and sprayed non-monotonic-DTS warnings.
	audioInGraph := banner && c.VizStyle != "none"
	geom := bannerLayout(c.Width, c.Height)

	// drawtext needs the textfile to exist before the process starts.
	if err := os.MkdirAll(filepath.Dir(c.NowTxtPath), 0o755); err != nil {
		return nil, err
	}
	if err := os.WriteFile(c.NowTxtPath, []byte(" "), 0o644); err != nil {
		return nil, err
	}
	vizMaskPath := ""
	if audioInGraph {
		// Static pill mask for the visualization, supersampled 4x and
		// scaled down in the graph for smooth capsule edges.
		vizMaskPath = filepath.Join(filepath.Dir(c.ArtLivePath), "viz_mask.png")
		mask := pillMaskPNG(geom.pillsW*4, geom.vizH*4, (geom.pill+geom.pillGap)*4, geom.pill*4)
		if err := os.WriteFile(vizMaskPath, mask, 0o644); err != nil {
			return nil, err
		}
	}
	// zmqAddr is the loopback endpoint the banner's zmq filter binds; the fade
	// commands are sent here. Chosen per-encoder so restarts don't collide.
	zmqAddr := ""
	if banner {
		// The art input file must exist (at its canonical size) before ffmpeg
		// starts; the first publish swaps in the real cover once a song starts.
		// It is updated with overwriteInPlace, never renamed, so ffmpeg's
		// per-frame re-opens can't hit a missing file. The fade mask is gone —
		// the banner alpha is driven over ZMQ (see zmqSender).
		if err := os.WriteFile(c.ArtLivePath, placeholderPNG(), 0o644); err != nil {
			return nil, err
		}
		if port, err := freePort(); err == nil {
			zmqAddr = fmt.Sprintf("tcp://127.0.0.1:%d", port)
		} else {
			// Extremely unlikely; without a port the banner can't fade in and
			// would stay hidden, so fail the start rather than air a dead banner.
			return nil, fmt.Errorf("banner zmq port: %w", err)
		}
	}
	// With a visualization on, the filtergraph gets its own PCM copy over
	// loopback TCP (see encoder.vizCh) so it can never back-pressure stdin.
	var vizLn net.Listener
	if audioInGraph {
		var err error
		if vizLn, err = net.Listen("tcp", "127.0.0.1:0"); err != nil {
			return nil, err
		}
	}

	args := []string{"-hide_banner", "-loglevel", "warning",
		// Audio FIRST so it anchors the muxer clock.
		"-f", "s16le", "-ar", "48000", "-ac", "2", "-i", "pipe:0",
	}

	if c.VideoEnabled {
		// Video input: looping still image if present, else a flat color source.
		if c.BgImagePath != "" {
			if _, err := os.Stat(c.BgImagePath); err == nil {
				// Pre-scale the still to the output size ONCE. The -loop 1 input
				// re-decodes its source every frame, so feeding the original
				// (often a multi-megapixel photo) makes ffmpeg re-decode and
				// re-downscale it at the video rate — ~a quarter core for a 12MP
				// JPEG, plus swscaler's full-range "deprecated pixel format"
				// warning. A pre-scaled rgb24 PNG is trivial to re-decode and
				// needs no range conversion. On any failure we fall back to the
				// original path (unchanged behavior).
				bgInput := c.BgImagePath
				if scaled, ok := prescaleBackground(c.FFmpegPath, c.BgImagePath, c.Width, c.Height, filepath.Dir(c.ArtLivePath)); ok {
					bgInput = scaled
				}
				args = append(args, "-loop", "1", "-framerate", itoa(c.FPS), "-i", bgInput)
			} else {
				args = append(args, colorInput(c)...)
			}
		} else {
			args = append(args, colorInput(c)...)
		}
		if banner {
			// Cover art (input 2). image2 with -loop 1 re-reads the file on every
			// loop iteration, so an in-place overwrite (never a rename — see
			// overwriteInPlace) updates the content mid-stream. The art swaps once
			// per transition while the banner is hidden, so a single re-read
			// collision at most. thread_queue_size buffers decoded frames so that
			// re-read hitch can't starve the filtergraph. A queue of N delays a
			// content update by up to N frames; small enough to be imperceptible
			// for the banner, big enough to absorb the hitch. (The fade no longer
			// churns a file — the banner alpha rides ZMQ; see zmqSender.)
			bannerTQS := "1"
			if os.Getenv("PLAYOUT_DIAG") != "" {
				if v := os.Getenv("PLAYOUT_DIAG_TQS"); v != "" {
					bannerTQS = v
				}
			}
			args = append(args,
				"-thread_queue_size", bannerTQS, "-loop", "1", "-framerate", itoa(c.FPS), "-f", "image2", "-i", c.ArtLivePath,
			)
			if audioInGraph {
				// Static for the whole show, but fed at the video rate: a
				// slower rate (e.g. 1fps) makes the blend's framesync
				// intermittently drop the mask for whole seconds.
				args = append(args, "-thread_queue_size", bannerTQS, "-loop", "1", "-framerate", itoa(c.FPS), "-f", "image2", "-i", vizMaskPath)
				// Input 4: the visualization's own PCM feed over loopback TCP.
				// Keeping the viz off input 0 means the broadcast audio path is
				// stdin → AAC only — a slow video branch can starve the viz but
				// never the program audio. The generous thread queue plus the Go
				// side's buffered channel absorb multi-second video stalls.
				args = append(args,
					"-f", "s16le", "-ar", "48000", "-ac", "2",
					"-thread_queue_size", "512",
					"-i", "tcp://"+vizLn.Addr().String(),
				)
			}
		}

		filter := buildVideoFilter(c, geom, banner, audioInGraph, zmqAddr)

		gop := c.FPS
		// The broadcast audio is always mapped directly from the PCM input and
		// never touches the filtergraph (the viz renders from its own TCP copy,
		// input 4) — that is what keeps the AAC DTS monotonic and makes the
		// audio immune to video-side stalls.
		args = append(args, "-filter_complex", filter, "-map", "[v]", "-map", "0:a")
		// Off low-latency the 4x-rate VBV buffer gives rate control burst
		// headroom to re-sharpen the banner after it fades in; low-latency
		// shrinks it to 1x (~1s) to cut HRD buffering delay. Applies to both
		// encoders; the line rate itself stays constant.
		bufFactor := 4.0
		if c.LowLatency {
			bufFactor = 1.0
		}
		if c.VideoCodec == "h264_nvenc" {
			// GPU path: NVENC uploads the yuv420p system-memory frames itself,
			// so the CPU filtergraph above is unchanged. Off low-latency -preset
			// p5 leans toward quality (radio is low-fps); low-latency drops to
			// p4 with the ull ("ultra low latency") tune and disables B-frame
			// reorder / rate-control look-ahead / encoder output delay.
			//
			// KNOWN: on some GPUs/drivers (e.g. older Pascal), NVENC buffers ~8-10s
			// of frames before emitting its first packet at process start. That is
			// a ONE-TIME warm-up at encoder start — it back-pressures the whole
			// filtergraph (the viz feed backs up and the stream delivers nothing,
			// then bursts) so a viewer watching a *fresh* show start sees ~10s of
			// stutter, then perfectly smooth playback (song changes never re-stall,
			// since the encoder process stays warm). No NVENC flag observed to
			// avoid it (-delay/-rc-lookahead/-bf/-surfaces/-tune ll|ull all still
			// buffer); libx264 has no such warm-up. Measured via PLAYOUT_DIAG.
			// Select the CPU encoder in Settings for a stutter-free start.
			preset := "p5"
			if c.LowLatency {
				preset = "p4"
			}
			args = append(args,
				"-c:v", "h264_nvenc", "-preset", preset,
				"-pix_fmt", "yuv420p", "-r", itoa(c.FPS), "-g", itoa(gop),
				"-fps_mode", "cfr",
			)
			if c.LowLatency {
				args = append(args, "-tune", "ull", "-rc-lookahead", "0", "-bf", "0", "-delay", "0")
			}
			if c.VideoBitrate != "" {
				// NVENC-native CBR (the libx264 -x264-params nal-hrd=cbr flag is
				// rejected here). The VBV buffer matches the libx264 path above.
				args = append(args, "-rc", "cbr", "-b:v", c.VideoBitrate, "-maxrate", c.VideoBitrate,
					"-bufsize", scaleRate(c.VideoBitrate, bufFactor))
			}
		} else {
			// No -tune stillimage: with the banner's animated visualization the
			// frame has a permanently moving region, and stillimage's psy tuning
			// makes x264 take many seconds to re-sharpen it after the banner
			// fades in under a tight CBR budget. preset medium is still cheap at
			// these low frame rates and buys sharper pills per bit — except in
			// low-latency mode, where the medium preset's ~40-frame rc-lookahead is
			// ~4s of delay at these low frame rates, so we drop to veryfast and
			// disable every look-ahead/reorder stage below.
			preset := "medium"
			if c.LowLatency {
				preset = "veryfast"
			}
			args = append(args,
				"-c:v", "libx264", "-preset", preset,
				"-pix_fmt", "yuv420p", "-r", itoa(c.FPS), "-g", itoa(gop),
				"-fps_mode", "cfr",
			)
			// x264-params assembled once so low-latency keys apply in both the CBR
			// and CRF (empty bitrate) paths.
			var x264Params []string
			if c.VideoBitrate != "" {
				// CBR on the wire (min=max=target + HRD filler).
				args = append(args, "-b:v", c.VideoBitrate, "-minrate", c.VideoBitrate, "-maxrate", c.VideoBitrate,
					"-bufsize", scaleRate(c.VideoBitrate, bufFactor))
				x264Params = append(x264Params, "nal-hrd=cbr")
			}
			if c.LowLatency {
				// The x264 equivalent of NVENC "Ultra Low Latency": no B-frame
				// reorder, no rate-control/sync look-ahead, slice-threaded (frame
				// threading adds threads-worth of frames of delay), no scenecut.
				// mbtree needs a look-ahead window; with rc-lookahead=0 x264 warns
				// ("lookaheadless mb-tree requires intra refresh or infinite
				// keyint") and disables it anyway, so turn it off explicitly.
				x264Params = append(x264Params,
					"bframes=0", "rc-lookahead=0", "sync-lookahead=0", "sliced-threads=1", "scenecut=0", "mbtree=0")
			}
			if len(x264Params) > 0 {
				args = append(args, "-x264-params", strings.Join(x264Params, ":"))
			}
		}
	}

	args = append(args, "-c:a", "aac", "-b:a", c.AudioBitrate, "-ar", "48000", "-ac", "2")
	// Audio is always mapped directly from 0:a (never through the filtergraph),
	// so the resampler always applies here as a simple output filter.
	args = append(args, "-af", "aresample=async=1000:first_pts=0")
	if c.LowLatency {
		// Push each packet to the wire as soon as it's muxed instead of letting
		// the FLV muxer accumulate a buffer.
		args = append(args, "-flush_packets", "1")
	}
	// Never defer a writable packet waiting for the other stream to catch up:
	// audio is fed slightly ahead of the CFR video (300ms lead + pacing bursts),
	// so without this the FLV muxer's interleave queue can back up
	// ("N buffers queued in out_#0:1") and hard re-base the audio timeline.
	args = append(args, "-max_interleave_delta", "0")
	args = append(args,
		// -shortest: the looping video never EOFs on its own, so tie output
		// length to the audio pipe. Closing stdin then ends the stream cleanly
		// (finalizing the FLV trailer) instead of requiring a hard kill.
		// (No-op in audio-only mode, where stdin is the only input.)
		"-shortest",
		"-f", "flv", c.RTMPURL,
	)

	cmd := exec.Command(c.FFmpegPath, args...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		if vizLn != nil {
			vizLn.Close()
		}
		return nil, err
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		if vizLn != nil {
			vizLn.Close()
		}
		return nil, fmt.Errorf("encoder ffmpeg start failed: %w", err)
	}

	e := &encoder{cmd: cmd, stdin: stdin, nowTxt: c.NowTxtPath, artLive: c.ArtLivePath, zmqAddr: zmqAddr, bannerOn: banner, done: make(chan struct{})}
	e.stdinCh = make(chan []byte, 256) // ~5s of 20ms chunks; decouples the pacing loop from pipe:0
	go e.stdinPump()
	if banner {
		// Drive the banner fade over ZMQ. The tickle is buffered so a fade level
		// pushed before zmqSender finishes dialing isn't lost (fadeLevel holds the
		// latest and the pending tickle survives).
		e.fadeTickle = make(chan struct{}, 1)
		go e.zmqSender()
	}
	if vizLn != nil {
		e.vizLn = vizLn
		e.vizCh = make(chan []byte, 256) // ~5s of 20ms chunks
		go e.vizFeed()
	}
	go func() { _ = cmd.Wait(); close(e.done) }()
	return e, nil
}

// vizFeed accepts the encoder ffmpeg's single connection to the viz PCM input
// and pumps chunks from vizCh into it. The TCP byte count IS the viz timeline
// (showfreqs derives PTS from samples received), so bytes Write had to drop are
// repaid as silence before the next chunk — the bars flatline briefly under
// duress instead of running ahead of the audio forever. Exits when vizCh
// closes (encoder Stop) or the peer goes away; a full channel never blocks the
// pacing loop.
func (e *encoder) vizFeed() {
	conn, err := e.vizLn.Accept()
	if err != nil {
		return // listener closed before ffmpeg connected (startup failure path)
	}
	defer conn.Close()
	var zeros []byte
	for chunk := range e.vizCh {
		if d := e.vizDropped.Swap(0); d > 0 {
			// Beyond ~5s the viz timeline is lost anyway; don't spam zeros.
			d = min(d, int64(5*bytesPerSec))
			d -= d % frameBytes
			log.Printf("playout: viz feed dropped %d bytes — repaying as silence", d)
			if zeros == nil {
				zeros = make([]byte, 64*1024)
			}
			for d > 0 {
				n := int64(len(zeros))
				if d < n {
					n = d
				}
				if _, err := conn.Write(zeros[:n]); err != nil {
					for range e.vizCh {
					}
					return
				}
				d -= n
			}
		}
		if _, err := conn.Write(chunk); err != nil {
			// ffmpeg is gone or stalled hard; drain until close so senders'
			// buffered chunks don't linger.
			for range e.vizCh {
			}
			return
		}
	}
}

// Write hands a PCM chunk to the encoder. It never touches stdin directly:
// stdinPump owns that write so a stalled pipe:0 can't freeze this call (and the
// viz feed with it). A prior pipe:0 error (ffmpeg died) is surfaced here so the
// pacing loop stops the show. run allocates a fresh chunk per tick and discards
// it after Write, so both the viz send and the pump handoff can retain it
// without copying.
func (e *encoder) Write(p []byte) error {
	if v := e.stdinErr.Load(); v != nil {
		return v.(error)
	}
	if e.vizCh != nil {
		select {
		case e.vizCh <- p:
		default:
			// Viz feed backed up — drop rather than block, but account for the
			// bytes so vizFeed repays them as silence and the viz timeline
			// stays sample-accurate.
			e.vizDropped.Add(int64(len(p)))
		}
	}
	// Buffered (~5s). Immediate while pipe:0 keeps up; blocks only if ffmpeg
	// stalls its output for the whole buffer — a real sustained failure, not the
	// transient muxer starvation the buffer exists to ride through.
	e.stdinCh <- p
	return nil
}

// stdinPump drains stdinCh into ffmpeg's pipe:0, off the pacing loop. Closing
// stdinCh (Stop) flushes the remaining chunks and then EOFs the pipe (via the
// deferred Close), which is what -shortest keys the clean FLV shutdown off. A
// write error means ffmpeg exited; it's recorded for Write to surface, and the
// channel is drained so senders never block on a dead pipe.
func (e *encoder) stdinPump() {
	defer e.stdin.Close()
	for p := range e.stdinCh {
		start := time.Now()
		_, err := e.stdin.Write(p)
		if d := time.Since(start); d > 100*time.Millisecond {
			log.Printf("playout: encoder stdin write stalled %dms — ffmpeg not consuming audio", d.Milliseconds())
		}
		if err != nil {
			e.stdinErr.Store(err)
			for range e.stdinCh { // drain until Stop closes the channel
			}
			return
		}
	}
}

// SetNowPlaying updates the overlay text in place (see overwriteInPlace —
// drawtext reload=1 re-reads the file every frame; worst case a single frame
// renders a mix of old and new text). The banner renders uppercase (drawtext
// has no text-transform, so it happens here).
func (e *encoder) SetNowPlaying(text string) {
	if text == "" {
		text = " "
	}
	overwriteInPlace(e.nowTxt, []byte(strings.ToUpper(text)))
}

// SetNowArt swaps the banner cover art. src must be a normalized
// artSize×artSize PNG — anything else would break the running filter graph;
// "" (or an unreadable file) restores the placeholder.
func (e *encoder) SetNowArt(src string) {
	if !e.bannerOn {
		return
	}
	data := placeholderPNG()
	if src != "" {
		if b, err := os.ReadFile(src); err == nil {
			data = b
		}
	}
	overwriteInPlace(e.artLive, data)
}

// fadeLevels quantizes banner fade alpha; level 0 = hidden, fadeLevels = shown.
const fadeLevels = 15

// SetBannerFade sets the banner fade to level (0..fadeLevels). The engine steps
// the level over time to animate the banner in and out. This only stamps the
// latest level and pokes zmqSender — it never blocks the pacing loop (a full
// tickle is dropped; fadeLevel still holds the newest value for the sender).
func (e *encoder) SetBannerFade(level int) {
	if !e.bannerOn {
		return
	}
	if level < 0 {
		level = 0
	}
	if level > fadeLevels {
		level = fadeLevels
	}
	e.fadeLevel.Store(int32(level))
	select {
	case e.fadeTickle <- struct{}{}:
	default:
	}
}

// zmqSender owns the ZMQ REQ socket and translates fade levels into
// colorchannelmixer@fade "aa" (alpha gain) commands to ffmpeg's zmq REP socket.
// REQ/REP is strict lockstep — every Send is paired with a Recv. It coalesces:
// only the newest level is sent, so a burst of SetBannerFade calls during a fade
// collapses to at most one command per completed round-trip. Exits when the
// encoder process ends (done closed) or the socket errors (ffmpeg gone).
func (e *encoder) zmqSender() {
	req := zmq4.NewReq(context.Background())
	defer req.Close()
	// ffmpeg binds its REP socket a moment after process start; retry the dial.
	dialed := false
	for i := 0; i < 50; i++ {
		if err := req.Dial(e.zmqAddr); err == nil {
			dialed = true
			break
		}
		select {
		case <-e.done:
			return
		case <-time.After(100 * time.Millisecond):
		}
	}
	if !dialed {
		log.Printf("playout: banner ZMQ dial failed (%s) — banner fade disabled", e.zmqAddr)
		return
	}
	last := int32(-1)
	for {
		select {
		case <-e.done:
			return
		case <-e.fadeTickle:
		}
		lvl := e.fadeLevel.Load()
		if lvl == last {
			continue
		}
		last = lvl
		cmd := fmt.Sprintf("colorchannelmixer@fade aa %.4f", float64(lvl)/float64(fadeLevels))
		if err := req.Send(zmq4.NewMsgString(cmd)); err != nil {
			log.Printf("playout: banner ZMQ send failed: %v", err)
			return
		}
		if _, err := req.Recv(); err != nil { // REQ/REP lockstep
			log.Printf("playout: banner ZMQ recv failed: %v", err)
			return
		}
	}
}

// freePort reserves an ephemeral loopback TCP port and returns its number. The
// listener is closed immediately, so there is a small window before the zmq
// filter re-binds it; acceptable since only one encoder runs at a time.
func freePort() (int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port, nil
}

// Stop closes stdinCh so stdinPump flushes any buffered PCM and then EOFs pipe:0
// (→ -shortest ends the stream and finalizes the FLV), and reaps the process,
// falling back to a kill if ffmpeg hangs. Safe because run's defer calls Stop
// only after the pacing loop has stopped calling Write.
func (e *encoder) Stop() {
	close(e.stdinCh)
	select {
	case <-e.done:
	case <-time.After(3 * time.Second):
		if e.cmd.Process != nil {
			_ = e.cmd.Process.Kill()
		}
		<-e.done
	}
	if e.vizCh != nil {
		close(e.vizCh) // run has stopped writing by now; lets vizFeed exit
	}
	if e.vizLn != nil {
		e.vizLn.Close()
	}
}

// Done returns a channel closed when the encoder process exits.
func (e *encoder) Done() <-chan struct{} { return e.done }

func colorInput(c encoderConfig) []string {
	return []string{"-f", "lavfi", "-i",
		fmt.Sprintf("color=c=0x0a1628:s=%dx%d:r=%d", c.Width, c.Height, c.FPS)}
}

// prescaleBackground renders the still background to a single output-sized rgb24
// PNG (dest dir/"bg_scaled.png") so the persistent encoder's -loop 1 input can
// re-decode a trivially small image every frame instead of re-decoding and
// re-downscaling the original (potentially many-megapixel) source. It matches the
// filtergraph's scale=W:H exactly (stretch, no aspect preservation), so framing is
// unchanged. Returns the scaled path and true on success; on any failure the
// caller feeds the original image (unchanged behavior). Bounded so a pathological
// source can't wedge stream startup.
func prescaleBackground(ffmpegPath, src string, w, h int, destDir string) (string, bool) {
	dest := filepath.Join(destDir, "bg_scaled.png")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, ffmpegPath,
		"-hide_banner", "-loglevel", "error", "-y",
		"-i", src,
		"-vf", fmt.Sprintf("scale=%d:%d", w, h),
		"-frames:v", "1", "-pix_fmt", "rgb24", dest,
	)
	if err := cmd.Run(); err != nil {
		return "", false
	}
	return dest, true
}

// bannerGeom is the lower-third layout: the bar's position in final output
// pixels (inset into the title-safe area, 5% margin from the left/right/bottom
// edges), and the art/text/viz placement in bar-relative pixels. The banner is
// composed on its own bar-sized canvas so text and viz clip at the bar's edge
// instead of spilling over it.
type bannerGeom struct {
	barX, barY, barW, barH int // bar placement in the output frame
	art                    int // art tile side (== barH, at the bar's left end)
	textX, textY, fontSize int // bar-relative
	vizX, vizY, vizW, vizH int // bar-relative

	// Pill grid for the bars visualization: capsule columns of width pill
	// every pill+pillGap pixels, pillBars of them spanning pillsW, starting at
	// pillsX (the viz region's width rounded down to whole pills, centered).
	pill, pillGap            int
	pillBars, pillsW, pillsX int
}

func bannerLayout(w, h int) bannerGeom {
	even := func(n int) int { return n &^ 1 }
	g := bannerGeom{}
	marginX := even(w * 5 / 100)
	marginY := even(h * 5 / 100)
	g.barH = even(h * 16 / 100)
	g.barX = marginX
	g.barW = even(w - 2*marginX)
	g.barY = h - g.barH - marginY
	g.art = g.barH
	pad := w * 15 / 1000
	g.textX = g.art + pad
	g.textY = g.barH * 14 / 100
	g.fontSize = g.barH * 30 / 100
	g.vizX = g.textX
	g.vizW = even(g.barW - g.textX - pad)
	g.vizH = even(g.barH * 40 / 100)
	g.vizY = g.barH - g.vizH - g.barH*10/100

	g.pill = g.vizH * 10 / 100
	if g.pill < 3 {
		g.pill = 3
	}
	g.pillGap = g.pill * 3 / 4
	if g.pillGap < 2 {
		g.pillGap = 2
	}
	period := g.pill + g.pillGap
	g.pillBars = g.vizW / period
	g.pillsW = g.pillBars * period
	g.pillsX = g.vizX + (g.vizW-g.pillsW)/2
	return g
}

// buildVideoFilter assembles the filter_complex for the video output. Inputs:
// [1:v] background (still or lavfi color), [2:v] cover art (banner only), and
// with a visualization on ([3:v] pill mask, [4:a] the viz's own PCM feed). The
// background is scaled to the output size FIRST so the banner overlays in final
// pixel space; the banner itself is built on a translucent bar-sized canvas so
// its contents clip at the bar's bounds. The banner alpha is scaled by the
// colorchannelmixer@fade filter, whose "aa" gain the engine drives at runtime
// over ZMQ (zmqBind is the zmq filter's bind endpoint) to fade it in/out — no
// per-frame mask file to re-read. The zmq filter is a pass-through spliced onto
// the continuous background stream so it services commands every frame.
func buildVideoFilter(c encoderConfig, g bannerGeom, banner, audioInGraph bool, zmqBind string) string {
	if !banner {
		return fmt.Sprintf("[1:v]scale=%d:%d,setsar=1,format=yuv420p,fps=%d[v]", c.Width, c.Height, c.FPS)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "[1:v]zmq=bind_address=%s,scale=%d:%d,setsar=1,fps=%d[bg];", zmqBindArg(zmqBind), c.Width, c.Height, c.FPS)
	fmt.Fprintf(&b, "[2:v]scale=%d:%d[art];", g.art, g.art)
	// The banner canvas: the translucent box, or a fully transparent surface
	// of the same size when the box is turned off.
	boxColor := "black@0.55"
	if !c.BannerBox {
		boxColor = "black@0.0"
	}
	fmt.Fprintf(&b, "color=c=%s:s=%dx%d:r=%d,format=rgba[bar0];", boxColor, g.barW, g.barH, c.FPS)
	b.WriteString("[bar0][art]overlay=x=0:y=0[bar1];")

	drawtext := "drawtext="
	if c.FontFile != "" {
		drawtext += "fontfile=" + escapeFilterPath(c.FontFile) + ":"
	}
	fontSize := g.fontSize
	textY := itoa(g.textY)
	if !audioInGraph {
		// No visualization below the text — center it vertically in the bar
		// and let it take more of the height.
		fontSize = g.barH * 42 / 100
		textY = "(h-text_h)/2"
	}
	shadow := ""
	if !c.BannerBox {
		// Without the box the text sits straight on the video — give it a
		// drop shadow so it stays readable on bright backgrounds.
		shadow = fmt.Sprintf(":shadowcolor=black@0.7:shadowx=%d:shadowy=%d", fontSize/16+1, fontSize/16+1)
	}
	fmt.Fprintf(&b, "[bar1]%stextfile=%s:reload=1:expansion=none:x=%d:y=%s:fontsize=%d:fontcolor=white%s[bar2];",
		drawtext, escapeFilterPath(c.NowTxtPath), g.textX, textY, fontSize, shadow)

	last := "bar2"
	if audioInGraph {
		// The visualization renders from its own PCM feed ([4:a], the loopback
		// TCP input) — never from [0:a]. The AAC output is mapped directly from
		// 0:a, so the broadcast audio neither rides this graph's framesync
		// (which once reordered it into non-monotonic DTS) nor gets
		// back-pressured when the video branch stalls.
		// Both styles render one column per pill, get blown up with
		// nearest-neighbor into uniform chunky bars, then the pill mask
		// ([3:v]) multiplies the alpha to cut the gaps and round the ends.
		switch c.VizStyle {
		case "wave":
			// Centered amplitude envelope (audiogram look). Mono downmix on
			// the viz branch only; cline's soft gradient is forced to solid
			// white by thresholding the alpha.
			// draw=full paints covered pixels at full intensity — without it,
			// real-world (quieter) audio accumulates faint per-sample dots
			// that crumble to speckles at the alpha threshold below.
			// Full video rate: each frame spans 1/FPS of audio. A halved rate
			// (wider time window) was tried and felt choppy/out-of-sync.
			fmt.Fprintf(&b, "[4:a]aformat=channel_layouts=mono,showwaves=s=%dx%d:mode=cline:rate=%d:scale=sqrt:draw=full[visraw];",
				g.pillBars, g.vizH, c.FPS)
			fmt.Fprintf(&b, "[visraw]scale=%d:%d:flags=neighbor,format=rgba,lutrgb=r=255:g=255:b=255:a='if(gt(val,60),255,0)'[visq];",
				g.pillsW, g.vizH)
		default: // "bars"
			fmt.Fprintf(&b, "[4:a]showfreqs=s=%dx%d:mode=bar:ascale=log:fscale=log:win_size=1024:averaging=4:cmode=combined:colors=white:rate=%d[visraw];",
				g.pillBars, g.vizH, c.FPS)
			fmt.Fprintf(&b, "[visraw]scale=%d:%d:flags=neighbor,format=rgba[visq];", g.pillsW, g.vizH)
		}
		fmt.Fprintf(&b, "[3:v]scale=%d:%d,format=rgba[pmask];", g.pillsW, g.vizH)
		b.WriteString("[visq][pmask]blend=c3_mode=multiply[vis];")
		fmt.Fprintf(&b, "[bar2][vis]overlay=x=%d:y=%d:format=auto[bar3];", g.pillsX, g.vizY)
		last = "bar3"
	}
	// colorchannelmixer@fade scales only the alpha plane (aa gain) uniformly, so
	// setting aa in 0..1 fades the whole banner in/out. It starts at 0 (hidden,
	// matching the engine's fadeAlpha=0 at startup); the engine drives aa up/down
	// over ZMQ. Named "@fade" so the zmq command can target this instance.
	fmt.Fprintf(&b, "[%s]format=rgba,colorchannelmixer@fade=aa=0[bnr];", last)
	fmt.Fprintf(&b, "[bg][bnr]overlay=x=%d:y=%d:format=auto[bv];[bv]format=yuv420p[v]", g.barX, g.barY)
	return b.String()
}

// zmqBindArg formats a "tcp://host:port" endpoint for use as the zmq filter's
// bind_address inside a filtergraph. ffmpeg's option parser treats ':' as an
// option separator even mid-value, and the graph parser strips one backslash
// level, so each colon must be double-backslash escaped (verified empirically:
// a single backslash is stripped and the bind fails to parse).
func zmqBindArg(addr string) string {
	return strings.ReplaceAll(addr, ":", "\\\\:")
}

// escapeFilterPath makes a filesystem path safe inside an ffmpeg filtergraph
// option value. ffmpeg requires the value single-quoted AND the Windows drive
// colon backslash-escaped even when quoted (verified empirically). Forward
// slashes work on all platforms. On Linux paths have no colon, so this is just
// a harmless quote.
func escapeFilterPath(p string) string {
	p = filepath.ToSlash(p)
	p = strings.ReplaceAll(p, "\\", "/")
	p = strings.ReplaceAll(p, ":", "\\:")
	return "'" + p + "'"
}

// findBoldFont returns a bold TTF for the banner title, falling back to the
// regular weights (and finally "" → fontconfig default).
func findBoldFont() string {
	var candidates []string
	switch runtime.GOOS {
	case "windows":
		candidates = []string{`C:\Windows\Fonts\arialbd.ttf`, `C:\Windows\Fonts\seguisb.ttf`}
	default:
		candidates = []string{
			"/usr/share/fonts/truetype/dejavu/DejaVuSans-Bold.ttf",
			"/usr/share/fonts/TTF/DejaVuSans-Bold.ttf",
			"/usr/share/fonts/dejavu/DejaVuSans-Bold.ttf",
			"/usr/share/fonts/truetype/noto/NotoSans-Bold.ttf",
			"/usr/share/fonts/noto/NotoSans-Bold.ttf",
		}
	}
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			return c
		}
	}
	return findFont()
}

// findFont returns a usable TTF for drawtext across platforms, or "" to let
// fontconfig resolve a default.
func findFont() string {
	var candidates []string
	switch runtime.GOOS {
	case "windows":
		candidates = []string{`C:\Windows\Fonts\arial.ttf`, `C:\Windows\Fonts\segoeui.ttf`}
	default:
		candidates = []string{
			"/usr/share/fonts/noto/NotoSans-Regular.ttf",
			"/usr/share/fonts/truetype/dejavu/DejaVuSans.ttf",
			"/usr/share/fonts/TTF/DejaVuSans.ttf",
			"/usr/share/fonts/dejavu/DejaVuSans.ttf",
		}
	}
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			return c
		}
	}
	return ""
}

func itoa(n int) string { return fmt.Sprintf("%d", n) }

// DetectNVENC reports whether NVIDIA hardware H.264 encoding is actually usable
// with this ffmpeg build and host. Presence of h264_nvenc in `-encoders` only
// proves it was compiled in, not that a GPU/driver is reachable (e.g. inside a
// container), so this runs a tiny real encode and checks the exit code. Meant to
// be called once at startup; the result is cached by the caller.
//
// The probe frame is 256x256: NVENC has a minimum frame size and rejects tiny
// frames with "Frame Dimension less than the minimum supported value", so a
// 64x64 probe fails even on a fully working GPU. On failure the encoder's stderr
// is logged so the real reason (no device, driver mismatch, missing nvcuda.dll)
// is visible instead of a bare "not available".
func DetectNVENC(ffmpegPath string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, ffmpegPath,
		"-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "color=black:s=256x256:r=5", "-t", "0.2",
		"-c:v", "h264_nvenc", "-f", "null", "-",
	)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			if i := strings.IndexByte(msg, '\n'); i >= 0 {
				msg = msg[:i]
			}
			log.Printf("NVENC probe failed (%v): %s", err, msg)
		}
		return false
	}
	return true
}

// resolveVideoCodec maps a settings preference ("auto"|"cpu"|"nvenc") plus the
// detected NVENC availability to the concrete ffmpeg encoder name. "nvenc"
// (forced) still falls back to libx264 when unavailable so a misconfigured host
// can't take the stream down.
func resolveVideoCodec(pref string, nvencAvailable bool) string {
	switch pref {
	case "cpu":
		return "libx264"
	case "nvenc":
		if nvencAvailable {
			return "h264_nvenc"
		}
		log.Printf("video encoder: 'nvenc' selected but NVENC not available — falling back to libx264")
		return "libx264"
	default: // "auto" and any unknown value
		if nvencAvailable {
			return "h264_nvenc"
		}
		return "libx264"
	}
}

// scaleRate multiplies an ffmpeg bitrate ("500k" × 4 → "2000k") for -bufsize.
// Unparseable rates fall back unchanged, which ffmpeg treats as a 1x buffer.
func scaleRate(rate string, factor float64) string {
	num, suffix := rate, ""
	if len(num) > 0 {
		switch num[len(num)-1] {
		case 'k', 'K', 'm', 'M':
			suffix = string(num[len(num)-1])
			num = num[:len(num)-1]
		}
	}
	v, err := strconv.ParseFloat(num, 64)
	if err != nil {
		return rate
	}
	return strconv.FormatFloat(v*factor, 'f', -1, 64) + suffix
}
