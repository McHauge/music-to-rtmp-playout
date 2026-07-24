package services

import "time"

// Track is a single audio item stored on disk (downloaded or uploaded).
type Track struct {
	ID          int64
	Title       string
	Artist      string
	Source      string // "youtube" | "upload"
	FilePath    string
	DurationSec float64
	ArtPath     string // normalized square cover art (300x300 png); "" = none
	AddedAt     time.Time
}

// Display returns "Artist — Title" or just the title when no artist is known.
func (t Track) Display() string {
	if t.Artist != "" {
		return t.Artist + " — " + t.Title
	}
	return t.Title
}

// Playlist (a "show") owns an ordered list of FlowItems.
type Playlist struct {
	ID              int64
	Name            string
	DefaultBreakSec int // preferred spacing between songs, in seconds
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// FlowItem types.
const (
	ItemSong  = "song"
	ItemBreak = "break"
	ItemGate  = "gate"
)

// FlowItem is one step in a show's playout flow.
type FlowItem struct {
	ID         int64
	PlaylistID int64
	Position   int
	Type       string // song | break | gate
	TrackID    *int64
	BreakSec   int
	Label      string
	AutoNext   bool

	// Joined for display (not persisted on the row itself).
	Track *Track
}

// SoundboardClip is an uploaded clip, pre-decoded to raw PCM for instant trigger.
type SoundboardClip struct {
	ID        int64
	Name      string
	FilePath  string
	PCMPath   string
	CreatedAt time.Time
}

// Settings is the single-row app configuration editable from the UI.
type Settings struct {
	RTMPURL      string
	StreamKey    string
	BgImagePath  string
	VideoFPS     int
	VideoWidth   int
	VideoHeight  int
	VideoEnabled bool   // false = audio-only stream, no video track
	VideoBitrate string // CBR rate; empty = auto (CRF)
	VideoEncoder string // "auto" | "cpu" | "nvenc" — GPU (NVENC) selection
	AudioBitrate string
	NowOverlay   bool   // show the "now playing" lower-third banner
	VizStyle     string // banner visualization: "bars" | "wave" | "none"
	BannerBox    bool   // translucent box behind the banner
	Theme        string
}

// FullRTMPURL joins the base URL and stream key into the final ingest URL.
func (s Settings) FullRTMPURL() string {
	if s.StreamKey == "" {
		return s.RTMPURL
	}
	sep := "/"
	if len(s.RTMPURL) > 0 && s.RTMPURL[len(s.RTMPURL)-1] == '/' {
		sep = ""
	}
	return s.RTMPURL + sep + s.StreamKey
}
