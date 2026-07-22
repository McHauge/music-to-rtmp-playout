package handlers

import (
	"fmt"
	"html"
	"net/http"
	"strconv"
	"strings"
	"sync"

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

	var mu sync.Mutex
	lines := []string{"Starting yt-dlp…"}
	patch := func() {
		mu.Lock()
		defer mu.Unlock()
		var b strings.Builder
		b.WriteString(`<div id="import-log" class="import-log">`)
		// keep the last ~40 lines
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
	patch()

	tracks, err := app.Library.ImportYouTube(url, func(line string) {
		mu.Lock()
		lines = append(lines, line)
		mu.Unlock()
		patch()
	})

	mu.Lock()
	if err != nil {
		lines = append(lines, "Error: "+err.Error())
	} else {
		lines = append(lines, fmt.Sprintf("Done — imported %d track(s).", len(tracks)))
	}
	mu.Unlock()
	patch()

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
