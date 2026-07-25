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
	id := formInt64(r, "id")
	if id != 0 {
		_ = app.Soundboard.Delete(id)
	}
	http.Redirect(w, r, "/soundboard", http.StatusSeeOther)
}

// PreviewClip serves a clip's original audio file for in-browser pre-listening.
func (app *App) PreviewClip(w http.ResponseWriter, r *http.Request) {
	clip, err := app.Soundboard.Get(queryInt64(r, "id"))
	var path string
	if clip != nil {
		path = clip.FilePath
	}
	servePreview(w, r, path, err)
}

// TriggerClip overlays a clip on the live stream (Datastar @post). Returns an
// SSE response so the caller can surface errors via the console.
func (app *App) TriggerClip(w http.ResponseWriter, r *http.Request) {
	sse := datastar.NewSSE(w, r)
	id := queryInt64(r, "id")
	clip, err := app.Soundboard.Get(id)
	if err != nil || clip == nil {
		sse.ConsoleError(errClipNotFound)
		return
	}
	if err := app.Engine.TriggerClip(strconv.FormatInt(clip.ID, 10), clip.PCMPath, 0.8); err != nil {
		sse.ConsoleError(err)
		return
	}
}
