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

// FlowPage shows the show list and the flow builder for the selected show:
// ?id= when given (and remembered for later visits), otherwise the live show,
// the last-opened show, or the newest one.
func (app *App) FlowPage(w http.ResponseWriter, r *http.Request) {
	playlists, err := app.Flow.ListPlaylists()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	var id int64
	explicit := r.URL.Query().Get("id") != ""
	if explicit {
		id, _ = strconv.ParseInt(r.URL.Query().Get("id"), 10, 64)
	} else if status := app.Engine.Status(); status.Running && status.PlaylistID != 0 {
		id = status.PlaylistID
	} else {
		id = app.lastShowID(r)
	}

	pl, _ := app.Flow.GetPlaylist(id)
	if explicit && pl != nil {
		app.rememberShow(w, r, pl.ID)
	}
	// Stale or absent selection: default to the newest show (list is ordered
	// most-recently-updated first).
	if pl == nil && !explicit && len(playlists) > 0 {
		pl, _ = app.Flow.GetPlaylist(playlists[0].ID)
	}

	extra := map[string]any{"Playlists": playlists}
	if pl != nil {
		items, _ := app.Flow.GetItems(pl.ID)
		tracks, _ := app.Library.ListTracks()
		runtime, _ := app.Flow.EstimateRuntimeSec(pl.ID)
		extra["Selected"] = pl
		extra["Tracks"] = tracks
		extra["RuntimeSec"] = runtime
		extra["Rundown"] = map[string]any{"Playlist": pl, "Items": items}
	}

	app.render(w, "page-flow", PageData{
		Title: "Flow Builder", Page: "flow", Theme: app.currentTheme(), Extra: extra,
	})
}

// StreamPage is the operator console. While live the start dropdown is pinned
// to the streaming show; otherwise ?play= (deep link from the flow page) or the
// last-opened show pre-selects it.
func (app *App) StreamPage(w http.ResponseWriter, r *http.Request) {
	playlists, _ := app.Flow.ListPlaylists()
	st, _ := app.Settings.Get()
	clips, _ := app.Soundboard.List()
	status := app.Engine.Status()
	running := status.Running

	var playID int64
	if running && status.PlaylistID != 0 {
		playID = status.PlaylistID
	} else if playStr := r.URL.Query().Get("play"); playStr != "" {
		playID, _ = strconv.ParseInt(playStr, 10, 64)
		if pl, _ := app.Flow.GetPlaylist(playID); pl != nil {
			app.rememberShow(w, r, playID)
		}
	} else if id := app.lastShowID(r); id != 0 {
		if pl, _ := app.Flow.GetPlaylist(id); pl != nil {
			playID = id
		}
	}

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
			"Clips":     clips,
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
	vw, vh := parseVideoSize(r.FormValue("video_size"), r.FormValue("video_size_custom"))
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
		VideoWidth:   vw,
		VideoHeight:  vh,
		VideoEnabled: r.FormValue("video_enabled") != "",
		VideoBitrate: strings.TrimSpace(r.FormValue("video_bitrate")),
		VideoEncoder: r.FormValue("video_encoder"),
		AudioBitrate: r.FormValue("audio_bitrate"),
		NowOverlay:   r.FormValue("now_overlay") != "",
		VizStyle:     r.FormValue("viz_style"),
		BannerBox:    r.FormValue("banner_box") != "",
		LowLatency:   r.FormValue("low_latency") != "",
		Theme:        r.FormValue("theme"),
	}
	if !IsValidTheme(st.Theme) {
		st.Theme = "teal"
	}
	switch st.VizStyle {
	case "bars", "wave", "none":
	default:
		st.VizStyle = "bars"
	}
	switch st.VideoEncoder {
	case "auto", "cpu", "nvenc":
	default:
		st.VideoEncoder = "auto"
	}
	if err := app.Settings.Save(st); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/settings", http.StatusSeeOther)
}

// BgPreview serves the currently configured background image so the settings
// page can show a preview. The path comes from the (admin-edited) settings row,
// never from the request.
func (app *App) BgPreview(w http.ResponseWriter, r *http.Request) {
	st, _ := app.Settings.Get()
	if st.BgImagePath == "" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	http.ServeFile(w, r, st.BgImagePath)
}

// parseVideoSize resolves the resolution form fields ("WxH" preset value, or
// the free-text field when the preset is "custom") into even width/height —
// yuv420p requires even dimensions. Unparseable input falls back to 720p.
func parseVideoSize(sel, custom string) (int, int) {
	if sel == "custom" {
		sel = custom
	}
	parts := strings.SplitN(strings.ToLower(strings.TrimSpace(sel)), "x", 2)
	if len(parts) == 2 {
		w, errW := strconv.Atoi(strings.TrimSpace(parts[0]))
		h, errH := strconv.Atoi(strings.TrimSpace(parts[1]))
		if errW == nil && errH == nil && w > 0 && h > 0 {
			return w &^ 1, h &^ 1
		}
	}
	return 1280, 720
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
