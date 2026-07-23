#!/bin/sh
# Dev-image entrypoint: run the MediaMTX relay alongside the playout app in
# one container. tini (the ENTRYPOINT) reaps zombies. If MediaMTX dies, take
# the container down so the orchestrator restarts the whole thing cleanly.
# The production image (default Dockerfile target) has no relay and runs
# /app/playout directly.
set -e

mediamtx /app/mediamtx.yml &
MEDIAMTX_PID=$!

# If the relay exits, stop the app too.
trap 'kill $MEDIAMTX_PID 2>/dev/null' EXIT

# Give the relay a moment to bind its ports.
sleep 1

exec /app/playout
