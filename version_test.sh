#!/usr/bin/env bash
set -euo pipefail

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
version_script="$script_dir/version.sh"

expect_release_version() {
    local version="$1" output
    output="$(VERSION="$version" "$version_script" --release)"
    if [[ "$output" != "$version" ]]; then
        echo "release version $version resolved to $output" >&2
        exit 1
    fi
}

expect_non_release_version() {
    local version="$1"
    if VERSION="$version" "$version_script" --release >/dev/null 2>&1; then
        echo "non-release version $version was accepted for release" >&2
        exit 1
    fi
}

expect_development_version() {
    local version="$1" output
    output="$(VERSION="$version" "$version_script")"
    if [[ "$output" != "$version" ]]; then
        echo "development version $version resolved to $output" >&2
        exit 1
    fi
}

for version in 0.0.0 1.7.0 10.20.30; do
    expect_release_version "$version"
done

for version in 1.7.0-rc.1 1.7.0+build.1 01.2.3 1.02.3 1.2.03; do
    expect_non_release_version "$version"
done

expect_development_version 1.7.0-rc.1
expect_development_version 1.7.0-dev.3.gabcdef
