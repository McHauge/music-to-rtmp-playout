package handlers

import (
	"strings"
	"testing"

	"music-to-rtmp-playout/services"
)

// Templates are parsed at startup and rendered by name, so a broken {{define}}
// or a missing func only shows up at runtime. Load the real set and render the
// fragments this package patches over SSE.
func TestTemplatesLoadAndRenderFragments(t *testing.T) {
	tp, err := LoadTemplates("../templates")
	if err != nil {
		t.Fatalf("LoadTemplates: %v", err)
	}

	out, err := tp.Render("import-log", map[string]any{
		"ID":    "bulk-log",
		"Lines": []string{"plain line", "skipped: nope", "<script>x</script>"},
	})
	if err != nil {
		t.Fatalf("import-log: %v", err)
	}
	for _, want := range []string{`id="bulk-log"`, `class="log-warn"`, "&lt;script&gt;"} {
		if !strings.Contains(out, want) {
			t.Errorf("import-log missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "<script>x</script>") {
		t.Errorf("import-log did not escape the log line:\n%s", out)
	}

	if out, err := tp.Render("flow-runtime", 3725.0); err != nil {
		t.Fatalf("flow-runtime: %v", err)
	} else if !strings.Contains(out, `id="flow-runtime"`) {
		t.Errorf("flow-runtime markup: %s", out)
	}

	clips := []services.SoundboardClip{{ID: 7, Name: "airhorn"}}
	if out, err := tp.Render("stream-soundboard", clips); err != nil {
		t.Fatalf("stream-soundboard: %v", err)
	} else if !strings.Contains(out, "airhorn") || !strings.Contains(out, "id=7") {
		t.Errorf("stream-soundboard missing the shared clip-controls output:\n%s", out)
	}

	tr := &services.Track{ID: 1, Title: "Song", Artist: "Band", DurationSec: 90}
	items := []services.FlowItem{
		{ID: 1, Type: services.ItemSong, Track: tr},
		{ID: 2, Type: services.ItemBreak, BreakSec: 20},
		{ID: 3, Type: services.ItemGate, Label: "Top of hour"},
	}
	if out, err := tp.Render("flow-rundown", map[string]any{
		"Playlist": &services.Playlist{ID: 1, Name: "Show"}, "Items": items,
	}); err != nil {
		t.Fatalf("flow-rundown: %v", err)
	} else if !strings.Contains(out, "Band") || !strings.Contains(out, "Top of hour") {
		t.Errorf("flow-rundown missing shared item descriptions:\n%s", out)
	}

	if out, err := tp.Render("stream-rundown", map[string]any{
		"Running": true, "Items": items, "ItemIndex": 0, "NextIndex": 1,
		"Played": map[int]bool{}, "PlaylistID": int64(1), "ElapsedSec": 1.0,
	}); err != nil {
		t.Fatalf("stream-rundown: %v", err)
	} else if !strings.Contains(out, "Band") || !strings.Contains(out, "Top of hour") {
		t.Errorf("stream-rundown missing shared item descriptions:\n%s", out)
	}
}
