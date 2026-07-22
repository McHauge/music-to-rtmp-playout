package handlers

import (
	"fmt"
	"html"
	"net/http"
	"strconv"
	"strings"

	"music-to-rtmp-playout/services"

	"github.com/starfederation/datastar-go/datastar"
)

// bulkLogger returns a printf-style logger that re-patches #bulk-log with a
// rolling 40-line buffer on every call.
func bulkLogger(sse *datastar.ServerSentEventGenerator) func(format string, args ...any) {
	var lines []string
	return func(format string, args ...any) {
		lines = append(lines, fmt.Sprintf(format, args...))
		var b strings.Builder
		b.WriteString(`<div id="bulk-log" class="import-log">`)
		start := 0
		if len(lines) > 40 {
			start = len(lines) - 40
		}
		for _, l := range lines[start:] {
			b.WriteString(html.EscapeString(l))
			b.WriteString("<br>")
		}
		b.WriteString(`</div>`)
		sse.PatchElements(b.String())
	}
}

// BulkUploadToFlow uploads many audio files and appends each to the show's
// rundown as a song, with the show's break spacing between songs. Per-file
// progress streams to #bulk-log over SSE, then the rundown is re-patched.
func (app *App) BulkUploadToFlow(w http.ResponseWriter, r *http.Request) {
	// The multipart body must be fully parsed before the SSE response starts:
	// NewSSE flushes response headers, after which the request body may no
	// longer be readable on HTTP/1.1.
	r.Body = http.MaxBytesReader(w, r.Body, app.Cfg.MaxUploadSize)
	parseErr := r.ParseMultipartForm(32 << 20)

	sse := datastar.NewSSE(w, r)
	logf := bulkLogger(sse)

	if parseErr != nil {
		logf("Upload failed: %v — raise MAX_UPLOAD_MB if the batch is large.", parseErr)
		return
	}

	plID, _ := strconv.ParseInt(r.FormValue("playlist_id"), 10, 64)
	pl, err := app.Flow.GetPlaylist(plID)
	if err != nil || pl == nil {
		logf("Unknown show.")
		return
	}
	files := r.MultipartForm.File["files"]
	if len(files) == 0 {
		logf("Choose one or more audio files first.")
		return
	}

	breakSec, _ := strconv.Atoi(r.FormValue("break_sec"))
	if breakSec < 0 {
		breakSec = 0
	}
	_ = app.Flow.SetDefaultBreakSec(plID, breakSec)

	withBreaks := breakSec > 0
	// Manual start: playback parks before each new song. With breaks the hold
	// sits on the break (song flows into its break, then waits); without
	// breaks it sits on the song itself.
	manual := r.FormValue("start_mode") == "manual"
	songAuto := !(manual && !withBreaks)

	// Breaks go between consecutive songs — including between the current last
	// item (if it is a song) and the first new one — but never at the end.
	needBreak := false
	if items, err := app.Flow.GetItems(plID); err == nil && len(items) > 0 {
		needBreak = items[len(items)-1].Type == services.ItemSong
	}

	added := 0
	for i, hdr := range files {
		logf("%d/%d: %s…", i+1, len(files), hdr.Filename)
		f, err := hdr.Open()
		if err != nil {
			logf("  skipped: %v", err)
			continue
		}
		track, err := app.Library.AddUpload(hdr.Filename, f)
		f.Close()
		if err != nil {
			logf("  skipped: %v", err)
			continue
		}
		if track.DurationSec <= 0 {
			logf("  warning: could not read duration — is this an audio file?")
		}
		if needBreak && withBreaks {
			_, _ = app.Flow.AddItem(plID, services.ItemBreak, nil, breakSec, "", !manual)
		}
		_, _ = app.Flow.AddItem(plID, services.ItemSong, &track.ID, 0, "", songAuto)
		needBreak = true
		added++
	}

	logf("Done — added %d of %d file(s) to the rundown.", added, len(files))
	app.patchFlowBuilder(sse, plID)
}

// ImportYouTubeToFlow downloads a YouTube video/playlist into the library and
// appends the resulting tracks to the show's rundown in playlist order, with
// the same break/auto-next options as bulk upload. Progress streams to
// #bulk-log over SSE.
func (app *App) ImportYouTubeToFlow(w http.ResponseWriter, r *http.Request) {
	// Same ordering constraint as BulkUploadToFlow: the multipart form (shared
	// with the file-upload button) must be parsed before NewSSE flushes headers.
	r.Body = http.MaxBytesReader(w, r.Body, app.Cfg.MaxUploadSize)
	parseErr := r.ParseMultipartForm(32 << 20)

	sse := datastar.NewSSE(w, r)
	logf := bulkLogger(sse)

	if parseErr != nil {
		logf("Import failed: %v", parseErr)
		return
	}

	plID, _ := strconv.ParseInt(r.FormValue("playlist_id"), 10, 64)
	pl, err := app.Flow.GetPlaylist(plID)
	if err != nil || pl == nil {
		logf("Unknown show.")
		return
	}
	url := strings.TrimSpace(r.FormValue("youtube_url"))
	if url == "" {
		logf("Paste a YouTube URL first.")
		return
	}

	breakSec, _ := strconv.Atoi(r.FormValue("break_sec"))
	if breakSec < 0 {
		breakSec = 0
	}
	_ = app.Flow.SetDefaultBreakSec(plID, breakSec)

	withBreaks := breakSec > 0
	// Manual start: playback parks before each new song. With breaks the hold
	// sits on the break (song flows into its break, then waits); without
	// breaks it sits on the song itself.
	manual := r.FormValue("start_mode") == "manual"
	songAuto := !(manual && !withBreaks)

	// Breaks go between consecutive songs — including between the current last
	// item (if it is a song) and the first new one — but never at the end.
	needBreak := false
	if items, err := app.Flow.GetItems(plID); err == nil && len(items) > 0 {
		needBreak = items[len(items)-1].Type == services.ItemSong
	}

	logf("Starting yt-dlp…")
	tracks, err := app.Library.ImportYouTube(url, func(line string) { logf("%s", line) })
	if err != nil {
		logf("Error: %v", err)
		return
	}
	if len(tracks) == 0 {
		logf("No tracks were imported — check the URL and the log above.")
		return
	}

	for _, t := range tracks {
		if t.DurationSec <= 0 {
			logf("warning: no duration for %q", t.Title)
		}
		if needBreak && withBreaks {
			_, _ = app.Flow.AddItem(plID, services.ItemBreak, nil, breakSec, "", !manual)
		}
		tid := t.ID
		_, _ = app.Flow.AddItem(plID, services.ItemSong, &tid, 0, "", songAuto)
		needBreak = true
	}

	logf("Done — added %d track(s) to the rundown.", len(tracks))
	sse.PatchSignals([]byte(`{"yturl":""}`))
	app.patchFlowBuilder(sse, plID)
}

// patchFlowBuilder re-renders the rundown list, runtime badge, and add-a-song
// dropdown after a change made over SSE.
func (app *App) patchFlowBuilder(sse *datastar.ServerSentEventGenerator, playlistID int64) {
	pl, err := app.Flow.GetPlaylist(playlistID)
	if err != nil || pl == nil {
		return
	}
	items, err := app.Flow.GetItems(playlistID)
	if err != nil {
		sse.ConsoleError(err)
		return
	}
	out, err := app.Tmpl.Render("flow-rundown", map[string]any{"Playlist": pl, "Items": items})
	if err != nil {
		sse.ConsoleError(err)
		return
	}
	sse.PatchElements(out)

	if runtime, err := app.Flow.EstimateRuntimeSec(playlistID); err == nil {
		sse.PatchElements(`<strong id="flow-runtime">` + fmtDuration(runtime) + `</strong>`)
	}
	if tracks, err := app.Library.ListTracks(); err == nil {
		if sel, err := app.Tmpl.Render("flow-song-select", map[string]any{"Tracks": tracks}); err == nil {
			sse.PatchElements(sel)
		}
	}
}
