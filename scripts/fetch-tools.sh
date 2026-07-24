#!/bin/sh
# Downloads self-contained ffmpeg, ffprobe, and yt-dlp into ./bin (plus the
# Datastar client bundle into static/vendor) so the app needs nothing installed
# on the host. Re-run to update the binaries.
#
#   ./scripts/fetch-tools.sh
set -e

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
BIN="$ROOT/bin"
mkdir -p "$BIN"

# Datastar client bundle. MUST match the datastar-go SDK in go.mod (v1.x).
# Datastar parses keyed attributes with a colon (data-on:click), so the
# templates and this bundle version have to stay in lockstep.
DATASTAR_VERSION="v1.0.2"
mkdir -p "$ROOT/static/vendor"
echo "==> datastar $DATASTAR_VERSION"
curl -L --fail -o "$ROOT/static/vendor/datastar.js" \
    "https://cdn.jsdelivr.net/gh/starfederation/datastar@${DATASTAR_VERSION}/bundles/datastar.js"

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
    # Pinned to the n7.1 release branch (NOT master): master needs NVIDIA driver
    # >= 610 for h264_nvenc, but Pascal GPUs (e.g. Quadro P2000) are EOL at the
    # 580 driver branch, so master's NVENC can't run there. n7.1 needs ~550+.
    # Note the trailing "-7.1" in the asset name (master has no such suffix).
    url="https://github.com/BtbN/FFmpeg-Builds/releases/download/latest/ffmpeg-n7.1-latest-${FF}-gpl-7.1.tar.xz"
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
