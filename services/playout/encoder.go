package playout

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// encoder is the persistent ffmpeg process that muxes the canonical PCM stream
// (received on stdin) with a static/looping background + now-playing overlay,
// and pushes FLV to the RTMP target. It must stay up for the whole show; the
// mixer keeps feeding it PCM (silence during gaps) so it never starves.
type encoder struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	nowTxt string
	done   chan struct{}
}

// encoderConfig captures everything the encoder ffmpeg needs.
type encoderConfig struct {
	FFmpegPath   string
	RTMPURL      string
	BgImagePath  string
	NowTxtPath   string
	FontFile     string
	Width        int
	Height       int
	FPS          int
	AudioBitrate string
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
		c.FontFile = findFont()
	}

	// drawtext needs the textfile to exist before the process starts.
	if err := os.MkdirAll(filepath.Dir(c.NowTxtPath), 0o755); err != nil {
		return nil, err
	}
	if err := os.WriteFile(c.NowTxtPath, []byte(" "), 0o644); err != nil {
		return nil, err
	}

	args := []string{"-hide_banner", "-loglevel", "warning",
		// Audio FIRST so it anchors the muxer clock.
		"-f", "s16le", "-ar", "48000", "-ac", "2", "-i", "pipe:0",
	}

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

	drawtext := "drawtext="
	if c.FontFile != "" {
		drawtext += "fontfile=" + escapeFilterPath(c.FontFile) + ":"
	}
	drawtext += "textfile=" + escapeFilterPath(c.NowTxtPath) +
		":reload=1:expansion=none:x=(w-text_w)/2:y=h-120:fontsize=40:fontcolor=white:box=1:boxcolor=black@0.5:boxborderw=12"

	filter := fmt.Sprintf("[1:v]%s,scale=%d:%d,format=yuv420p,fps=%d[v]", drawtext, c.Width, c.Height, c.FPS)

	gop := c.FPS * 2
	args = append(args,
		"-filter_complex", filter,
		"-map", "[v]", "-map", "0:a",
		"-c:v", "libx264", "-preset", "veryfast", "-tune", "stillimage",
		"-pix_fmt", "yuv420p", "-r", itoa(c.FPS), "-g", itoa(gop),
		"-c:a", "aac", "-b:a", c.AudioBitrate, "-ar", "48000", "-ac", "2",
		"-fps_mode", "cfr", "-af", "aresample=async=1:first_pts=0",
		// -shortest: the looping video never EOFs on its own, so tie output
		// length to the audio pipe. Closing stdin then ends the stream cleanly
		// (finalizing the FLV trailer) instead of requiring a hard kill.
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

	e := &encoder{cmd: cmd, stdin: stdin, nowTxt: c.NowTxtPath, done: make(chan struct{})}
	go func() { _ = cmd.Wait(); close(e.done) }()
	return e, nil
}

// Write feeds a PCM chunk to the encoder. A write error (EPIPE) means ffmpeg died.
func (e *encoder) Write(p []byte) error {
	_, err := e.stdin.Write(p)
	return err
}

// SetNowPlaying atomically updates the overlay text (temp file + rename) so
// drawtext never reads a half-written line.
func (e *encoder) SetNowPlaying(text string) {
	if text == "" {
		text = " "
	}
	tmp := e.nowTxt + ".tmp"
	if err := os.WriteFile(tmp, []byte(text), 0o644); err != nil {
		return
	}
	_ = os.Rename(tmp, e.nowTxt)
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
