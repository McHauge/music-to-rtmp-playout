package playout

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// encoder is the persistent ffmpeg process that muxes the canonical PCM stream
// (received on stdin) with a static/looping background + now-playing overlay,
// and pushes FLV to the RTMP target. It must stay up for the whole show; the
// mixer keeps feeding it PCM (silence during gaps) so it never starves.
type encoder struct {
	cmd      *exec.Cmd
	stdin    io.WriteCloser
	nowTxt   string
	artLive  string // live-swapped banner art; "" when the banner is off
	fadeLive string // live-swapped banner fade mask (uniform-alpha png)
	bannerOn bool
	done     chan struct{}
}

// encoderConfig captures everything the encoder ffmpeg needs.
type encoderConfig struct {
	FFmpegPath   string
	RTMPURL      string
	BgImagePath  string
	NowTxtPath   string
	ArtLivePath  string // square cover art swapped per song (banner)
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
	fadeLivePath := ""
	if banner {
		// The art and fade-mask input files must exist (at their canonical
		// sizes) before ffmpeg starts; the first publish swaps in the real
		// cover and fades the banner in once a song actually starts.
		if err := os.WriteFile(c.ArtLivePath, placeholderPNG(), 0o644); err != nil {
			return nil, err
		}
		fadeLivePath = filepath.Join(filepath.Dir(c.ArtLivePath), "banner_fade.png")
		if err := os.WriteFile(fadeLivePath, uniformAlphaPNG(0), 0o644); err != nil {
			return nil, err
		}
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

	args := []string{"-hide_banner", "-loglevel", "warning",
		// Audio FIRST so it anchors the muxer clock.
		"-f", "s16le", "-ar", "48000", "-ac", "2", "-i", "pipe:0",
	}

	if c.VideoEnabled {
		// Video input: looping still image if present, else a flat color source.
		if c.BgImagePath != "" {
			if _, err := os.Stat(c.BgImagePath); err == nil {
				args = append(args, "-loop", "1", "-framerate", itoa(c.FPS), "-i", c.BgImagePath)
			} else {
				args = append(args, colorInput(c)...)
			}
		} else {
			args = append(args, colorInput(c)...)
		}
		if banner {
			// Cover art (input 2) and fade mask (input 3). image2 with -loop 1
			// re-reads the file on every loop iteration, so an atomic rename
			// swaps the content mid-stream. -thread_queue_size 1 keeps the
			// demuxer from reading ahead, so a swap shows within ~2 frames;
			// re-decoding these tiny PNGs at the video rate is negligible.
			args = append(args,
				"-thread_queue_size", "1", "-loop", "1", "-framerate", itoa(c.FPS), "-f", "image2", "-i", c.ArtLivePath,
				"-thread_queue_size", "1", "-loop", "1", "-framerate", itoa(c.FPS), "-f", "image2", "-i", fadeLivePath,
			)
			if audioInGraph {
				// Static for the whole show, but fed at the video rate: a
				// slower rate (e.g. 1fps) makes the blend's framesync
				// intermittently drop the mask for whole seconds.
				args = append(args, "-thread_queue_size", "1", "-loop", "1", "-framerate", itoa(c.FPS), "-f", "image2", "-i", vizMaskPath)
			}
		}

		filter := buildVideoFilter(c, geom, banner, audioInGraph)

		gop := c.FPS
		// Audio is always mapped directly from the PCM input, never through the
		// filtergraph. With the viz on, the graph taps a copy of 0:a for
		// showfreqs/showwaves only (see buildVideoFilter); the output audio stays
		// on the simple -map 0:a + -af path, which is what keeps its DTS monotonic.
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
		return nil, err
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("encoder ffmpeg start failed: %w", err)
	}

	e := &encoder{cmd: cmd, stdin: stdin, nowTxt: c.NowTxtPath, artLive: c.ArtLivePath, fadeLive: fadeLivePath, bannerOn: banner, done: make(chan struct{})}
	go func() { _ = cmd.Wait(); close(e.done) }()
	return e, nil
}

// Write feeds a PCM chunk to the encoder. A write error (EPIPE) means ffmpeg died.
func (e *encoder) Write(p []byte) error {
	_, err := e.stdin.Write(p)
	return err
}

// SetNowPlaying atomically updates the overlay text (temp file + rename) so
// drawtext never reads a half-written line. The banner renders uppercase
// (drawtext has no text-transform, so it happens here).
func (e *encoder) SetNowPlaying(text string) {
	if text == "" {
		text = " "
	}
	text = strings.ToUpper(text)
	tmp := e.nowTxt + ".tmp"
	if err := os.WriteFile(tmp, []byte(text), 0o644); err != nil {
		return
	}
	_ = os.Rename(tmp, e.nowTxt)
}

// SetNowArt atomically swaps the banner cover art (temp file + rename, like
// SetNowPlaying). src must be a normalized artSize×artSize PNG — anything else
// would break the running filter graph; "" (or an unreadable file) restores the
// placeholder. Returns false if the swap could not be completed (caller may
// retry).
func (e *encoder) SetNowArt(src string) bool {
	if !e.bannerOn {
		return true
	}
	data := placeholderPNG()
	if src != "" {
		if b, err := os.ReadFile(src); err == nil {
			data = b
		}
	}
	return swapFile(e.artLive, data)
}

// fadeLevels quantizes banner fade alpha; level 0 = hidden, fadeLevels = shown.
const fadeLevels = 15

// SetBannerFade atomically swaps the fade mask to level (0..fadeLevels). The
// engine steps the level over time to animate the banner in and out. Returns
// false if the swap could not be completed — the caller must retry, or the
// banner would stick at a stale fade level.
func (e *encoder) SetBannerFade(level int) bool {
	if !e.bannerOn {
		return true
	}
	if level < 0 {
		level = 0
	}
	if level > fadeLevels {
		level = fadeLevels
	}
	return swapFile(e.fadeLive, uniformAlphaPNG(uint8(level*255/fadeLevels)))
}

// swapFile atomically replaces path with data (temp file + rename), reporting
// success. The rename can transiently collide with ffmpeg's periodic re-read of
// the file on Windows, hence the short immediate retry. No sleep: this runs on
// the run goroutine (via SetNowArt/SetBannerFade in publish), and a failed swap
// is harmless — the caller retries on the next 5ms tick.
func swapFile(path string, data []byte) bool {
	if data == nil {
		return false
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return false
	}
	for i := 0; i < 3; i++ {
		if err := os.Rename(tmp, path); err == nil {
			return true
		}
	}
	return false
}

// Stop closes stdin (audio EOF → -shortest ends the stream and finalizes the
// FLV) and reaps the process, falling back to a kill if ffmpeg hangs.
func (e *encoder) Stop() {
	_ = e.stdin.Close()
	select {
	case <-e.done:
	case <-time.After(3 * time.Second):
		if e.cmd.Process != nil {
			_ = e.cmd.Process.Kill()
		}
		<-e.done
	}
}

// Done returns a channel closed when the encoder process exits.
func (e *encoder) Done() <-chan struct{} { return e.done }

func colorInput(c encoderConfig) []string {
	return []string{"-f", "lavfi", "-i",
		fmt.Sprintf("color=c=0x0a1628:s=%dx%d:r=%d", c.Width, c.Height, c.FPS)}
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
// [1:v] background (still or lavfi color), [2:v] cover art and [3:v] fade mask
// (banner only), [0:a] program PCM (tapped only when a visualization is on —
// audioInGraph). The background is scaled to the output size FIRST so the
// banner overlays in final pixel space; the banner itself is built on a
// translucent bar-sized canvas so its contents clip at the bar's bounds, then
// the fade mask multiplies its alpha so the engine can fade it in/out.
func buildVideoFilter(c encoderConfig, g bannerGeom, banner, audioInGraph bool) string {
	if !banner {
		return fmt.Sprintf("[1:v]scale=%d:%d,setsar=1,format=yuv420p,fps=%d[v]", c.Width, c.Height, c.FPS)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "[1:v]scale=%d:%d,setsar=1,fps=%d[bg];", c.Width, c.Height, c.FPS)
	fmt.Fprintf(&b, "[2:v]scale=%d:%d[art];", g.art, g.art)
	fmt.Fprintf(&b, "[3:v]scale=%d:%d,format=rgba[fmask];", g.barW, g.barH)
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
		// Tap a copy of the program audio for the visualization only, straight
		// from [0:a]. The AAC output is mapped directly from 0:a (with its own
		// -af aresample), so it never rides this graph's framesync — an earlier
		// asplit=2[aout][avis] here routed the output audio through the graph and
		// delivered it to the AAC encoder out of order (non-monotonic DTS).
		// Both styles render one column per pill, get blown up with
		// nearest-neighbor into uniform chunky bars, then the pill mask
		// ([4:v]) multiplies the alpha to cut the gaps and round the ends.
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
			fmt.Fprintf(&b, "[0:a]aformat=channel_layouts=mono,showwaves=s=%dx%d:mode=cline:rate=%d:scale=sqrt:draw=full[visraw];",
				g.pillBars, g.vizH, c.FPS)
			fmt.Fprintf(&b, "[visraw]scale=%d:%d:flags=neighbor,format=rgba,lutrgb=r=255:g=255:b=255:a='if(gt(val,60),255,0)'[visq];",
				g.pillsW, g.vizH)
		default: // "bars"
			fmt.Fprintf(&b, "[0:a]showfreqs=s=%dx%d:mode=bar:ascale=log:fscale=log:win_size=1024:averaging=4:cmode=combined:colors=white:rate=%d[visraw];",
				g.pillBars, g.vizH, c.FPS)
			fmt.Fprintf(&b, "[visraw]scale=%d:%d:flags=neighbor,format=rgba[visq];", g.pillsW, g.vizH)
		}
		fmt.Fprintf(&b, "[4:v]scale=%d:%d,format=rgba[pmask];", g.pillsW, g.vizH)
		b.WriteString("[visq][pmask]blend=c3_mode=multiply[vis];")
		fmt.Fprintf(&b, "[bar2][vis]overlay=x=%d:y=%d:format=auto[bar3];", g.pillsX, g.vizY)
		last = "bar3"
	}
	// blend outputs the top layer's (banner's) color planes and multiplies the
	// alpha planes, so the mask's uniform alpha scales the whole banner.
	fmt.Fprintf(&b, "[%s]format=rgba[bnr0];[bnr0][fmask]blend=c3_mode=multiply[bnr];", last)
	fmt.Fprintf(&b, "[bg][bnr]overlay=x=%d:y=%d:format=auto[bv];[bv]format=yuv420p[v]", g.barX, g.barY)
	return b.String()
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
