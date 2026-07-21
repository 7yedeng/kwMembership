#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT_DIR"

git pull --ff-only
bash scripts/build.sh
bash scripts/check.sh
systemctl restart kwmembership-worker.service
systemctl --no-pager --full status kwmembership-worker.service
