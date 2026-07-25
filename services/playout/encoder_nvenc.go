package playout

import (
	"bytes"
	"context"
	"log"
	"os/exec"
	"strings"
	"time"
)

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
