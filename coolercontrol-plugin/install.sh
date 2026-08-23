#!/usr/bin/env bash
set -euo pipefail

cid="${UNRAID_VSOCK_CID:-42}"
port="${UNRAID_VSOCK_PORT:-19090}"
plugin_dir="${CC_PLUGINS_DIR:-/var/lib/coolercontrol/plugins}/unraid-vsock-sensors-cc"
script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
build_dir="$(mktemp -d)"
trap 'rm -rf "$build_dir"' EXIT

if (( EUID == 0 )); then
    elevate=()
elif command -v sudo >/dev/null 2>&1; then
    elevate=(sudo)
else
    echo "L'installation nécessite root ou la commande sudo" >&2
    exit 1
fi

for arg in "$@"; do
    case "$arg" in
        --cid=*) cid="${arg#*=}" ;;
        --port=*) port="${arg#*=}" ;;
        *) echo "Option inconnue : $arg" >&2; exit 2 ;;
    esac
done

case "$cid:$port" in
    *[!0-9:]*|:*|*:) echo "CID ou port invalide" >&2; exit 2 ;;
esac
if (( 10#$cid < 3 || 10#$cid > 4294967295 || 10#$port < 1 || 10#$port > 4294967295 )); then
    echo "Le CID doit être compris entre 3 et 4294967295 et le port entre 1 et 4294967295" >&2
    exit 2
fi

(
    cd "$script_dir"
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o "$build_dir/unraid-vsock-sensors-cc" .
)
"${elevate[@]}" install -d -m 0755 "$plugin_dir"
"${elevate[@]}" install -m 0755 "$build_dir/unraid-vsock-sensors-cc" "$plugin_dir/unraid-vsock-sensors-cc"
"${elevate[@]}" install -m 0644 "$script_dir/manifest.toml" "$plugin_dir/manifest.toml"
"${elevate[@]}" install -d -m 0755 "$plugin_dir/ui"
"${elevate[@]}" install -m 0644 "$script_dir/ui/index.html" "$plugin_dir/ui/index.html"

config_tmp="$build_dir/config.json"
printf '{"cid":%s,"port":%s}\n' "$cid" "$port" > "$config_tmp"
"${elevate[@]}" install -m 0644 "$config_tmp" "$plugin_dir/config.json"

echo "Plugin installé dans $plugin_dir"
echo "Redémarre CoolerControl : sudo systemctl restart coolercontrold"
