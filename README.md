# Music → RTMP Playout

A self-hosted web app that turns a curated set of songs into a controllable
private "radio" broadcast over RTMP — built for small online events. Import
songs, arrange a playout **flow** (songs, timed breaks, manual holds), then
stream it live with skip / hold / resume control and a soundboard that mixes
clips on top.

Go + Datastar (SSE) + SQLite, packaged as a single Docker container with
FFmpeg, yt-dlp, and a bundled MediaMTX relay.

## Features

- **Library** — import audio from a YouTube video/playlist URL (yt-dlp) or
  upload files directly. Edit title/artist, delete.
- **Flow builder** — order **songs**, fixed-length silent **breaks**, and
  **manual holds** (gates that pause playout until you press Play). Per-item
  "auto-next vs. hold after" toggle. Live runtime estimate.
- **Stream console** — start/stop the RTMP stream; the connection stays live
  across songs, breaks, and holds. Skip, hold/resume, now-playing + next-up,
  all updated live over SSE.
- **Soundboard** — upload clips (pre-decoded to PCM) and trigger them to mix on
  top of the live program audio.
- **Settings** — RTMP URL/key, background image, encoder FPS/bitrate, theme.

## How the playout engine works

One persistent FFmpeg **encoder** muxes a continuous 48 kHz/stereo PCM stream
(fed from Go) with a looping background image + a now-playing text overlay, and
pushes FLV to RTMP. A Go **mixer** always produces audio at real time — pulling
decoded song samples from a ring buffer, emitting silence during breaks/holds
(so the RTMP connection never starves), and summing soundboard clips on top.
The encoder targets a local **MediaMTX** relay that forwards to your real
upstream and reconnects silently on blips, so the program stream never drops.

Audio is the master clock (`aresample=async=1` + CFR video) to bound A/V drift
over long shows.

## Run with Docker

```bash
cp .env.example .env       # set SESSION_SECRET, ADMIN_USERNAME/PASSWORD, etc.
docker compose up --build
```

Open http://localhost:8080. Ports: `8080` (web UI), `1935` (RTMP), `8888` (HLS).

The bundled MediaMTX accepts the encoder's feed at
`rtmp://localhost:1935/live/show`. Viewers can pull from the relay directly
(`rtmp://<host>:1935/live/show` or HLS at `http://<host>:8888/live/show`), or
configure `mediamtx.yml` (`runOnReady`) to forward to an external RTMP ingest.

## Run locally (dev)

Requires Go 1.26+. **ffmpeg, ffprobe, and yt-dlp don't need to be installed** —
fetch self-contained copies into `./bin` once:

```bash
# Windows
pwsh ./scripts/fetch-tools.ps1
# Linux / macOS
./scripts/fetch-tools.sh
```

The app resolves tools from `./bin` first (set via `BIN_DIR`), then falls back
to anything on PATH. For a live stream you also need an RTMP target (run
MediaMTX, or point `RTMP_URL` at any RTMP server).

```bash
go build -o playout . && \
SESSION_SECRET=dev ADMIN_USERNAME=admin ADMIN_PASSWORD=changeme ./playout
```

Then visit http://localhost:8080 and sign in. Configure the RTMP target in
**Settings**, build a show in **Flow**, and go live from **Stream**.

## Configuration

All settings have env defaults (see [.env.example](.env.example)); the
RTMP target, background, encoder params, and theme are also editable in the UI.

## Self-contained tooling

ffmpeg, ffprobe, and yt-dlp are bundled, not assumed to be on the host:

- **Local:** `scripts/fetch-tools.{ps1,sh}` download them into `./bin`
  (gyan.dev on Windows, BtbN static on Linux, evermeet on macOS; yt-dlp
  standalone). Re-run to update.
- **Docker:** the image vendors the same binaries into `/app/bin` at build time
  (no reliance on distro package repos for the media tools).

Resolution order per tool: `*_PATH` env override → `BIN_DIR` (`./bin`) → PATH.

## Notes

- A blank background path makes the encoder render a solid color instead of an
  image.
- Re-run the fetch script (or rebuild the image) periodically to update yt-dlp
  as YouTube changes.
