package services

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// SpotifyTrack is one track resolved from a Spotify playlist or a pasted
// track list. DurationSec is 0 when unknown (paste fallback) — the YouTube
// match scoring then falls back to title/channel heuristics alone.
type SpotifyTrack struct {
	Artist      string
	Title       string
	DurationSec int
}

// Query returns the YouTube search string for this track.
func (t SpotifyTrack) Query() string {
	if t.Artist == "" {
		return t.Title
	}
	return t.Artist + " " + t.Title
}

// Label returns the human-readable "Artist - Title" form for log lines.
func (t SpotifyTrack) Label() string {
	if t.Artist == "" {
		return t.Title
	}
	return t.Artist + " - " + t.Title
}

// ErrSpotifyPlaylistUnavailable marks a 404 from the playlist endpoint. Since
// late 2024 the Spotify Web API returns 404 for Spotify-made editorial and
// algorithmic playlists (and for private ones) when accessed by third-party
// apps, so the message points the user at the paste fallback.
var ErrSpotifyPlaylistUnavailable = errors.New(
	"playlist not accessible via the Spotify API — Spotify-made and private playlists are blocked for third-party apps; use the paste-a-tracklist box instead")

// SpotifyService resolves playlist URLs to track metadata via the Spotify Web
// API using the client-credentials flow. It never touches audio — Spotify
// only provides the what; the audio comes from YouTube via yt-dlp.
type SpotifyService struct {
	clientID     string
	clientSecret string
	http         *http.Client

	mu       sync.Mutex // guards token/tokenExp
	token    string
	tokenExp time.Time
}

func NewSpotifyService(clientID, clientSecret string) *SpotifyService {
	return &SpotifyService{
		clientID:     clientID,
		clientSecret: clientSecret,
		http:         &http.Client{Timeout: 20 * time.Second},
	}
}

// Configured reports whether API credentials are present.
func (s *SpotifyService) Configured() bool {
	return s.clientID != "" && s.clientSecret != ""
}

var spotifyPlaylistIDRe = regexp.MustCompile(`^[0-9A-Za-z]{22}$`)

// ParseSpotifyPlaylistID extracts the playlist ID from an
// open.spotify.com/playlist/... URL (including /intl-xx/ paths and ?si=
// tracking junk), a spotify:playlist:... URI, or a bare 22-char ID.
func ParseSpotifyPlaylistID(input string) (string, error) {
	input = strings.TrimSpace(input)
	if spotifyPlaylistIDRe.MatchString(input) {
		return input, nil
	}
	if rest, ok := strings.CutPrefix(input, "spotify:playlist:"); ok {
		if spotifyPlaylistIDRe.MatchString(rest) {
			return rest, nil
		}
		return "", fmt.Errorf("malformed Spotify playlist URI %q", input)
	}
	u, err := url.Parse(input)
	if err != nil || !strings.HasSuffix(u.Hostname(), "spotify.com") {
		return "", fmt.Errorf("not a Spotify playlist link: %q", input)
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	for i, p := range parts {
		if p == "playlist" && i+1 < len(parts) && spotifyPlaylistIDRe.MatchString(parts[i+1]) {
			return parts[i+1], nil
		}
	}
	return "", fmt.Errorf("no playlist ID in URL — expected https://open.spotify.com/playlist/…")
}

// accessToken returns a cached app token, fetching a fresh one via the
// client-credentials flow when missing or near expiry.
func (s *SpotifyService) accessToken() (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.token != "" && time.Now().Before(s.tokenExp) {
		return s.token, nil
	}

	req, err := http.NewRequest("POST", "https://accounts.spotify.com/api/token",
		strings.NewReader("grant_type=client_credentials"))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Authorization", "Basic "+
		base64.StdEncoding.EncodeToString([]byte(s.clientID+":"+s.clientSecret)))

	resp, err := s.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("spotify auth request failed: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("spotify auth failed (%d) — check SPOTIFY_CLIENT_ID / SPOTIFY_CLIENT_SECRET: %s",
			resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var tok struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &tok); err != nil || tok.AccessToken == "" {
		return "", fmt.Errorf("spotify auth: unexpected token response")
	}
	s.token = tok.AccessToken
	s.tokenExp = time.Now().Add(time.Duration(tok.ExpiresIn-60) * time.Second)
	return s.token, nil
}

// invalidateToken drops the cached token so the next call re-authenticates.
func (s *SpotifyService) invalidateToken() {
	s.mu.Lock()
	s.token = ""
	s.mu.Unlock()
}

// PlaylistTracks pages through the playlist's tracks and returns all
// (artist, title, duration) tuples in playlist order.
func (s *SpotifyService) PlaylistTracks(playlistID string) ([]SpotifyTrack, error) {
	next := "https://api.spotify.com/v1/playlists/" + url.PathEscape(playlistID) +
		"/tracks?limit=100&additional_types=track&fields=" +
		url.QueryEscape("items(track(name,artists(name),duration_ms,is_local)),next")

	var out []SpotifyTrack
	for page := 0; next != "" && page < 30; page++ {
		body, err := s.apiGET(next)
		if err != nil {
			return nil, err
		}
		var pg struct {
			Items []struct {
				Track *struct {
					Name       string `json:"name"`
					DurationMS int    `json:"duration_ms"`
					IsLocal    bool   `json:"is_local"`
					Artists    []struct {
						Name string `json:"name"`
					} `json:"artists"`
				} `json:"track"`
			} `json:"items"`
			Next *string `json:"next"`
		}
		if err := json.Unmarshal(body, &pg); err != nil {
			return nil, fmt.Errorf("spotify: unexpected playlist response: %w", err)
		}
		for _, it := range pg.Items {
			t := it.Track
			if t == nil || t.Name == "" || t.IsLocal {
				continue // removed tracks, episodes, local files
			}
			st := SpotifyTrack{Title: t.Name, DurationSec: t.DurationMS / 1000}
			if len(t.Artists) > 0 {
				st.Artist = t.Artists[0].Name
			}
			out = append(out, st)
		}
		next = ""
		if pg.Next != nil {
			next = *pg.Next
		}
	}
	return out, nil
}

// apiGET performs an authenticated GET, retrying once on an expired token
// (401) and once on rate limiting (429, honoring Retry-After up to 10s).
func (s *SpotifyService) apiGET(apiURL string) ([]byte, error) {
	retried401, retried429 := false, false
	for {
		token, err := s.accessToken()
		if err != nil {
			return nil, err
		}
		req, err := http.NewRequest("GET", apiURL, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := s.http.Do(req)
		if err != nil {
			return nil, fmt.Errorf("spotify request failed: %w", err)
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		resp.Body.Close()

		switch {
		case resp.StatusCode == http.StatusOK:
			return body, nil
		case resp.StatusCode == http.StatusNotFound:
			return nil, ErrSpotifyPlaylistUnavailable
		case resp.StatusCode == http.StatusUnauthorized && !retried401:
			retried401 = true
			s.invalidateToken()
		case resp.StatusCode == http.StatusTooManyRequests && !retried429:
			retried429 = true
			wait := 2 * time.Second
			if secs, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && secs > 0 {
				wait = time.Duration(min(secs, 10)) * time.Second
			}
			time.Sleep(wait)
		default:
			return nil, fmt.Errorf("spotify API error (%d): %s",
				resp.StatusCode, strings.TrimSpace(string(body)))
		}
	}
}

var trackLineNumberRe = regexp.MustCompile(`^\s*\d+[.)]\s*`)

// ParseTrackLines parses pasted text into tracks: one per line, either
// "Artist - Title" (split on the first " - ") or just a title. Blank lines
// are skipped and a leading "N." / "N)" numbering prefix is stripped.
func ParseTrackLines(text string) []SpotifyTrack {
	var out []SpotifyTrack
	for _, line := range strings.Split(text, "\n") {
		line = trackLineNumberRe.ReplaceAllString(strings.TrimSpace(line), "")
		if line == "" {
			continue
		}
		artist, title, found := strings.Cut(line, " - ")
		if !found {
			out = append(out, SpotifyTrack{Title: line})
			continue
		}
		out = append(out, SpotifyTrack{
			Artist: strings.TrimSpace(artist),
			Title:  strings.TrimSpace(title),
		})
	}
	return out
}
