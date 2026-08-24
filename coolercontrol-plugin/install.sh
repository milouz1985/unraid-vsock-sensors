#!/usr/bin/env bash
set -euo pipefail

fail() { echo "$1" >&2; exit "${2:-1}"; }

read_config_value() { sed -nE "s/.*\"$1\"[[:space:]]*:[[:space:]]*([0-9]+).*/\\1/p" "$2" | head -n 1; }

validate_number() {
    local name="$1" value="$2" minimum="$3"
    [[ "$value" =~ ^[0-9]+$ ]] || fail "$name invalide : $value" 2
    (( 10#$value >= minimum && 10#$value <= 4294967294 )) || fail "$name doit être compris entre $minimum et 4294967294" 2
}

install_file() {
    local mode="$1" source="$2" destination="$3"
    [[ -f "$source" ]] || fail "Fichier absent du paquet : ${source#"$package_dir/"}"
    "${elevate[@]}" install -m "$mode" "$source" "$destination"
}

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
    fail "L'installation nécessite root ou la commande sudo"
fi

for arg in "$@"; do
    case "$arg" in
        --cid=*) cid="${arg#*=}"; cid_explicit=1 ;;
        --port=*) port="${arg#*=}"; port_explicit=1 ;;
        *) fail "Option inconnue : $arg" 2 ;;
    esac
done

write_config=1
installed_config="$plugin_dir/config.json"
if [[ -f "$installed_config" ]] && (( cid_explicit == 0 && port_explicit == 0 )); then
    write_config=0
elif [[ -f "$installed_config" ]]; then
    (( cid_explicit == 1 )) || cid="$(read_config_value cid "$installed_config")"
    (( port_explicit == 1 )) || port="$(read_config_value port "$installed_config")"
fi

validate_number "Le CID" "$cid" 3
validate_number "Le port" "$port" 1

"${elevate[@]}" install -d -m 0755 "$plugin_dir" "$plugin_dir/ui"
install_file 0755 "$package_dir/unraid-vsock-sensors-cc" "$plugin_dir/unraid-vsock-sensors-cc"
install_file 0644 "$package_dir/manifest.toml" "$plugin_dir/manifest.toml"
install_file 0644 "$package_dir/ui/index.html" "$plugin_dir/ui/index.html"

config_tmp="$temporary_dir/config.json"
if (( write_config == 1 )); then
    printf '{"cid":%s,"port":%s}\n' "$cid" "$port" > "$config_tmp"
    "${elevate[@]}" install -m 0644 "$config_tmp" "$installed_config"
else
    echo "Configuration existante conservée : $installed_config"
fi

echo "Plugin installé dans $plugin_dir"
echo "Redémarre CoolerControl : sudo systemctl restart coolercontrold"
