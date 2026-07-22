package services

import (
	"bufio"
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

// LibraryService manages the track library: uploads, yt-dlp imports, metadata.
type LibraryService struct {
	db          *sql.DB
	mediaDir    string
	ffprobePath string
	ytdlpPath   string
}

func NewLibraryService(db *sql.DB, mediaDir, ffprobePath, ytdlpPath string) *LibraryService {
	return &LibraryService{db: db, mediaDir: mediaDir, ffprobePath: ffprobePath, ytdlpPath: ytdlpPath}
}

// ListTracks returns all tracks, most recently added first.
func (s *LibraryService) ListTracks() ([]Track, error) {
	rows, err := s.db.Query(`SELECT id, title, artist, source, file_path, duration_sec, added_at FROM tracks ORDER BY added_at DESC, id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Track
	for rows.Next() {
		var t Track
		if err := rows.Scan(&t.ID, &t.Title, &t.Artist, &t.Source, &t.FilePath, &t.DurationSec, &t.AddedAt); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// GetTrack returns one track by id.
func (s *LibraryService) GetTrack(id int64) (*Track, error) {
	var t Track
	err := s.db.QueryRow(`SELECT id, title, artist, source, file_path, duration_sec, added_at FROM tracks WHERE id = ?`, id).
		Scan(&t.ID, &t.Title, &t.Artist, &t.Source, &t.FilePath, &t.DurationSec, &t.AddedAt)
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
	return s.insertTrack(base, "", "upload", dest, dur)
}

// ImportYouTube downloads audio (and a playlist's worth, if the URL is a
// playlist) via yt-dlp, then imports each resulting file. progress, if
// non-nil, receives yt-dlp's stdout/stderr lines for live UI feedback.
// Returns the tracks the URL resolved to in playlist order, including ones
// that were already in the library.
func (s *LibraryService) ImportYouTube(url string, progress func(line string)) ([]Track, error) {
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
		"--ignore-errors",
		"-o", outTmpl,
		url,
	}
	cmd := exec.Command(s.ytdlpPath, args...)
	stdout, _ := cmd.StdoutPipe()
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

	return s.importInfoJSONs(progress)
}

// importInfoJSONs scans the media dir for *.info.json written by yt-dlp and
// resolves each to a track — inserting new ones, reusing existing rows (deduped
// by file path) — returned in playlist order (playlist_index ascending; entries
// without an index, i.e. single videos, come last in filename order).
func (s *LibraryService) importInfoJSONs(progress func(string)) ([]Track, error) {
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
		if t, err := s.insertTrack(info.Title, artist, "youtube", audioPath, dur); err == nil {
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
	err := s.db.QueryRow(`SELECT id, title, artist, source, file_path, duration_sec, added_at
		FROM tracks WHERE file_path = ?`, filePath).
		Scan(&t.ID, &t.Title, &t.Artist, &t.Source, &t.FilePath, &t.DurationSec, &t.AddedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &t, nil
}

func (s *LibraryService) insertTrack(title, artist, source, filePath string, dur float64) (*Track, error) {
	if title == "" {
		title = filepath.Base(filePath)
	}
	res, err := s.db.Exec(`INSERT INTO tracks (title, artist, source, file_path, duration_sec) VALUES (?, ?, ?, ?, ?)`,
		title, artist, source, filePath, dur)
	if err != nil {
		return nil, err
	}
	id, _ := res.LastInsertId()
	return &Track{ID: id, Title: title, Artist: artist, Source: source, FilePath: filePath, DurationSec: dur, AddedAt: time.Now()}, nil
}

// probeDuration returns the media duration in seconds using ffprobe, or 0 on error.
func (s *LibraryService) probeDuration(path string) float64 {
	cmd := exec.Command(s.ffprobePath, "-v", "error",
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
