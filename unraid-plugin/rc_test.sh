#!/bin/bash
set -euo pipefail

# The rc script starts this file as a stand-in daemon during the test. Preserve
# its path as argv[0] so running_pid() recognizes it after exec.
if [[ "${1:-}" == "serve" ]]; then
    if [[ -n "${UVSS_RC_TEST_ARGS_FILE:-}" ]]; then
        printf '%s\n' "$@" > "$UVSS_RC_TEST_ARGS_FILE"
    fi
    if [[ -n "${UVSS_RC_TEST_STARTED_FILE:-}" ]]; then
        printf '%s\n' "$$" >> "$UVSS_RC_TEST_STARTED_FILE"
    fi
    exec -a "$0" bash -c '
        if [[ "${UVSS_RC_TEST_IGNORE_TERM:-0}" == 1 ]]; then
            trap "" TERM
        else
            trap "exit 0" TERM
        fi
        trap '\''printf "refresh\n" > "$UVSS_RC_TEST_REFRESH_FILE"'\'' USR1
        while :; do
            sleep 0.1 &
            wait "$!" || true
        done
    '
fi

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
rc_script="$script_dir/rc.unraid-vsock-sensors"
test_dir="$(mktemp -d)"
pid_file="$test_dir/service.pid"
lock_file="$test_dir/service.lock"
args_file="$test_dir/service.args"
refresh_file="$test_dir/service.refresh"
started_file="$test_dir/service.started"
pipe_reader_pid=""
foreign_pid=""

cleanup() {
    UVSS_RC_BINARY="$script_dir/rc_test.sh" \
        UVSS_RC_CONFIG="$test_dir/missing.cfg" \
        UVSS_RC_PID_FILE="$pid_file" \
        UVSS_RC_LOCK_FILE="$lock_file" \
        UVSS_RC_TEST_ARGS_FILE="$args_file" \
        "$rc_script" stop >/dev/null 2>&1 || true
    if [[ -n "$foreign_pid" ]]; then
        kill "$foreign_pid" 2>/dev/null || true
        wait "$foreign_pid" 2>/dev/null || true
    fi
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
    local action="$1" ignore_term="${2:-0}" config_path="${3:-$test_dir/missing.cfg}"
    timeout 10 env \
        UVSS_RC_BINARY="$script_dir/rc_test.sh" \
        UVSS_RC_CONFIG="$config_path" \
        UVSS_RC_PID_FILE="$pid_file" \
        UVSS_RC_LOCK_FILE="$lock_file" \
        UVSS_RC_TEST_ARGS_FILE="$args_file" \
        UVSS_RC_TEST_REFRESH_FILE="$refresh_file" \
        UVSS_RC_TEST_STARTED_FILE="$started_file" \
        UVSS_RC_TEST_IGNORE_TERM="$ignore_term" \
        "$rc_script" "$action"
}

echo "Checking concurrent starts"
run_rc start > "$test_dir/start-1.output" &
start_1_pid=$!
run_rc start > "$test_dir/start-2.output" &
start_2_pid=$!
wait "$start_1_pid"
wait "$start_2_pid"

mapfile -t started_pids < "$started_file"
if (( ${#started_pids[@]} != 1 )); then
    echo "concurrent starts launched ${#started_pids[@]} daemons instead of one" >&2
    exit 1
fi
if [[ ! -r "$pid_file" ]]; then
    echo "concurrent starts did not leave a PID file" >&2
    exit 1
fi
read -r daemon_pid < "$pid_file"
if [[ "$daemon_pid" != "${started_pids[0]}" ]] || ! kill -0 "$daemon_pid" 2>/dev/null; then
    echo "PID file does not identify the daemon started concurrently" >&2
    exit 1
fi
run_rc stop >/dev/null
rm -f "$started_file"

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
expected_args=(serve --port 990 --hba-mode enabled --hba-backend mpt3ctl --syslog)
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

refresh_started="$(date +%s%N)"
refresh_output="$(run_rc refresh)"
refresh_elapsed=$(( $(date +%s%N) - refresh_started ))
if [[ "$refresh_output" != *"refresh requested"* ]]; then
    echo "refresh did not report success: $refresh_output" >&2
    exit 1
fi
for _ in {1..100}; do
    [[ -e "$refresh_file" ]] && break
    sleep 0.01
done
if [[ ! -e "$refresh_file" ]]; then
    echo "active daemon did not receive SIGUSR1" >&2
    exit 1
fi
if (( refresh_elapsed >= 1000000000 )); then
    echo "refresh waited too long: ${refresh_elapsed}ns" >&2
    exit 1
fi
if ! kill -0 "$daemon_pid" 2>/dev/null; then
    echo "daemon exited after refresh" >&2
    exit 1
fi

run_rc stop >/dev/null

run_rc start 0 "$script_dir/default.cfg" >/dev/null
mapfile -t daemon_args < "$args_file"
if [[ "${daemon_args[*]}" != "${expected_args[*]}" ]]; then
    echo "default plugin config forced an interval: ${daemon_args[*]}" >&2
    exit 1
fi
run_rc stop >/dev/null

for backend in mpt3ctl storcli; do
    printf 'HBA_BACKEND="%s"\n' "$backend" > "$test_dir/hba.cfg"
    run_rc start 0 "$test_dir/hba.cfg" >/dev/null
    mapfile -t daemon_args < "$args_file"
    expected_args=(serve --port 990 --hba-mode enabled --hba-backend "$backend" --syslog)
    if [[ "${daemon_args[*]}" != "${expected_args[*]}" ]]; then
        echo "$backend default forced an interval: ${daemon_args[*]}" >&2
        exit 1
    fi
    run_rc stop >/dev/null

    printf 'HBA_BACKEND="%s"\nHBA_INTERVAL="1m"\n' "$backend" > "$test_dir/hba.cfg"
    run_rc start 0 "$test_dir/hba.cfg" >/dev/null
    mapfile -t daemon_args < "$args_file"
    expected_args=(serve --port 990 --hba-mode enabled --hba-backend "$backend" --hba-interval 1m --syslog)
    if [[ "${daemon_args[*]}" != "${expected_args[*]}" ]]; then
        echo "$backend explicit interval was lost: ${daemon_args[*]}" >&2
        exit 1
    fi
    run_rc stop >/dev/null

    if [[ "$backend" == mpt3ctl ]]; then
        backend_interval=10s
        other_interval=5m
    else
        backend_interval=5m
        other_interval=15s
    fi
    printf 'HBA_BACKEND="%s"\nHBA_INTERVAL="%s"\n' "$backend" "$backend_interval" > "$test_dir/hba.cfg"
    run_rc start 0 "$test_dir/hba.cfg" >/dev/null
    mapfile -t daemon_args < "$args_file"
    expected_args=(serve --port 990 --hba-mode enabled --hba-backend "$backend" --hba-interval "$backend_interval" --syslog)
    if [[ "${daemon_args[*]}" != "${expected_args[*]}" ]]; then
        echo "$backend rejected its own interval: ${daemon_args[*]}" >&2
        exit 1
    fi
    run_rc stop >/dev/null

    printf 'HBA_BACKEND="%s"\nHBA_INTERVAL="45s"\n' "$backend" > "$test_dir/hba.cfg"
    if run_rc start 0 "$test_dir/hba.cfg" > "$test_dir/invalid.output" 2>&1; then
        echo "$backend accepted an interval outside its fixed choices" >&2
        exit 1
    fi
    if ! grep -Fq "refresh interval: 45s" "$test_dir/invalid.output"; then
        echo "unexpected $backend invalid interval error: $(cat "$test_dir/invalid.output")" >&2
        exit 1
    fi

    printf 'HBA_BACKEND="%s"\nHBA_INTERVAL="%s"\n' "$backend" "$other_interval" > "$test_dir/hba.cfg"
    if run_rc start 0 "$test_dir/hba.cfg" > "$test_dir/invalid.output" 2>&1; then
        echo "$backend accepted an interval reserved for the other backend" >&2
        exit 1
    fi
done

refresh_output="$(run_rc refresh)"
if [[ "$refresh_output" != *"is not running"* ]]; then
    echo "refresh without daemon failed unexpectedly: $refresh_output" >&2
    exit 1
fi

sleep 30 &
foreign_pid=$!
printf '%s\n' "$foreign_pid" > "$pid_file"
refresh_output="$(run_rc refresh)"
if [[ "$refresh_output" != *"is not running"* ]] || ! kill -0 "$foreign_pid" 2>/dev/null; then
    echo "stale PID file caused an unrelated process to be signalled: $refresh_output" >&2
    exit 1
fi
rm -f "$pid_file"

process_is_alive() {
    local pid="$1" state

    [[ -r "/proc/$pid/status" ]] || return 1
    state="$(awk '$1 == "State:" { print $2 }' "/proc/$pid/status")" || return 1
    [[ "$state" != "Z" && "$state" != "X" ]]
}

echo "Checking SIGKILL fallback for a daemon ignoring SIGTERM"
run_rc start 1 >/dev/null
read -r stubborn_pid < "$pid_file"
stop_output="$(run_rc stop 2>&1)"
if [[ "$stop_output" != *"sending SIGKILL"* ||
      "$stop_output" != *"stopped after SIGKILL"* ]]; then
    echo "stop did not report the SIGKILL fallback: $stop_output" >&2
    exit 1
fi
if process_is_alive "$stubborn_pid"; then
    echo "daemon $stubborn_pid survived the SIGKILL fallback" >&2
    exit 1
fi
if [[ -e "$pid_file" ]]; then
    echo "PID file still exists after the SIGKILL fallback" >&2
    exit 1
fi
