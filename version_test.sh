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

expect_invalid_version() {
    local version="$1"
    if VERSION="$version" "$version_script" >/dev/null 2>&1; then
        echo "invalid version $version was accepted" >&2
        exit 1
    fi
}

expect_debian_version() {
    local version="$1" expected="$2" output
    output="$(VERSION="$version" "$version_script" --debian)"
    if [[ "$output" != "$expected" ]]; then
        echo "Debian version for $version resolved to $output, expected $expected" >&2
        exit 1
    fi
}

expect_debian_version_less_than() {
    local left="$1" right="$2"
    if ! dpkg --compare-versions "$left" lt "$right"; then
        echo "Debian version $left does not sort before $right" >&2
        exit 1
    fi
}

for version in 0.0.0 1.7.0 10.20.30; do
    expect_release_version "$version"
done

for version in 1.7.0-rc.1 1.7.0+build.1 01.2.3 1.02.3 1.2.03; do
    expect_non_release_version "$version"
done

for version in \
    1.7.0-rc.1 \
    1.7.0-dev.3.gabcdef \
    1.7.0-dev.3.gabcdef.dirty \
    1.7.0-alpha-beta \
    1.7.0-0.3.7 \
    1.7.0+build.1 \
    1.7.0-rc.1+build.5; do
    expect_development_version "$version"
done

for version in \
    1.7.0-. \
    1.7.0-.. \
    1.7.0-rc..1 \
    01.2.3 \
    01.2.3-rc.1 \
    1.02.3 \
    1.2.03 \
    1.7.0-rc.01 \
    1.7.0- \
    1.7.0+ \
    1.7.0+build..1 \
    1.7.0-rc_1; do
    expect_invalid_version "$version"
done

expect_debian_version 1.7.0 1.7.0
expect_debian_version 1.7.0-rc.1 1.7.0~rc.1
expect_debian_version 1.7.0-rc.2 1.7.0~rc.2
expect_debian_version 1.7.0-dev.4.gabcdef 1.7.0+dev.4.gabcdef
expect_debian_version 1.7.0-rc.1-dev.2.gabcdef 1.7.0~rc.1+dev.2.gabcdef
expect_debian_version 0.0.0-vmtest.3 0.0.0~vmtest.3

if command -v dpkg >/dev/null 2>&1; then
    debian_versions=(
        1.7.0~rc.1-1
        1.7.0~rc.1+dev.2.gabcdef-1
        1.7.0~rc.2-1
        1.7.0-1
        1.7.0+dev.4.gabcdef-1
        1.7.1-1
    )
    for ((i = 1; i < ${#debian_versions[@]}; i++)); do
        expect_debian_version_less_than \
            "${debian_versions[i - 1]}" "${debian_versions[i]}"
    done
fi
