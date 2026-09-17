#!/usr/bin/env bash
set -euo pipefail

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
test_dir="$(mktemp -d)"
trap 'rm -rf -- "$test_dir"' EXIT

cat > "$test_dir/binary" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$@" > "$UVSS_SERVICE_TEST_BINARY_ARGS"
# Simulate the Go CLI reporting an error on stderr (as the real daemon does).
if [[ -n "${UVSS_SERVICE_TEST_BINARY_STDERR:-}" ]]; then
    printf '%s\n' "$UVSS_SERVICE_TEST_BINARY_STDERR" >&2
fi
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
rm -f "$test_dir/binary.args" "$test_dir/rc.args"

# set-disk-policy forwards to the CLI client (which talks to the control
# socket) and must NOT invoke the rc refresh action anymore.
"$script_dir/service.sh" set-disk-policy V0RDX0lE include
mapfile -t binary_args < "$test_dir/binary.args"
[[ "${binary_args[*]}" == "disks set --id-base64 V0RDX0lE --policy include" ]]
if [[ -e "$test_dir/rc.args" ]]; then
    echo "set-disk-policy invoked the rc refresh action" >&2
    exit 1
fi

# A failed policy write must fail and must not fall through to the rc script.
rm -f "$test_dir/rc.args"
export UVSS_SERVICE_TEST_BINARY_STATUS=7
if "$script_dir/service.sh" set-disk-policy V0RDX0lE exclude; then
    echo "service accepted a failed policy write" >&2
    exit 1
fi
[[ ! -e "$test_dir/rc.args" ]]

# reset-disk-policies forwards to the CLI client and must NOT invoke the rc
# refresh action anymore.
unset UVSS_SERVICE_TEST_BINARY_STATUS
"$script_dir/service.sh" reset-disk-policies
mapfile -t binary_args < "$test_dir/binary.args"
[[ "${binary_args[*]}" == "disks reset" ]]
if [[ -e "$test_dir/rc.args" ]]; then
    echo "reset-disk-policies invoked the rc refresh action" >&2
    exit 1
fi

# A failed reset must fail and must not fall through to the rc script.
rm -f "$test_dir/rc.args"
export UVSS_SERVICE_TEST_BINARY_STATUS=7
if "$script_dir/service.sh" reset-disk-policies; then
    echo "service accepted a failed disk policy reset" >&2
    exit 1
fi
[[ ! -e "$test_dir/rc.args" ]]

unset UVSS_SERVICE_TEST_BINARY_STATUS

# A CLI error written on stderr must be retrievable on stdout through
# service.sh (the Unraid WebUI only captures stdout). The CLI exits non-zero on
# error, so the command substitution is guarded against `set -e`.
export UVSS_SERVICE_TEST_BINARY_STATUS=1
export UVSS_SERVICE_TEST_BINARY_STDERR="unraid-vsock-sensors daemon is not running"
stderr_to_stdout=""
stderr_to_stdout="$("$script_dir/service.sh" set-disk-policy V0RDX0lE include 2>/dev/null)" || true
if [[ "$stderr_to_stdout" != *"unraid-vsock-sensors daemon is not running"* ]]; then
    echo "set-disk-policy stderr was not surfaced on stdout: $stderr_to_stdout" >&2
    exit 1
fi
stderr_to_stdout=""
stderr_to_stdout="$("$script_dir/service.sh" reset-disk-policies 2>/dev/null)" || true
if [[ "$stderr_to_stdout" != *"unraid-vsock-sensors daemon is not running"* ]]; then
    echo "reset-disk-policies stderr was not surfaced on stdout: $stderr_to_stdout" >&2
    exit 1
fi
unset UVSS_SERVICE_TEST_BINARY_STATUS UVSS_SERVICE_TEST_BINARY_STDERR

# Other actions are forwarded to the rc script unchanged.
"$script_dir/service.sh" start
mapfile -t rc_args < "$test_dir/rc.args"
[[ "${rc_args[*]}" == "start" ]]
