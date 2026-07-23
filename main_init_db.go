package main

import (
	"database/sql"
	"log"
	"strings"
)

// initDB creates the SQLite schema if it does not already exist. SQLite with
// the pure-Go modernc driver keeps the container single-binary (no CGO).
func initDB(db *sql.DB) error {
	// Pragmas: WAL for concurrent reads during playout, foreign keys on.
	if _, err := db.Exec(`PRAGMA journal_mode=WAL; PRAGMA foreign_keys=ON; PRAGMA busy_timeout=5000;`); err != nil {
		return err
	}

	stmts := []string{
		`CREATE TABLE IF NOT EXISTS users (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			username TEXT NOT NULL UNIQUE,
			password_hash TEXT NOT NULL,
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		)`,

		`CREATE TABLE IF NOT EXISTS tracks (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			title TEXT NOT NULL,
			artist TEXT NOT NULL DEFAULT '',
			source TEXT NOT NULL DEFAULT 'upload',   -- 'youtube' | 'upload' | 'spotify'
			file_path TEXT NOT NULL,
			duration_sec REAL NOT NULL DEFAULT 0,
			art_path TEXT NOT NULL DEFAULT '',        -- normalized square cover art (300x300 png)
			added_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		)`,

		`CREATE TABLE IF NOT EXISTS playlists (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			name TEXT NOT NULL,
			default_break_sec INTEGER NOT NULL DEFAULT 20,
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		)`,

		`CREATE TABLE IF NOT EXISTS flow_items (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			playlist_id INTEGER NOT NULL REFERENCES playlists(id) ON DELETE CASCADE,
			position INTEGER NOT NULL,
			type TEXT NOT NULL,                       -- 'song' | 'break' | 'gate'
			track_id INTEGER REFERENCES tracks(id) ON DELETE SET NULL,
			break_sec INTEGER NOT NULL DEFAULT 0,
			label TEXT NOT NULL DEFAULT '',
			auto_next INTEGER NOT NULL DEFAULT 1      -- bool: continue automatically after this item
		)`,
		`CREATE INDEX IF NOT EXISTS idx_flow_items_playlist ON flow_items(playlist_id, position)`,

		`CREATE TABLE IF NOT EXISTS soundboard_clips (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			name TEXT NOT NULL,
			file_path TEXT NOT NULL,
			pcm_path TEXT NOT NULL DEFAULT '',
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		)`,

		`CREATE TABLE IF NOT EXISTS settings (
			id INTEGER PRIMARY KEY CHECK (id = 1),
			rtmp_url TEXT NOT NULL DEFAULT '',
			stream_key TEXT NOT NULL DEFAULT '',
			bg_image_path TEXT NOT NULL DEFAULT '',
			video_fps INTEGER NOT NULL DEFAULT 10,
			video_width INTEGER NOT NULL DEFAULT 0,   -- 0 = backfilled from env config at startup
			video_height INTEGER NOT NULL DEFAULT 0,
			video_enabled INTEGER NOT NULL DEFAULT 1, -- bool: 0 = audio-only stream
			video_bitrate TEXT NOT NULL DEFAULT '500k', -- CBR; empty = auto (CRF)
			audio_bitrate TEXT NOT NULL DEFAULT '160k',
			now_overlay INTEGER NOT NULL DEFAULT 1,   -- bool: "now playing" overlay
			viz_style TEXT NOT NULL DEFAULT 'bars',   -- banner visualization: 'bars' | 'wave' | 'none'
			banner_box INTEGER NOT NULL DEFAULT 1,    -- bool: translucent box behind the banner
			theme TEXT NOT NULL DEFAULT 'teal'
		)`,
		`INSERT OR IGNORE INTO settings (id) VALUES (1)`,
	}

	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			return err
		}
	}

	// Additive migrations for databases created before these columns existed.
	if err := ensureColumn(db, "playlists", "default_break_sec INTEGER NOT NULL DEFAULT 20"); err != nil {
		return err
	}
	if err := ensureColumn(db, "settings", "video_width INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	if err := ensureColumn(db, "settings", "video_height INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	if err := ensureColumn(db, "settings", "video_enabled INTEGER NOT NULL DEFAULT 1"); err != nil {
		return err
	}
	if err := ensureColumn(db, "settings", "video_bitrate TEXT NOT NULL DEFAULT '500k'"); err != nil {
		return err
	}
	if err := ensureColumn(db, "settings", "now_overlay INTEGER NOT NULL DEFAULT 1"); err != nil {
		return err
	}
	if err := ensureColumn(db, "settings", "viz_style TEXT NOT NULL DEFAULT 'bars'"); err != nil {
		return err
	}
	if err := ensureColumn(db, "settings", "banner_box INTEGER NOT NULL DEFAULT 1"); err != nil {
		return err
	}
	if err := ensureColumn(db, "tracks", "art_path TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}

	log.Println("SQLite schema initialized")
	return nil
}

// ensureColumn adds a column to an existing table; a "duplicate column name"
// error means it is already there and is ignored.
func ensureColumn(db *sql.DB, table, columnDef string) error {
	_, err := db.Exec(`ALTER TABLE ` + table + ` ADD COLUMN ` + columnDef)
	if err != nil && strings.Contains(err.Error(), "duplicate column") {
		return nil
	}
	return err
}
