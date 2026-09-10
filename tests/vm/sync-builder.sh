#!/usr/bin/env bash
set -Eeuo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"

[[ -r "$SCRIPT_DIR/template.env" ]] || {
    echo "Copy tests/vm/template.env.example to tests/vm/template.env and configure it first." >&2
    exit 1
}
# shellcheck disable=SC1091
source "$SCRIPT_DIR/template.env"

: "${PVE_HOST:?PVE_HOST must be configured}"

PVE_SSH_USER="${PVE_SSH_USER:-root}"
PVE_TEMPLATE_DIR="${PVE_TEMPLATE_DIR:-/root/uvss-template-builder}"
[[ "$PVE_TEMPLATE_DIR" =~ ^/[A-Za-z0-9._/-]+$ ]] || {
    echo "PVE_TEMPLATE_DIR must be an absolute path without shell metacharacters." >&2
    exit 2
}
PVE_TARGET="${PVE_SSH_USER}@${PVE_HOST}"

# PVE_TEMPLATE_DIR is validated above and intentionally expanded by the client.
# shellcheck disable=SC2029
ssh "$PVE_TARGET" \
    "mkdir -p '$PVE_TEMPLATE_DIR'"

rsync -av \
    "$SCRIPT_DIR/build-template.sh" \
    "$SCRIPT_DIR/common.sh" \
    "$SCRIPT_DIR/go-version" \
    "$SCRIPT_DIR/template.env" \
    "$PVE_TARGET:${PVE_TEMPLATE_DIR}/"

if [[ "${1:-}" == "--rebuild" ]]; then
    # PVE_TEMPLATE_DIR is validated above and intentionally expanded by the client.
    # shellcheck disable=SC2029
    ssh -t "$PVE_TARGET" \
        "cd '$PVE_TEMPLATE_DIR' && bash build-template.sh --replace"
fi
