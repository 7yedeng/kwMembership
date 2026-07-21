#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT_DIR"

if ! command -v go >/dev/null 2>&1; then
  echo "Go 1.26+ is required"
  exit 1
fi

mkdir -p bin
VERSION="$(git describe --always --dirty 2>/dev/null || printf 'dev')"
go build -trimpath -ldflags "-s -w -X main.version=$VERSION" -o bin/kwmembership-worker ./cmd/membership-worker
echo "built bin/kwmembership-worker ($VERSION)"
