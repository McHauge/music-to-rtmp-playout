package handlers

import (
	"log"
	"net/http"
)

// LoginPage renders the login (or first-run setup) screen.
func (app *App) LoginPage(w http.ResponseWriter, r *http.Request) {
	if app.isAuthed(r) {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	hasUsers, _ := app.Auth.HasUsers()
	data := PageData{
		Title: "Sign in",
		Page:  "login",
		Theme: app.currentTheme(),
		Extra: map[string]any{"Setup": !hasUsers},
	}
	if err := app.Tmpl.Execute(w, "page-login", data); err != nil {
		log.Printf("render login: %v", err)
		http.Error(w, "template error", http.StatusInternalServerError)
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

func (app *App) loginError(w http.ResponseWriter, r *http.Request, msg string) {
	hasUsers, _ := app.Auth.HasUsers()
	data := PageData{
		Title: "Sign in",
		Page:  "login",
		Theme: app.currentTheme(),
		Extra: map[string]any{"Setup": !hasUsers, "Error": msg},
	}
	w.WriteHeader(http.StatusUnauthorized)
	_ = app.Tmpl.Execute(w, "page-login", data)
}
