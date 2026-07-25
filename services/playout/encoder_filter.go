package playout

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
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
