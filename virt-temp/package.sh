#!/usr/bin/env bash
set -euo pipefail

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
repo_dir="$(cd -- "$script_dir/.." && pwd)"
go_command="${GO:-go}"
architecture="${GOARCH:-amd64}"
version="$(VERSION="${VERSION:-}" "$repo_dir/version.sh")"
output_dir="${DIST_DIR:-$repo_dir/dist}"
package_name="unraid-vsock-sensors-hwmon-$version-linux-$architecture"
build_dir="$(mktemp -d)"
package_dir="$build_dir/$package_name"
trap 'rm -rf -- "$build_dir"' EXIT

case "$architecture" in
    amd64|arm64) ;;
    *) echo "Architecture non prise en charge : $architecture" >&2; exit 2 ;;
esac

mkdir -p "$package_dir" "$output_dir"
(
    cd "$repo_dir"
    CGO_ENABLED=0 GOOS=linux GOARCH="$architecture" \
        "$go_command" build -buildvcs=false -trimpath \
        -ldflags="-s -w -X main.version=$version" \
        -o "$package_dir/unraid-vsock-sensors" .
)

VERSION="$version" "$script_dir/prepare-dkms.sh" "$package_dir" >/dev/null
printf '%s\n' "$version" > "$package_dir/VERSION"
install -m 0755 "$script_dir/install.sh" "$package_dir/install.sh"
install -m 0644 "$script_dir/unraid-vsock-hwmon.service" \
    "$package_dir/unraid-vsock-hwmon.service"
install -m 0644 "$script_dir/README.md" "$package_dir/README.md"

archive="$output_dir/$package_name.tar.gz"
tar -C "$build_dir" -czf "$archive" "$package_name"
(
    cd "$output_dir"
    sha256sum "$package_name.tar.gz" > "$package_name.tar.gz.sha256"
)

echo "$archive"
echo "$archive.sha256"
