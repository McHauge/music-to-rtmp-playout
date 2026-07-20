package handlers

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"music-to-rtmp-playout/services"
)

func (app *App) render(w http.ResponseWriter, name string, data PageData) {
	if err := app.Tmpl.Execute(w, name, data); err != nil {
		log.Printf("render %s: %v", name, err)
		http.Error(w, "template error", http.StatusInternalServerError)
	}
}

// LibraryPage shows the track library.
func (app *App) LibraryPage(w http.ResponseWriter, r *http.Request) {
	tracks, err := app.Library.ListTracks()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	app.render(w, "page-library", PageData{
		Title: "Library", Page: "library", Theme: app.currentTheme(),
		Extra: map[string]any{"Tracks": tracks},
	})
}

// FlowPage shows the show list and, when ?id= is given, the flow builder for
// that show.
func (app *App) FlowPage(w http.ResponseWriter, r *http.Request) {
	playlists, err := app.Flow.ListPlaylists()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	extra := map[string]any{"Playlists": playlists}

	if idStr := r.URL.Query().Get("id"); idStr != "" {
		if id, err := strconv.ParseInt(idStr, 10, 64); err == nil {
			pl, _ := app.Flow.GetPlaylist(id)
			if pl != nil {
				items, _ := app.Flow.GetItems(id)
				tracks, _ := app.Library.ListTracks()
				runtime, _ := app.Flow.EstimateRuntimeSec(id)
				extra["Selected"] = pl
				extra["Tracks"] = tracks
				extra["RuntimeSec"] = runtime
				extra["Rundown"] = map[string]any{"Playlist": pl, "Items": items}
			}
		}
	}

	app.render(w, "page-flow", PageData{
		Title: "Flow Builder", Page: "flow", Theme: app.currentTheme(), Extra: extra,
	})
}

// StreamPage is the operator console. An optional ?play= query param
// pre-selects that show in the start dropdown (deep link from the flow page).
func (app *App) StreamPage(w http.ResponseWriter, r *http.Request) {
	playlists, _ := app.Flow.ListPlaylists()
	st, _ := app.Settings.Get()
	status := app.Engine.Status()
	running := status.Running
	playID, _ := strconv.ParseInt(r.URL.Query().Get("play"), 10, 64)

	// After a stop, preselect the last position so Start acts as "continue".
	startAt := 0
	if !running && status.PlaylistID != 0 && (playID == 0 || playID == status.PlaylistID) {
		startAt = status.ItemIndex
	}

	app.render(w, "page-stream", PageData{
		Title: "Stream", Page: "stream", Theme: app.currentTheme(),
		Extra: map[string]any{
			"Playlists": playlists,
			"Settings":  st,
			"Status":    status,
			"Running":   running,
			"PlayID":    playID,
			"StartAt":   startAt,
			"Rundown":   app.rundownData(playID),
		},
	})
}

// SoundboardPage shows clip buttons + upload.
func (app *App) SoundboardPage(w http.ResponseWriter, r *http.Request) {
	clips, _ := app.Soundboard.List()
	app.render(w, "page-soundboard", PageData{
		Title: "Soundboard", Page: "soundboard", Theme: app.currentTheme(),
		Extra: map[string]any{"Clips": clips, "Running": app.Engine.Running()},
	})
}

// SettingsPage shows the configuration form.
func (app *App) SettingsPage(w http.ResponseWriter, r *http.Request) {
	st, _ := app.Settings.Get()
	app.render(w, "page-settings", PageData{
		Title: "Settings", Page: "settings", Theme: app.currentTheme(),
		Extra: map[string]any{"Settings": st, "Themes": ThemeList},
	})
}

// SaveSettings persists the settings form. An optional uploaded background
// image is stored in the assets dir and takes precedence over the path field.
func (app *App) SaveSettings(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(app.Cfg.MaxUploadSize); err != nil {
		http.Error(w, "form too large", http.StatusBadRequest)
		return
	}
	fps, _ := strconv.Atoi(r.FormValue("video_fps"))
	if fps <= 0 {
		fps = 10
	}
	bgPath := r.FormValue("bg_image_path")
	if file, hdr, err := r.FormFile("bg_image_file"); err == nil {
		defer file.Close()
		saved, err := app.saveBackground(hdr.Filename, file)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		bgPath = saved
	}
	st := services.Settings{
		RTMPURL:      r.FormValue("rtmp_url"),
		StreamKey:    r.FormValue("stream_key"),
		BgImagePath:  bgPath,
		VideoFPS:     fps,
		AudioBitrate: r.FormValue("audio_bitrate"),
		Theme:        r.FormValue("theme"),
	}
	if !IsValidTheme(st.Theme) {
		st.Theme = "teal"
	}
	if err := app.Settings.Save(st); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/settings", http.StatusSeeOther)
}

// saveBackground stores an uploaded background image in the assets directory
// (as bg.<ext>, replacing any previous upload of the same type) and returns
// the path to store in settings.
func (app *App) saveBackground(name string, src io.Reader) (string, error) {
	ext := strings.ToLower(filepath.Ext(name))
	switch ext {
	case ".png", ".jpg", ".jpeg", ".webp", ".bmp":
	default:
		return "", fmt.Errorf("unsupported image type %q (use png, jpg, webp, or bmp)", ext)
	}
	if err := os.MkdirAll(app.Cfg.AssetsDir, 0o755); err != nil {
		return "", err
	}
	dst := filepath.Join(app.Cfg.AssetsDir, "bg"+ext)
	f, err := os.Create(dst)
	if err != nil {
		return "", err
	}
	defer f.Close()
	if _, err := io.Copy(f, src); err != nil {
		return "", err
	}
	return dst, nil
}
