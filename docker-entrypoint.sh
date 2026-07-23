#!/bin/sh
# Run the MediaMTX relay alongside the playout app in one container. tini (the
# ENTRYPOINT) reaps zombies. If MediaMTX dies, take the container down so the
# orchestrator restarts the whole thing cleanly.
#
# Set MEDIAMTX_ENABLED=false to skip the relay entirely — useful when pushing
# straight to an external RTMP ingest via RTMP_URL.
set -e

if [ "${MEDIAMTX_ENABLED:-true}" = "true" ]; then
    mediamtx /app/mediamtx.yml &
    MEDIAMTX_PID=$!

    # If the relay exits, stop the app too.
    trap 'kill $MEDIAMTX_PID 2>/dev/null' EXIT

    # Give the relay a moment to bind its ports.
    sleep 1
fi

exec /app/playout
