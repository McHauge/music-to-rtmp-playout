package services

import (
	"database/sql"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Canonical PCM format used everywhere in the playout pipeline.
const (
	SampleRate     = 48000
	Channels       = 2
	BytesPerSample = 2 // s16le
)

// SoundboardService manages uploaded clips. On upload each clip is pre-decoded
// to raw 48k/stereo/s16le PCM so triggering during a live show is instant
// (no decode latency on the audio thread).
type SoundboardService struct {
	db         *sql.DB
	dir        string
	ffmpegPath string
}

func NewSoundboardService(db *sql.DB, dir, ffmpegPath string) *SoundboardService {
	return &SoundboardService{db: db, dir: dir, ffmpegPath: ffmpegPath}
}

// List returns all clips, newest first.
func (s *SoundboardService) List() ([]SoundboardClip, error) {
	rows, err := s.db.Query(`SELECT id, name, file_path, pcm_path, created_at FROM soundboard_clips ORDER BY created_at DESC, id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SoundboardClip
	for rows.Next() {
		var c SoundboardClip
		if err := rows.Scan(&c.ID, &c.Name, &c.FilePath, &c.PCMPath, &c.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// Get returns one clip by id.
func (s *SoundboardService) Get(id int64) (*SoundboardClip, error) {
	var c SoundboardClip
	err := s.db.QueryRow(`SELECT id, name, file_path, pcm_path, created_at FROM soundboard_clips WHERE id = ?`, id).
		Scan(&c.ID, &c.Name, &c.FilePath, &c.PCMPath, &c.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// Add saves an uploaded clip, decodes it to canonical PCM, and inserts a row.
func (s *SoundboardService) Add(origName string, src io.Reader) (*SoundboardClip, error) {
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return nil, err
	}
	ext := filepath.Ext(origName)
	base := sanitize(strings.TrimSuffix(filepath.Base(origName), ext))
	if base == "" {
		base = "clip"
	}
	dest := uniquePath(filepath.Join(s.dir, base+ext))

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

	pcmPath := strings.TrimSuffix(dest, ext) + ".pcm"
	if err := s.decodeToPCM(dest, pcmPath); err != nil {
		_ = os.Remove(dest)
		return nil, err
	}

	res, err := s.db.Exec(`INSERT INTO soundboard_clips (name, file_path, pcm_path) VALUES (?, ?, ?)`, base, dest, pcmPath)
	if err != nil {
		return nil, err
	}
	id, _ := res.LastInsertId()
	return &SoundboardClip{ID: id, Name: base, FilePath: dest, PCMPath: pcmPath, CreatedAt: time.Now()}, nil
}

// Delete removes the row plus the source and PCM files.
func (s *SoundboardService) Delete(id int64) error {
	c, err := s.Get(id)
	if err != nil {
		return err
	}
	if c == nil {
		return nil
	}
	if _, err := s.db.Exec(`DELETE FROM soundboard_clips WHERE id = ?`, id); err != nil {
		return err
	}
	_ = os.Remove(c.FilePath)
	_ = os.Remove(c.PCMPath)
	return nil
}

// decodeToPCM converts any input to canonical 48k/stereo/s16le raw PCM.
func (s *SoundboardService) decodeToPCM(in, out string) error {
	cmd := exec.Command(s.ffmpegPath, "-hide_banner", "-loglevel", "error",
		"-i", in,
		"-ar", "48000", "-ac", "2", "-f", "s16le",
		"-y", out)
	return cmd.Run()
}
