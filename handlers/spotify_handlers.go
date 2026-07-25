package handlers

import (
	"net/http"
	"strings"

	"music-to-rtmp-playout/services"

	"github.com/starfederation/datastar-go/datastar"
)

// resolveSpotifyTracks turns the Spotify import form inputs into a track
// list: a pasted track list wins over a playlist URL (the URL path needs API
// credentials, the paste path never does). Errors are reported through logf;
// an empty return means there is nothing to import (already logged).
func (app *App) resolveSpotifyTracks(r *http.Request, logf logFunc) []services.SpotifyTrack {
	spotURL, paste := r.FormValue("spotify_url"), r.FormValue("spotify_paste")
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
	tracks, err := app.Spotify.PlaylistTracks(r.Context(), id)
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

	pairs := app.resolveSpotifyTracks(r, logf)
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
	sse, logf, ok := app.beginBulkSSE(w, r, "Import failed: %v")
	if !ok {
		return
	}
	plID, ok := app.bulkPlaylistID(r, logf)
	if !ok {
		return
	}
	pairs := app.resolveSpotifyTracks(r, logf)
	if len(pairs) == 0 {
		return
	}
	appender := app.newFlowAppender(r, plID)

	added := 0
	for i, p := range pairs {
		logf("[%d/%d] Searching: %s", i+1, len(pairs), p.Label())
		// Silent filler tracks ("30 Seconds Of Silence") never get downloaded:
		// with auto-breaks on, our own breaks already provide the spacing;
		// otherwise the filler becomes a break of its own duration.
		if sec, ok := p.SilenceSec(); ok {
			if appender.withBreaks() {
				logf("  silence track — skipped (using your %ds breaks instead)", appender.breakSec)
				continue
			}
			if sec <= 0 {
				sec = services.DefaultBreakSec
			}
			appender.addBreak(sec)
			logf("  silence track — added as a %ds break (no download)", sec)
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
		appender.appendSong(track.ID)
		added++
	}

	logf("Done — added %d of %d track(s) to the rundown.", added, len(pairs))
	sse.PatchSignals([]byte(`{"spoturl":"","spotpaste":""}`))
	app.patchFlowBuilder(sse, plID)
}
