package handlers

import (
	"net/http"
	"strconv"
	"strings"

	"music-to-rtmp-playout/services"

	"github.com/starfederation/datastar-go/datastar"
)

// resolveSpotifyTracks turns the Spotify import form inputs into a track
// list: a pasted track list wins over a playlist URL (the URL path needs API
// credentials, the paste path never does). Errors are reported through logf;
// an empty return means there is nothing to import (already logged).
func (app *App) resolveSpotifyTracks(spotURL, paste string, logf func(format string, args ...any)) []services.SpotifyTrack {
	if strings.TrimSpace(paste) != "" {
		tracks := services.ParseTrackLines(paste)
		if len(tracks) == 0 {
			logf("No parsable lines — use one track per line, e.g.  Daft Punk - One More Time")
		}
		return tracks
	}
	if strings.TrimSpace(spotURL) == "" {
		logf("Paste a Spotify playlist URL or a track list first.")
		return nil
	}
	id, err := services.ParseSpotifyPlaylistID(spotURL)
	if err != nil {
		logf("%v", err)
		return nil
	}
	if !app.Spotify.Configured() {
		logf("Spotify API credentials are not set (SPOTIFY_CLIENT_ID / SPOTIFY_CLIENT_SECRET) — paste the track list instead.")
		return nil
	}
	tracks, err := app.Spotify.PlaylistTracks(id)
	if err != nil {
		logf("%v", err)
		return nil
	}
	if len(tracks) == 0 {
		logf("The playlist has no importable tracks.")
		return nil
	}
	logf("Found %d track(s) on the playlist.", len(tracks))
	return tracks
}

// ImportSpotify imports a Spotify playlist (via API or pasted track list)
// into the library: each track is matched to its best YouTube version and
// downloaded via yt-dlp. Progress streams to #spotify-log over SSE.
func (app *App) ImportSpotify(w http.ResponseWriter, r *http.Request) {
	// Parse the form before NewSSE flushes response headers (same constraint
	// as the bulk handlers).
	parseErr := r.ParseForm()

	sse := datastar.NewSSE(w, r)
	logf := rollingLogger(sse, "spotify-log")

	if parseErr != nil {
		logf("Import failed: %v", parseErr)
		return
	}

	pairs := app.resolveSpotifyTracks(r.FormValue("spotify_url"), r.FormValue("spotify_paste"), logf)
	if len(pairs) == 0 {
		return
	}

	ok, failed := 0, 0
	for i, p := range pairs {
		logf("[%d/%d] Searching: %s", i+1, len(pairs), p.Label())
		if _, ok := p.SilenceSec(); ok {
			logf("  silence track — skipped (not downloaded)")
			continue
		}
		if _, err := app.Library.ImportSearch(r.Context(), p, "spotify", func(line string) { logf("%s", line) }); err != nil {
			logf("  failed: %v", err)
			failed++
			continue
		}
		ok++
	}
	logf("Done — %d imported, %d failed of %d track(s).", ok, failed, len(pairs))

	app.patchTrackList(sse)
	sse.PatchSignals([]byte(`{"spoturl":"","spotpaste":""}`))
}

// ImportSpotifyToFlow imports a Spotify playlist (via API or pasted track
// list) and appends each downloaded track to the show's rundown with the same
// break/auto-next options as the other bulk-add modes. Progress streams to
// #bulk-log over SSE.
func (app *App) ImportSpotifyToFlow(w http.ResponseWriter, r *http.Request) {
	// Same ordering constraint as BulkUploadToFlow: the multipart form (shared
	// with the file-upload button) must be parsed before NewSSE flushes headers.
	r.Body = http.MaxBytesReader(w, r.Body, app.Cfg.MaxUploadSize)
	parseErr := r.ParseMultipartForm(32 << 20)

	sse := datastar.NewSSE(w, r)
	logf := bulkLogger(sse)

	if parseErr != nil {
		logf("Import failed: %v", parseErr)
		return
	}

	plID, _ := strconv.ParseInt(r.FormValue("playlist_id"), 10, 64)
	pl, err := app.Flow.GetPlaylist(plID)
	if err != nil || pl == nil {
		logf("Unknown show.")
		return
	}

	pairs := app.resolveSpotifyTracks(r.FormValue("spotify_url"), r.FormValue("spotify_paste"), logf)
	if len(pairs) == 0 {
		return
	}

	breakSec, _ := strconv.Atoi(r.FormValue("break_sec"))
	if breakSec < 0 {
		breakSec = 0
	}
	_ = app.Flow.SetDefaultBreakSec(plID, breakSec)

	withBreaks := breakSec > 0
	// Manual start: playback parks before each new song. With breaks the hold
	// sits on the break (song flows into its break, then waits); without
	// breaks it sits on the song itself.
	manual := r.FormValue("start_mode") == "manual"
	songAuto := !(manual && !withBreaks)

	// Breaks go between consecutive songs — including between the current last
	// item (if it is a song) and the first new one — but never at the end.
	needBreak := false
	if items, err := app.Flow.GetItems(plID); err == nil && len(items) > 0 {
		needBreak = items[len(items)-1].Type == services.ItemSong
	}

	added := 0
	for i, p := range pairs {
		logf("[%d/%d] Searching: %s", i+1, len(pairs), p.Label())
		// Silent filler tracks ("30 Seconds Of Silence") never get downloaded:
		// with auto-breaks on, our own breaks already provide the spacing;
		// otherwise the filler becomes a break of its own duration.
		if sec, ok := p.SilenceSec(); ok {
			if withBreaks {
				logf("  silence track — skipped (using your %ds breaks instead)", breakSec)
				continue
			}
			if sec <= 0 {
				sec = 20
			}
			_, _ = app.Flow.AddItem(plID, services.ItemBreak, nil, sec, "", !manual)
			logf("  silence track — added as a %ds break (no download)", sec)
			needBreak = false
			continue
		}
		track, err := app.Library.ImportSearch(r.Context(), p, "spotify", func(line string) { logf("%s", line) })
		if err != nil {
			logf("  skipped: %v", err)
			continue
		}
		if track.DurationSec <= 0 {
			logf("  warning: no duration for %q", track.Title)
		}
		if needBreak && withBreaks {
			_, _ = app.Flow.AddItem(plID, services.ItemBreak, nil, breakSec, "", !manual)
		}
		tid := track.ID
		_, _ = app.Flow.AddItem(plID, services.ItemSong, &tid, 0, "", songAuto)
		needBreak = true
		added++
	}

	logf("Done — added %d of %d track(s) to the rundown.", added, len(pairs))
	sse.PatchSignals([]byte(`{"spoturl":"","spotpaste":""}`))
	app.patchFlowBuilder(sse, plID)
}
