#!/usr/bin/env bash
set -euo pipefail
umask 022

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
repo_dir="$(cd -- "$script_dir/.." && pwd)"
go_command="${GO:-go}"
architecture="${GOARCH:-amd64}"
version="$(VERSION="${VERSION:-}" "$repo_dir/version.sh")"
debian_upstream="$(VERSION="$version" "$repo_dir/version.sh" --debian)"
debian_revision="${DEBIAN_REVISION:-1}"
# A release builds its artifacts before the descriptor commit and tag exist.
# Use the source commit timestamp unless the caller provides an explicit epoch.
if [[ -z "${SOURCE_DATE_EPOCH:-}" ]]; then
    SOURCE_DATE_EPOCH="$(git -C "$repo_dir" log -1 --format=%ct 2>/dev/null || true)"
    SOURCE_DATE_EPOCH="${SOURCE_DATE_EPOCH:-0}"
fi
if [[ ! "$SOURCE_DATE_EPOCH" =~ ^[0-9]+$ ]]; then
    echo "SOURCE_DATE_EPOCH must be a non-negative Unix timestamp" >&2
    exit 2
fi
export SOURCE_DATE_EPOCH
output_dir="${DIST_DIR:-$repo_dir/dist}"
package="unraid-vsock-sensors-hwmon"
build_dir="$(mktemp -d)"
package_root="$build_dir/root"
trap 'rm -rf -- "$build_dir"' EXIT

if [[ "$architecture" != "amd64" ]]; then
    echo "Le paquet Proxmox prend uniquement en charge amd64." >&2
    exit 2
fi
if [[ ! "$debian_revision" =~ ^[1-9][0-9]*$ ]]; then
    echo "Révision Debian invalide : $debian_revision" >&2
    exit 2
fi

# Debian sorts prereleases marked with '~' before the corresponding final
# release, while Git snapshots marked with '+dev' sort after their base.
# The final -N component remains the packaging revision.
debian_version="$debian_upstream-$debian_revision"
output="$output_dir/${package}_${debian_version}_${architecture}.deb"

mkdir -p \
    "$package_root/DEBIAN" \
    "$package_root/usr/bin" \
    "$package_root/usr/lib/modules-load.d" \
    "$package_root/usr/lib/systemd/system" \
    "$package_root/usr/share/$package" \
    "$package_root/usr/share/doc/$package/LICENSES" \
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
install -m 0644 "$repo_dir/LICENSE" \
    "$package_root/usr/share/doc/$package/LICENSE"
install -m 0644 "$script_dir/debian/copyright" \
    "$package_root/usr/share/doc/$package/copyright"
install -m 0644 "$repo_dir/THIRD_PARTY_NOTICES.md" \
    "$package_root/usr/share/doc/$package/THIRD_PARTY_NOTICES.md"
install -m 0644 "$repo_dir"/LICENSES/*.txt \
    "$package_root/usr/share/doc/$package/LICENSES/"
install -m 0644 "$script_dir/default" \
    "$package_root/usr/share/$package/unraid-vsock-hwmon.default"
printf 'virt-temp\n' > "$package_root/usr/lib/modules-load.d/virt-temp.conf"

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
