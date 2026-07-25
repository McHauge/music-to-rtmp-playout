package handlers

import (
	"net/http"
)

// LoginPage renders the login (or first-run setup) screen.
func (app *App) LoginPage(w http.ResponseWriter, r *http.Request) {
	if app.isAuthed(r) {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	app.render(w, "page-login", app.loginPageData(""))
}

// loginPageData builds the login screen's page data. First run (no accounts
// yet) switches the form to setup mode; msg, when non-empty, is shown as an
// error above it.
func (app *App) loginPageData(msg string) PageData {
	hasUsers, _ := app.Auth.HasUsers()
	extra := map[string]any{"Setup": !hasUsers}
	if msg != "" {
		extra["Error"] = msg
	}
	return PageData{
		Title: "Sign in",
		Page:  "login",
		Theme: app.currentTheme(),
		Extra: extra,
	}
}

// Login authenticates a form submission. The browser sends a SHA-256 hash in
// the password field; we verify against the stored bcrypt.
func (app *App) Login(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	username := r.FormValue("username")
	clientHash := r.FormValue("password")

	id, err := app.Auth.Authenticate(username, clientHash)
	if err != nil {
		app.loginError(w, r, "Invalid username or password.")
		return
	}
	if err := app.setSession(w, r, id); err != nil {
		app.loginError(w, r, "Could not start session.")
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// Setup creates the first account (first-run only).
func (app *App) Setup(w http.ResponseWriter, r *http.Request) {
	if has, _ := app.Auth.HasUsers(); has {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	_ = r.ParseForm()
	username := r.FormValue("username")
	clientHash := r.FormValue("password")
	if err := app.Auth.CreateUser(username, clientHash); err != nil {
		app.loginError(w, r, "Could not create account: "+err.Error())
		return
	}
	id, err := app.Auth.Authenticate(username, clientHash)
	if err == nil {
		_ = app.setSession(w, r, id)
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// Logout clears the session.
func (app *App) Logout(w http.ResponseWriter, r *http.Request) {
	app.clearSession(w, r)
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// loginError re-renders the login screen with msg and a 401.
func (app *App) loginError(w http.ResponseWriter, r *http.Request, msg string) {
	w.WriteHeader(http.StatusUnauthorized)
	app.render(w, "page-login", app.loginPageData(msg))
}
