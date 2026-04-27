#!/usr/bin/env bash
# hub-bootstrap.sh — idempotent.
# Per ADR 0024: opens a Fossil checkout at the project root and seeds
# the hub from git-tracked files. Files Fossil writes (.fslckout,
# .fossil-settings/, .orchestrator/) must be gitignored.
# Writes server PIDs to .orchestrator/pids for hub-shutdown.sh.
# No-op if servers are already up.
#
# Prerequisites: `fossil`, `nats-server`, and `git` must be in $PATH.
set -euo pipefail

ROOT="$(git rev-parse --show-toplevel)"
ORCH_DIR="$ROOT/.orchestrator"
HUB_REPO="$ORCH_DIR/hub.fossil"
PID_DIR="$ORCH_DIR/pids"
mkdir -p "$PID_DIR"

# Fresh-start detection: if no fossil PID is alive, wipe stale state
# (hub repo, checkout metadata, settings dir) so each session starts
# from a clean substrate. Working-tree files are untouched. Per ADR
# 0024 §2.
fossil_alive=false
if [[ -f "$PID_DIR/fossil.pid" ]] && \
   kill -0 "$(cat "$PID_DIR/fossil.pid")" 2>/dev/null; then
    fossil_alive=true
fi

if [[ "$fossil_alive" == "false" ]]; then
    rm -rf "$HUB_REPO" "$ROOT/.fslckout" "$ROOT/.fossil-settings"
fi

# 1) Hub fossil repo + checkout-at-root + git-tracked seed (ADR 0024 §1, §3)
if [[ ! -f "$HUB_REPO" ]]; then
    fossil new "$HUB_REPO" --admin-user orchestrator >/dev/null
    (
        cd "$ROOT"
        fossil open --force "$HUB_REPO" >/dev/null
        if [[ "$(git ls-files | wc -l | tr -d ' ')" -eq 0 ]]; then
            echo "hub-bootstrap: error: no git-tracked files to seed from" >&2
            exit 1
        fi
        git ls-files -z | xargs -0 fossil add >/dev/null
        fossil commit --user orchestrator \
            -m "session base: $(git rev-parse --short HEAD)" >/dev/null
    )
fi

# 2) Fossil HTTP server
if [[ -f "$PID_DIR/fossil.pid" ]] && \
   kill -0 "$(cat "$PID_DIR/fossil.pid")" 2>/dev/null; then
    echo "fossil server already running (pid=$(cat "$PID_DIR/fossil.pid"))"
else
    fossil server "$HUB_REPO" --localhost --port 8765 \
        >"$ORCH_DIR/fossil.log" 2>&1 &
    echo $! >"$PID_DIR/fossil.pid"
    sleep 0.3
fi

# 3) NATS server with JetStream
if [[ -f "$PID_DIR/nats.pid" ]] && \
   kill -0 "$(cat "$PID_DIR/nats.pid")" 2>/dev/null; then
    echo "nats-server already running (pid=$(cat "$PID_DIR/nats.pid"))"
else
    nats-server -js -p 4222 >"$ORCH_DIR/nats.log" 2>&1 &
    echo $! >"$PID_DIR/nats.pid"
    sleep 0.3
fi

echo "hub-bootstrap: hub at http://127.0.0.1:8765, nats at nats://127.0.0.1:4222"
