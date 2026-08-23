#!/usr/bin/env bash
set -euo pipefail

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
repo_dir="$(cd -- "$script_dir/.." && pwd)"
architecture="${GOARCH:-amd64}"
version="$(sed -n 's/^version = "\([^"]*\)"/\1/p' "$script_dir/manifest.toml")"

case "$architecture" in
    amd64|arm64) ;;
    *) echo "Architecture non prise en charge : $architecture" >&2; exit 2 ;;
esac
if [[ -z "$version" ]]; then
    echo "Version absente du manifeste" >&2
    exit 1
fi

package_name="unraid-vsock-sensors-cc-${version}-linux-${architecture}"
build_dir="$(mktemp -d)"
package_dir="$build_dir/$package_name"
output_dir="$repo_dir/dist"
trap 'rm -rf "$build_dir"' EXIT

mkdir -p "$package_dir/ui" "$output_dir"
(
    cd "$script_dir"
    CGO_ENABLED=0 GOOS=linux GOARCH="$architecture" \
        go build -trimpath -ldflags="-s -w" -o "$package_dir/unraid-vsock-sensors-cc" .
)
install -m 0644 "$script_dir/manifest.toml" "$package_dir/manifest.toml"
install -m 0644 "$script_dir/ui/index.html" "$package_dir/ui/index.html"
install -m 0644 "$script_dir/README.md" "$package_dir/README.md"
install -m 0755 "$script_dir/install.sh" "$package_dir/install.sh"

archive="$output_dir/$package_name.tar.gz"
tar -C "$build_dir" -czf "$archive" "$package_name"
(
    cd "$output_dir"
    sha256sum "$package_name.tar.gz" > "$package_name.tar.gz.sha256"
)
echo "$archive"
