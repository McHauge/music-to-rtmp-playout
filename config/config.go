package config

import (
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
)

// Encoder/video defaults, shared by the env loader, the playout encoder's
// zero-value fallbacks, and the settings-form parser so the canonical numbers
// live in exactly one place.
const (
	DefaultVideoWidth   = 1280
	DefaultVideoHeight  = 720
	DefaultVideoFPS     = 10
	DefaultAudioBitrate = "160k"
)

// Config holds all application configuration, loaded from environment variables
// with sensible defaults for a single-container self-hosted deployment.
type Config struct {
	// Server
	Host string
	Port string

	// Session / auth
	SessionSecret string
	SessionMaxAge int // seconds

	// Storage paths (all live on mounted volumes in Docker)
	DBPath        string // sqlite file
	MediaDir      string // downloaded/uploaded songs
	SoundboardDir string // soundboard clips + .pcm cache
	AssetsDir     string // background images, runtime now.txt

	// Uploads
	MaxUploadSize int64

	// External tools
	FFmpegPath  string
	FFprobePath string
	YtDlpPath   string

	// Playout / encoder defaults (overridable per-show in settings)
	RTMPURL      string // upstream RTMP target, e.g. rtmp://localhost:1935/live/show
	VideoWidth   int
	VideoHeight  int
	VideoFPS     int
	VideoBitrate string // CBR rate, e.g. "500k"; empty = auto (CRF)
	AudioBitrate string // e.g. "160k"
	BgImagePath  string // background still for the video track

	// UI
	Theme string

	// Admin bootstrap (optional — first-run setup screen used otherwise)
	AdminUsername string
	AdminPassword string

	// Spotify Web API (optional — playlist import via URL; the paste fallback
	// in the UI needs no credentials)
	SpotifyClientID     string
	SpotifyClientSecret string
}

// LoadConfig reads configuration from the environment, applying defaults.
func LoadConfig() *Config {
	// Bundled-tools directory: ffmpeg/ffprobe/yt-dlp are resolved here first so
	// the app is self-contained (no host install needed). Populate it with
	// scripts/fetch-tools.{ps1,sh}.
	binDir := getEnv("BIN_DIR", "./bin")

	cfg := &Config{
		Host: getEnv("HOST", ""),
		Port: getEnv("PORT", "8080"),

		SessionSecret: getEnv("SESSION_SECRET", "change-me-in-production"),
		SessionMaxAge: 86400 * 7,

		DBPath:        getEnv("DB_PATH", "./data/playout.db"),
		MediaDir:      getEnv("MEDIA_DIR", "./media"),
		SoundboardDir: getEnv("SOUNDBOARD_DIR", "./soundboard"),
		AssetsDir:     getEnv("ASSETS_DIR", "./assets"),

		MaxUploadSize: int64(getEnvInt("MAX_UPLOAD_MB", 200)) * 1024 * 1024,

		FFmpegPath:  resolveTool("FFMPEG_PATH", "ffmpeg", binDir),
		FFprobePath: resolveTool("FFPROBE_PATH", "ffprobe", binDir),
		YtDlpPath:   resolveTool("YTDLP_PATH", "yt-dlp", binDir),

		RTMPURL:      getEnv("RTMP_URL", "rtmp://localhost:1935/live/show"),
		VideoWidth:   getEnvInt("VIDEO_WIDTH", DefaultVideoWidth),
		VideoHeight:  getEnvInt("VIDEO_HEIGHT", DefaultVideoHeight),
		VideoFPS:     getEnvInt("VIDEO_FPS", DefaultVideoFPS),
		VideoBitrate: getEnv("VIDEO_BITRATE", "500k"),
		AudioBitrate: getEnv("AUDIO_BITRATE", DefaultAudioBitrate),
		BgImagePath:  getEnv("BG_IMAGE_PATH", "./assets/bg.png"),

		Theme: getEnv("THEME", "teal"),

		AdminUsername: os.Getenv("ADMIN_USERNAME"),
		AdminPassword: os.Getenv("ADMIN_PASSWORD"),

		SpotifyClientID:     os.Getenv("SPOTIFY_CLIENT_ID"),
		SpotifyClientSecret: os.Getenv("SPOTIFY_CLIENT_SECRET"),
	}

	if cfg.SessionSecret == "change-me-in-production" {
		log.Println("WARNING: SESSION_SECRET not set; using insecure default.")
	}
	log.Printf("Tools: ffmpeg=%s ffprobe=%s yt-dlp=%s", cfg.FFmpegPath, cfg.FFprobePath, cfg.YtDlpPath)
	return cfg
}

func getEnv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func getEnvInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

// resolveTool locates an external binary, preferring (in order): an explicit
// env override, a bundled copy under binDir, then the bare name on PATH. The
// bundled copy makes the app self-contained — run scripts/fetch-tools to fill
// binDir with platform binaries.
func resolveTool(envKey, baseName, binDir string) string {
	if v := os.Getenv(envKey); v != "" {
		return v
	}
	name := baseName
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	local := filepath.Join(binDir, name)
	if _, err := os.Stat(local); err == nil {
		if abs, err := filepath.Abs(local); err == nil {
			return abs
		}
		return local
	}
	return baseName // fall back to PATH
}
