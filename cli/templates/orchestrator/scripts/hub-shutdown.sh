#!/usr/bin/env bash
# hub-shutdown.sh — thin shim around `bones hub stop`.
set -euo pipefail
exec bones hub stop
