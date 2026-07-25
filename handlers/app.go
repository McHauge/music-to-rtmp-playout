package handlers

import (
	"music-to-rtmp-playout/config"
	"music-to-rtmp-playout/services"
	"music-to-rtmp-playout/services/playout"

	"github.com/gorilla/sessions"
)

// App is the dependency-injection container shared by all handlers. There is no
// *sql.DB here on purpose: every handler reaches the database through a service.
type App struct {
	Store *sessions.CookieStore
	Cfg   *config.Config
	Tmpl  *Templates

	Auth       *services.AuthService
	Library    *services.LibraryService
	Flow       *services.FlowService
	Soundboard *services.SoundboardService
	Settings   *services.SettingsService
	Spotify    *services.SpotifyService
	Engine     *playout.Engine
}

// NewApp wires the container.
func NewApp(store *sessions.CookieStore, cfg *config.Config,
	tmpl *Templates,
	auth *services.AuthService, lib *services.LibraryService, flow *services.FlowService,
	sb *services.SoundboardService, settings *services.SettingsService,
	spotify *services.SpotifyService, engine *playout.Engine) *App {
	return &App{
		Store: store, Cfg: cfg, Tmpl: tmpl,
		Auth: auth, Library: lib, Flow: flow, Soundboard: sb, Settings: settings,
		Spotify: spotify, Engine: engine,
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
