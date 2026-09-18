#!/usr/bin/env bash
set -euo pipefail
unset VERSION REPOSITORY_URL PLUGIN_URL PACKAGE_URL

plugin_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
repo_dir="$(cd -- "$plugin_dir/.." && pwd)"
test_dir="$(mktemp -d)"
test_repo="$test_dir/repo"
trap 'rm -rf -- "$test_dir"' EXIT

mkdir -p "$test_repo/dist" "$test_repo/unraid-plugin"
cp "$repo_dir/Makefile" "$test_repo/Makefile"
cp "$repo_dir/version.sh" "$test_repo/version.sh"
cp "$plugin_dir/update-plg.sh" "$test_repo/unraid-plugin/update-plg.sh"
cp "$plugin_dir/unraid-vsock-sensors.plg.in" \
    "$test_repo/unraid-plugin/unraid-vsock-sensors.plg.in"

if "$test_repo/unraid-plugin/update-plg.sh" > "$test_dir/output" 2>&1; then
    echo "update-plg accepted a missing VERSION" >&2
    exit 1
fi
grep -Fq "VERSION is required" "$test_dir/output"

if make --no-print-directory -C "$test_repo" update-plg VERSION=2.0.0-rc.1 \
    > "$test_dir/output" 2>&1; then
    echo "update-plg accepted a prerelease" >&2
    exit 1
fi
grep -Fq "Version de release invalide" "$test_dir/output"

if VERSION=2.0.0 "$test_repo/unraid-plugin/update-plg.sh" \
    > "$test_dir/output" 2>&1; then
    echo "update-plg accepted a missing package" >&2
    exit 1
fi
grep -Fq "make unraid-package VERSION=2.0.0" "$test_dir/output"
grep -Fq "make artifacts VERSION=2.0.0" "$test_dir/output"

package_name="unraid-vsock-sensors-2.0.0-x86_64-1.txz"
package_path="$test_repo/dist/$package_name"
plugin_output="$test_repo/unraid-plugin/unraid-vsock-sensors.plg"
printf 'test package\n' > "$package_path"
package_md5="$(md5sum "$package_path" | cut -d' ' -f1)"
package_sha256="$(sha256sum "$package_path" | cut -d' ' -f1)"

output="$(make --no-print-directory -C "$test_repo" update-plg VERSION=2.0.0)"
if [[ "$output" != "$plugin_output" ]]; then
    echo "update-plg output $output, expected $plugin_output" >&2
    exit 1
fi
grep -Fq '<!ENTITY version "2.0.0">' "$plugin_output"
grep -Fq '<!ENTITY pluginURL "https://raw.githubusercontent.com/milouz1985/unraid-vsock-sensors/main/unraid-plugin/unraid-vsock-sensors.plg">' "$plugin_output"
grep -Fq 'support="https://github.com/milouz1985/unraid-vsock-sensors/issues"' "$plugin_output"
grep -Fq 'project="https://github.com/milouz1985/unraid-vsock-sensors"' "$plugin_output"
grep -Fq "<!ENTITY package \"$package_name\">" "$plugin_output"
grep -Fq "<!ENTITY packageURL \"https://github.com/milouz1985/unraid-vsock-sensors/releases/download/v2.0.0/$package_name\">" "$plugin_output"
grep -Fq "<!ENTITY packageMD5 \"$package_md5\">" "$plugin_output"
grep -Fq "<!ENTITY packageSHA256 \"$package_sha256\">" "$plugin_output"
if grep -Fq 'rm -f "$cfgdir/$name.cfg"' "$plugin_output"; then
    echo "generated plugin still deletes persistent configuration on uninstall" >&2
    exit 1
fi
grep -Fq 'Configuration preserved in $cfgdir' "$plugin_output"
if [[ "$(stat -c '%a' "$plugin_output")" != "644" ]]; then
    echo "update-plg did not create the public descriptor with mode 0644" >&2
    exit 1
fi
if [[ -e "$test_repo/dist/unraid-vsock-sensors.plg" ]]; then
    echo "update-plg created an intermediate descriptor in dist" >&2
    exit 1
fi

VERSION=2.0.0 \
REPOSITORY_URL="https://example.invalid/repository" \
    "$test_repo/unraid-plugin/update-plg.sh" >/dev/null
grep -Fq "<!ENTITY packageURL \"https://example.invalid/repository/releases/download/v2.0.0/$package_name\">" "$plugin_output"

VERSION=2.0.0 \
PLUGIN_URL="https://example.invalid/plugin.plg" \
PACKAGE_URL="https://example.invalid/package.txz" \
    "$test_repo/unraid-plugin/update-plg.sh" >/dev/null
grep -Fq '<!ENTITY pluginURL "https://example.invalid/plugin.plg">' "$plugin_output"
grep -Fq '<!ENTITY packageURL "https://example.invalid/package.txz">' "$plugin_output"

checksum_before="$(sha256sum "$plugin_output" | cut -d' ' -f1)"
if VERSION=2.0.0 PLUGIN_URL='https://example.invalid/plugin&bad.plg' \
    "$test_repo/unraid-plugin/update-plg.sh" > "$test_dir/output" 2>&1; then
    echo "update-plg accepted an unsafe URL" >&2
    exit 1
fi
grep -Fq "Version and URLs must not contain | or &" "$test_dir/output"
checksum_after="$(sha256sum "$plugin_output" | cut -d' ' -f1)"
if [[ "$checksum_after" != "$checksum_before" ]]; then
    echo "failed update-plg invocation modified the public descriptor" >&2
    exit 1
fi
