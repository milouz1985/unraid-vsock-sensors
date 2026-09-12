#!/bin/bash
binary="${UVSS_SERVICE_BINARY:-/usr/local/sbin/unraid-vsock-sensors}"
rc="${UVSS_SERVICE_RC:-/etc/rc.d/rc.unraid-vsock-sensors}"
if [[ "${1:-}" == "set-disk-policy" ]]; then
    [[ $# == 3 ]] || { echo "Usage: service.sh set-disk-policy ID_BASE64 {auto|include|exclude}" >&2; exit 2; }
    "$binary" disks set --id-base64 "$2" --policy "$3" 2>&1 || exit $?
    exec "$rc" refresh 2>&1
fi
exec "$rc" "$@"
