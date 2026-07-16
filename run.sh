#!/usr/bin/env bash
# run.sh — rebuild cheep from source and launch it, for local testing.
#
#   bash run.sh            # build + run the TUI
#   bash run.sh version    # build + run with args passed through
#
# This builds a local ./cheep and runs THAT — it never touches the installed
# ~/.local/bin/cheep, so `cheep upgrade` and this dev loop stay out of each
# other's way. The version will read v0.0.1 (it's only stamped in real releases).
set -euo pipefail

# Run from the repo root (this script's directory), so it works from anywhere.
cd "$(dirname "$0")"

BIN=./cheep

echo "→ building cheep…"
rm -f "$BIN"                                          # fresh inode: dodges the macOS code-signing "killed" bug
go build -o "$BIN" .
codesign --force --sign - "$BIN" 2>/dev/null || true  # harmless ad-hoc re-sign on Apple Silicon; no-op elsewhere

echo "→ launching local build…"
exec "$BIN" "$@"
