#!/usr/bin/env bash
set -euo pipefail

fail() { echo "$1" >&2; exit "${2:-1}"; }

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
repo_dir="$(cd -- "$script_dir/.." && pwd)"
source_dir="$script_dir/module"

if (( $# != 1 )); then
    fail "Usage: $0 OUTPUT_DIRECTORY" 2
fi

version="$(VERSION="${VERSION:-}" "$repo_dir/version.sh")"
destination_root="$1"
destination="$destination_root/virt-temp-$version"

if [[ -e "$destination" ]]; then
    fail "Le répertoire de destination existe déjà : $destination"
fi

install -d -m 0755 "$destination"
install -m 0644 "$source_dir/Makefile" "$source_dir/virt-temp.c" "$destination/"
sed "s/@VERSION@/$version/g" "$source_dir/dkms.conf.in" > "$destination/dkms.conf"

printf '%s\n' "$destination"
