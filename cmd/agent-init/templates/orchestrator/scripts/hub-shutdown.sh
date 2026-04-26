#!/usr/bin/env bash
# hub-shutdown.sh — kills hub processes by PID file.
# Idempotent: missing PID file or stale PID is not an error.
set -euo pipefail

ROOT="$(git rev-parse --show-toplevel)"
PID_DIR="$ROOT/.orchestrator/pids"

for kind in fossil nats; do
    pidfile="$PID_DIR/$kind.pid"
    if [[ -f "$pidfile" ]]; then
        pid="$(cat "$pidfile")"
        if kill -0 "$pid" 2>/dev/null; then
            kill "$pid" 2>/dev/null || true
            for _ in 1 2 3 4 5; do
                kill -0 "$pid" 2>/dev/null || break
                sleep 0.2
            done
            kill -9 "$pid" 2>/dev/null || true
        fi
        rm -f "$pidfile"
    fi
done

echo "hub-shutdown: stopped"
