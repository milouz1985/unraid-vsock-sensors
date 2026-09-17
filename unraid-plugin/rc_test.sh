#!/bin/bash
set -euo pipefail

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
rc_script="$script_dir/rc.unraid-vsock-sensors"
test_dir="$(mktemp -d)"
pid_file="$test_dir/service.pid"
lock_file="$test_dir/service.lock"
control_socket="$test_dir/control.sock"
args_file="$test_dir/service.args"
refresh_file="$test_dir/service.refresh"
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
if [[ -n "${UVSS_CONTROL_SOCKET:-}" ]]; then
    # Represent the daemon control socket in the background so the readiness
    # check does not depend on Python start time.
    (
        python3 -c "import socket,sys; s=socket.socket(socket.AF_UNIX); s.bind(sys.argv[1])" "$UVSS_CONTROL_SOCKET" 2>/dev/null || true
    ) &
fi
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
# before replacing itself with the long-lived helper; on control commands it
# records the requested operation.
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
    disks)
        shift
        if [[ -n "\${UVSS_RC_TEST_REFRESH_FILE:-}" && "\${1:-}" == "refresh" ]]; then
            printf 'refresh\n' > "\$UVSS_RC_TEST_REFRESH_FILE"
        fi
        exit "\${UVSS_RC_TEST_BINARY_STATUS:-0}"
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
        UVSS_CONTROL_SOCKET="$control_socket" \
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
        UVSS_CONTROL_SOCKET="$control_socket" \
        UVSS_RC_TEST_ARGS_FILE="$args_file" \
        UVSS_RC_TEST_REFRESH_FILE="$refresh_file" \
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

# Wait explicitly for the fake daemon's control socket to be present before
# driving the refresh/poll control calls, so the test does not depend on the
# fake daemon (and its socket creation) being up before the PID check.
for _ in {1..200}; do
    [[ -S "$control_socket" ]] && break
    sleep 0.01
done
if [[ ! -S "$control_socket" ]]; then
    echo "fake daemon did not create its control socket" >&2
    exit 1
fi

poll_output="$(run_rc poll)"
if [[ "$poll_output" != *"SMART poll reported"* ]]; then
    echo "poll did not report success: $poll_output" >&2
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
    echo "active daemon did not receive the refresh control call" >&2
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
rm -f "$control_socket"

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

# The loop above always ends with a failed start (no socket left behind); make
# sure the control socket is absent so the next refresh check sees no daemon.
rm -f "$control_socket"

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
kill "$foreign_pid" 2>/dev/null || true
wait "$foreign_pid" 2>/dev/null || true
foreign_pid=""

# PID reuse may also point at another invocation of the UVSS binary. Matching
# argv[0] alone is insufficient: only the long-lived `serve` subcommand is the
# daemon and may be signalled by this service script.
(exec -a "$fake_binary_path" yes disks >/dev/null) &
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
