#!/usr/bin/env bash
set -Eeuo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"

if [[ -f "$SCRIPT_DIR/local.env" ]]; then
    # shellcheck disable=SC1091
    source "$SCRIPT_DIR/local.env"
fi

: "${PVE_HOST:?PVE_HOST must be configured}"

PVE_USER="${PVE_USER:-root}"
PVE_TEMPLATE_DIR="${PVE_TEMPLATE_DIR:-/root/uvss-template-builder}"

ssh "${PVE_USER}@${PVE_HOST}" \
    "mkdir -p '$PVE_TEMPLATE_DIR'"

rsync -av \
    "$SCRIPT_DIR/build-template.sh" \
    "$SCRIPT_DIR/common.sh" \
    "$SCRIPT_DIR/go-version" \
    "$SCRIPT_DIR/template.env" \
    "$HOME/.ssh/id_ed25519.pub" \
    "${PVE_USER}@${PVE_HOST}:${PVE_TEMPLATE_DIR}/"

if [[ "${1:-}" == "--rebuild" ]]; then
    ssh -t "${PVE_USER}@${PVE_HOST}" \
        "cd '$PVE_TEMPLATE_DIR' && bash build-template.sh --replace"
fi