package handlers

import (
	"log"
	"net/http"
	"strconv"

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
				extra["Items"] = items
				extra["Tracks"] = tracks
				extra["RuntimeSec"] = runtime
			}
		}
	}

	app.render(w, "page-flow", PageData{
		Title: "Flow Builder", Page: "flow", Theme: app.currentTheme(), Extra: extra,
	})
}

// StreamPage is the operator console.
func (app *App) StreamPage(w http.ResponseWriter, r *http.Request) {
	playlists, _ := app.Flow.ListPlaylists()
	st, _ := app.Settings.Get()
	status := app.Engine.Status()
	app.render(w, "page-stream", PageData{
		Title: "Stream", Page: "stream", Theme: app.currentTheme(),
		Extra: map[string]any{
			"Playlists": playlists,
			"Settings":  st,
			"Status":    status,
			"Running":   app.Engine.Running(),
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

// SaveSettings persists the settings form.
func (app *App) SaveSettings(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	fps, _ := strconv.Atoi(r.FormValue("video_fps"))
	if fps <= 0 {
		fps = 10
	}
	st := services.Settings{
		RTMPURL:      r.FormValue("rtmp_url"),
		StreamKey:    r.FormValue("stream_key"),
		BgImagePath:  r.FormValue("bg_image_path"),
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
