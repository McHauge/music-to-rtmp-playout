package handlers

import (
	"net/http"
	"strconv"
)

// Request-parameter helpers. Every form/query id in this package is an optional
// integer that falls back to zero when absent or unparseable — the service layer
// then reports "not found" — so the ignored-error idiom is collapsed here rather
// than repeated at ~20 call sites.

// formInt64 reads an int64 form field, 0 when absent or unparseable.
func formInt64(r *http.Request, name string) int64 {
	v, _ := strconv.ParseInt(r.FormValue(name), 10, 64)
	return v
}

// queryInt64 reads an int64 query parameter, 0 when absent or unparseable.
func queryInt64(r *http.Request, name string) int64 {
	v, _ := strconv.ParseInt(r.URL.Query().Get(name), 10, 64)
	return v
}

// queryInt reads an int query parameter, 0 when absent or unparseable.
func queryInt(r *http.Request, name string) int {
	v, _ := strconv.Atoi(r.URL.Query().Get(name))
	return v
}

// redirectFlow sends the browser back to a show's builder page.
func redirectFlow(w http.ResponseWriter, r *http.Request, playlistID int64) {
	http.Redirect(w, r, "/flow?id="+strconv.FormatInt(playlistID, 10), http.StatusSeeOther)
}

// servePreview serves a media file for in-browser pre-listening, given the
// result of whatever lookup produced its path. http.ServeFile handles Range
// requests, so the <audio> element can seek. An empty path (no such row) is a
// 404; a lookup error is a 500.
func servePreview(w http.ResponseWriter, r *http.Request, path string, err error) {
	switch {
	case err != nil:
		http.Error(w, err.Error(), http.StatusInternalServerError)
	case path == "":
		http.NotFound(w, r)
	default:
		http.ServeFile(w, r, path)
	}
}
