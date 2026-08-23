#!/usr/bin/env bash
set -euo pipefail

cid="${UNRAID_VSOCK_CID:-42}"
port="${UNRAID_VSOCK_PORT:-19090}"
cid_explicit=0
port_explicit=0
[[ -v UNRAID_VSOCK_CID ]] && cid_explicit=1
[[ -v UNRAID_VSOCK_PORT ]] && port_explicit=1
plugins_root="${CC_PLUGINS_DIR:-/var/lib/coolercontrol/plugins}"
plugin_dir="$plugins_root/unraid-vsock-sensors-cc"
package_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
temporary_dir="$(mktemp -d)"
trap 'rm -rf "$temporary_dir"' EXIT

if (( EUID == 0 )); then
    elevate=()
elif [[ -d "$plugins_root" && -w "$plugins_root" ]]; then
    elevate=()
elif command -v sudo >/dev/null 2>&1; then
    elevate=(sudo)
else
    echo "L'installation nécessite root ou la commande sudo" >&2
    exit 1
fi

for arg in "$@"; do
    case "$arg" in
        --cid=*) cid="${arg#*=}"; cid_explicit=1 ;;
        --port=*) port="${arg#*=}"; port_explicit=1 ;;
        *) echo "Option inconnue : $arg" >&2; exit 2 ;;
    esac
done

write_config=1
installed_config="$plugin_dir/config.json"
if [[ -f "$installed_config" ]]; then
    if (( cid_explicit == 0 && port_explicit == 0 )); then
        write_config=0
    else
        if (( cid_explicit == 0 )); then
            cid="$(sed -nE 's/.*"cid"[[:space:]]*:[[:space:]]*([0-9]+).*/\1/p' "$installed_config" | head -n 1)"
        fi
        if (( port_explicit == 0 )); then
            port="$(sed -nE 's/.*"port"[[:space:]]*:[[:space:]]*([0-9]+).*/\1/p' "$installed_config" | head -n 1)"
        fi
    fi
fi

case "$cid:$port" in
    *[!0-9:]*|:*|*:) echo "CID ou port invalide" >&2; exit 2 ;;
esac
if (( 10#$cid < 3 || 10#$cid > 4294967295 || 10#$port < 1 || 10#$port > 4294967295 )); then
    echo "Le CID doit être compris entre 3 et 4294967295 et le port entre 1 et 4294967295" >&2
    exit 2
fi

for required in unraid-vsock-sensors-cc manifest.toml ui/index.html; do
    if [[ ! -e "$package_dir/$required" ]]; then
        echo "Fichier absent du paquet : $required" >&2
        exit 1
    fi
done

"${elevate[@]}" install -d -m 0755 "$plugin_dir" "$plugin_dir/ui"
"${elevate[@]}" install -m 0755 "$package_dir/unraid-vsock-sensors-cc" "$plugin_dir/unraid-vsock-sensors-cc"
"${elevate[@]}" install -m 0644 "$package_dir/manifest.toml" "$plugin_dir/manifest.toml"
"${elevate[@]}" install -m 0644 "$package_dir/ui/index.html" "$plugin_dir/ui/index.html"

config_tmp="$temporary_dir/config.json"
if (( write_config == 1 )); then
    printf '{"cid":%s,"port":%s}\n' "$cid" "$port" > "$config_tmp"
    "${elevate[@]}" install -m 0644 "$config_tmp" "$installed_config"
else
    echo "Configuration existante conservée : $installed_config"
fi

echo "Plugin installé dans $plugin_dir"
echo "Redémarre CoolerControl : sudo systemctl restart coolercontrold"
