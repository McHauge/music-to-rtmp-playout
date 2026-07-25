package handlers

import (
	"net/http"
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
	id := formInt64(r, "id")
	if id != 0 {
		if err := app.Library.DeleteTrack(id); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}
	http.Redirect(w, r, "/library", http.StatusSeeOther)
}

// PreviewTrack serves a track's audio file for in-browser pre-listening.
func (app *App) PreviewTrack(w http.ResponseWriter, r *http.Request) {
	t, err := app.Library.GetTrack(queryInt64(r, "id"))
	var path string
	if t != nil {
		path = t.FilePath
	}
	servePreview(w, r, path, err)
}

// EditTrack updates title/artist.
func (app *App) EditTrack(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	id := formInt64(r, "id")
	if id != 0 {
		if err := app.Library.UpdateMeta(id, r.FormValue("title"), r.FormValue("artist")); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}
	http.Redirect(w, r, "/library", http.StatusSeeOther)
}

// ImportYouTube streams yt-dlp progress over SSE, then refreshes the track
// list. Triggered by a Datastar @get with the playlist/video URL as ?url=.
func (app *App) ImportYouTube(w http.ResponseWriter, r *http.Request) {
	sse := datastar.NewSSE(w, r)
	// The progress callback runs synchronously on this goroutine (importVia's
	// scanner loop), so the shared rolling logger needs no locking.
	logf := app.rollingLogger(sse, "import-log")

	url := r.URL.Query().Get("url")
	if strings.TrimSpace(url) == "" {
		logf("Paste a YouTube URL first.")
		return
	}
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
