#!/usr/bin/env bash
# hub-bootstrap.sh — idempotent.
# Boots the orchestrator's hub via EdgeSync's bin/leaf.
# Writes server PID to .orchestrator/pids for hub-shutdown.sh.
# No-op if the leaf is already up.
#
# Prerequisites: `bin/leaf` from EdgeSync must be built and on $PATH or
# at $ROOT/bin/leaf (or sibling repo at $ROOT/../EdgeSync/bin/leaf).
set -euo pipefail

ROOT="$(git rev-parse --show-toplevel)"
ORCH_DIR="$ROOT/.orchestrator"
HUB_REPO="$ORCH_DIR/hub.fossil"
PID_DIR="$ORCH_DIR/pids"
mkdir -p "$PID_DIR"

# Resolve leaf binary: $LEAF_BIN env > $ROOT/bin/leaf > sibling EdgeSync repo > $PATH.
EDGESYNC_DIR="${EDGESYNC_DIR:-$ROOT/../EdgeSync}"
LEAF_BIN="${LEAF_BIN:-$ROOT/bin/leaf}"
if [[ ! -x "$LEAF_BIN" ]]; then
    if [[ -x "$EDGESYNC_DIR/bin/leaf" ]]; then
        LEAF_BIN="$EDGESYNC_DIR/bin/leaf"
    elif command -v leaf >/dev/null 2>&1; then
        LEAF_BIN=$(command -v leaf)
    elif [[ -d "$EDGESYNC_DIR/leaf/cmd/leaf" ]]; then
        echo "hub-bootstrap: building bin/leaf in $EDGESYNC_DIR" >&2
        (cd "$EDGESYNC_DIR" && make leaf >&2)
        LEAF_BIN="$EDGESYNC_DIR/bin/leaf"
    else
        echo "error: bin/leaf not found and EdgeSync repo not at $EDGESYNC_DIR" >&2
        echo "       set LEAF_BIN or EDGESYNC_DIR, or build bin/leaf manually" >&2
        exit 1
    fi
fi

if [[ ! -x "$LEAF_BIN" ]]; then
    echo "error: $LEAF_BIN is not executable" >&2
    exit 1
fi

# Hub fossil repo: leaf does not auto-create, so initialise here if missing.
mkdir -p "$ORCH_DIR"
if [[ ! -f "$HUB_REPO" ]]; then
    if ! command -v fossil >/dev/null 2>&1; then
        echo "error: fossil not in PATH; cannot create $HUB_REPO" >&2
        exit 1
    fi
    fossil new "$HUB_REPO" --admin-user orchestrator >/dev/null
fi

# Leaf binary running as the hub: serves HTTP xfer + embedded NATS.
# LEAF_NATS_URL="" disables upstream (no leaf-node connect); --serve-nats +
# LEAF_NATS_CLIENT_PORT=4222 stand up the embedded NATS server on :4222.
# OTEL_* unset to avoid hub bootstrap blocking on a slow/unreachable collector.
if [[ -f "$PID_DIR/leaf.pid" ]] && kill -0 "$(cat "$PID_DIR/leaf.pid")" 2>/dev/null; then
    echo "leaf hub already running (pid=$(cat "$PID_DIR/leaf.pid"))"
else
    env -u OTEL_EXPORTER_OTLP_ENDPOINT -u OTEL_EXPORTER_OTLP_HEADERS \
        LEAF_NATS_URL="" \
        LEAF_NATS_CLIENT_PORT=4222 \
        "$LEAF_BIN" \
        --repo "$HUB_REPO" \
        --serve-http :8765 \
        --serve-nats \
        --autosync off \
        >"$ORCH_DIR/leaf.log" 2>&1 &
    echo $! >"$PID_DIR/leaf.pid"
    sleep 0.5
fi

echo "hub-bootstrap: hub at http://127.0.0.1:8765, embedded NATS at nats://127.0.0.1:4222"
