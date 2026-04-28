#!/usr/bin/env bash
# hub-bootstrap.sh — thin shim around `bones hub start --detach`.
# Kept for backward compatibility with .claude/settings.json hooks
# generated before the Go-native hub. New consumers should call
# `bones hub start` directly.
set -euo pipefail
exec bones hub start --detach
