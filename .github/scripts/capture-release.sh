#!/usr/bin/env bash
# Collect build artifacts produced by a CI packager job into a single
# release/<platform>/<arch>/<filename> tree.
#
# The filename must match what DSH's in-app / in-tunnel updater expects,
# i.e. exactly what scripts/release.sh writes into updates/manifest.json.
#
# Usage:
#   capture-release.sh <platform> <arch> <glob> [<glob> ...]
#
# <glob> patterns are expanded relative to the repo root; the first file
# that matches wins. Some Tauri output paths vary between CLI/bundle
# versions (e.g. .../universal/release vs .../arm64/release,
# *.msi vs *_x64-setup.exe), so callers pass candidates in priority order.
set -euo pipefail

if [ "$#" -lt 3 ]; then
  echo "usage: $0 <platform> <arch> <glob> [<glob> ...]" >&2
  exit 2
fi

PLATFORM="$1"; shift
ARCH="$1"; shift

ROOT="${GITHUB_WORKSPACE:-$PWD}"
DEST="$ROOT/release/$PLATFORM/$ARCH"
mkdir -p "$DEST"

chosen=""
for pat in "$@"; do
  # shellcheck disable=SC2086
  for f in $pat; do
    if [ -f "$f" ]; then
      chosen="$f"
      break 2
    fi
  done
done

if [ -z "$chosen" ]; then
  echo "::error::no file matched any of: $*"
  echo "--- searching likely output dirs ---"
  find "$ROOT" -maxdepth 12 \
    \( -name '*.apk' -o -name '*.aab' -o -name '*.msi' -o -name '*.exe' \
       -o -name '*.deb' -o -name '*.rpm' -o -name '*.AppImage' -o -name '*.tar.gz' \
       -o -name '*.dmg' \) \
    -not -path '*/node_modules/*' -not -path '*/.git/*' 2>/dev/null || true
  exit 1
fi

base="$(basename "$chosen")"
cp -f "$chosen" "$DEST/$base"

size=$(stat -c %s "$DEST/$base" 2>/dev/null || stat -f %z "$DEST/$base")
sum=$( (sha256sum "$DEST/$base" 2>/dev/null || shasum -a 256 "$DEST/$base") | cut -d' ' -f1)
echo "captured $PLATFORM/$ARCH <- $chosen ($size bytes, sha256 $sum)"
# Emit machine-readable metadata for the manifest job.
printf '%s\t%s\t%s\t%s\n' "$PLATFORM" "$ARCH" "$base" "$sum" >> "$ROOT/release/.artifacts.tsv"
