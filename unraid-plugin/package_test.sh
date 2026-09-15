#!/usr/bin/env bash
set -euo pipefail

plugin_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
build_dir="$(mktemp -d)"
trap 'rm -rf "$build_dir"' EXIT

cat >"$build_dir/fake-go" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
while (($#)); do
    if [[ $1 == -o ]]; then
        cp /bin/true "$2"
        exit 0
    fi
    shift
done
exit 1
EOF
chmod +x "$build_dir/fake-go"

package_path="$(GO="$build_dir/fake-go" VERSION=2.0.1 DIST_DIR="$build_dir" "$plugin_dir/package.sh")"
package_members="$(tar -tJf "$package_path")"
for page in UnraidVsockSensors.page UnraidVsockSensorsDiagnostics.page; do
    expected="./usr/local/emhttp/plugins/unraid-vsock-sensors/$page"
    if ! grep -Fxq "$expected" <<<"$package_members"; then
        echo "package missing $page" >&2
        exit 1
    fi
done

grep -Fq 'href="/Settings/UnraidVsockSensorsDiagnostics"' "$plugin_dir/UnraidVsockSensors.page"
echo 'Unraid plugin page packaging: OK'
