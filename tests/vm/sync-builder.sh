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
SSH_PUBLIC_KEY_SOURCE="${SSH_PUBLIC_KEY_SOURCE:-${HOME}/.ssh/id_ed25519.pub}"
SSH_PUBLIC_KEY_FILE="${SSH_PUBLIC_KEY_FILE:-${SCRIPT_DIR}/id_ed25519.pub}"
SSH_PUBLIC_KEY_NAME="${SSH_PUBLIC_KEY_FILE##*/}"
[[ -r "$SSH_PUBLIC_KEY_SOURCE" ]] || {
    echo "SSH public key not readable: $SSH_PUBLIC_KEY_SOURCE" >&2
    exit 1
}

PVE_TARGET="${PVE_SSH_USER}@${PVE_HOST}"

ssh "$PVE_TARGET" \
    "mkdir -p '$PVE_TEMPLATE_DIR'"

rsync -av \
    "$SCRIPT_DIR/build-template.sh" \
    "$SCRIPT_DIR/common.sh" \
    "$SCRIPT_DIR/go-version" \
    "$SCRIPT_DIR/template.env" \
    "$PVE_TARGET:${PVE_TEMPLATE_DIR}/"

rsync -av \
    "$SSH_PUBLIC_KEY_SOURCE" \
    "$PVE_TARGET:${PVE_TEMPLATE_DIR}/${SSH_PUBLIC_KEY_NAME}"

if [[ "${1:-}" == "--rebuild" ]]; then
    ssh -t "$PVE_TARGET" \
        "cd '$PVE_TEMPLATE_DIR' && bash build-template.sh --replace"
fi
