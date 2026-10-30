#!/usr/bin/env bash
# Cross-compile bisonw for Windows amd64 (64-bit).
# Requires: Go 1.24+, Node.js 18+ (site UI). Run from Linux or Windows.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../../.." && pwd)"
SITE="$ROOT/client/webserver/site"
OUT="${1:-$ROOT/client/cmd/bisonw/bisonw-windows-amd64.exe}"
VERSION="${VERSION:-1.1.0-rc2+revivel.lbc}"

if ! command -v go >/dev/null; then
  echo "Go 1.24+ is required" >&2
  exit 1
fi
if ! command -v node >/dev/null; then
  echo "Node.js 18+ is required to build the web UI" >&2
  exit 1
fi
node_major="$(node -p 'process.versions.node.split(".")[0]')"
if (( node_major < 18 )); then
  echo "Node.js 18+ is required (found $(node -v)). nvm users: nvm use 18" >&2
  exit 1
fi

if [[ ! -f "$SITE/dist/entry.js" ]]; then
  echo "building web UI..."
  (cd "$SITE" && npm ci && npm run build)
fi

echo "building bisonw -> $OUT"
cd "$ROOT/client/cmd/bisonw"
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -trimpath -buildvcs=true \
  -ldflags "-s -w -X decred.org/dcrdex/client/app.Version=${VERSION}" \
  -o "$OUT" .
echo "ok $(ls -lh "$OUT" | awk '{print $5}')"
