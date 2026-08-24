#!/usr/bin/env bash
# Build bisonw for Linux amd64 (Ubuntu 22.04/24.04).
# Requires: Go 1.24+, Node.js 18+ (site UI).
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../../.." && pwd)"
SITE="$ROOT/client/webserver/site"
OUT="${1:-$ROOT/client/cmd/bisonw/bisonw-linux-amd64}"
VERSION="${VERSION:-1.1.0-rc2+revivel.lbc}"

if ! command -v go >/dev/null; then
  echo "Go 1.24+ is required" >&2
  exit 1
fi
if ! command -v node >/dev/null; then
  echo "Node.js 18+ is required to build the web UI" >&2
  exit 1
fi

if [[ ! -f "$SITE/dist/entry.js" ]]; then
  echo "building web UI..."
  (cd "$SITE" && npm ci && npm run build)
fi

echo "building bisonw -> $OUT"
cd "$ROOT/client/cmd/bisonw"
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath \
  -ldflags "-s -w -X decred.org/dcrdex/client/app.Version=${VERSION}" \
  -o "$OUT" .
echo "ok $(ls -lh "$OUT" | awk '{print $5}')"
