package handlers

import (
	"net/http"
	"strconv"
	"strings"

	"music-to-rtmp-playout/services"
)

// CreatePlaylist makes a new show and opens its builder.
func (app *App) CreatePlaylist(w http.ResponseWriter, r *http.Request) {
	id, err := app.Flow.CreatePlaylist(r.FormValue("name"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	redirectFlow(w, r, id)
}

// RenamePlaylist updates a show name.
func (app *App) RenamePlaylist(w http.ResponseWriter, r *http.Request) {
	id := formInt64(r, "id")
	if err := app.Flow.RenamePlaylist(id, r.FormValue("name")); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	redirectFlow(w, r, id)
}

// DeletePlaylist removes a show.
func (app *App) DeletePlaylist(w http.ResponseWriter, r *http.Request) {
	id := formInt64(r, "id")
	if err := app.Flow.DeletePlaylist(id); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/flow", http.StatusSeeOther)
}

// AddItem appends a song, break, or gate to a show's flow.
func (app *App) AddItem(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	plID := formInt64(r, "playlist_id")
	itemType := r.FormValue("type")

	var trackID *int64
	breakSec := 0
	switch itemType {
	case "song":
		if tid, err := strconv.ParseInt(r.FormValue("track_id"), 10, 64); err == nil {
			trackID = &tid
		}
	case "break":
		breakSec, _ = strconv.Atoi(r.FormValue("break_sec"))
		if breakSec <= 0 {
			breakSec = services.DefaultBreakSec
		}
	}
	autoNext := r.FormValue("auto_next") != "off"
	if _, err := app.Flow.AddItem(plID, itemType, trackID, breakSec, r.FormValue("label"), autoNext); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	redirectFlow(w, r, plID)
}

// DeleteItem removes a flow item.
func (app *App) DeleteItem(w http.ResponseWriter, r *http.Request) {
	plID := formInt64(r, "playlist_id")
	itemID := formInt64(r, "id")
	if err := app.Flow.DeleteItem(plID, itemID); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	redirectFlow(w, r, plID)
}

// MoveItem reorders one item up or down by swapping positions with its neighbor.
func (app *App) MoveItem(w http.ResponseWriter, r *http.Request) {
	plID := formInt64(r, "playlist_id")
	itemID := formInt64(r, "id")
	dir := r.FormValue("dir") // "up" | "down"

	items, err := app.Flow.GetItems(plID)
	if err != nil {
		// Without this the read failure looks exactly like "item not found" and
		// the redirect below reports success.
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	ids := make([]int64, len(items))
	pos := -1
	for i, it := range items {
		ids[i] = it.ID
		if it.ID == itemID {
			pos = i
		}
	}
	if pos >= 0 {
		swap := -1
		if dir == "up" && pos > 0 {
			swap = pos - 1
		} else if dir == "down" && pos < len(ids)-1 {
			swap = pos + 1
		}
		if swap >= 0 {
			ids[pos], ids[swap] = ids[swap], ids[pos]
			if err := app.Flow.Reorder(plID, ids); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
		}
	}
	redirectFlow(w, r, plID)
}

// ReorderItems persists a full ordering posted by the drag-and-drop UI as a
// comma-separated id list. The client DOM already shows the new order, so a
// 204 is enough (see static/flow-dnd.js).
func (app *App) ReorderItems(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	plID := formInt64(r, "playlist_id")
	var ids []int64
	for _, part := range strings.Split(r.FormValue("ids"), ",") {
		if id, err := strconv.ParseInt(strings.TrimSpace(part), 10, 64); err == nil {
			ids = append(ids, id)
		}
	}
	if plID == 0 || len(ids) == 0 {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if err := app.Flow.Reorder(plID, ids); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// SetBreakSec updates a break item's length from the inline field in the rundown.
func (app *App) SetBreakSec(w http.ResponseWriter, r *http.Request) {
	plID := formInt64(r, "playlist_id")
	itemID := formInt64(r, "id")
	sec, _ := strconv.Atoi(r.FormValue("break_sec"))
	if sec <= 0 {
		sec = services.DefaultBreakSec
	}
	if err := app.Flow.SetBreakSec(itemID, sec); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	redirectFlow(w, r, plID)
}

// ToggleAutoNext flips an item's auto-continue flag.
func (app *App) ToggleAutoNext(w http.ResponseWriter, r *http.Request) {
	plID := formInt64(r, "playlist_id")
	itemID := formInt64(r, "id")
	auto := r.FormValue("auto_next") == "on"
	if err := app.Flow.SetAutoNext(itemID, auto); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	redirectFlow(w, r, plID)
}
