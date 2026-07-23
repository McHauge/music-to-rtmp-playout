package services

import "database/sql"

// SettingsService reads/writes the single-row settings table.
type SettingsService struct {
	db *sql.DB
}

func NewSettingsService(db *sql.DB) *SettingsService { return &SettingsService{db: db} }

// Get returns the current settings row (always present — seeded at init).
func (s *SettingsService) Get() (Settings, error) {
	var st Settings
	err := s.db.QueryRow(`SELECT rtmp_url, stream_key, bg_image_path, video_fps, video_width, video_height, video_enabled, video_bitrate, audio_bitrate, now_overlay, viz_style, banner_box, theme FROM settings WHERE id = 1`).
		Scan(&st.RTMPURL, &st.StreamKey, &st.BgImagePath, &st.VideoFPS, &st.VideoWidth, &st.VideoHeight, &st.VideoEnabled, &st.VideoBitrate, &st.AudioBitrate, &st.NowOverlay, &st.VizStyle, &st.BannerBox, &st.Theme)
	return st, err
}

// Save persists the settings row.
func (s *SettingsService) Save(st Settings) error {
	_, err := s.db.Exec(`UPDATE settings SET rtmp_url=?, stream_key=?, bg_image_path=?, video_fps=?, video_width=?, video_height=?, video_enabled=?, video_bitrate=?, audio_bitrate=?, now_overlay=?, viz_style=?, banner_box=?, theme=? WHERE id = 1`,
		st.RTMPURL, st.StreamKey, st.BgImagePath, st.VideoFPS, st.VideoWidth, st.VideoHeight, st.VideoEnabled, st.VideoBitrate, st.AudioBitrate, st.NowOverlay, st.VizStyle, st.BannerBox, st.Theme)
	return err
}
