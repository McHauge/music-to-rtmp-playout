# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

A self-hosted Go web app that streams a curated flow of songs as a live RTMP "radio" broadcast. Stack: Go + Gorilla mux + Datastar (SSE-driven hypermedia UI) + pure-Go SQLite (modernc, no CGO), driving FFmpeg/yt-dlp subprocesses, packaged as a single Docker container with a bundled MediaMTX relay. See `README.md` for the user-facing feature overview.

## Commands

```bash
# Fetch self-contained ffmpeg/ffprobe/yt-dlp into ./bin (run once; not on PATH)
pwsh ./scripts/fetch-tools.ps1          # Windows
./scripts/fetch-tools.sh                # Linux/macOS

# Build & run locally (Go 1.26+). Auth env vars are required at first run.
go build -o playout . && \
  SESSION_SECRET=dev ADMIN_USERNAME=admin ADMIN_PASSWORD=changeme ./playout
# → http://localhost:8080

go build ./...     # compile-check everything
go vet ./...       # static checks
go test ./...      # unit tests (mixer, ring buffer, player state machine, encoder backoff)
gofmt -w .         # format

# Dev container (app + MediaMTX relay, `dev` image target): web 8080, RTMP 1935, HLS 8888
docker compose up --build
# Production image (default target, no relay — pushes to an external ingest):
docker build -t playout .
```

Test coverage is light — pure/logic-level unit tests live in `services/playout/*_test.go` (the mixer, ring buffer, player flow state machine, and encoder reconnect backoff); there is no end-to-end/integration suite. A live stream also needs an RTMP target — either the bundled MediaMTX (Docker) or any RTMP server pointed at by `RTMP_URL`.

## Architecture

### Request → render flow (Datastar, not a JSON SPA)
The UI is server-rendered Go `html/template` patched live over SSE; there is no client framework or JSON API. Handlers return HTML fragments, not JSON.
- `main.go` wires config → DB → services → `handlers.App` (the DI container, `handlers/app.go`) → Gorilla mux routes. All routes are auth-gated via `app.RequirePage` (redirects to login) or `app.RequireAuth` (API).
- Templates load once at startup (`handlers/template.go`, `LoadTemplates`) from `templates/{themes,partials,pages}/*.gohtml` into one set. Handlers call `Tmpl.Render(name, data)` to produce a fragment string.
- Control endpoints (`/api/stream/*`, etc.) open a `datastar.NewSSE`, mutate state, then `PatchElements` the re-rendered partial back. `StreamStatus` is a long-lived SSE that `Subscribe()`s to the engine and re-patches the status panel on every status change.

### The playout engine (`services/playout/`) — the core of the app
One `Engine` owns a single live show. **All playback state lives in one `run` goroutine** (`engine.go`); control (skip/play/stop) flows in over a buffered `cmd` channel, status flows out via a snapshot + subscriber fan-out (`Subscribe`/`broadcast`). This single-owner design is why `player` needs no locks.

The audio pipeline is fixed-format **48 kHz / stereo / s16le** PCM end to end (constants in `mix.go`; soundboard PCM cache must match):
- `encoder.go` (+ `encoder_filter.go` filtergraph/arg building, `encoder_nvenc.go` codec detection) — one **persistent** ffmpeg for the whole show. Reads PCM on stdin, muxes with a looping background image + a `drawtext` now-playing overlay (text/art updated in place via `overwriteInPlace`; the banner fade alpha is driven at runtime over ZMQ, not a per-frame mask file), pushes FLV to RTMP. **Audio is the master clock** (`-af aresample=async=1`, CFR video, `-shortest`) to bound A/V drift. Closing stdin (audio EOF) ends the stream cleanly. If ffmpeg dies mid-show (e.g. the RTMP relay drops), `engine.run` **auto-restarts the encoder** with backoff rather than ending the show (`reconnectEncoder`); the pacing loop emits silence across the gap.
- `engine.run` paces real time: every 5ms tick it produces audio up to `elapsed + lead`, so the encoder **never starves** — gaps (breaks/holds) emit silence rather than stopping the ffmpeg process, keeping the RTMP connection alive across the whole show.
- `player.go` — position within the flow. Songs spawn a `decoder`; breaks count down silence; gates set `holding`. `AutoNext` decides auto-advance vs. park-and-wait. Owned solely by `run`.
- `decoder.go` + `ringbuffer.go` — a short-lived ffmpeg normalizes one song to canonical PCM into a back-pressured ring buffer, decoupling decode jitter from the mix tick.
- `mix.go` (`voiceMixer`) — sums triggered soundboard clips on top of the program chunk (int32 sum, hard-clip to int16). Concurrent-safe; the only engine state touched outside `run`.

### Data & services (`services/`)
SQLite schema is created in code at startup (`main_init_db.go`, `initDB`) — WAL mode, `SetMaxOpenConns(1)` to serialize writes. Tables: `users`, `tracks`, `playlists`, `flow_items` (type = `song|break|gate`), `soundboard_clips`, single-row `settings`. Each service (`*_service.go`) wraps the `*sql.DB` for one domain. Models and the flow-item type constants live in `models.go`. Settings are seeded from config env defaults on first run and are then editable in the UI; `Settings.FullRTMPURL()` joins base URL + stream key.

Spotify playlist import (`spotify_service.go`) is metadata-only: `SpotifyService` (client-credentials flow, stdlib HTTP, cached app token; `SPOTIFY_CLIENT_ID/SECRET` env-only) or a pasted "Artist - Title" list resolves to `SpotifyTrack` tuples, then `LibraryService.ImportSearch` scores the top YouTube search results (Spotify duration proximity, version-keyword penalties, "- Topic" channel bonus) and downloads the winner through the normal yt-dlp pipeline — yt-dlp stays the only downloader.

### External tools & config
`config/config.go` `resolveTool` locates each binary in order: `*_PATH` env → `BIN_DIR` (`./bin`) → bare name on PATH. This is what makes the app self-contained — tools are bundled, not assumed installed. All config has env defaults (`.env` via godotenv); the RTMP target, background, encoder params, and theme are also DB-backed and editable in Settings.

## Conventions

- **Handlers return HTML, never JSON** — render a `templates/partials/*.gohtml` fragment and `PatchElements` it over SSE. Add a template func in `handlers/template.go` rather than formatting in the handler. The one exception is `handlers/debug_handlers.go`, which serves JSON on purpose for machine consumption and is registered only when `PLAYOUT_DIAG` is set.
- **Don't touch engine playback state from handlers** — go through `Engine` methods (`Start/Stop/Skip/Play/TriggerClip`), which marshal onto the `run` goroutine via channels.
- **Audio format is invariant** — 48k/stereo/s16le. Any new PCM source (soundboard, new item type) must produce this exact format or the mix math and encoder break.
- The bundled Linux ffmpeg/yt-dlp are vendored into the Docker image at build time (`Dockerfile` tools stage). The Dockerfile has two final targets: the default (production) image is app-only; the `dev` target (used by docker compose) adds MediaMTX and runs it alongside the app (`docker-entrypoint.sh`).
