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
source_tree="$build_dir/source"
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

for tool in dpkg-buildpackage dh rsync; do
    command -v "$tool" >/dev/null || {
        echo "Outil de construction Debian manquant : $tool" >&2
        exit 1
    }
done

mkdir -p "$source_tree" "$output_dir"
rsync -a \
    --exclude=/.git \
    --exclude=/bin \
    --exclude=/dist \
    --exclude=/tests/vm/template.env \
    "$repo_dir/" "$source_tree/"

printf '%s (%s) unstable; urgency=medium\n\n  * Build project package.\n\n -- François HOYEZ <francois.hoyez@gmail.com>  %s\n' \
    unraid-vsock-sensors "$debian_version" \
    "$(date -u -R -d "@$SOURCE_DATE_EPOCH")" \
    > "$source_tree/debian/changelog"
(
    cd "$source_tree"
    GO="$go_command" UVSS_VERSION="$version" \
        dpkg-buildpackage -b -us -uc -d
)
install -m 0644 "$build_dir/${package}_${debian_version}_${architecture}.deb" "$output"
echo "$output"
