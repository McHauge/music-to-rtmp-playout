package playout

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"math"
	"sync"
)

// artSize is the canonical square size of banner cover art. Every file swapped
// onto the live art path must be exactly this size (a dimension change would
// break the encoder's running filter graph), so library imports normalize to it
// and the placeholder is generated at it.
const artSize = 300

var (
	placeholderOnce sync.Once
	placeholderData []byte
)

// pillMaskPNG returns a transparent PNG with full-height white capsule columns
// (rounded ends) of width pill repeating every period pixels. Multiplied into
// the bars visualization's alpha, it breaks the continuous spectrum into
// regular pill-shaped bars with transparent gaps. Callers generate it at a
// supersampled size and let ffmpeg scale it down for smooth edges.
func pillMaskPNG(w, h, period, pill int) []byte {
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	r := float64(pill) / 2
	for x0 := 0; x0+pill <= w; x0 += period {
		for y := 0; y < h; y++ {
			for x := 0; x < pill; x++ {
				// Rounded-rect membership: clamp the pixel center to the
				// capsule's core, then test distance against the radius.
				fx, fy := float64(x)+0.5, float64(y)+0.5
				cx := math.Min(math.Max(fx, r), float64(pill)-r)
				cy := math.Min(math.Max(fy, r), float64(h)-r)
				dx, dy := fx-cx, fy-cy
				if dx*dx+dy*dy <= r*r {
					i := img.PixOffset(x0+x, y)
					img.Pix[i], img.Pix[i+1], img.Pix[i+2], img.Pix[i+3] = 0xff, 0xff, 0xff, 0xff
				}
			}
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil
	}
	return buf.Bytes()
}

// placeholderPNG returns a generated artSize×artSize cover used when a track
// has no art: the background navy with a vinyl-record motif.
func placeholderPNG() []byte {
	placeholderOnce.Do(func() {
		// Lighter than the 0x0a1628 stream fallback color so the tile reads
		// as a distinct square on the banner even over a plain background.
		bg := color.RGBA{0x16, 0x26, 0x40, 0xff}
		disc := color.RGBA{0x24, 0x3a, 0x5c, 0xff}
		ring := color.RGBA{0x38, 0x55, 0x7e, 0xff}

		img := image.NewRGBA(image.Rect(0, 0, artSize, artSize))
		c := artSize / 2
		for y := 0; y < artSize; y++ {
			for x := 0; x < artSize; x++ {
				dx, dy := x-c, y-c
				r2 := dx*dx + dy*dy
				px := bg
				switch {
				case r2 < 18*18: // spindle hole
					px = bg
				case r2 < 60*60: // label
					px = ring
				case r2 < 120*120: // disc
					px = disc
				}
				img.SetRGBA(x, y, px)
			}
		}

		var buf bytes.Buffer
		if err := png.Encode(&buf, img); err != nil {
			// Encoding an in-memory RGBA cannot realistically fail; keep a nil
			// guard anyway so callers fall back to writing nothing.
			placeholderData = nil
			return
		}
		placeholderData = buf.Bytes()
	})
	return placeholderData
}
