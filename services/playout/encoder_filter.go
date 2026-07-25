package playout

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

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
// The result is memoized: startEncoder runs on every reconnect attempt, and
// against a down RTMP relay that would otherwise mean an ffmpeg spawn every few
// hundred milliseconds to redo identical work.
func prescaleBackground(ffmpegPath, src string, w, h int, destDir string) (string, bool) {
	dest := filepath.Join(destDir, "bg_scaled.png")
	key := prescaleKey{src: src, w: w, h: h, dest: dest}
	if fi, err := os.Stat(src); err == nil {
		key.srcSize, key.srcMod = fi.Size(), fi.ModTime()
	}
	if p, ok := prescaleCache.get(key, dest); ok {
		return p, true
	}

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
	prescaleCache.put(key)
	return dest, true
}

// prescaleKey identifies a scaled background by its source (path + size +
// mtime, so editing the image in place still invalidates) and target size.
type prescaleKey struct {
	src     string
	srcSize int64
	srcMod  time.Time
	w, h    int
	dest    string
}

// generatedAssetCache remembers the last generated artifact so an identical
// request can skip the work. One entry is enough: a running show regenerates
// the same asset over and over, it never alternates between two.
type generatedAssetCache struct {
	mu    sync.Mutex
	valid bool
	key   prescaleKey
}

var prescaleCache generatedAssetCache

// get reports a hit only when the key matches *and* the output is still on
// disk, so clearing the assets dir cannot leave a stale hit behind.
func (c *generatedAssetCache) get(k prescaleKey, dest string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.valid || c.key != k {
		return "", false
	}
	if _, err := os.Stat(dest); err != nil {
		c.valid = false
		return "", false
	}
	return dest, true
}

func (c *generatedAssetCache) put(k prescaleKey) {
	c.mu.Lock()
	c.key, c.valid = k, true
	c.mu.Unlock()
}

// vizMaskKey identifies a rendered pill mask by everything pillMaskPNG consumes.
type vizMaskKey struct {
	path                    string
	w, h, period, pillWidth int
}

var vizMaskCache struct {
	mu    sync.Mutex
	valid bool
	key   vizMaskKey
}

// writeVizMask renders the visualization's pill mask to path, skipping both the
// PNG encode and the write when the identical mask is already there. The 4x
// supersample is scaled back down in the filtergraph for smooth capsule edges.
func writeVizMask(path string, g bannerGeom) error {
	const supersample = 4
	k := vizMaskKey{
		path:      path,
		w:         g.pillsW * supersample,
		h:         g.vizH * supersample,
		period:    (g.pill + g.pillGap) * supersample,
		pillWidth: g.pill * supersample,
	}

	vizMaskCache.mu.Lock()
	defer vizMaskCache.mu.Unlock()
	if vizMaskCache.valid && vizMaskCache.key == k {
		if _, err := os.Stat(path); err == nil {
			return nil
		}
	}
	if err := os.WriteFile(path, pillMaskPNG(k.w, k.h, k.period, k.pillWidth), 0o644); err != nil {
		vizMaskCache.valid = false
		return err
	}
	vizMaskCache.key, vizMaskCache.valid = k, true
	return nil
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
	textY := strconv.Itoa(g.textY)
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

// Font candidates for the drawtext overlay, most preferred first. Empty means
// no candidate exists on disk and fontconfig resolves a default.
var (
	boldFontCandidates = map[string][]string{
		"windows": {`C:\Windows\Fonts\arialbd.ttf`, `C:\Windows\Fonts\seguisb.ttf`},
		"": {
			"/usr/share/fonts/truetype/dejavu/DejaVuSans-Bold.ttf",
			"/usr/share/fonts/TTF/DejaVuSans-Bold.ttf",
			"/usr/share/fonts/dejavu/DejaVuSans-Bold.ttf",
			"/usr/share/fonts/truetype/noto/NotoSans-Bold.ttf",
			"/usr/share/fonts/noto/NotoSans-Bold.ttf",
		},
	}
	regularFontCandidates = map[string][]string{
		"windows": {`C:\Windows\Fonts\arial.ttf`, `C:\Windows\Fonts\segoeui.ttf`},
		"": {
			"/usr/share/fonts/noto/NotoSans-Regular.ttf",
			"/usr/share/fonts/truetype/dejavu/DejaVuSans.ttf",
			"/usr/share/fonts/TTF/DejaVuSans.ttf",
			"/usr/share/fonts/dejavu/DejaVuSans.ttf",
		},
	}
)

// firstExisting returns the first candidate for this OS that exists on disk,
// or "" when none do.
func firstExisting(byOS map[string][]string) string {
	candidates, ok := byOS[runtime.GOOS]
	if !ok {
		candidates = byOS[""] // every non-Windows platform shares the fontconfig paths
	}
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			return c
		}
	}
	return ""
}

// findBoldFont returns a bold TTF for the banner title, falling back to the
// regular weights (and finally "" → fontconfig default).
func findBoldFont() string {
	if f := firstExisting(boldFontCandidates); f != "" {
		return f
	}
	return findFont()
}

// findFont returns a usable TTF for drawtext across platforms, or "" to let
// fontconfig resolve a default.
func findFont() string { return firstExisting(regularFontCandidates) }

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
