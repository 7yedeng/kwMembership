#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT_DIR"

git pull --ff-only
bash scripts/build.sh
bash scripts/check.sh
systemctl restart kwmembership-worker.service
CHECKOUT_EXECUTOR="$(sed -n 's/^[[:space:]]*KWMEMBERSHIP_CHECKOUT_EXECUTOR[[:space:]]*=[[:space:]]*//p' .env | tail -n 1 | tr -d '\"' | tr -d "'")"
CHECKOUT_EXECUTOR="${CHECKOUT_EXECUTOR:-python}"
if [ "$CHECKOUT_EXECUTOR" = "python" ]; then
  systemctl restart kwmembership-python-executor.service
else
  systemctl disable --now kwmembership-python-executor.service >/dev/null 2>&1 || true
fi
systemctl --no-pager --full status kwmembership-worker.service
if systemctl is-enabled --quiet kwmembership-python-executor.service; then
  systemctl --no-pager --full status kwmembership-python-executor.service
fi
