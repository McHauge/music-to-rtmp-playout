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

// logFunc is the printf-style progress sink a bulk handler streams to.
type logFunc = func(format string, args ...any)

// bulkFormMemory is the in-memory cap for a parsed multipart form; anything
// larger spills to temp files. The overall body size is capped separately by
// MaxUploadSize.
const bulkFormMemory = 32 << 20

// beginBulkSSE starts the SSE response for a bulk-add handler and returns its
// rolling #bulk-log logger. The multipart body must be fully parsed *before*
// NewSSE flushes response headers — after that the request body may no longer
// be readable on HTTP/1.1 — so the parse happens here and its error is reported
// over the stream. parseFailMsg takes the error as its single %v verb. A false
// third return means the caller should stop.
func (app *App) beginBulkSSE(w http.ResponseWriter, r *http.Request, parseFailMsg string) (*datastar.ServerSentEventGenerator, logFunc, bool) {
	r.Body = http.MaxBytesReader(w, r.Body, app.Cfg.MaxUploadSize)
	parseErr := r.ParseMultipartForm(bulkFormMemory)

	sse := datastar.NewSSE(w, r)
	logf := rollingLogger(sse, "bulk-log")
	if parseErr != nil {
		logf(parseFailMsg, parseErr)
		return sse, logf, false
	}
	return sse, logf, true
}

// bulkPlaylistID resolves the playlist_id form field and confirms the show
// exists, reporting over the stream when it does not.
func (app *App) bulkPlaylistID(r *http.Request, logf logFunc) (int64, bool) {
	plID, _ := strconv.ParseInt(r.FormValue("playlist_id"), 10, 64)
	pl, err := app.Flow.GetPlaylist(plID)
	if err != nil || pl == nil {
		logf("Unknown show.")
		return 0, false
	}
	return plID, true
}

// flowAppender appends songs to a show's rundown under the break/auto-next
// options every bulk-add mode shares (upload, YouTube, Spotify).
//
// Breaks go *between* consecutive songs — including between the show's current
// last item, if it is a song, and the first appended one — but never at the end.
type flowAppender struct {
	flow     *services.FlowService
	plID     int64
	breakSec int
	// manual start: playback parks before each new song. With breaks the hold
	// sits on the break (song flows into its break, then waits); without breaks
	// it sits on the song itself.
	manual    bool
	songAuto  bool
	needBreak bool // a break is owed before the next song
}

// newFlowAppender reads the break_sec/start_mode form fields shared by the bulk
// forms, persists break_sec as the show's default, and seeds the break lookback
// from the rundown's current tail.
func (app *App) newFlowAppender(r *http.Request, plID int64) *flowAppender {
	breakSec, _ := strconv.Atoi(r.FormValue("break_sec"))
	if breakSec < 0 {
		breakSec = 0
	}
	_ = app.Flow.SetDefaultBreakSec(plID, breakSec)

	manual := r.FormValue("start_mode") == "manual"
	a := &flowAppender{
		flow:     app.Flow,
		plID:     plID,
		breakSec: breakSec,
		manual:   manual,
		songAuto: !(manual && breakSec == 0),
	}
	if items, err := app.Flow.GetItems(plID); err == nil && len(items) > 0 {
		a.needBreak = items[len(items)-1].Type == services.ItemSong
	}
	return a
}

// withBreaks reports whether auto-breaks are in play for this batch.
func (a *flowAppender) withBreaks() bool { return a.breakSec > 0 }

// appendSong adds trackID to the rundown, preceded by a break when one is owed.
func (a *flowAppender) appendSong(trackID int64) {
	if a.needBreak && a.withBreaks() {
		a.addBreak(a.breakSec)
	}
	_, _ = a.flow.AddItem(a.plID, services.ItemSong, &trackID, 0, "", a.songAuto)
	a.needBreak = true
}

// addBreak appends an explicit break of sec seconds and clears the owed-break
// flag, so a following song does not stack a second break on top of it.
func (a *flowAppender) addBreak(sec int) {
	_, _ = a.flow.AddItem(a.plID, services.ItemBreak, nil, sec, "", !a.manual)
	a.needBreak = false
}

// rollingLogger returns a printf-style logger that re-patches the given div
// with a rolling 40-line buffer on every call. Warning/failure lines are
// tinted so they stand out in the stream.
func rollingLogger(sse *datastar.ServerSentEventGenerator, divID string) func(format string, args ...any) {
	var lines []string
	return func(format string, args ...any) {
		lines = append(lines, fmt.Sprintf(format, args...))
		var b strings.Builder
		b.WriteString(`<div id="` + divID + `" class="import-log">`)
		start := 0
		if len(lines) > 40 {
			start = len(lines) - 40
		}
		for _, l := range lines[start:] {
			if isWarnLine(l) {
				b.WriteString(`<span class="log-warn">`)
				b.WriteString(html.EscapeString(l))
				b.WriteString(`</span>`)
			} else {
				b.WriteString(html.EscapeString(l))
			}
			b.WriteString("<br>")
		}
		b.WriteString(`</div>`)
		sse.PatchElements(b.String())
	}
}

// isWarnLine reports whether a log line should be highlighted as a warning.
func isWarnLine(l string) bool {
	l = strings.ToLower(strings.TrimSpace(l))
	for _, p := range []string{"⚠", "warning:", "failed:", "skipped:", "error:"} {
		if strings.HasPrefix(l, p) {
			return true
		}
	}
	return false
}

// BulkUploadToFlow uploads many audio files and appends each to the show's
// rundown as a song, with the show's break spacing between songs. Per-file
// progress streams to #bulk-log over SSE, then the rundown is re-patched.
func (app *App) BulkUploadToFlow(w http.ResponseWriter, r *http.Request) {
	sse, logf, ok := app.beginBulkSSE(w, r, "Upload failed: %v — raise MAX_UPLOAD_MB if the batch is large.")
	if !ok {
		return
	}
	plID, ok := app.bulkPlaylistID(r, logf)
	if !ok {
		return
	}
	files := r.MultipartForm.File["files"]
	if len(files) == 0 {
		logf("Choose one or more audio files first.")
		return
	}
	appender := app.newFlowAppender(r, plID)

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
		appender.appendSong(track.ID)
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
	sse, logf, ok := app.beginBulkSSE(w, r, "Import failed: %v")
	if !ok {
		return
	}
	plID, ok := app.bulkPlaylistID(r, logf)
	if !ok {
		return
	}
	url := strings.TrimSpace(r.FormValue("youtube_url"))
	if url == "" {
		logf("Paste a YouTube URL first.")
		return
	}
	appender := app.newFlowAppender(r, plID)

	logf("Starting yt-dlp…")
	tracks, err := app.Library.ImportYouTube(r.Context(), url, func(line string) { logf("%s", line) })
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
		appender.appendSong(t.ID)
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
