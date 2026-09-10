#!/bin/bash
set -euo pipefail

# The rc script starts this file as a stand-in daemon during the test. Preserve
# its path as argv[0] so running_pid() recognizes it after exec.
if [[ "${1:-}" == "serve" ]]; then
    if [[ -n "${UVSS_RC_TEST_ARGS_FILE:-}" ]]; then
        printf '%s\n' "$@" > "$UVSS_RC_TEST_ARGS_FILE"
    fi
    if [[ "${UVSS_RC_TEST_IGNORE_TERM:-0}" == 1 ]]; then
        trap '' TERM
    fi
    exec -a "$0" sleep 30
fi

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
rc_script="$script_dir/rc.unraid-vsock-sensors"
test_dir="$(mktemp -d)"
pid_file="$test_dir/service.pid"
args_file="$test_dir/service.args"
pipe_reader_pid=""

cleanup() {
    UVSS_RC_BINARY="$script_dir/rc_test.sh" \
        UVSS_RC_CONFIG="$test_dir/missing.cfg" \
        UVSS_RC_PID_FILE="$pid_file" \
        UVSS_RC_TEST_ARGS_FILE="$args_file" \
        "$rc_script" stop >/dev/null 2>&1 || true
    exec 9>&- 2>/dev/null || true
    if [[ -n "$pipe_reader_pid" ]]; then
        wait "$pipe_reader_pid" 2>/dev/null || true
    fi
    rm -rf -- "$test_dir"
}
trap cleanup EXIT

# Simulate Plugin Manager: stdout is captured by a pipe and an unrelated pipe
# is inherited as fd 9. A daemon retaining either one would keep the caller or
# its pipe reader alive after restart returns.
exec 9> >(cat >/dev/null)
pipe_reader_pid=$!

run_rc() {
    local action="$1" ignore_term="${2:-0}"
    timeout 10 env \
        UVSS_RC_BINARY="$script_dir/rc_test.sh" \
        UVSS_RC_CONFIG="$test_dir/missing.cfg" \
        UVSS_RC_PID_FILE="$pid_file" \
        UVSS_RC_TEST_ARGS_FILE="$args_file" \
        UVSS_RC_TEST_IGNORE_TERM="$ignore_term" \
        "$rc_script" "$action"
}

run_rc start >/dev/null
restart_output="$(run_rc restart)"
if [[ "$restart_output" != *"started (PID "* ]]; then
    echo "restart did not report a running daemon: $restart_output" >&2
    exit 1
fi

read -r daemon_pid < "$pid_file"
if ! kill -0 "$daemon_pid" 2>/dev/null; then
    echo "daemon $daemon_pid is not running after restart" >&2
    exit 1
fi
if ! grep -Fxq -- "--syslog" "$args_file"; then
    echo "daemon was not started with syslog logging" >&2
    exit 1
fi
mapfile -t daemon_args < "$args_file"
expected_args=(serve --port 990 --hba-mode enabled --hba-backend mpt3ctl --hba-interval 15s --disk-interval 30s --syslog)
if [[ "${daemon_args[*]}" != "${expected_args[*]}" ]]; then
    echo "unexpected daemon arguments: ${daemon_args[*]}" >&2
    exit 1
fi
for descriptor in "/proc/$daemon_pid/fd/"*; do
    target="$(readlink "$descriptor" 2>/dev/null || true)"
    if [[ "$target" == pipe:* ]]; then
        echo "daemon $daemon_pid retained $descriptor -> $target" >&2
        exit 1
    fi
done

run_rc stop >/dev/null

echo "Checking SIGKILL fallback for a daemon ignoring SIGTERM"
run_rc start 1 >/dev/null
read -r stubborn_pid < "$pid_file"
stop_output="$(run_rc stop 2>&1)"
if [[ "$stop_output" != *"sending SIGKILL"* ||
      "$stop_output" != *"stopped after SIGKILL"* ]]; then
    echo "stop did not report the SIGKILL fallback: $stop_output" >&2
    exit 1
fi
if kill -0 "$stubborn_pid" 2>/dev/null; then
    echo "daemon $stubborn_pid survived the SIGKILL fallback" >&2
    exit 1
fi
if [[ -e "$pid_file" ]]; then
    echo "PID file still exists after the SIGKILL fallback" >&2
    exit 1
fi
