package handlers

import (
	"database/sql"

	"music-to-rtmp-playout/config"
	"music-to-rtmp-playout/services"
	"music-to-rtmp-playout/services/playout"

	"github.com/gorilla/sessions"
)

// App is the dependency-injection container shared by all handlers.
type App struct {
	Db    *sql.DB
	Store *sessions.CookieStore
	Cfg   *config.Config
	Tmpl  *Templates

	Auth       *services.AuthService
	Library    *services.LibraryService
	Flow       *services.FlowService
	Soundboard *services.SoundboardService
	Settings   *services.SettingsService
	Engine     *playout.Engine
}

// NewApp wires the container.
func NewApp(db *sql.DB, store *sessions.CookieStore, cfg *config.Config,
	tmpl *Templates,
	auth *services.AuthService, lib *services.LibraryService, flow *services.FlowService,
	sb *services.SoundboardService, settings *services.SettingsService, engine *playout.Engine) *App {
	return &App{
		Db: db, Store: store, Cfg: cfg, Tmpl: tmpl,
		Auth: auth, Library: lib, Flow: flow, Soundboard: sb, Settings: settings, Engine: engine,
	}
}

// PageData is the base data passed to every full-page render.
type PageData struct {
	Title string
	Page  string // active nav key
	Theme string
	Extra interface{}
}

// currentTheme resolves the theme for a request: settings DB value, falling
// back to config default.
func (app *App) currentTheme() string {
	if st, err := app.Settings.Get(); err == nil && st.Theme != "" {
		return st.Theme
	}
	return app.Cfg.Theme
}
