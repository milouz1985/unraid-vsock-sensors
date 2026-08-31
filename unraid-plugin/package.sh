#!/usr/bin/env bash
set -euo pipefail

plugin_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
repo_dir="$(cd -- "$plugin_dir/.." && pwd)"
go_command="${GO:-go}"
version="$(VERSION="${VERSION:-}" "$repo_dir/version.sh")"
repository_url="${REPOSITORY_URL:-https://git.lan.home/francois/unraid-vsock-sensors}"
plugin_url="${PLUGIN_URL:-$repository_url/raw/branch/main/unraid-plugin/unraid-vsock-sensors.plg}"
package_version="${version//-/_}"
package_version="${package_version//+/_}"
package_name="unraid-vsock-sensors-${package_version}-x86_64-1.txz"
package_url="${PACKAGE_URL:-$repository_url/releases/download/v$version/$package_name}"
dist_dir="${DIST_DIR:-$repo_dir/dist}"
plugin_output="${PLUGIN_OUTPUT:-$plugin_dir/unraid-vsock-sensors.plg}"
build_dir="$(mktemp -d)"
stage_dir="$build_dir/package"
trap 'rm -rf "$build_dir"' EXIT

case "$version$plugin_url$package_url" in
    *'|'*|*'&'*) echo "Version and URLs must not contain | or &" >&2; exit 2 ;;
esac

mkdir -p \
    "$stage_dir/etc/rc.d" \
    "$stage_dir/install" \
    "$stage_dir/usr/local/emhttp/plugins/unraid-vsock-sensors/images" \
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
install -m 0644 "$plugin_dir/images/icon.png" \
    "$stage_dir/usr/local/emhttp/plugins/unraid-vsock-sensors/images/icon.png"
install -m 0644 "$plugin_dir/default.cfg" \
    "$stage_dir/usr/local/emhttp/plugins/unraid-vsock-sensors/default.cfg"
install -m 0644 "$repo_dir/LICENSE" \
    "$stage_dir/usr/local/emhttp/plugins/unraid-vsock-sensors/LICENSE"
install -m 0755 "$plugin_dir/service.sh" \
    "$stage_dir/usr/local/emhttp/plugins/unraid-vsock-sensors/service.sh"
install -m 0644 "$plugin_dir/slack-desc" "$stage_dir/install/slack-desc"

package_path="$dist_dir/$package_name"
# Fixed metadata makes identical sources produce the same package checksum.
tar --sort=name --mtime='UTC 1970-01-01' --owner=0 --group=0 --numeric-owner \
    -C "$stage_dir" -cJf "$package_path" .
package_md5="$(md5sum "$package_path" | cut -d' ' -f1)"
package_sha256="$(sha256sum "$package_path" | cut -d' ' -f1)"

sed \
    -e "s|@VERSION@|$version|g" \
    -e "s|@PLUGIN_URL@|$plugin_url|g" \
    -e "s|@PACKAGE_NAME@|$package_name|g" \
    -e "s|@PACKAGE_URL@|$package_url|g" \
    -e "s|@PACKAGE_MD5@|$package_md5|g" \
    -e "s|@PACKAGE_SHA256@|$package_sha256|g" \
    "$plugin_dir/unraid-vsock-sensors.plg.in" > "$plugin_output"
cp "$plugin_output" "$dist_dir/unraid-vsock-sensors.plg"

echo "$package_path"
echo "$dist_dir/unraid-vsock-sensors.plg"
