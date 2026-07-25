package handlers

import (
	"encoding/json"
	"net/http"
	"strconv"
)

// Debug endpoints, registered only when PLAYOUT_DIAG is set (see main.go) and
// intended for localhost use while chasing playback-timing issues. They are
// unauthenticated on purpose so an operator (or an assistant) can drive and
// observe a show without a session; do NOT enable PLAYOUT_DIAG in production.

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// DebugStats returns the current live status plus the run-loop health snapshot.
func (app *App) DebugStats(w http.ResponseWriter, r *http.Request) {
	st := app.Engine.Status()
	writeJSON(w, map[string]any{
		"running":         st.Running,
		"paused":          st.Paused,
		"itemIndex":       st.ItemIndex,
		"nowPlaying":      st.NowPlaying,
		"elapsedSec":      st.ElapsedSec,
		"itemElapsedSec":  st.ItemElapsedSec,
		"error":           st.Error,
		"diag":            app.Engine.Diag(),
	})
}

// DebugPlaylists lists playlists (id + name) so a debug start can pick one.
func (app *App) DebugPlaylists(w http.ResponseWriter, r *http.Request) {
	pls, err := app.Flow.ListPlaylists()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	out := make([]map[string]any, 0, len(pls))
	for _, p := range pls {
		out = append(out, map[string]any{"id": p.ID, "name": p.Name})
	}
	writeJSON(w, out)
}

// DebugStart starts (and immediately plays) a show. ?id= selects the playlist;
// omitted → the first playlist. ?at= sets the start item. ?play=0 leaves it cued.
func (app *App) DebugStart(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.URL.Query().Get("id"), 10, 64)
	at, _ := strconv.Atoi(r.URL.Query().Get("at"))
	if id == 0 {
		pls, err := app.Flow.ListPlaylists()
		if err != nil || len(pls) == 0 {
			http.Error(w, "no playlists", http.StatusNotFound)
			return
		}
		id = pls[0].ID
	}
	items, err := app.Flow.GetItems(id)
	if err != nil || len(items) == 0 {
		http.Error(w, "playlist empty or missing", http.StatusNotFound)
		return
	}
	st, err := app.Settings.Get()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := app.Engine.Start(items, st, id, at); err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	if r.URL.Query().Get("play") != "0" {
		app.Engine.Play()
	}
	writeJSON(w, map[string]any{"ok": true, "playlistID": id, "items": len(items), "at": at})
}

// DebugStop ends the live show.
func (app *App) DebugStop(w http.ResponseWriter, r *http.Request) {
	app.Engine.Stop()
	writeJSON(w, map[string]any{"ok": true})
}

// DebugSkip advances to the next flow item (crossfades like a real skip).
func (app *App) DebugSkip(w http.ResponseWriter, r *http.Request) {
	app.Engine.Skip()
	writeJSON(w, map[string]any{"ok": true})
}

// DebugPlay releases a hold / resumes a paused show.
func (app *App) DebugPlay(w http.ResponseWriter, r *http.Request) {
	app.Engine.Play()
	writeJSON(w, map[string]any{"ok": true})
}
