#!/bin/sh
# Downloads self-contained ffmpeg, ffprobe, and yt-dlp into ./bin so the app
# needs nothing installed on the host. Re-run to update the binaries.
#
#   ./scripts/fetch-tools.sh
set -e

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
BIN="$ROOT/bin"
mkdir -p "$BIN"

OS="$(uname -s)"
ARCH="$(uname -m)"

echo "==> yt-dlp"
curl -L --fail -o "$BIN/yt-dlp" \
    "https://github.com/yt-dlp/yt-dlp/releases/latest/download/yt-dlp"
chmod +x "$BIN/yt-dlp"

echo "==> ffmpeg + ffprobe"
case "$OS" in
  Linux)
    case "$ARCH" in
      x86_64|amd64) FF="linux64" ;;
      aarch64|arm64) FF="linuxarm64" ;;
      *) echo "Unsupported arch $ARCH"; exit 1 ;;
    esac
    # BtbN static builds (glibc, GitHub-hosted). Self-contained, no deps.
    url="https://github.com/BtbN/FFmpeg-Builds/releases/download/latest/ffmpeg-master-latest-${FF}-gpl.tar.xz"
    tmp="$(mktemp -d)"
    curl -L --fail -o "$tmp/ffmpeg.tar.xz" "$url"
    tar -xf "$tmp/ffmpeg.tar.xz" -C "$tmp"
    cp "$(find "$tmp" -type f -name ffmpeg | head -1)" "$BIN/ffmpeg"
    cp "$(find "$tmp" -type f -name ffprobe | head -1)" "$BIN/ffprobe"
    chmod +x "$BIN/ffmpeg" "$BIN/ffprobe"
    rm -rf "$tmp"
    ;;
  Darwin)
    # evermeet.cx publishes notarized static macOS builds (one zip per tool).
    for tool in ffmpeg ffprobe; do
      tmp="$(mktemp -d)"
      curl -L --fail -o "$tmp/$tool.zip" "https://evermeet.cx/ffmpeg/getrelease/$tool/zip"
      unzip -o "$tmp/$tool.zip" -d "$BIN" >/dev/null
      chmod +x "$BIN/$tool"
      rm -rf "$tmp"
    done
    ;;
  *)
    echo "Unsupported OS $OS — install ffmpeg/ffprobe manually into $BIN"
    exit 1
    ;;
esac

echo ""
echo "Done. Bundled tools in $BIN:"
ls -1 "$BIN" | grep -v '^.gitkeep$' | sed 's/^/  /'
