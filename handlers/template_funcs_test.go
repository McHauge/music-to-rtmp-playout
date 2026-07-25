package handlers

import (
	"html/template"
	"testing"
)

func TestFmtDuration(t *testing.T) {
	tests := []struct {
		sec  float64
		want string
	}{
		{0, "0:00"},
		{9, "0:09"},
		{59, "0:59"},
		{59.6, "1:00"}, // rounds to nearest, not truncates
		{60, "1:00"},
		{125, "2:05"},
		{3599, "59:59"},
		{3600, "1:00:00"},
		{3725, "1:02:05"},
		{86400, "24:00:00"}, // a 24h show reads as hours, not days
	}
	for _, tt := range tests {
		if got := fmtDuration(tt.sec); got != tt.want {
			t.Errorf("fmtDuration(%v) = %q, want %q", tt.sec, got, tt.want)
		}
	}
}

// pct drives a progress-bar width, so it must stay in 0..100 and never divide
// by a zero/unknown duration.
func TestPctClampsAndGuardsZeroDuration(t *testing.T) {
	pct := templateFuncs()["pct"].(func(elapsed, dur float64) int)
	tests := []struct {
		elapsed, dur float64
		want         int
	}{
		{0, 100, 0},
		{50, 100, 50},
		{100, 100, 100},
		{150, 100, 100}, // overshoot clamps
		{-5, 100, 0},    // negative clamps
		{50, 0, 0},      // unknown duration → no bar, not a divide by zero
		{50, -10, 0},
	}
	for _, tt := range tests {
		if got := pct(tt.elapsed, tt.dur); got != tt.want {
			t.Errorf("pct(%v, %v) = %d, want %d", tt.elapsed, tt.dur, got, tt.want)
		}
	}
}

func TestRemainingClampsAtZero(t *testing.T) {
	remaining := templateFuncs()["remaining"].(func(dur, elapsed float64) string)
	tests := []struct {
		dur, elapsed float64
		want         string
	}{
		{180, 0, "3:00"},
		{180, 89, "1:31"},
		{180, 180, "0:00"},
		{180, 200, "0:00"}, // past the end never shows a negative countdown
	}
	for _, tt := range tests {
		if got := remaining(tt.dur, tt.elapsed); got != tt.want {
			t.Errorf("remaining(%v, %v) = %q, want %q", tt.dur, tt.elapsed, got, tt.want)
		}
	}
}

func TestIsWarnLine(t *testing.T) {
	warn := []string{
		"⚠ duration mismatch",
		"warning: no duration",
		"  Failed: nope", // leading space + mixed case
		"SKIPPED: bad file",
		"error: boom",
	}
	for _, l := range warn {
		if !isWarnLine(l) {
			t.Errorf("isWarnLine(%q) = false, want true", l)
		}
	}
	plain := []string{
		"imported: Song",
		"",
		"1/10: track.mp3…",
		"downloading, no error here", // only a *prefix* marks a warning
	}
	for _, l := range plain {
		if isWarnLine(l) {
			t.Errorf("isWarnLine(%q) = true, want false", l)
		}
	}
}

func TestIsValidTheme(t *testing.T) {
	if !isValidTheme("teal") {
		t.Error("teal should be valid")
	}
	if isValidTheme("chartreuse") || isValidTheme("") {
		t.Error("unknown themes must be rejected — an invalid one renders an unstyled page")
	}
	// Every listed theme must validate, or the settings dropdown could offer a
	// value the page handler then rejects.
	for _, th := range themeList {
		if !isValidTheme(th.Name) {
			t.Errorf("listed theme %q does not validate", th.Name)
		}
	}
}

// The func map is wired into every template at load; a nil or wrongly-typed
// entry only fails when a page that uses it is rendered.
func TestTemplateFuncsAreUsable(t *testing.T) {
	fm := templateFuncs()
	for _, name := range []string{"add", "printf", "warn", "fmtDuration", "fmtSecs", "runtimeSec", "remaining", "pct"} {
		if fm[name] == nil {
			t.Errorf("template func %q is missing", name)
		}
	}
	if _, err := template.New("t").Funcs(fm).Parse(
		`{{add 1 2}}{{fmtDuration 65.0}}{{fmtSecs 20}}{{pct 1.0 2.0}}{{remaining 2.0 1.0}}{{warn "error: x"}}`,
	); err != nil {
		t.Fatalf("func map does not parse into a template: %v", err)
	}
}
