package main

import (
	"context"
	"database/sql"
	"log"
	"mime"
	"net/http"
	_ "net/http/pprof" // registers /debug/pprof handlers on the default mux (PLAYOUT_DIAG)
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"music-to-rtmp-playout/config"
	"music-to-rtmp-playout/handlers"
	"music-to-rtmp-playout/services"
	"music-to-rtmp-playout/services/playout"

	"github.com/gorilla/mux"
	"github.com/gorilla/sessions"
	"github.com/joho/godotenv"
	_ "modernc.org/sqlite"
)

func main() {
	if err := godotenv.Load(); err != nil {
		log.Println("No .env file found; using environment variables")
	}

	cfg := config.LoadConfig()
	log.SetFlags(log.LstdFlags | log.Lshortfile)

	// Diagnostics (opt-in via PLAYOUT_DIAG=1): expose net/http/pprof on :6060 for
	// CPU/goroutine profiling while chasing playback-timing issues.
	if os.Getenv("PLAYOUT_DIAG") != "" {
		go func() {
			log.Println("PLAYOUT_DIAG: pprof listening on http://localhost:6060/debug/pprof/")
			log.Println(http.ListenAndServe("localhost:6060", nil))
		}()
	}

	// Ensure runtime directories exist.
	for _, d := range []string{
		filepath.Dir(cfg.DBPath), cfg.MediaDir, cfg.SoundboardDir, cfg.AssetsDir,
	} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			log.Fatalf("create dir %s: %v", d, err)
		}
	}

	// Open SQLite (pure-Go driver, no CGO).
	db, err := sql.Open("sqlite", cfg.DBPath)
	if err != nil {
		log.Fatalf("open db: %v", err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1) // SQLite: serialize writes to avoid "database is locked"
	if err := initDB(db); err != nil {
		log.Fatalf("init db: %v", err)
	}

	store := sessions.NewCookieStore([]byte(cfg.SessionSecret))
	store.Options = &sessions.Options{
		Path:     "/",
		MaxAge:   cfg.SessionMaxAge,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	}

	// Services.
	authSvc := services.NewAuthService(db)
	if err := authSvc.EnsureAdmin(cfg.AdminUsername, cfg.AdminPassword); err != nil {
		log.Printf("admin bootstrap: %v", err)
	}
	librarySvc := services.NewLibraryService(db, cfg.MediaDir, cfg.FFmpegPath, cfg.FFprobePath, cfg.YtDlpPath)
	flowSvc := services.NewFlowService(db)
	soundboardSvc := services.NewSoundboardService(db, cfg.SoundboardDir, cfg.FFmpegPath)
	settingsSvc := services.NewSettingsService(db)
	spotifySvc := services.NewSpotifyService(cfg.SpotifyClientID, cfg.SpotifyClientSecret)

	// Seed settings from config defaults on first run (empty RTMP URL). The
	// resolution backfill also covers DBs migrated from before it was a setting.
	if st, err := settingsSvc.Get(); err == nil {
		seed := st.RTMPURL == ""
		if seed {
			st.RTMPURL = cfg.RTMPURL
			st.BgImagePath = cfg.BgImagePath
			st.VideoFPS = cfg.VideoFPS
			st.VideoBitrate = cfg.VideoBitrate
			st.AudioBitrate = cfg.AudioBitrate
			st.Theme = cfg.Theme
		}
		if st.VideoWidth <= 0 || st.VideoHeight <= 0 {
			st.VideoWidth, st.VideoHeight = cfg.VideoWidth, cfg.VideoHeight
			seed = true
		}
		if seed {
			_ = settingsSvc.Save(st)
		}
	}

	nvencAvailable := playout.DetectNVENC(cfg.FFmpegPath)
	if nvencAvailable {
		log.Printf("video encoder: NVENC (h264_nvenc) available — 'auto' will use the GPU")
	} else {
		log.Printf("video encoder: NVENC not available — using software libx264")
	}

	engine := playout.NewEngine(playout.EngineConfig{
		FFmpegPath:     cfg.FFmpegPath,
		NowTxtPath:     filepath.Join(cfg.AssetsDir, "now.txt"),
		ArtLivePath:    filepath.Join(cfg.AssetsDir, "art_live.png"),
		NVENCAvailable: nvencAvailable,
	})

	tmpl, err := handlers.LoadTemplates("./templates")
	if err != nil {
		log.Fatalf("load templates: %v", err)
	}

	app := handlers.NewApp(db, store, cfg, tmpl, authSvc, librarySvc, flowSvc, soundboardSvc, settingsSvc, spotifySvc, engine)

	// MIME fixes for Windows dev.
	mime.AddExtensionType(".css", "text/css")
	mime.AddExtensionType(".js", "application/javascript")
	mime.AddExtensionType(".woff2", "font/woff2")

	r := mux.NewRouter()

	// Auth.
	r.HandleFunc("/login", app.LoginPage).Methods("GET")
	r.HandleFunc("/login", app.Login).Methods("POST")
	r.HandleFunc("/setup", app.Setup).Methods("POST")
	r.HandleFunc("/logout", app.Logout).Methods("POST", "GET")

	// Pages (auth-gated, redirect to login).
	r.HandleFunc("/", app.RequirePage(app.StreamPage)).Methods("GET")
	r.HandleFunc("/library", app.RequirePage(app.LibraryPage)).Methods("GET")
	r.HandleFunc("/flow", app.RequirePage(app.FlowPage)).Methods("GET")
	r.HandleFunc("/stream", app.RequirePage(app.StreamPage)).Methods("GET")
	r.HandleFunc("/soundboard", app.RequirePage(app.SoundboardPage)).Methods("GET")
	r.HandleFunc("/settings", app.RequirePage(app.SettingsPage)).Methods("GET")

	// Library API.
	r.HandleFunc("/api/library/upload", app.RequireAuth(app.UploadTrack)).Methods("POST")
	r.HandleFunc("/api/library/delete", app.RequireAuth(app.DeleteTrack)).Methods("POST")
	r.HandleFunc("/api/library/edit", app.RequireAuth(app.EditTrack)).Methods("POST")
	r.HandleFunc("/api/library/preview", app.RequireAuth(app.PreviewTrack)).Methods("GET")
	r.HandleFunc("/api/library/import", app.RequireAuth(app.ImportYouTube)).Methods("GET")
	r.HandleFunc("/api/library/import-spotify", app.RequireAuth(app.ImportSpotify)).Methods("POST")

	// Flow API.
	r.HandleFunc("/api/flow/create", app.RequireAuth(app.CreatePlaylist)).Methods("POST")
	r.HandleFunc("/api/flow/rename", app.RequireAuth(app.RenamePlaylist)).Methods("POST")
	r.HandleFunc("/api/flow/delete", app.RequireAuth(app.DeletePlaylist)).Methods("POST")
	r.HandleFunc("/api/flow/item/add", app.RequireAuth(app.AddItem)).Methods("POST")
	r.HandleFunc("/api/flow/item/delete", app.RequireAuth(app.DeleteItem)).Methods("POST")
	r.HandleFunc("/api/flow/item/move", app.RequireAuth(app.MoveItem)).Methods("POST")
	r.HandleFunc("/api/flow/item/autonext", app.RequireAuth(app.ToggleAutoNext)).Methods("POST")
	r.HandleFunc("/api/flow/item/breaksec", app.RequireAuth(app.SetBreakSec)).Methods("POST")
	r.HandleFunc("/api/flow/reorder", app.RequireAuth(app.ReorderItems)).Methods("POST")
	r.HandleFunc("/api/flow/bulk-upload", app.RequireAuth(app.BulkUploadToFlow)).Methods("POST")
	r.HandleFunc("/api/flow/import-youtube", app.RequireAuth(app.ImportYouTubeToFlow)).Methods("POST")
	r.HandleFunc("/api/flow/import-spotify", app.RequireAuth(app.ImportSpotifyToFlow)).Methods("POST")

	// Soundboard API.
	r.HandleFunc("/api/soundboard/upload", app.RequireAuth(app.UploadClip)).Methods("POST")
	r.HandleFunc("/api/soundboard/delete", app.RequireAuth(app.DeleteClip)).Methods("POST")
	r.HandleFunc("/api/soundboard/trigger", app.RequireAuth(app.TriggerClip)).Methods("POST", "GET")
	r.HandleFunc("/api/soundboard/preview", app.RequireAuth(app.PreviewClip)).Methods("GET")

	// Stream control API.
	r.HandleFunc("/api/stream/start", app.RequireAuth(app.StartStream)).Methods("POST", "GET")
	r.HandleFunc("/api/stream/stop", app.RequireAuth(app.StopStream)).Methods("POST", "GET")
	r.HandleFunc("/api/stream/skip", app.RequireAuth(app.SkipItem)).Methods("POST", "GET")
	r.HandleFunc("/api/stream/play", app.RequireAuth(app.PlayResume)).Methods("POST", "GET")
	r.HandleFunc("/api/stream/pause", app.RequireAuth(app.PauseStream)).Methods("POST", "GET")
	r.HandleFunc("/api/stream/prev", app.RequireAuth(app.PrevItem)).Methods("POST", "GET")
	r.HandleFunc("/api/stream/restart", app.RequireAuth(app.RestartItem)).Methods("POST", "GET")
	r.HandleFunc("/api/stream/jump", app.RequireAuth(app.JumpToItem)).Methods("POST", "GET")
	r.HandleFunc("/api/stream/pause-after", app.RequireAuth(app.TogglePauseAfter)).Methods("POST", "GET")
	r.HandleFunc("/api/stream/autonext", app.RequireAuth(app.StreamSetAutoNext)).Methods("POST", "GET")
	r.HandleFunc("/api/stream/rundown", app.RequireAuth(app.StreamRundown)).Methods("GET")
	r.HandleFunc("/api/stream/status", app.RequireAuth(app.StreamStatus)).Methods("GET")

	// Settings.
	r.HandleFunc("/api/settings/save", app.RequireAuth(app.SaveSettings)).Methods("POST")
	r.HandleFunc("/api/settings/bg-preview", app.RequireAuth(app.BgPreview)).Methods("GET")

	// Debug control + stats (localhost, unauthenticated) — only when PLAYOUT_DIAG
	// is set. Lets an operator drive and observe a show while chasing timing bugs.
	if os.Getenv("PLAYOUT_DIAG") != "" {
		r.HandleFunc("/debug/playout/stats", app.DebugStats).Methods("GET")
		r.HandleFunc("/debug/playout/playlists", app.DebugPlaylists).Methods("GET")
		r.HandleFunc("/debug/playout/start", app.DebugStart).Methods("GET", "POST")
		r.HandleFunc("/debug/playout/stop", app.DebugStop).Methods("GET", "POST")
		r.HandleFunc("/debug/playout/skip", app.DebugSkip).Methods("GET", "POST")
		r.HandleFunc("/debug/playout/play", app.DebugPlay).Methods("GET", "POST")
		log.Println("PLAYOUT_DIAG: debug control at http://localhost:8080/debug/playout/{stats,playlists,start,stop}")
	}

	// Static assets.
	r.PathPrefix("/static/").Handler(http.StripPrefix("/static/", http.FileServer(http.Dir("./static"))))

	handler := handlers.SecurityHeadersMiddleware(r)

	server := &http.Server{Addr: cfg.Host + ":" + cfg.Port, Handler: handler}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		log.Printf("Playout server listening on %s", server.Addr)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("server error: %v", err)
		}
	}()

	<-ctx.Done()
	log.Println("Shutting down…")
	engine.Stop()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = server.Shutdown(shutdownCtx)
	log.Println("Bye")
}
