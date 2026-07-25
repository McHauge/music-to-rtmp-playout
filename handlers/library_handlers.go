package handlers

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/starfederation/datastar-go/datastar"
)

// UploadTrack handles a multipart upload of one or more files into the library.
func (app *App) UploadTrack(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(app.Cfg.MaxUploadSize); err != nil {
		http.Error(w, "upload too large", http.StatusBadRequest)
		return
	}
	files := r.MultipartForm.File["files"]
	if len(files) == 0 {
		files = r.MultipartForm.File["file"] // older form field name
	}
	if len(files) == 0 {
		http.Error(w, "no file", http.StatusBadRequest)
		return
	}
	for _, hdr := range files {
		file, err := hdr.Open()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		_, err = app.Library.AddUpload(hdr.Filename, file)
		file.Close()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}
	http.Redirect(w, r, "/library", http.StatusSeeOther)
}

// DeleteTrack removes a track (file + row).
func (app *App) DeleteTrack(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.FormValue("id"), 10, 64)
	if id != 0 {
		_ = app.Library.DeleteTrack(id)
	}
	http.Redirect(w, r, "/library", http.StatusSeeOther)
}

// PreviewTrack serves a track's audio file for in-browser pre-listening.
// http.ServeFile handles Range requests, so the <audio> element can seek.
func (app *App) PreviewTrack(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.URL.Query().Get("id"), 10, 64)
	t, err := app.Library.GetTrack(id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if t == nil {
		http.NotFound(w, r)
		return
	}
	http.ServeFile(w, r, t.FilePath)
}

// EditTrack updates title/artist.
func (app *App) EditTrack(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	id, _ := strconv.ParseInt(r.FormValue("id"), 10, 64)
	if id != 0 {
		_ = app.Library.UpdateMeta(id, r.FormValue("title"), r.FormValue("artist"))
	}
	http.Redirect(w, r, "/library", http.StatusSeeOther)
}

// ImportYouTube streams yt-dlp progress over SSE, then refreshes the track
// list. Triggered by a Datastar @get with the playlist/video URL as ?url=.
func (app *App) ImportYouTube(w http.ResponseWriter, r *http.Request) {
	sse := datastar.NewSSE(w, r)
	url := r.URL.Query().Get("url")
	if strings.TrimSpace(url) == "" {
		sse.PatchElements(`<div id="import-log" class="import-log">Enter a YouTube URL first.</div>`)
		return
	}

	// The progress callback runs synchronously on this goroutine (importVia's
	// scanner loop), so the shared rolling logger needs no locking.
	logf := rollingLogger(sse, "import-log")
	logf("Starting yt-dlp…")

	tracks, err := app.Library.ImportYouTube(r.Context(), url, func(line string) {
		logf("%s", line)
	})

	if err != nil {
		logf("Error: %s", err.Error())
	} else {
		logf("Done — imported %d track(s).", len(tracks))
	}

	// Refresh the track table fragment.
	app.patchTrackList(sse)
	// Clear the URL input.
	sse.PatchSignals([]byte(`{"url":""}`))
}

// patchTrackList re-renders the #track-list fragment from current DB state.
func (app *App) patchTrackList(sse *datastar.ServerSentEventGenerator) {
	tracks, err := app.Library.ListTracks()
	if err != nil {
		sse.ConsoleError(err)
		return
	}
	out, err := app.Tmpl.Render("track-list", map[string]any{"Tracks": tracks})
	if err != nil {
		sse.ConsoleError(err)
		return
	}
	sse.PatchElements(out)
}
