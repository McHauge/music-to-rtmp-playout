package playout

import (
	"bytes"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// escapeFilterPath's whole reason to exist is the Windows drive-letter colon:
// ffmpeg's graph parser treats ':' as an option separator even inside a quoted
// value, so an unescaped one silently truncates the path and the filter fails.
func TestEscapeFilterPath(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"windows drive letter", `C:\assets\bg.png`, `'C\:/assets/bg.png'`},
		{"windows unc", `\\host\share\now.txt`, `'//host/share/now.txt'`},
		{"posix", "/app/assets/now.txt", "'/app/assets/now.txt'"},
		{"posix with colon", "/tmp/a:b/now.txt", `'/tmp/a\:b/now.txt'`},
		{"empty", "", "''"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := escapeFilterPath(tt.in); got != tt.want {
				t.Errorf("escapeFilterPath(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// The zmq bind address needs *double* backslashes: the graph parser strips one
// level before ffmpeg's option parser sees the value.
func TestZMQBindArg(t *testing.T) {
	if got, want := zmqBindArg("tcp://127.0.0.1:5555"), `tcp\\://127.0.0.1\\:5555`; got != want {
		t.Errorf("zmqBindArg = %q, want %q", got, want)
	}
	if got := zmqBindArg("nocolons"); got != "nocolons" {
		t.Errorf("zmqBindArg left a colon-free address alone? got %q", got)
	}
}

func TestBannerLayoutFitsInsideTheFrame(t *testing.T) {
	sizes := []struct{ w, h int }{
		{1280, 720}, {1920, 1080}, {854, 480}, {640, 360}, {320, 180},
	}
	for _, s := range sizes {
		g := bannerLayout(s.w, s.h)

		if g.barX < 0 || g.barY < 0 {
			t.Errorf("%dx%d: bar origin negative (%d,%d)", s.w, s.h, g.barX, g.barY)
		}
		if g.barX+g.barW > s.w || g.barY+g.barH > s.h {
			t.Errorf("%dx%d: bar %dx%d at (%d,%d) spills out of the frame",
				s.w, s.h, g.barW, g.barH, g.barX, g.barY)
		}
		// yuv420p needs even dimensions; an odd bar makes ffmpeg round and shift
		// the overlay by a pixel.
		if g.barW%2 != 0 || g.barH%2 != 0 || g.vizW%2 != 0 || g.vizH%2 != 0 {
			t.Errorf("%dx%d: odd dimension — bar %dx%d viz %dx%d",
				s.w, s.h, g.barW, g.barH, g.vizW, g.vizH)
		}
		// The viz is composed on the bar-sized canvas, so it must fit there.
		if g.vizY+g.vizH > g.barH {
			t.Errorf("%dx%d: viz bottom %d exceeds bar height %d", s.w, s.h, g.vizY+g.vizH, g.barH)
		}
		if g.pillsW > g.vizW {
			t.Errorf("%dx%d: pill grid %d wider than the viz region %d", s.w, s.h, g.pillsW, g.vizW)
		}
		// pillMaskPNG and showfreqs are both sized from these; zero or negative
		// would produce an invalid filter.
		if g.pill <= 0 || g.pillGap <= 0 || g.pillBars <= 0 || g.art <= 0 || g.fontSize <= 0 {
			t.Errorf("%dx%d: non-positive geometry %+v", s.w, s.h, g)
		}
	}
}

func TestScaleRate(t *testing.T) {
	tests := []struct {
		rate   string
		factor float64
		want   string
	}{
		{"500k", 4, "2000k"},
		{"500k", 1, "500k"},
		{"1M", 1.5, "1.5M"},
		{"2000", 2, "4000"},
		{"500K", 2, "1000K"},
		// Unparseable input falls back unchanged — ffmpeg then treats it as a 1x
		// buffer rather than rejecting the whole command line.
		{"", 4, ""},
		{"abc", 4, "abc"},
		{"k", 4, "k"},
	}
	for _, tt := range tests {
		if got := scaleRate(tt.rate, tt.factor); got != tt.want {
			t.Errorf("scaleRate(%q, %v) = %q, want %q", tt.rate, tt.factor, got, tt.want)
		}
	}
}

func TestResolveVideoCodec(t *testing.T) {
	tests := []struct {
		pref      string
		available bool
		want      string
	}{
		{"auto", true, "h264_nvenc"},
		{"auto", false, "libx264"},
		{"cpu", true, "libx264"}, // an explicit CPU choice is never overridden
		{"cpu", false, "libx264"},
		{"nvenc", true, "h264_nvenc"},
		// Forced NVENC on a host without it must fall back, not fail: a
		// misconfigured setting should never take the stream down.
		{"nvenc", false, "libx264"},
		{"", false, "libx264"},
		{"nonsense", true, "h264_nvenc"},
	}
	for _, tt := range tests {
		if got := resolveVideoCodec(tt.pref, tt.available); got != tt.want {
			t.Errorf("resolveVideoCodec(%q, %v) = %q, want %q", tt.pref, tt.available, got, tt.want)
		}
	}
}

// The generated PNGs feed ffmpeg image inputs whose dimensions must not change
// mid-stream, so assert they decode and are exactly the requested size.
func TestGeneratedPNGsDecodeAtTheRequestedSize(t *testing.T) {
	mask := pillMaskPNG(400, 40, 12, 6)
	img, err := png.Decode(bytes.NewReader(mask))
	if err != nil {
		t.Fatalf("pillMaskPNG did not decode: %v", err)
	}
	if b := img.Bounds(); b.Dx() != 400 || b.Dy() != 40 {
		t.Errorf("pillMaskPNG size = %dx%d, want 400x40", b.Dx(), b.Dy())
	}

	art, err := png.Decode(bytes.NewReader(placeholderPNG()))
	if err != nil {
		t.Fatalf("placeholderPNG did not decode: %v", err)
	}
	if b := art.Bounds(); b.Dx() != artSize || b.Dy() != artSize {
		t.Errorf("placeholderPNG size = %dx%d, want %dx%d", b.Dx(), b.Dy(), artSize, artSize)
	}
}

// overwriteInPlace must leave no window in which the file is absent or longer
// than its new content — ffmpeg re-reads these files continuously.
func TestOverwriteInPlaceTruncates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "now.txt")

	overwriteInPlace(path, []byte("A LONG FIRST LINE"))
	if got, err := os.ReadFile(path); err != nil || string(got) != "A LONG FIRST LINE" {
		t.Fatalf("first write: %q, %v", got, err)
	}

	// Shorter content must not leave the old tail behind.
	overwriteInPlace(path, []byte("SHORT"))
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read after overwrite: %v", err)
	}
	if string(got) != "SHORT" {
		t.Errorf("after overwrite = %q, want %q (stale tail not truncated)", got, "SHORT")
	}
}

// A missing directory is reported through the log, not a panic or a partial
// file — the caller (SetNowPlaying/SetNowArt) has no error to return.
func TestOverwriteInPlaceSurvivesAnUnwritablePath(t *testing.T) {
	overwriteInPlace(filepath.Join(t.TempDir(), "no-such-dir", "now.txt"), []byte("x"))
}

func TestFindFontReturnsAnExistingPathOrEmpty(t *testing.T) {
	for _, f := range []string{findFont(), findBoldFont()} {
		if f == "" {
			continue // fontconfig default; valid
		}
		if _, err := os.Stat(f); err != nil {
			t.Errorf("font %q does not exist: %v", f, err)
		}
		if !strings.HasSuffix(strings.ToLower(f), ".ttf") {
			t.Errorf("font %q is not a TTF", f)
		}
	}
}
