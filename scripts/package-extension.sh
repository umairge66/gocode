#!/bin/sh
# package-extension.sh — build the Chrome Web Store upload zip of the gocode
# browser extension.
#
# Usage:
#   scripts/package-extension.sh [output-dir]     (default: dist/)
#
# Writes <output-dir>/gocode-chrome-extension-<version>.zip and a .sha256 file
# next to it, and prints both the path and the sha256. The zip contains only
# the extension's runtime files (extension.RuntimeFiles: manifest.json,
# background.js and the PNG icons in icons/), with the "key" field removed from manifest.json because the
# Chrome Web Store assigns the published extension its own id. Entries are
# sorted, timestamps and permissions fixed and the files stored uncompressed,
# so the same sources always give the same sha256.
#
# Needs only a local Go toolchain; no network access is used (GOPROXY=off,
# GOTOOLCHAIN=local). Upload the zip by hand in the Chrome Web Store
# Developer Dashboard; see extension/store/checklist.md.
set -eu

root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
out=${1:-"$root/dist"}
case $out in
/*) ;;
*) out=$(pwd)/$out ;;
esac

cd "$root"
GOTOOLCHAIN=local GOPROXY=off GOFLAGS=-mod=readonly go run ./cmd/package-extension -o "$out"
