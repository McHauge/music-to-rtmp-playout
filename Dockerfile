# ---- Build stage ----
FROM golang:1.26-bookworm AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# Pure-Go SQLite (modernc) means CGO can stay off — static binary.
RUN CGO_ENABLED=0 GOOS=linux go build -o playout .

# ---- Tools stage: vendor ffmpeg/ffprobe/yt-dlp/mediamtx into /tools ----
FROM debian:bookworm-slim AS tools
ARG MEDIAMTX_VERSION=v1.9.3
ARG TARGETARCH=amd64
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates curl xz-utils tar \
    && rm -rf /var/lib/apt/lists/*
WORKDIR /tools
RUN set -eux; \
    case "$TARGETARCH" in \
      amd64) FF=linux64; MM=amd64 ;; \
      arm64) FF=linuxarm64; MM=arm64v8 ;; \
      *) echo "unsupported arch $TARGETARCH"; exit 1 ;; \
    esac; \
    # yt-dlp (standalone)
    curl -L --fail -o yt-dlp https://github.com/yt-dlp/yt-dlp/releases/latest/download/yt-dlp; \
    chmod +x yt-dlp; \
    # ffmpeg + ffprobe (BtbN glibc static)
    curl -L --fail -o ffmpeg.tar.xz "https://github.com/BtbN/FFmpeg-Builds/releases/download/latest/ffmpeg-master-latest-${FF}-gpl.tar.xz"; \
    tar -xf ffmpeg.tar.xz; \
    cp "$(find . -type f -name ffmpeg | head -1)" ffmpeg; \
    cp "$(find . -type f -name ffprobe | head -1)" ffprobe; \
    chmod +x ffmpeg ffprobe; \
    # MediaMTX relay
    curl -L --fail -o mediamtx.tar.gz "https://github.com/bluenviron/mediamtx/releases/download/${MEDIAMTX_VERSION}/mediamtx_${MEDIAMTX_VERSION}_linux_${MM}.tar.gz"; \
    tar -xzf mediamtx.tar.gz mediamtx; \
    rm -f ffmpeg.tar.xz mediamtx.tar.gz; \
    rm -rf ffmpeg-*

# ---- Runtime stage ----
FROM debian:bookworm-slim
WORKDIR /app

# Only runtime needs: fonts for drawtext, tini for reaping decoder zombies,
# ca-certs + python for yt-dlp. No media tools from apt — they are vendored.
RUN apt-get update && apt-get install -y --no-install-recommends \
        ca-certificates \
        tini \
        python3 \
        fontconfig \
        fonts-dejavu-core \
        fonts-noto-core \
    && fc-cache -f \
    && rm -rf /var/lib/apt/lists/*

# Vendored, self-contained tools live in /app/bin (BIN_DIR).
COPY --from=tools /tools/ffmpeg /tools/ffprobe /tools/yt-dlp /app/bin/
COPY --from=tools /tools/mediamtx /usr/local/bin/mediamtx

COPY --from=builder /app/playout .
COPY templates ./templates
COPY static ./static
COPY mediamtx.yml ./mediamtx.yml
COPY docker-entrypoint.sh /usr/local/bin/docker-entrypoint.sh
RUN chmod +x /usr/local/bin/docker-entrypoint.sh /app/bin/* \
    && mkdir -p /app/data /app/media /app/soundboard /app/assets

ENV BIN_DIR=/app/bin \
    RTMP_URL=rtmp://localhost:1935/live/show

EXPOSE 8080 1935 8888

ENTRYPOINT ["/usr/bin/tini", "--", "/usr/local/bin/docker-entrypoint.sh"]
