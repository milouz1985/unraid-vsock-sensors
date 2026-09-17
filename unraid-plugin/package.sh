#!/usr/bin/env bash
set -euo pipefail
umask 022

plugin_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
repo_dir="$(cd -- "$plugin_dir/.." && pwd)"
go_command="${GO:-go}"
version="$(VERSION="${VERSION:-}" "$repo_dir/version.sh")"
package_version="${version//-/_}"
package_version="${package_version//+/_}"
package_name="unraid-vsock-sensors-${package_version}-x86_64-1.txz"
dist_dir="${DIST_DIR:-$repo_dir/dist}"
build_dir="$(mktemp -d)"
stage_dir="$build_dir/package"
trap 'rm -rf "$build_dir"' EXIT

mkdir -p \
    "$stage_dir/etc/rc.d" \
    "$stage_dir/install" \
    "$stage_dir/usr/local/emhttp/plugins/unraid-vsock-sensors/event" \
    "$stage_dir/usr/local/emhttp/plugins/unraid-vsock-sensors/LICENSES" \
    "$stage_dir/usr/local/sbin" \
    "$dist_dir"

(
    cd "$repo_dir"
    CGO_ENABLED=0 GOOS=linux GOARCH=amd64 "$go_command" build \
        -buildvcs=false -trimpath -ldflags="-s -w -X main.version=$version" \
        -o "$stage_dir/usr/local/sbin/unraid-vsock-sensors" .
)
install -m 0755 "$plugin_dir/rc.unraid-vsock-sensors" "$stage_dir/etc/rc.d/rc.unraid-vsock-sensors"
install -m 0644 "$plugin_dir/UnraidVsockSensors.page" \
    "$stage_dir/usr/local/emhttp/plugins/unraid-vsock-sensors/UnraidVsockSensors.page"
install -m 0644 "$plugin_dir/UnraidVsockSensorsDiagnostics.page" \
    "$stage_dir/usr/local/emhttp/plugins/unraid-vsock-sensors/UnraidVsockSensorsDiagnostics.page"
install -m 0644 "$plugin_dir/uvss_control.php" \
    "$stage_dir/usr/local/emhttp/plugins/unraid-vsock-sensors/uvss_control.php"
install -m 0644 "$plugin_dir/uvss_action.php" \
    "$stage_dir/usr/local/emhttp/plugins/unraid-vsock-sensors/uvss_action.php"
install -m 0644 "$plugin_dir/images/icon.png" \
    "$stage_dir/usr/local/emhttp/plugins/unraid-vsock-sensors/icon.png"
install -m 0644 "$plugin_dir/default.cfg" \
    "$stage_dir/usr/local/emhttp/plugins/unraid-vsock-sensors/default.cfg"
install -m 0755 "$plugin_dir/poll_attributes" \
    "$stage_dir/usr/local/emhttp/plugins/unraid-vsock-sensors/event/poll_attributes"
install -m 0644 "$repo_dir/LICENSE" \
    "$stage_dir/usr/local/emhttp/plugins/unraid-vsock-sensors/LICENSE"
install -m 0644 "$repo_dir/THIRD_PARTY_NOTICES.md" \
    "$stage_dir/usr/local/emhttp/plugins/unraid-vsock-sensors/THIRD_PARTY_NOTICES.md"
install -m 0644 "$repo_dir"/LICENSES/*.txt \
    "$stage_dir/usr/local/emhttp/plugins/unraid-vsock-sensors/LICENSES/"
install -m 0755 "$plugin_dir/service.sh" \
    "$stage_dir/usr/local/emhttp/plugins/unraid-vsock-sensors/service.sh"
install -m 0644 "$plugin_dir/slack-desc" "$stage_dir/install/slack-desc"

package_path="$dist_dir/$package_name"
# Fixed metadata makes identical sources produce the same package checksum.
tar --sort=name --mtime='UTC 1970-01-01' --owner=0 --group=0 --numeric-owner \
    -C "$stage_dir" -cJf "$package_path" .

echo "$package_path"
