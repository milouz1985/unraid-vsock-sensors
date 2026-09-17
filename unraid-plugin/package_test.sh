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
for web_file in UnraidVsockSensors.page UnraidVsockSensorsDiagnostics.page uvss_control.php uvss_action.php service.sh; do
    expected="./usr/local/emhttp/plugins/unraid-vsock-sensors/$web_file"
    if ! grep -Fxq "$expected" <<<"$package_members"; then
        echo "package missing $web_file" >&2
        exit 1
    fi
done

grep -Fq 'href="/Settings/UnraidVsockSensorsDiagnostics"' "$plugin_dir/UnraidVsockSensors.page"
grep -Fq 'action="/plugins/unraid-vsock-sensors/uvss_action.php"' "$plugin_dir/UnraidVsockSensors.page"
grep -Fq 'exec /etc/rc.d/rc.unraid-vsock-sensors "$@"' "$plugin_dir/service.sh"
if grep -Eq 'disks (set|reset|list|validate|refresh)' "$plugin_dir/service.sh"; then
    echo 'service.sh still contains disk-policy CLI plumbing' >&2
    exit 1
fi
echo 'Unraid plugin page packaging: OK'
