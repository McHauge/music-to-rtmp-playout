package handlers

import (
	"net/http"
	"strconv"

	"github.com/starfederation/datastar-go/datastar"
)

// UploadClip handles a soundboard clip upload (decoded to PCM on save).
func (app *App) UploadClip(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(app.Cfg.MaxUploadSize); err != nil {
		http.Error(w, "upload too large", http.StatusBadRequest)
		return
	}
	file, hdr, err := r.FormFile("file")
	if err != nil {
		http.Error(w, "no file", http.StatusBadRequest)
		return
	}
	defer file.Close()
	if _, err := app.Soundboard.Add(hdr.Filename, file); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/soundboard", http.StatusSeeOther)
}

// DeleteClip removes a clip.
func (app *App) DeleteClip(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.FormValue("id"), 10, 64)
	if id != 0 {
		_ = app.Soundboard.Delete(id)
	}
	http.Redirect(w, r, "/soundboard", http.StatusSeeOther)
}

// PreviewClip serves a clip's original audio file for in-browser pre-listening.
// http.ServeFile handles Range requests, so the <audio> element can seek.
func (app *App) PreviewClip(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.URL.Query().Get("id"), 10, 64)
	clip, err := app.Soundboard.Get(id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if clip == nil {
		http.NotFound(w, r)
		return
	}
	http.ServeFile(w, r, clip.FilePath)
}

// TriggerClip overlays a clip on the live stream (Datastar @post). Returns an
// SSE response so the caller can surface errors via the console.
func (app *App) TriggerClip(w http.ResponseWriter, r *http.Request) {
	sse := datastar.NewSSE(w, r)
	id, _ := strconv.ParseInt(r.URL.Query().Get("id"), 10, 64)
	clip, err := app.Soundboard.Get(id)
	if err != nil || clip == nil {
		sse.ConsoleError(errClipNotFound)
		return
	}
	if err := app.Engine.TriggerClip(clip.PCMPath, 0.8); err != nil {
		sse.ConsoleError(err)
		return
	}
}
