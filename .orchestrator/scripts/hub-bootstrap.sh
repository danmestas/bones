#!/usr/bin/env bash
# hub-bootstrap.sh — idempotent.
# Boots the orchestrator's hub fossil HTTP server and NATS server.
# Writes server PIDs to .orchestrator/pids for hub-shutdown.sh.
# No-op if both servers are already up.
#
# Prerequisites: `fossil` and `nats-server` must be in $PATH.
set -euo pipefail

ROOT="$(git rev-parse --show-toplevel)"
ORCH_DIR="$ROOT/.orchestrator"
HUB_REPO="$ORCH_DIR/hub.fossil"
PID_DIR="$ORCH_DIR/pids"
mkdir -p "$PID_DIR"

# 1) Hub fossil repo
if [[ ! -f "$HUB_REPO" ]]; then
    fossil new "$HUB_REPO" --admin-user orchestrator >/dev/null
fi

# 2) Fossil HTTP server
if [[ -f "$PID_DIR/fossil.pid" ]] && kill -0 "$(cat "$PID_DIR/fossil.pid")" 2>/dev/null; then
    echo "fossil server already running (pid=$(cat "$PID_DIR/fossil.pid"))"
else
    fossil server "$HUB_REPO" --localhost --port 8765 --busytimeout 30000 >"$ORCH_DIR/fossil.log" 2>&1 &
    echo $! >"$PID_DIR/fossil.pid"
    sleep 0.3
fi

# 3) NATS server with JetStream
if [[ -f "$PID_DIR/nats.pid" ]] && kill -0 "$(cat "$PID_DIR/nats.pid")" 2>/dev/null; then
    echo "nats-server already running (pid=$(cat "$PID_DIR/nats.pid"))"
else
    nats-server -js -p 4222 >"$ORCH_DIR/nats.log" 2>&1 &
    echo $! >"$PID_DIR/nats.pid"
    sleep 0.3
fi

echo "hub-bootstrap: hub at http://127.0.0.1:8765, nats at nats://127.0.0.1:4222"
