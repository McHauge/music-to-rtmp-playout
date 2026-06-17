package config

import (
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
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
	FFmpegPath string
	FFprobePath string
	YtDlpPath  string

	// Playout / encoder defaults (overridable per-show in settings)
	RTMPURL      string // upstream RTMP target, e.g. rtmp://localhost:1935/live/show
	VideoWidth   int
	VideoHeight  int
	VideoFPS     int
	AudioBitrate string // e.g. "160k"
	BgImagePath  string // background still for the video track

	// UI
	Theme string

	// Admin bootstrap (optional — first-run setup screen used otherwise)
	AdminUsername string
	AdminPassword string
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
		VideoWidth:   getEnvInt("VIDEO_WIDTH", 1280),
		VideoHeight:  getEnvInt("VIDEO_HEIGHT", 720),
		VideoFPS:     getEnvInt("VIDEO_FPS", 10),
		AudioBitrate: getEnv("AUDIO_BITRATE", "160k"),
		BgImagePath:  getEnv("BG_IMAGE_PATH", "./assets/bg.png"),

		Theme: getEnv("THEME", "teal"),

		AdminUsername: os.Getenv("ADMIN_USERNAME"),
		AdminPassword: os.Getenv("ADMIN_PASSWORD"),
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
