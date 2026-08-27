#!/usr/bin/env bash
set -euo pipefail

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
repo_dir="$(cd -- "$script_dir/.." && pwd)"
go_command="${GO:-go}"
architecture="${GOARCH:-amd64}"
version="$(VERSION="${VERSION:-}" "$repo_dir/version.sh")"
output_dir="${DIST_DIR:-$repo_dir/dist}"
package="unraid-vsock-sensors-hwmon"
build_dir="$(mktemp -d)"
package_root="$build_dir/root"
trap 'rm -rf -- "$build_dir"' EXIT

if [[ "$architecture" != "amd64" ]]; then
    echo "Le paquet Proxmox prend uniquement en charge amd64." >&2
    exit 2
fi

# Debian uses ~ for prereleases and reserves the final -N component for the
# packaging revision. A tagged 0.4.0 therefore becomes 0.4.0-1, while a Git
# development build sorts before it as 0.4.0~dev.N.gHASH-1.
debian_version="${version/-dev./~dev.}-1"
output="$output_dir/${package}_${debian_version}_${architecture}.deb"

mkdir -p \
    "$package_root/DEBIAN" \
    "$package_root/etc/default" \
    "$package_root/usr/bin" \
    "$package_root/usr/lib/modules-load.d" \
    "$package_root/usr/lib/systemd/system" \
    "$package_root/usr/share/doc/$package" \
    "$output_dir"

(
    cd "$repo_dir"
    CGO_ENABLED=0 GOOS=linux GOARCH="$architecture" \
        "$go_command" build -buildvcs=false -trimpath \
        -ldflags="-s -w -X main.version=$version" \
        -o "$package_root/usr/bin/unraid-vsock-sensors" .
)

VERSION="$version" "$script_dir/prepare-dkms.sh" \
    "$package_root/usr/src" >/dev/null
install -m 0644 "$script_dir/unraid-vsock-hwmon.service" \
    "$package_root/usr/lib/systemd/system/"
install -m 0644 "$script_dir/README.md" \
    "$package_root/usr/share/doc/$package/README.md"
install -m 0644 "$script_dir/default" \
    "$package_root/etc/default/unraid-vsock-hwmon"
printf 'virt-temp\n' > "$package_root/usr/lib/modules-load.d/virt-temp.conf"
printf '/etc/default/unraid-vsock-hwmon\n' > "$package_root/DEBIAN/conffiles"

sed -e "s/@DEBIAN_VERSION@/$debian_version/g" \
    -e "s/@ARCHITECTURE@/$architecture/g" \
    "$script_dir/debian/control.in" > "$package_root/DEBIAN/control"
for maintainer_script in postinst prerm postrm; do
    sed "s/@VERSION@/$version/g" \
        "$script_dir/debian/$maintainer_script.in" \
        > "$package_root/DEBIAN/$maintainer_script"
    chmod 0755 "$package_root/DEBIAN/$maintainer_script"
done

dpkg-deb --build --root-owner-group "$package_root" "$output" >/dev/null
echo "$output"
