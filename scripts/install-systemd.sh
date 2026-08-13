#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "$0")/.." && pwd)"
INSTALL_DIR="${KWMEMBERSHIP_INSTALL_DIR:-/opt/kwmembership}"
RUN_USER="${KWMEMBERSHIP_USER:-${SUDO_USER:-}}"
ENV_FILE="${KWMEMBERSHIP_ENV_FILE:-/etc/kwmembership.env}"

if [ "$(id -u)" -ne 0 ]; then
  echo "run this installer as root"
  exit 1
fi
if [ ! -f "$ROOT_DIR/.env" ]; then
  echo "missing $ROOT_DIR/.env"
  exit 1
fi
if [ -z "$RUN_USER" ] || ! id "$RUN_USER" >/dev/null 2>&1; then
  echo "set KWMEMBERSHIP_USER to the same Unix user that runs kwRedeem"
  exit 1
fi
PYTHON_BIN="$(command -v python3 || true)"
if [ -z "$PYTHON_BIN" ]; then
  echo "Python 3 is required for the serial payment executor"
  exit 1
fi
CHECKOUT_EXECUTOR="$(sed -n 's/^[[:space:]]*KWMEMBERSHIP_CHECKOUT_EXECUTOR[[:space:]]*=[[:space:]]*//p' "$ROOT_DIR/.env" | tail -n 1 | tr -d '\"' | tr -d "'")"
CHECKOUT_EXECUTOR="${CHECKOUT_EXECUTOR:-python}"

KAWANG_ROOT="$(sed -n 's/^[[:space:]]*KAWANG_PROJECT_ROOT[[:space:]]*=[[:space:]]*//p' "$ROOT_DIR/.env" | tail -n 1)"
KAWANG_ROOT="${KAWANG_ROOT%\"}"
KAWANG_ROOT="${KAWANG_ROOT#\"}"
KAWANG_ROOT="${KAWANG_ROOT%\'}"
KAWANG_ROOT="${KAWANG_ROOT#\'}"
if [ -z "$KAWANG_ROOT" ]; then
  echo "KAWANG_PROJECT_ROOT is missing from $ROOT_DIR/.env"
  exit 1
fi
if [[ "$KAWANG_ROOT" != /* ]]; then
  KAWANG_ROOT="$(cd "$ROOT_DIR/$KAWANG_ROOT" && pwd)"
fi
if [ ! -f "$KAWANG_ROOT/.env" ] || [ ! -f "$KAWANG_ROOT/package.json" ]; then
  echo "KAWANG_PROJECT_ROOT is not a configured kwRedeem checkout: $KAWANG_ROOT"
  exit 1
fi
KAWANG_DATA_DIR="${KAWANG_DATA_DIR:-$KAWANG_ROOT/data}"

mkdir -p "$INSTALL_DIR"
rsync -a --delete \
  --exclude '.env' --exclude 'bin' \
  "$ROOT_DIR/" "$INSTALL_DIR/"
install -m 0600 -o root -g "$RUN_USER" "$ROOT_DIR/.env" "$ENV_FILE"
chown -R "$RUN_USER:$RUN_USER" "$INSTALL_DIR"

cd "$INSTALL_DIR"
runuser -u "$RUN_USER" -- env KWMEMBERSHIP_PROJECT_ROOT="$INSTALL_DIR" KAWANG_PROJECT_ROOT="$KAWANG_ROOT" bash scripts/build.sh
runuser -u "$RUN_USER" -- env KWMEMBERSHIP_PROJECT_ROOT="$INSTALL_DIR" KAWANG_PROJECT_ROOT="$KAWANG_ROOT" bash scripts/check.sh

sed \
  -e "s|@@INSTALL_DIR@@|$INSTALL_DIR|g" \
  -e "s|@@RUN_USER@@|$RUN_USER|g" \
  -e "s|@@ENV_FILE@@|$ENV_FILE|g" \
  -e "s|@@KAWANG_DATA_DIR@@|$KAWANG_DATA_DIR|g" \
  "$INSTALL_DIR/systemd/kwmembership-worker.service" > /etc/systemd/system/kwmembership-worker.service
sed \
  -e "s|@@INSTALL_DIR@@|$INSTALL_DIR|g" \
  -e "s|@@RUN_USER@@|$RUN_USER|g" \
  -e "s|@@ENV_FILE@@|$ENV_FILE|g" \
  -e "s|@@KAWANG_DATA_DIR@@|$KAWANG_DATA_DIR|g" \
  -e "s|@@PYTHON_BIN@@|$PYTHON_BIN|g" \
  "$INSTALL_DIR/systemd/kwmembership-python-executor.service" > /etc/systemd/system/kwmembership-python-executor.service

systemctl daemon-reload
systemctl disable --now kwmembership-api.service >/dev/null 2>&1 || true
rm -f /etc/systemd/system/kwmembership-api.service
systemctl enable --now kwmembership-worker.service
if [ "$CHECKOUT_EXECUTOR" = "python" ]; then
  systemctl enable --now kwmembership-python-executor.service
else
  systemctl disable --now kwmembership-python-executor.service >/dev/null 2>&1 || true
fi
systemctl --no-pager --full status kwmembership-worker.service
if systemctl is-enabled --quiet kwmembership-python-executor.service; then
  systemctl --no-pager --full status kwmembership-python-executor.service
fi
