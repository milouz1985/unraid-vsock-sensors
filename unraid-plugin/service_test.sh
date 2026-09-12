#!/usr/bin/env bash
set -euo pipefail

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
test_dir="$(mktemp -d)"
trap 'rm -rf -- "$test_dir"' EXIT

cat > "$test_dir/binary" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$@" > "$UVSS_SERVICE_TEST_BINARY_ARGS"
exit "${UVSS_SERVICE_TEST_BINARY_STATUS:-0}"
EOF
cat > "$test_dir/rc" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$@" > "$UVSS_SERVICE_TEST_RC_ARGS"
EOF
chmod 0755 "$test_dir/binary" "$test_dir/rc"

export UVSS_SERVICE_BINARY="$test_dir/binary"
export UVSS_SERVICE_RC="$test_dir/rc"
export UVSS_SERVICE_TEST_BINARY_ARGS="$test_dir/binary.args"
export UVSS_SERVICE_TEST_RC_ARGS="$test_dir/rc.args"

"$script_dir/service.sh" set-disk-policy V0RDX0lE include
mapfile -t binary_args < "$test_dir/binary.args"
mapfile -t rc_args < "$test_dir/rc.args"
[[ "${binary_args[*]}" == "disks set --id-base64 V0RDX0lE --policy include" ]]
[[ "${rc_args[*]}" == "refresh" ]]

rm -f "$test_dir/rc.args"
export UVSS_SERVICE_TEST_BINARY_STATUS=7
if "$script_dir/service.sh" set-disk-policy V0RDX0lE exclude; then
    echo "service accepted a failed policy write" >&2
    exit 1
fi
[[ ! -e "$test_dir/rc.args" ]]
