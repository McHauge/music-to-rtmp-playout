package handlers

import (
	"net/http"
	"strconv"
	"strings"
)

// CreatePlaylist makes a new show and opens its builder.
func (app *App) CreatePlaylist(w http.ResponseWriter, r *http.Request) {
	id, err := app.Flow.CreatePlaylist(r.FormValue("name"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/flow?id="+strconv.FormatInt(id, 10), http.StatusSeeOther)
}

// RenamePlaylist updates a show name.
func (app *App) RenamePlaylist(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.FormValue("id"), 10, 64)
	_ = app.Flow.RenamePlaylist(id, r.FormValue("name"))
	http.Redirect(w, r, "/flow?id="+strconv.FormatInt(id, 10), http.StatusSeeOther)
}

// DeletePlaylist removes a show.
func (app *App) DeletePlaylist(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.FormValue("id"), 10, 64)
	_ = app.Flow.DeletePlaylist(id)
	http.Redirect(w, r, "/flow", http.StatusSeeOther)
}

// AddItem appends a song, break, or gate to a show's flow.
func (app *App) AddItem(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	plID, _ := strconv.ParseInt(r.FormValue("playlist_id"), 10, 64)
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
			breakSec = 20
		}
	}
	autoNext := r.FormValue("auto_next") != "off"
	_, _ = app.Flow.AddItem(plID, itemType, trackID, breakSec, r.FormValue("label"), autoNext)
	http.Redirect(w, r, "/flow?id="+strconv.FormatInt(plID, 10), http.StatusSeeOther)
}

// DeleteItem removes a flow item.
func (app *App) DeleteItem(w http.ResponseWriter, r *http.Request) {
	plID, _ := strconv.ParseInt(r.FormValue("playlist_id"), 10, 64)
	itemID, _ := strconv.ParseInt(r.FormValue("id"), 10, 64)
	_ = app.Flow.DeleteItem(plID, itemID)
	http.Redirect(w, r, "/flow?id="+strconv.FormatInt(plID, 10), http.StatusSeeOther)
}

// MoveItem reorders one item up or down by swapping positions with its neighbor.
func (app *App) MoveItem(w http.ResponseWriter, r *http.Request) {
	plID, _ := strconv.ParseInt(r.FormValue("playlist_id"), 10, 64)
	itemID, _ := strconv.ParseInt(r.FormValue("id"), 10, 64)
	dir := r.FormValue("dir") // "up" | "down"

	items, _ := app.Flow.GetItems(plID)
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
			_ = app.Flow.Reorder(plID, ids)
		}
	}
	http.Redirect(w, r, "/flow?id="+strconv.FormatInt(plID, 10), http.StatusSeeOther)
}

// ReorderItems persists a full ordering posted by the drag-and-drop UI as a
// comma-separated id list. The client DOM already shows the new order, so a
// 204 is enough (see static/flow-dnd.js).
func (app *App) ReorderItems(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	plID, _ := strconv.ParseInt(r.FormValue("playlist_id"), 10, 64)
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

// ToggleAutoNext flips an item's auto-continue flag.
func (app *App) ToggleAutoNext(w http.ResponseWriter, r *http.Request) {
	plID, _ := strconv.ParseInt(r.FormValue("playlist_id"), 10, 64)
	itemID, _ := strconv.ParseInt(r.FormValue("id"), 10, 64)
	auto := r.FormValue("auto_next") == "on"
	_ = app.Flow.SetAutoNext(itemID, auto)
	http.Redirect(w, r, "/flow?id="+strconv.FormatInt(plID, 10), http.StatusSeeOther)
}
