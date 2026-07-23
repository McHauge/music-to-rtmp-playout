package handlers

import "net/http"

const sessionName = "playout-session"

// userID returns the logged-in user id from the session, or 0.
func (app *App) userID(r *http.Request) int64 {
	session, _ := app.Store.Get(r, sessionName)
	if v, ok := session.Values["user_id"].(int64); ok {
		return v
	}
	return 0
}

// isAuthed reports whether the request has a valid session.
func (app *App) isAuthed(r *http.Request) bool {
	id := app.userID(r)
	return id != 0 && app.Auth.UserExists(id)
}

// RequireAuth wraps a handler, returning 401 for API calls or redirecting
// page requests to the login screen.
func (app *App) RequireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if app.isAuthed(r) {
			next(w, r)
			return
		}
		http.Error(w, "Authentication required", http.StatusUnauthorized)
	}
}

// RequirePage wraps a page handler, redirecting unauthenticated users to /login.
func (app *App) RequirePage(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if app.isAuthed(r) {
			next(w, r)
			return
		}
		http.Redirect(w, r, "/login", http.StatusSeeOther)
	}
}

// lastShowID returns the show the user last explicitly opened, or 0.
func (app *App) lastShowID(r *http.Request) int64 {
	session, _ := app.Store.Get(r, sessionName)
	if v, ok := session.Values["last_show_id"].(int64); ok {
		return v
	}
	return 0
}

// rememberShow stores the show id in the session so Flow and Stream reopen it
// on later visits. No-op when the value is already current.
func (app *App) rememberShow(w http.ResponseWriter, r *http.Request, id int64) {
	if id == 0 || app.lastShowID(r) == id {
		return
	}
	session, _ := app.Store.Get(r, sessionName)
	session.Values["last_show_id"] = id
	_ = session.Save(r, w)
}

// setSession stores the user id in an encrypted session cookie.
func (app *App) setSession(w http.ResponseWriter, r *http.Request, id int64) error {
	session, _ := app.Store.Get(r, sessionName)
	session.Values["user_id"] = id
	return session.Save(r, w)
}

// clearSession logs the user out.
func (app *App) clearSession(w http.ResponseWriter, r *http.Request) {
	session, _ := app.Store.Get(r, sessionName)
	session.Values["user_id"] = int64(0)
	session.Options.MaxAge = -1
	_ = session.Save(r, w)
}
