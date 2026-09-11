#!/usr/bin/env bash
set -euo pipefail
umask 022

plugin_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
repo_dir="$(cd -- "$plugin_dir/.." && pwd)"

if [[ -z "${VERSION:-}" ]]; then
    echo "VERSION is required (example: make update-plg VERSION=1.7.0)" >&2
    exit 2
fi

version="$(VERSION="$VERSION" "$repo_dir/version.sh" --release)"
repository_url="${REPOSITORY_URL:-https://github.com/milouz1985/unraid-vsock-sensors}"
plugin_url="${PLUGIN_URL:-https://raw.githubusercontent.com/milouz1985/unraid-vsock-sensors/main/unraid-plugin/unraid-vsock-sensors.plg}"
package_name="unraid-vsock-sensors-${version}-x86_64-1.txz"
package_path="$repo_dir/dist/$package_name"
package_url="${PACKAGE_URL:-$repository_url/releases/download/v$version/$package_name}"
plugin_output="$plugin_dir/unraid-vsock-sensors.plg"

case "$version$plugin_url$package_url" in
    *'|'*|*'&'*) echo "Version and URLs must not contain | or &" >&2; exit 2 ;;
esac

if [[ ! -f "$package_path" ]]; then
    echo "Missing $package_path; run make unraid-package VERSION=$version or make artifacts VERSION=$version first" >&2
    exit 1
fi

package_md5="$(md5sum "$package_path" | cut -d' ' -f1)"
package_sha256="$(sha256sum "$package_path" | cut -d' ' -f1)"
temporary_output="$(mktemp "$plugin_dir/.unraid-vsock-sensors.plg.XXXXXX")"
trap 'rm -f -- "$temporary_output"' EXIT

sed \
    -e "s|@VERSION@|$version|g" \
    -e "s|@PLUGIN_URL@|$plugin_url|g" \
    -e "s|@PACKAGE_NAME@|$package_name|g" \
    -e "s|@PACKAGE_URL@|$package_url|g" \
    -e "s|@PACKAGE_MD5@|$package_md5|g" \
    -e "s|@PACKAGE_SHA256@|$package_sha256|g" \
    "$plugin_dir/unraid-vsock-sensors.plg.in" > "$temporary_output"
chmod 0644 "$temporary_output"
mv -f -- "$temporary_output" "$plugin_output"
trap - EXIT

echo "$plugin_output"
