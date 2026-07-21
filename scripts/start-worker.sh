#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT_DIR"

if [ ! -x bin/kwmembership-worker ]; then
  echo "missing bin/kwmembership-worker; run bash scripts/build.sh first"
  exit 1
fi

exec bin/kwmembership-worker
