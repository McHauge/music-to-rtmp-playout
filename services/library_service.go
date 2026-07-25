package services

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Bounds for the self-contained ffmpeg/ffprobe/yt-dlp helpers, which operate on
// local files or quick metadata calls and should finish in well under these
// limits — the timeout only fires when a subprocess wedges, so it never pins an
// upload/import handler indefinitely. The full download path is bounded by the
// caller's request context instead (cancels on client disconnect).
const (
	probeTimeout   = 30 * time.Second // ffprobe duration read
	artNormTimeout = 60 * time.Second // ffmpeg cover-art crop/scale
	searchTimeout  = 90 * time.Second // yt-dlp metadata-only search
)

// LibraryService manages the track library: uploads, yt-dlp imports, metadata.
type LibraryService struct {
	db          *sql.DB
	mediaDir    string
	ffmpegPath  string
	ffprobePath string
	ytdlpPath   string
}

func NewLibraryService(db *sql.DB, mediaDir, ffmpegPath, ffprobePath, ytdlpPath string) *LibraryService {
	return &LibraryService{db: db, mediaDir: mediaDir, ffmpegPath: ffmpegPath, ffprobePath: ffprobePath, ytdlpPath: ytdlpPath}
}

// ListTracks returns all tracks, most recently added first.
func (s *LibraryService) ListTracks() ([]Track, error) {
	rows, err := s.db.Query(`SELECT id, title, artist, source, file_path, duration_sec, art_path, added_at FROM tracks ORDER BY added_at DESC, id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Track
	for rows.Next() {
		var t Track
		if err := rows.Scan(&t.ID, &t.Title, &t.Artist, &t.Source, &t.FilePath, &t.DurationSec, &t.ArtPath, &t.AddedAt); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// GetTrack returns one track by id.
func (s *LibraryService) GetTrack(id int64) (*Track, error) {
	var t Track
	err := s.db.QueryRow(`SELECT id, title, artist, source, file_path, duration_sec, art_path, added_at FROM tracks WHERE id = ?`, id).
		Scan(&t.ID, &t.Title, &t.Artist, &t.Source, &t.FilePath, &t.DurationSec, &t.ArtPath, &t.AddedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &t, nil
}

// DeleteTrack removes the DB row and the underlying file.
func (s *LibraryService) DeleteTrack(id int64) error {
	t, err := s.GetTrack(id)
	if err != nil {
		return err
	}
	if t == nil {
		return nil
	}
	if _, err := s.db.Exec(`DELETE FROM tracks WHERE id = ?`, id); err != nil {
		return err
	}
	_ = os.Remove(t.FilePath) // best-effort; orphan files are harmless
	if t.ArtPath != "" {
		_ = os.Remove(t.ArtPath)
	}
	return nil
}

// UpdateMeta updates editable metadata (title/artist).
func (s *LibraryService) UpdateMeta(id int64, title, artist string) error {
	_, err := s.db.Exec(`UPDATE tracks SET title = ?, artist = ? WHERE id = ?`, title, artist, id)
	return err
}

// AddUpload saves an uploaded file to the media dir, probes its duration, and
// inserts a track. origName is used to derive the title.
func (s *LibraryService) AddUpload(origName string, src io.Reader) (*Track, error) {
	if err := os.MkdirAll(s.mediaDir, 0o755); err != nil {
		return nil, err
	}
	ext := filepath.Ext(origName)
	base := sanitize(strings.TrimSuffix(filepath.Base(origName), ext))
	if base == "" {
		base = "upload"
	}
	dest := uniquePath(filepath.Join(s.mediaDir, base+ext))

	f, err := os.Create(dest)
	if err != nil {
		return nil, err
	}
	if _, err := io.Copy(f, src); err != nil {
		f.Close()
		_ = os.Remove(dest)
		return nil, err
	}
	f.Close()

	dur := s.probeDuration(dest)
	return s.insertTrack(base, "", "upload", dest, dur, "")
}

// ImportYouTube downloads audio (and a playlist's worth, if the URL is a
// playlist) via yt-dlp, then imports each resulting file. progress, if
// non-nil, receives yt-dlp's stdout/stderr lines for live UI feedback.
// Returns the tracks the URL resolved to in playlist order, including ones
// that were already in the library.
func (s *LibraryService) ImportYouTube(ctx context.Context, url string, progress func(line string)) ([]Track, error) {
	return s.importVia(ctx, url, "youtube", progress)
}

// importVia runs yt-dlp on url (a real URL or a ytsearchN: query) and imports
// each resulting file with the given source tag. ctx bounds the (potentially
// long, playlist-sized) download — pass the request context so a client
// disconnect kills yt-dlp instead of leaving it pinned.
func (s *LibraryService) importVia(ctx context.Context, url, source string, progress func(line string)) ([]Track, error) {
	if strings.TrimSpace(url) == "" {
		return nil, fmt.Errorf("empty URL")
	}
	if err := os.MkdirAll(s.mediaDir, 0o755); err != nil {
		return nil, err
	}

	outTmpl := filepath.Join(s.mediaDir, "%(id)s.%(ext)s")
	args := []string{
		"-x", "--audio-format", "mp3", "--audio-quality", "0",
		"--write-info-json", "--no-progress",
		"--write-thumbnail", "--convert-thumbnails", "png",
		"--ignore-errors",
		"-o", outTmpl,
	}
	// The audio extraction + thumbnail conversion above need ffmpeg. Our ffmpeg
	// is bundled in BIN_DIR, which isn't on PATH, so point yt-dlp at it directly
	// (accepts the binary's directory) instead of relying on its own lookup.
	if s.ffmpegPath != "" {
		args = append(args, "--ffmpeg-location", filepath.Dir(s.ffmpegPath))
	}
	args = append(args, url)
	cmd := exec.CommandContext(ctx, s.ytdlpPath, args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("yt-dlp stdout pipe: %w", err)
	}
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("yt-dlp start failed (is it installed?): %w", err)
	}
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for scanner.Scan() {
		if progress != nil {
			progress(scanner.Text())
		}
	}
	if err := cmd.Wait(); err != nil {
		// yt-dlp may exit non-zero with --ignore-errors on partial failure;
		// still attempt to import whatever landed.
		if progress != nil {
			progress("yt-dlp finished with: " + err.Error())
		}
	}

	return s.importInfoJSONs(source, progress)
}

// ImportSearch downloads the best-matching YouTube result for a track and
// imports it with the given source tag. Instead of blindly taking the top
// search hit, it fetches metadata for the top few results and scores them
// against the wanted track (duration proximity, version keywords, official
// "- Topic" channels) so slowed/sped-up/live re-uploads lose to the real
// version. Returns the resulting track (possibly a pre-existing library row,
// deduped by path).
func (s *LibraryService) ImportSearch(ctx context.Context, want SpotifyTrack, source string, progress func(line string)) (*Track, error) {
	query := strings.TrimSpace(want.Query())
	if query == "" {
		return nil, fmt.Errorf("empty track")
	}
	cands, err := s.searchCandidates(ctx, query, 5)
	if err != nil {
		return nil, err
	}
	if len(cands) == 0 {
		return nil, fmt.Errorf("no YouTube result for %q", query)
	}
	best := pickBestCandidate(cands, want)
	if progress != nil {
		dur := ""
		if best.DurationSec > 0 {
			dur = fmt.Sprintf(" (%d:%02d)", best.DurationSec/60, best.DurationSec%60)
		}
		progress(fmt.Sprintf("matched: %s [%s]%s", best.Title, best.Channel, dur))
		if warn := matchWarning(best, want); warn != "" {
			progress("⚠ " + warn)
		}
	}
	tracks, err := s.importVia(ctx, "https://www.youtube.com/watch?v="+best.ID, source, progress)
	if err != nil {
		return nil, err
	}
	if len(tracks) == 0 {
		return nil, fmt.Errorf("download failed for %q", best.Title)
	}
	return &tracks[0], nil
}

// searchCandidate is one YouTube search result (metadata only, no download).
type searchCandidate struct {
	ID          string
	Title       string
	Channel     string
	DurationSec int
}

// searchCandidates fetches metadata for the top n YouTube search results.
func (s *LibraryService) searchCandidates(ctx context.Context, query string, n int) ([]searchCandidate, error) {
	ctx, cancel := context.WithTimeout(ctx, searchTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, s.ytdlpPath,
		"--flat-playlist", "--dump-json", "--no-download", "--no-warnings",
		fmt.Sprintf("ytsearch%d:%s", n, query))
	out, err := cmd.Output()
	if err != nil && len(out) == 0 {
		return nil, fmt.Errorf("yt-dlp search failed: %w", err)
	}
	var cands []searchCandidate
	sc := bufio.NewScanner(strings.NewReader(string(out)))
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		var e struct {
			ID       string  `json:"id"`
			Title    string  `json:"title"`
			Channel  string  `json:"channel"`
			Uploader string  `json:"uploader"`
			Duration float64 `json:"duration"`
		}
		if json.Unmarshal(sc.Bytes(), &e) != nil || e.ID == "" {
			continue
		}
		ch := e.Channel
		if ch == "" {
			ch = e.Uploader
		}
		cands = append(cands, searchCandidate{
			ID: e.ID, Title: e.Title, Channel: ch, DurationSec: int(e.Duration),
		})
	}
	return cands, nil
}

// versionWords are title markers of alternate versions we don't want unless
// the wanted track's own title contains the same word.
var versionWords = []string{
	"slowed", "sped", "speed up", "reverb", "nightcore", "8d",
	"live", "cover", "remix", "instrumental", "karaoke", "loop", "1 hour",
}

// pickBestCandidate scores candidates against the wanted track and returns
// the best. Duration proximity dominates when the wanted duration is known;
// otherwise version-keyword penalties and official-upload bonuses decide.
func pickBestCandidate(cands []searchCandidate, want SpotifyTrack) searchCandidate {
	wantTitle := strings.ToLower(want.Title)
	best, bestScore := cands[0], -1<<30
	for _, c := range cands {
		score := 0
		title := strings.ToLower(c.Title)
		if want.DurationSec > 0 && c.DurationSec > 0 {
			diff := c.DurationSec - want.DurationSec
			if diff < 0 {
				diff = -diff
			}
			switch {
			case diff <= 3:
				score += 100
			case diff > 20:
				score -= 100
			default:
				score -= diff * 3
			}
		}
		for _, w := range versionWords {
			if strings.Contains(title, w) && !strings.Contains(wantTitle, w) {
				score -= 40
			}
		}
		if strings.HasSuffix(c.Channel, " - Topic") {
			score += 30 // auto-generated label upload: the official audio
		}
		if strings.Contains(title, "official audio") || strings.Contains(title, "official video") {
			score += 15
		}
		if score > bestScore {
			best, bestScore = c, score
		}
	}
	return best
}

// matchWarning returns a non-empty message when the chosen candidate looks
// like it may be the wrong version, so the import log can flag it instead of
// failing silently.
func matchWarning(c searchCandidate, want SpotifyTrack) string {
	if want.DurationSec > 0 && c.DurationSec > 0 {
		diff := c.DurationSec - want.DurationSec
		if diff < 0 {
			diff = -diff
		}
		if diff > 5 {
			return fmt.Sprintf("closest match runs %d:%02d but Spotify lists %d:%02d — this may be a different version, verify it",
				c.DurationSec/60, c.DurationSec%60, want.DurationSec/60, want.DurationSec%60)
		}
		return ""
	}
	title, wantTitle := strings.ToLower(c.Title), strings.ToLower(want.Title)
	for _, w := range versionWords {
		if strings.Contains(title, w) && !strings.Contains(wantTitle, w) {
			return fmt.Sprintf("title contains %q — this may be a different version, verify it", w)
		}
	}
	return ""
}

// importInfoJSONs scans the media dir for *.info.json written by yt-dlp and
// resolves each to a track — inserting new ones, reusing existing rows (deduped
// by file path) — returned in playlist order (playlist_index ascending; entries
// without an index, i.e. single videos, come last in filename order).
func (s *LibraryService) importInfoJSONs(source string, progress func(string)) ([]Track, error) {
	type ytInfo struct {
		ID            string  `json:"id"`
		Title         string  `json:"title"`
		Artist        string  `json:"artist"`
		Uploader      string  `json:"uploader"`
		Duration      float64 `json:"duration"`
		PlaylistIndex *int    `json:"playlist_index"` // null for single videos
	}
	type entry struct {
		info     ytInfo
		jsonPath string
	}

	matches, _ := filepath.Glob(filepath.Join(s.mediaDir, "*.info.json"))
	var entries []entry
	for _, infoPath := range matches {
		raw, err := os.ReadFile(infoPath)
		if err != nil {
			continue
		}
		var info ytInfo
		if err := json.Unmarshal(raw, &info); err != nil {
			continue
		}
		entries = append(entries, entry{info: info, jsonPath: infoPath})
	}

	sort.SliceStable(entries, func(i, j int) bool {
		a, b := entries[i].info.PlaylistIndex, entries[j].info.PlaylistIndex
		switch {
		case a != nil && b != nil:
			return *a < *b
		case a != nil:
			return true
		case b != nil:
			return false
		default:
			return entries[i].jsonPath < entries[j].jsonPath
		}
	})

	var out []Track
	for _, e := range entries {
		info := e.info
		audioPath := filepath.Join(s.mediaDir, info.ID+".mp3")
		if _, err := os.Stat(audioPath); err != nil {
			_ = os.Remove(e.jsonPath)
			continue
		}
		if existing, _ := s.trackByPath(audioPath); existing != nil {
			out = append(out, *existing)
			if progress != nil {
				progress("already in library: " + existing.Title)
			}
			_ = os.Remove(e.jsonPath)
			continue
		}
		artist := info.Artist
		if artist == "" {
			artist = info.Uploader
		}
		dur := info.Duration
		if dur == 0 {
			dur = s.probeDuration(audioPath)
		}
		artPath := s.importArt(info.ID)
		if t, err := s.insertTrack(info.Title, artist, source, audioPath, dur, artPath); err == nil {
			out = append(out, *t)
			if progress != nil {
				progress("imported: " + info.Title)
			}
		}
		_ = os.Remove(e.jsonPath) // tidy: metadata is now in the DB
	}
	return out, nil
}

// trackByPath returns the track whose file_path matches, or nil if none does.
func (s *LibraryService) trackByPath(filePath string) (*Track, error) {
	var t Track
	err := s.db.QueryRow(`SELECT id, title, artist, source, file_path, duration_sec, art_path, added_at
		FROM tracks WHERE file_path = ?`, filePath).
		Scan(&t.ID, &t.Title, &t.Artist, &t.Source, &t.FilePath, &t.DurationSec, &t.ArtPath, &t.AddedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &t, nil
}

func (s *LibraryService) insertTrack(title, artist, source, filePath string, dur float64, artPath string) (*Track, error) {
	if title == "" {
		title = filepath.Base(filePath)
	}
	res, err := s.db.Exec(`INSERT INTO tracks (title, artist, source, file_path, duration_sec, art_path) VALUES (?, ?, ?, ?, ?, ?)`,
		title, artist, source, filePath, dur, artPath)
	if err != nil {
		return nil, err
	}
	id, _ := res.LastInsertId()
	return &Track{ID: id, Title: title, Artist: artist, Source: source, FilePath: filePath, DurationSec: dur, ArtPath: artPath, AddedAt: time.Now()}, nil
}

// importArt locates the thumbnail yt-dlp wrote for a video id, normalizes it to
// the canonical square cover art, and returns the art path ("" when no
// thumbnail landed or normalization failed). The raw thumbnail is removed
// either way.
func (s *LibraryService) importArt(videoID string) string {
	var raw string
	for _, ext := range []string{".png", ".webp", ".jpg", ".jpeg"} {
		cand := filepath.Join(s.mediaDir, videoID+ext)
		if _, err := os.Stat(cand); err == nil {
			raw = cand
			break
		}
	}
	if raw == "" {
		return ""
	}
	dest := filepath.Join(s.mediaDir, videoID+".art.png")
	err := s.normalizeArt(raw, dest)
	_ = os.Remove(raw)
	if err != nil {
		return ""
	}
	return dest
}

// normalizeArt center-crops an image to a square and scales it to the exact
// canonical size the live banner art swap requires (the encoder's image input
// must never change dimensions mid-stream).
func (s *LibraryService) normalizeArt(src, dest string) error {
	ctx, cancel := context.WithTimeout(context.Background(), artNormTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, s.ffmpegPath, "-y", "-hide_banner", "-loglevel", "error",
		"-i", src,
		"-vf", "crop='min(iw,ih)':'min(iw,ih)',scale=300:300",
		"-frames:v", "1", dest)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("art normalize failed: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// probeDuration returns the media duration in seconds using ffprobe, or 0 on error.
func (s *LibraryService) probeDuration(path string) float64 {
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, s.ffprobePath, "-v", "error",
		"-show_entries", "format=duration",
		"-of", "default=noprint_wrappers=1:nokey=1", path)
	out, err := cmd.Output()
	if err != nil {
		return 0
	}
	d, _ := strconv.ParseFloat(strings.TrimSpace(string(out)), 64)
	return d
}

// sanitize strips path-hostile characters from a filename base.
func sanitize(s string) string {
	repl := func(r rune) rune {
		switch r {
		case '/', '\\', ':', '*', '?', '"', '<', '>', '|':
			return '_'
		}
		return r
	}
	return strings.Map(repl, s)
}

// uniquePath appends -1, -2, ... if the path already exists.
func uniquePath(p string) string {
	if _, err := os.Stat(p); os.IsNotExist(err) {
		return p
	}
	ext := filepath.Ext(p)
	base := strings.TrimSuffix(p, ext)
	for i := 1; ; i++ {
		cand := fmt.Sprintf("%s-%d%s", base, i, ext)
		if _, err := os.Stat(cand); os.IsNotExist(err) {
			return cand
		}
	}
}
