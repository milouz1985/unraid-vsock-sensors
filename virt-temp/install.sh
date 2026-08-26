#!/usr/bin/env bash
set -euo pipefail

fail() { echo "$1" >&2; exit "${2:-1}"; }

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
[[ -r "$script_dir/VERSION" ]] || fail "Fichier VERSION absent du paquet"
read -r version < "$script_dir/VERSION"
module="virt-temp"
dkms_source="/usr/src/$module-$version"
generated_source="$script_dir/$module-$version"
binary="/usr/local/bin/unraid-vsock-sensors"
unit="/etc/systemd/system/unraid-vsock-hwmon.service"
config="/etc/default/unraid-vsock-hwmon"
modules_load="/etc/modules-load.d/virt-temp.conf"
uninstaller="/usr/local/sbin/uninstall-unraid-vsock-hwmon"
working_dir="$(mktemp -d)"
trap 'rm -rf -- "$working_dir"' EXIT

if (( EUID == 0 )); then
    elevate=()
elif command -v sudo >/dev/null 2>&1; then
    elevate=(sudo)
else
    fail "L'installation nécessite root ou la commande sudo"
fi

for command in dkms install make modprobe systemctl; do
    command -v "$command" >/dev/null 2>&1 || fail "Commande requise absente : $command"
done

kernel_build="/lib/modules/$(uname -r)/build"
if [[ ! -d "$kernel_build" ]]; then
    fail "En-têtes absents pour le noyau $(uname -r) ; installer le paquet proxmox-headers-$(uname -r)"
fi

[[ -x "$script_dir/unraid-vsock-sensors" ]] || fail "Binaire absent du paquet"
[[ -x "$script_dir/uninstall.sh" ]] || fail "Script de désinstallation absent du paquet"
[[ -d "$generated_source" ]] || fail "Sources DKMS absentes du paquet : $generated_source"

"${elevate[@]}" install -d -m 0755 "$dkms_source"
"${elevate[@]}" install -m 0644 \
    "$generated_source/Makefile" \
    "$generated_source/dkms.conf" \
    "$generated_source/virt-temp.c" \
    "$dkms_source/"

if ! "${elevate[@]}" dkms status -m "$module" -v "$version" 2>/dev/null \
    | grep -Fq "$module/$version"; then
    "${elevate[@]}" dkms add -m "$module" -v "$version"
fi
"${elevate[@]}" dkms build --force -m "$module" -v "$version"
"${elevate[@]}" dkms install --force -m "$module" -v "$version"

"${elevate[@]}" systemctl stop unraid-vsock-hwmon.service 2>/dev/null || true
if grep -q '^virt_temp ' /proc/modules; then
    "${elevate[@]}" modprobe -r virt_temp
fi

"${elevate[@]}" install -m 0755 "$script_dir/unraid-vsock-sensors" "$binary"
"${elevate[@]}" install -m 0755 "$script_dir/uninstall.sh" "$uninstaller"
"${elevate[@]}" install -m 0644 "$script_dir/unraid-vsock-hwmon.service" "$unit"
printf 'virt-temp\n' > "$working_dir/virt-temp.conf"
"${elevate[@]}" install -m 0644 "$working_dir/virt-temp.conf" "$modules_load"
if [[ ! -e "$config" ]]; then
    printf 'UNRAID_VSOCK_CID=3\nUNRAID_VSOCK_PORT=19090\nUNRAID_VSOCK_INTERVAL=1s\n' \
        > "$working_dir/unraid-vsock-hwmon"
    "${elevate[@]}" install -m 0644 "$working_dir/unraid-vsock-hwmon" "$config"
else
    echo "Configuration existante conservée : $config"
fi

"${elevate[@]}" modprobe virt_temp
"${elevate[@]}" systemctl daemon-reload
"${elevate[@]}" systemctl enable --now unraid-vsock-hwmon.service

if [[ ! -c /dev/virt-temp ]]; then
    fail "Le module est chargé, mais le périphérique /dev/virt-temp reste introuvable"
fi

while IFS= read -r previous_version; do
    [[ "$previous_version" == "$version" ]] && continue
    if [[ ! "$previous_version" =~ ^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?(\+[0-9A-Za-z.-]+)?$ ]]; then
        echo "Version DKMS invalide ignorée : $previous_version" >&2
        continue
    fi
    "${elevate[@]}" dkms remove -m "$module" -v "$previous_version" --all
    "${elevate[@]}" rm -rf -- "/usr/src/$module-$previous_version"
done < <(
    "${elevate[@]}" dkms status -m "$module" 2>/dev/null \
        | sed -n "s|^$module/\([^,:]*\).*|\1|p" \
        | sort -u
)

echo "virt-temp $version installé"
echo "Configuration : $config"
echo "Désinstallation : $uninstaller"
echo "Vérification : sensors && systemctl status unraid-vsock-hwmon.service"
