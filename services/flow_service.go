package services

import (
	"database/sql"
	"fmt"
)

// FlowService manages playlists ("shows") and their ordered flow items.
type FlowService struct {
	db *sql.DB
}

func NewFlowService(db *sql.DB) *FlowService { return &FlowService{db: db} }

// ListPlaylists returns all shows, newest first.
func (s *FlowService) ListPlaylists() ([]Playlist, error) {
	rows, err := s.db.Query(`SELECT id, name, default_break_sec, created_at, updated_at FROM playlists ORDER BY updated_at DESC, id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Playlist
	for rows.Next() {
		var p Playlist
		if err := rows.Scan(&p.ID, &p.Name, &p.DefaultBreakSec, &p.CreatedAt, &p.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// GetPlaylist returns one show by id.
func (s *FlowService) GetPlaylist(id int64) (*Playlist, error) {
	var p Playlist
	err := s.db.QueryRow(`SELECT id, name, default_break_sec, created_at, updated_at FROM playlists WHERE id = ?`, id).
		Scan(&p.ID, &p.Name, &p.DefaultBreakSec, &p.CreatedAt, &p.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

// CreatePlaylist makes a new empty show.
func (s *FlowService) CreatePlaylist(name string) (int64, error) {
	if name == "" {
		name = "Untitled show"
	}
	res, err := s.db.Exec(`INSERT INTO playlists (name) VALUES (?)`, name)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// RenamePlaylist updates a show's name.
func (s *FlowService) RenamePlaylist(id int64, name string) error {
	_, err := s.db.Exec(`UPDATE playlists SET name = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ?`, name, id)
	return err
}

// SetDefaultBreakSec persists a show's preferred spacing between songs.
func (s *FlowService) SetDefaultBreakSec(id int64, sec int) error {
	if sec < 0 {
		sec = 0
	}
	_, err := s.db.Exec(`UPDATE playlists SET default_break_sec = ? WHERE id = ?`, sec, id)
	return err
}

// DeletePlaylist removes a show and its items (cascade).
func (s *FlowService) DeletePlaylist(id int64) error {
	_, err := s.db.Exec(`DELETE FROM playlists WHERE id = ?`, id)
	return err
}

// GetItems returns a show's flow items in order, with song tracks joined in.
func (s *FlowService) GetItems(playlistID int64) ([]FlowItem, error) {
	rows, err := s.db.Query(`
		SELECT fi.id, fi.playlist_id, fi.position, fi.type, fi.track_id, fi.break_sec, fi.label, fi.auto_next,
		       t.id, t.title, t.artist, t.source, t.file_path, t.duration_sec, t.art_path
		FROM flow_items fi
		LEFT JOIN tracks t ON t.id = fi.track_id
		WHERE fi.playlist_id = ?
		ORDER BY fi.position ASC, fi.id ASC`, playlistID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []FlowItem
	for rows.Next() {
		var it FlowItem
		var trackID sql.NullInt64
		var autoNext int
		var tID sql.NullInt64
		var tTitle, tArtist, tSource, tPath, tArt sql.NullString
		var tDur sql.NullFloat64
		if err := rows.Scan(&it.ID, &it.PlaylistID, &it.Position, &it.Type, &trackID, &it.BreakSec, &it.Label, &autoNext,
			&tID, &tTitle, &tArtist, &tSource, &tPath, &tDur, &tArt); err != nil {
			return nil, err
		}
		it.AutoNext = autoNext != 0
		if trackID.Valid {
			id := trackID.Int64
			it.TrackID = &id
		}
		if tID.Valid {
			it.Track = &Track{
				ID: tID.Int64, Title: tTitle.String, Artist: tArtist.String,
				Source: tSource.String, FilePath: tPath.String, DurationSec: tDur.Float64,
				ArtPath: tArt.String,
			}
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// AddItem appends an item to the end of a show's flow.
func (s *FlowService) AddItem(playlistID int64, itemType string, trackID *int64, breakSec int, label string, autoNext bool) (int64, error) {
	// Not ignorable: on a read failure nextPos stays 0 and the new item silently
	// lands at the *front* of the rundown instead of the end.
	var nextPos int
	if err := s.db.QueryRow(`SELECT COALESCE(MAX(position), -1) + 1 FROM flow_items WHERE playlist_id = ?`, playlistID).Scan(&nextPos); err != nil {
		return 0, fmt.Errorf("next flow position: %w", err)
	}
	an := 0
	if autoNext {
		an = 1
	}
	res, err := s.db.Exec(`INSERT INTO flow_items (playlist_id, position, type, track_id, break_sec, label, auto_next)
		VALUES (?, ?, ?, ?, ?, ?, ?)`, playlistID, nextPos, itemType, trackID, breakSec, label, an)
	if err != nil {
		return 0, err
	}
	s.touch(playlistID)
	return res.LastInsertId()
}

// DeleteItem removes one flow item and compacts positions.
func (s *FlowService) DeleteItem(playlistID, itemID int64) error {
	if _, err := s.db.Exec(`DELETE FROM flow_items WHERE id = ? AND playlist_id = ?`, itemID, playlistID); err != nil {
		return err
	}
	s.touch(playlistID)
	return nil
}

// Reorder sets a new ordering from a list of item ids. Items not present keep
// their relative order after the listed ones.
func (s *FlowService) Reorder(playlistID int64, orderedIDs []int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for pos, id := range orderedIDs {
		if _, err := tx.Exec(`UPDATE flow_items SET position = ? WHERE id = ? AND playlist_id = ?`, pos, id, playlistID); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.touch(playlistID)
	return nil
}

// SetAutoNext toggles auto-continue for an item.
func (s *FlowService) SetAutoNext(itemID int64, autoNext bool) error {
	an := 0
	if autoNext {
		an = 1
	}
	_, err := s.db.Exec(`UPDATE flow_items SET auto_next = ? WHERE id = ?`, an, itemID)
	return err
}

// SetBreakSec updates the length of a break item.
func (s *FlowService) SetBreakSec(itemID int64, sec int) error {
	_, err := s.db.Exec(`UPDATE flow_items SET break_sec = ? WHERE id = ? AND type = 'break'`, sec, itemID)
	return err
}

// EstimateRuntimeSec sums song durations and break lengths for a show. Gates
// have no fixed duration (they hold indefinitely) so they contribute nothing.
func (s *FlowService) EstimateRuntimeSec(playlistID int64) (float64, error) {
	items, err := s.GetItems(playlistID)
	if err != nil {
		return 0, err
	}
	return SumRuntimeSec(items), nil
}

// SumRuntimeSec totals the known length of a flow: songs (track duration) plus
// breaks; gates/holds have no fixed length and count as zero.
func SumRuntimeSec(items []FlowItem) float64 {
	var total float64
	for _, it := range items {
		switch it.Type {
		case ItemSong:
			if it.Track != nil {
				total += it.Track.DurationSec
			}
		case ItemBreak:
			total += float64(it.BreakSec)
		}
	}
	return total
}

func (s *FlowService) touch(playlistID int64) {
	_, _ = s.db.Exec(`UPDATE playlists SET updated_at = CURRENT_TIMESTAMP WHERE id = ?`, playlistID)
}

// Describe returns a short human label for a flow item (used in status/UI).
func Describe(it FlowItem) string {
	switch it.Type {
	case ItemSong:
		if it.Track != nil {
			return it.Track.Display()
		}
		return "(missing track)"
	case ItemBreak:
		return fmt.Sprintf("Break — %ds", it.BreakSec)
	case ItemGate:
		if it.Label != "" {
			return "Hold: " + it.Label
		}
		return "Manual hold"
	}
	return it.Type
}
