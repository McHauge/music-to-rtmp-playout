package config

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestGetEnvFallsBackOnUnsetAndEmpty(t *testing.T) {
	t.Setenv("PLAYOUT_TEST_STR", "")
	if got := getEnv("PLAYOUT_TEST_STR", "fallback"); got != "fallback" {
		// An empty value is treated as unset on purpose: a blank line in .env
		// should not wipe a default.
		t.Errorf("empty env: got %q, want the default", got)
	}
	t.Setenv("PLAYOUT_TEST_STR", "set")
	if got := getEnv("PLAYOUT_TEST_STR", "fallback"); got != "set" {
		t.Errorf("set env: got %q, want %q", got, "set")
	}
	if got := getEnv("PLAYOUT_TEST_ABSENT_KEY", "fallback"); got != "fallback" {
		t.Errorf("unset env: got %q, want the default", got)
	}
}

func TestGetEnvIntFallsBackOnGarbage(t *testing.T) {
	tests := []struct {
		value string
		want  int
	}{
		{"", 42},
		{"not-a-number", 42}, // a typo must not silently become 0
		{"7", 7},
		{"0", 0},
		{"-3", -3},
	}
	for _, tt := range tests {
		t.Setenv("PLAYOUT_TEST_INT", tt.value)
		if got := getEnvInt("PLAYOUT_TEST_INT", 42); got != tt.want {
			t.Errorf("getEnvInt(%q) = %d, want %d", tt.value, got, tt.want)
		}
	}
}

// resolveTool is what makes the app self-contained; its precedence is
// env override > bundled binDir copy > bare name on PATH.
func TestResolveToolPrecedence(t *testing.T) {
	binDir := t.TempDir()
	name := "ffmpeg"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	bundled := filepath.Join(binDir, name)

	// Nothing bundled, no override: fall through to PATH by bare name.
	t.Setenv("PLAYOUT_TEST_FFMPEG", "")
	if got := resolveTool("PLAYOUT_TEST_FFMPEG", "ffmpeg", binDir); got != "ffmpeg" {
		t.Errorf("no bundle, no override: got %q, want the bare name", got)
	}

	// A bundled copy wins over PATH, and is returned as an absolute path so the
	// process's working directory can't change which binary runs.
	if err := os.WriteFile(bundled, []byte("#!/bin/true\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	got := resolveTool("PLAYOUT_TEST_FFMPEG", "ffmpeg", binDir)
	if !filepath.IsAbs(got) {
		t.Errorf("bundled tool %q is not absolute", got)
	}
	if filepath.Base(got) != name {
		t.Errorf("bundled tool = %q, want basename %q", got, name)
	}

	// An explicit override beats both.
	t.Setenv("PLAYOUT_TEST_FFMPEG", "/opt/custom/ffmpeg")
	if got := resolveTool("PLAYOUT_TEST_FFMPEG", "ffmpeg", binDir); got != "/opt/custom/ffmpeg" {
		t.Errorf("override: got %q, want /opt/custom/ffmpeg", got)
	}
}
