#!/bin/bash
set -euo pipefail

case "${1:-}" in
    start|stop|restart|status)
        exec /etc/rc.d/rc.unraid-vsock-sensors "$1"
        ;;
    *)
        echo "Usage: $0 {start|stop|restart|status}" >&2
        exit 2
        ;;
esac
