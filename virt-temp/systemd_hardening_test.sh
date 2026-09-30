#!/usr/bin/env bash
set -euo pipefail

unit_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
unit="$unit_dir/unraid-vsock-hwmon.service"

declare -A expected=(
    [CapabilityBoundingSet]=CAP_NET_BIND_SERVICE
    [ExecStartPre]="+/usr/sbin/modprobe virt_temp"
    [NoNewPrivileges]=yes
    [PrivateTmp]=yes
    [ProtectHome]=yes
    [ProtectSystem]=strict
    [ReadWritePaths]="-/sys/kernel/config/virt_temp"
    [RestrictAddressFamilies]=AF_VSOCK
    [RuntimeDirectory]=unraid-vsock-sensors
    [RuntimeDirectoryPreserve]=yes
    [StateDirectory]=unraid-vsock-sensors
)
declare -A actual=()

section=""
while IFS= read -r line || [[ -n "$line" ]]; do
    if [[ "$line" == \[*\] ]]; then
        section=${line:1:${#line}-2}
        continue
    fi
    [[ "$section" == Service && "$line" == *=* ]] || continue

    key=${line%%=*}
    [[ -v "expected[$key]" ]] || continue
    if [[ -v "actual[$key]" ]]; then
        echo "$unit has more than one $key directive in [Service]" >&2
        exit 1
    fi
    actual[$key]=${line#*=}
done < "$unit"

for key in "${!expected[@]}"; do
    if [[ ! -v "actual[$key]" ]]; then
        echo "$unit is missing $key in [Service]" >&2
        exit 1
    fi
    if [[ "${actual[$key]}" != "${expected[$key]}" ]]; then
        printf '%s has %s=%q, want %q\n' \
            "$unit" "$key" "${actual[$key]}" "${expected[$key]}" >&2
        exit 1
    fi
done

echo "systemd hardening contract: OK"
