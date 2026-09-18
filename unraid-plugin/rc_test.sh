#!/bin/bash
set -euo pipefail

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
rc_script="$script_dir/rc.unraid-vsock-sensors"
test_dir="$(mktemp -d)"
pid_file="$test_dir/service.pid"
lock_file="$test_dir/service.lock"
args_file="$test_dir/service.args"
poll_file="$test_dir/service.poll"
started_file="$test_dir/service.started"
pipe_reader_pid=""
foreign_pid=""
daemon_pid=""

# The rc script validates both argv[0] and argv[1] of the running daemon. Use a
# small helper named exactly `serve` so the fake daemon has the same command
# line shape as the real process: <binary> serve ...
fake_daemon_path="$test_dir/serve"
cat > "$fake_daemon_path" <<'EOF'
if [[ "${UVSS_RC_TEST_IGNORE_TERM:-0}" == 1 ]]; then
    trap "" TERM
else
    trap "exit 0" TERM
fi
# The real daemon records the emhttpd heartbeat on SIGUSR2. The fake daemon
# mirrors that by writing the poll marker file when signalled.
if [[ -n "${UVSS_RC_TEST_POLL_FILE:-}" ]]; then
    trap 'printf poll > "$UVSS_RC_TEST_POLL_FILE"' USR2
fi
while :; do
    sleep 0.1 &
    wait "$!" || true
done
EOF
chmod 0755 "$fake_daemon_path"

# The fake binary behaves like UVSS for the rc script: on serve it records argv
# before replacing itself with the long-lived helper.
fake_binary_path="$test_dir/binary"
cat > "$fake_binary_path" <<EOF
#!/bin/bash
case "\${1:-}" in
    serve)
        # Record the arguments the rc script launched. This runs before exec -a,
        # so $@ is the argument list (serve --port ... --syslog) without the
        # binary path, which is what the test asserts.
        if [[ -n "\${UVSS_RC_TEST_ARGS_FILE:-}" ]]; then
            printf '%s\n' "\$@" > "\$UVSS_RC_TEST_ARGS_FILE"
        fi
        if [[ -n "\${UVSS_RC_TEST_STARTED_FILE:-}" ]]; then
            printf '%s\n' "\$\$" >> "\$UVSS_RC_TEST_STARTED_FILE"
        fi
        cd -- "\$(dirname -- "\$0")"
        exec -a "\$0" bash serve
        ;;
    *)
        exit "\${UVSS_RC_TEST_BINARY_STATUS:-0}"
        ;;
esac
EOF
chmod 0755 "$fake_binary_path"

cleanup() {
    UVSS_RC_BINARY="$fake_binary_path" \
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
    timeout 20 env \
        UVSS_RC_BINARY="$fake_binary_path" \
        UVSS_RC_CONFIG="$config_path" \
        UVSS_RC_PID_FILE="$pid_file" \
        UVSS_RC_LOCK_FILE="$lock_file" \
        UVSS_RC_TEST_ARGS_FILE="$args_file" \
        UVSS_RC_TEST_POLL_FILE="$poll_file" \
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

poll_output="$(run_rc poll)"
if [[ -n "$poll_output" ]]; then
    echo "successful poll unexpectedly wrote output: $poll_output" >&2
    exit 1
fi
for _ in {1..100}; do
    [[ -e "$poll_file" ]] && break
    sleep 0.01
done
if [[ ! -e "$poll_file" ]]; then
    echo "active daemon did not receive the emhttpd poll SIGUSR2" >&2
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

run_rc stop >/dev/null

run_rc start 0 "$script_dir/default.cfg" >/dev/null
mapfile -t daemon_args < "$args_file"
expected_args=(serve --port 990 --hba-mode enabled --hba-backend mpt3ctl --syslog)
if [[ "${daemon_args[*]}" != "${expected_args[*]}" ]]; then
    echo "unexpected default plugin arguments: ${daemon_args[*]}" >&2
    exit 1
fi
run_rc stop >/dev/null

for backend in mpt3ctl storcli; do
    printf 'HBA_BACKEND="%s"\n' "$backend" > "$test_dir/hba.cfg"
    run_rc start 0 "$test_dir/hba.cfg" >/dev/null
    mapfile -t daemon_args < "$args_file"
    expected_args=(serve --port 990 --hba-mode enabled --hba-backend "$backend" --syslog)
    if [[ "${daemon_args[*]}" != "${expected_args[*]}" ]]; then
        echo "unexpected $backend arguments: ${daemon_args[*]}" >&2
        exit 1
    fi
    run_rc stop >/dev/null
done

# HBA_INTERVAL was configurable before refresh rates became fixed per backend.
# Existing persistent configs must remain usable across the upgrade, whatever
# value they contain; the legacy setting is ignored.
printf 'HBA_BACKEND="mpt3ctl"\nHBA_INTERVAL="5m"\n' > "$test_dir/legacy-hba.cfg"
run_rc start 0 "$test_dir/legacy-hba.cfg" >/dev/null
mapfile -t daemon_args < "$args_file"
expected_args=(serve --port 990 --hba-mode enabled --hba-backend mpt3ctl --syslog)
if [[ "${daemon_args[*]}" != "${expected_args[*]}" ]]; then
    echo "legacy HBA_INTERVAL leaked into daemon arguments: ${daemon_args[*]}" >&2
    exit 1
fi
run_rc stop >/dev/null

# PID reuse may also point at another invocation of the UVSS binary. Matching
# argv[0] alone is insufficient: only the long-lived `serve` subcommand is the
# daemon and may be signalled by this service script.
(exec -a "$fake_binary_path" yes hwmon >/dev/null) &
foreign_pid=$!
printf '%s\n' "$foreign_pid" > "$pid_file"
stop_output="$(run_rc stop)"
if [[ "$stop_output" != *"is not running"* ]] || ! kill -0 "$foreign_pid" 2>/dev/null; then
    echo "stale PID file caused another UVSS command to be signalled: $stop_output" >&2
    exit 1
fi
rm -f "$pid_file"
kill "$foreign_pid" 2>/dev/null || true
wait "$foreign_pid" 2>/dev/null || true
foreign_pid=""

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
if [[ "$stop_output" != *"did not stop within 12 seconds"* ||
      "$stop_output" != *"sending SIGKILL"* ||
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
