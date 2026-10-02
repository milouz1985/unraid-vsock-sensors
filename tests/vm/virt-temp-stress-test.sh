#!/usr/bin/env bash
set -Eeuo pipefail
# Stress and lifecycle test for the virt-temp kernel module.
#
# Usage: virt-temp-stress-test.sh /path/to/virt-temp.ko
#
# Scenarios (in order):
#   1. Nominal create/configure/read/remove
#   2. Open-FD deterministic lifecycle (FD open during rmdir -> ENODEV)
#   3. Multi-FD deterministic lifecycle (8 FDs, each returns ENODEV)
#   4. Unload refcount (rmmod refused with open FD, succeeds after close)
#   5. Concurrent stress (lifecycle + hwmon reader + miscdevice writer)
#   6. Unload/reload stress (30 cycles)
#   7. dmesg validation
#
# Requires: root, VM, the .ko file passed as argument.

cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.."
# shellcheck source=common.sh
source tests/vm/common.sh

[[ $EUID -eq 0 && "${UVSS_VM_TEST:-}" == 1 ]] ||
    die "Run only through the disposable UVSS VM test suite"
verify_local_test_image
case "$(systemd-detect-virt --vm)" in
    kvm|qemu) ;;
    *) die "Expected a QEMU/KVM VM" ;;
esac

MODULE_KO="${1:-}"
[[ -f "$MODULE_KO" ]] || die "virt-temp module not found: $MODULE_KO"

readonly CONFIG_ROOT=/sys/kernel/config/virt_temp
readonly DEV_ROOT=/dev/virt-temp
readonly SERVICE=unraid-vsock-hwmon.service
readonly STRESS_ITERS="${UVSS_STRESS_ITERS:-500}"
readonly RELOAD_CYCLES="${UVSS_RELOAD_CYCLES:-30}"
readonly WORKER_TIMEOUT=60
readonly DMEG_PATTERN='BUG:|WARNING:|KASAN:|KCSAN:|UBSAN:|use-after-free|general protection fault|kernel BUG|Oops:|refcount_t:|hung task|lockdep'
WORK_DIR="$(mktemp -d /run/uvss-virt-temp-stress.XXXXXX)"
readonly WORK_DIR

module_was_loaded=0
service_was_active=0
pids=()
open_fds=()
created_sensors=()

cleanup() {
    local rc=$?
    trap - EXIT
    local pid sensor
    for pid in "${pids[@]}"; do
        if kill -0 "$pid" 2>/dev/null; then
            kill "$pid" 2>/dev/null || true
            wait "$pid" 2>/dev/null || true
        fi
    done
    for fd in "${open_fds[@]}"; do
        exec {fd}>&- 2>/dev/null || true
    done
    for sensor in "${created_sensors[@]}"; do
        if [[ -d "$sensor" ]]; then rmdir "$sensor" 2>/dev/null || true; fi
    done
    # Best-effort cleanup of sensors created by the concurrent lifecycle worker.
    if [[ -n "${SENSOR_LOG_FILE:-}" && -f "$SENSOR_LOG_FILE" ]]; then
        while IFS= read -r sensor; do
            if [[ -d "$sensor" ]]; then rmdir "$sensor" 2>/dev/null || true; fi
        done < "$SENSOR_LOG_FILE"
    fi
    rm -rf -- "$WORK_DIR"
    if [[ "$module_was_loaded" -eq 0 ]]; then
        rmmod virt_temp 2>/dev/null || true
    fi
    if [[ "$service_was_active" -eq 1 ]]; then
        systemctl start "$SERVICE" 2>/dev/null || true
    fi
    exit "$rc"
}
trap cleanup EXIT

wait_for_path() {
    local path="$1" timeout_s="${2:-5}"
    local deadline=$((SECONDS + timeout_s))
    while [[ ! -e "$path" ]]; do
        (( SECONDS < deadline )) || die "Timeout waiting for $path"
        sleep 0.05
    done
}

wait_for_gone() {
    local path="$1" timeout_s="${2:-5}"
    local deadline=$((SECONDS + timeout_s))
    while [[ -e "$path" ]]; do
        (( SECONDS < deadline )) || die "Timeout waiting for $path to disappear"
        sleep 0.05
    done
}

hex_encode() {
    printf '%s' "$1" | od -A n -t x1 | tr -d ' \n'
}

config_path_for() {
    local family="$1" id="$2"

    printf '%s/%s/%s\n' \
        "$CONFIG_ROOT" \
        "$family" \
        "$(hex_encode "$id")"
}

device_path_for() {
    local family="$1" id="$2"

    printf '%s/%s\n' \
        "$DEV_ROOT" \
        "$(hex_encode "$family:$id")"
}

check_dmesg_since() {
    local marker="$1" label="${2:-stress}"
    local new_errors
    new_errors="$(dmesg 2>/dev/null | tail -n +"$((marker + 1))" | grep -E "$DMEG_PATTERN" || true)"
    [[ -z "$new_errors" ]] ||
        die "Kernel errors after $label:
$new_errors"
}

dmesg_line_count() {
    dmesg 2>/dev/null | wc -l
}

# Python helper: write to a device FD, exit 0 on success, 11 on ENODEV,
# 10 on ENOENT (device gone before open), non-zero on unexpected error.
MISC_WRITE_PY=$(cat <<'PYEOF'
import errno
import os
import sys

path = sys.argv[1]
try:
    fd = os.open(path, os.O_WRONLY)
except OSError as exc:
    if exc.errno in (errno.ENOENT, errno.ENODEV):
        raise SystemExit(10)
    raise

try:
    os.write(fd, b"37000\n")
except OSError as exc:
    if exc.errno == errno.ENODEV:
        raise SystemExit(11)
    raise
finally:
    os.close(fd)
PYEOF
)

# Python helper: write to an already-open FD, print the errno name on failure.
FD_WRITE_PY=$(cat <<'PYEOF'
import errno
import os
import sys

fd = int(sys.argv[1])
try:
    os.write(fd, b"40000\n")
    print("ok")
except OSError as exc:
    print(errno.errorcode.get(exc.errno, f"errno:{exc.errno}"))
PYEOF
)

# Python helper: read a sysfs file, exit 0 on success, 10 on ENOENT.
HWMON_READ_PY=$(cat <<'PYEOF'
import errno
import sys

path = sys.argv[1]
try:
    with open(path) as f:
        f.read()
except OSError as exc:
    if exc.errno in (errno.ENOENT, errno.ENODEV):
        raise SystemExit(10)
    raise
PYEOF
)

echo "=== virt-temp stress test ==="

# Configfs uses the suffix; the device name encodes the full namespaced ID.
[[ "$(config_path_for disk stress-nominal)" == \
    "$CONFIG_ROOT/disk/7374726573732d6e6f6d696e616c" ]] ||
    die "config_path_for contract mismatch"

[[ "$(device_path_for disk stress-nominal)" == \
    "$DEV_ROOT/6469736b3a7374726573732d6e6f6d696e616c" ]] ||
    die "device_path_for contract mismatch"

grep -q '^virt_temp ' /proc/modules || die "virt_temp module is not loaded"
[[ -d "$CONFIG_ROOT" ]] || die "configfs virt_temp not found"

if systemctl is-active --quiet "$SERVICE" 2>/dev/null; then
    service_was_active=1
    systemctl stop "$SERVICE"
fi

dmesg_before="$(dmesg_line_count)"

# ---------------------------------------------------------------------------
# 1. Nominal: create -> configure -> read -> remove
# ---------------------------------------------------------------------------
echo "--- 1. Nominal lifecycle ---"
TEST_ID="stress-nominal"
CONFIG_PATH="$(config_path_for disk "$TEST_ID")"
DEV_PATH="$(device_path_for disk "$TEST_ID")"

mkdir "$CONFIG_PATH"
created_sensors+=("$CONFIG_PATH")
wait_for_path "$DEV_PATH"
printf '42000\n' > "$DEV_PATH"
printf '%s\n' "Nominal Sensor" > "$CONFIG_PATH/label"

hwmon_found=0
for hwmon_dir in /sys/class/hwmon/hwmon*; do
    [[ -d "$hwmon_dir" ]] || continue
    if [[ -f "$hwmon_dir/temp1_input" && -f "$hwmon_dir/temp1_label" ]]; then
        temp_val="$(cat "$hwmon_dir/temp1_input")"
        label_val="$(cat "$hwmon_dir/temp1_label")"
        if [[ "$temp_val" == "42000" && "$label_val" == "Nominal Sensor" ]]; then
            hwmon_found=1
            break
        fi
    fi
done
(( hwmon_found )) || die "hwmon device not found for nominal sensor"

rmdir "$CONFIG_PATH"
created_sensors=()
wait_for_gone "$DEV_PATH"
echo "Nominal: OK"

# ---------------------------------------------------------------------------
# 2. Open-FD deterministic lifecycle (ENODEV exact)
# ---------------------------------------------------------------------------
echo "--- 2. Open-FD during rmdir ---"
FD_TEST_ID="stress-fd"
FD_CONFIG_PATH="$(config_path_for disk "$FD_TEST_ID")"
FD_DEV_PATH="$(device_path_for disk "$FD_TEST_ID")"

mkdir "$FD_CONFIG_PATH"
created_sensors+=("$FD_CONFIG_PATH")
wait_for_path "$FD_DEV_PATH"
printf '38000\n' > "$FD_DEV_PATH"
printf '%s\n' "FD Sensor" > "$FD_CONFIG_PATH/label"

exec {fd}> "$FD_DEV_PATH"
open_fds+=("$fd")

rmdir "$FD_CONFIG_PATH"
created_sensors=()
wait_for_gone "$FD_DEV_PATH"

# Write through the old FD: must get exactly ENODEV.
fd_result="$(python3 -c "$FD_WRITE_PY" "$fd")"
[[ "$fd_result" == "ENODEV" ]] ||
    die "Open-FD write after rmdir returned $fd_result, want ENODEV"

exec {fd}>&-
open_fds=()
echo "Open-FD: OK (ENODEV confirmed)"

# ---------------------------------------------------------------------------
# 3. Multi-FD deterministic lifecycle (8 FDs, each ENODEV)
# ---------------------------------------------------------------------------
echo "--- 3. Multi-FD (8 FDs) ---"
MF_ID="stress-multifd"
MF_CONFIG_PATH="$(config_path_for disk "$MF_ID")"
MF_DEV_PATH="$(device_path_for disk "$MF_ID")"

mkdir "$MF_CONFIG_PATH"
created_sensors+=("$MF_CONFIG_PATH")
wait_for_path "$MF_DEV_PATH"
printf '35000\n' > "$MF_DEV_PATH"
printf '%s\n' "MultiFD" > "$MF_CONFIG_PATH/label"

mf_fds=()
for i in $(seq 1 8); do
    exec {mf_fd_i}> "$MF_DEV_PATH"
    mf_fds+=("$mf_fd_i")
    open_fds+=("$mf_fd_i")
done

rmdir "$MF_CONFIG_PATH"
created_sensors=()
wait_for_gone "$MF_DEV_PATH"

mf_enodev_count=0
for fd in "${mf_fds[@]}"; do
    result="$(python3 -c "$FD_WRITE_PY" "$fd")"
    if [[ "$result" == "ENODEV" ]]; then
        (( ++mf_enodev_count ))
    else
        die "Multi-FD: FD write returned $result, want ENODEV"
    fi
done
(( mf_enodev_count == 8 )) || die "Multi-FD: $mf_enodev_count/8 ENODEV, want 8/8"

for fd in "${mf_fds[@]}"; do
    exec {fd}>&-
done
open_fds=()
echo "Multi-FD: OK (8/8 ENODEV)"

# ---------------------------------------------------------------------------
# 4. Unload refcount (rmdir first, then test FD holds module ref)
# ---------------------------------------------------------------------------
echo "--- 4. Unload refcount ---"
UL_ID="stress-unload"
UL_CONFIG_PATH="$(config_path_for disk "$UL_ID")"
UL_DEV_PATH="$(device_path_for disk "$UL_ID")"

mkdir "$UL_CONFIG_PATH"
created_sensors+=("$UL_CONFIG_PATH")
wait_for_path "$UL_DEV_PATH"
printf '33000\n' > "$UL_DEV_PATH"
printf '%s\n' "Unload" > "$UL_CONFIG_PATH/label"

exec {ul_fd}> "$UL_DEV_PATH"
open_fds+=("$ul_fd")

# Remove the sensor first — the FD remains open.
rmdir "$UL_CONFIG_PATH"
created_sensors=()
wait_for_gone "$UL_DEV_PATH"

# Now the only reference is the open FD. rmmod must fail.
if rmmod virt_temp 2>/dev/null; then
    die "rmmod succeeded with an open FD; refcount is broken"
fi
echo "Unload with FD: correctly refused"

exec {ul_fd}>&-
open_fds=()

# Now rmmod must succeed.
if ! rmmod virt_temp; then
    die "rmmod failed after closing all FDs"
fi
wait_for_gone "$CONFIG_ROOT" 10
insmod "$MODULE_KO"
wait_for_path "$CONFIG_ROOT" 10
echo "Unload refcount: OK"

# ---------------------------------------------------------------------------
# 5. Concurrent stress
# ---------------------------------------------------------------------------
echo "--- 5. Concurrent stress ($STRESS_ITERS iterations) ---"
dmesg_before_stress="$(dmesg_line_count)"

CURRENT_DEVICE_FILE="$WORK_DIR/current-device"
CURRENT_HWMON_FILE="$WORK_DIR/current-hwmon"
MISC_COUNTS_FILE="$WORK_DIR/misc-counts"
HWMON_COUNTS_FILE="$WORK_DIR/hwmon-counts"
SENSOR_LOG_FILE="$WORK_DIR/sensor-log"
: > "$CURRENT_DEVICE_FILE"
: > "$CURRENT_HWMON_FILE"
printf '0 0 0\n' > "$MISC_COUNTS_FILE"
printf '0\n' > "$HWMON_COUNTS_FILE"
: > "$SENSOR_LOG_FILE"

worker_lifecycle() {
    local i id dev cfg hwmon_dir
    local activity_deadline hwmon_reads misc_attempts _
    for i in $(seq 1 "$STRESS_ITERS"); do
        id="stress-lc-$i"
        dev="$(device_path_for disk "$id")"
        cfg="$(config_path_for disk "$id")"
        mkdir "$cfg" || return 1
        printf '%s\n' "$cfg" >> "$SENSOR_LOG_FILE"
        wait_for_path "$dev" 3
        printf '%s\n' "LC $i" > "$cfg/label" || return 1
        printf '36000\n' > "$dev" || return 1
        printf '%s\n' "$dev" > "$CURRENT_DEVICE_FILE"
        hwmon_dir=""
        for d in /sys/class/hwmon/hwmon*; do
            [[ -d "$d" ]] || continue
            if [[ "$(cat "$d/temp1_label" 2>/dev/null || true)" == "LC $i" ]]; then
                hwmon_dir="$d"
                break
            fi
        done
        [[ -n "$hwmon_dir" ]] || return 1
        printf '%s\n' "$hwmon_dir" > "$CURRENT_HWMON_FILE"
        # Let both observers exercise the first sensor before removing it.
        # Short runs can otherwise finish before either observer is scheduled.
        if (( i == 1 )); then
            activity_deadline=$((SECONDS + 5))
            until read -r hwmon_reads < "$HWMON_COUNTS_FILE" &&
                read -r misc_attempts _ < "$MISC_COUNTS_FILE" &&
                (( hwmon_reads > 0 && misc_attempts > 0 )); do
                (( SECONDS < activity_deadline )) || die "Stress observers did not exercise the first sensor"
                sleep 0.01
            done
        fi
        rmdir "$cfg" || return 1
        # Mark sensor as removed in the log (for final verification).
        wait_for_gone "$dev" 3
        : > "$CURRENT_DEVICE_FILE"
        : > "$CURRENT_HWMON_FILE"
    done
    return 0
}

worker_hwmon_reader() {
    local deadline=$((SECONDS + WORKER_TIMEOUT))
    local hwmon_dir path rc count
    while (( SECONDS < deadline )); do
        hwmon_dir="$(cat "$CURRENT_HWMON_FILE" 2>/dev/null || true)"
        [[ -n "$hwmon_dir" && -d "$hwmon_dir" ]] || { sleep 0.01; continue; }
        count=0
        for path in "$hwmon_dir/temp1_input" "$hwmon_dir/temp1_label"; do
            [[ -f "$path" ]] || continue
            if python3 -c "$HWMON_READ_PY" "$path" 2>/dev/null; then
                rc=0
            else
                rc=$?
            fi
            if (( rc == 0 )); then
                (( ++count ))
            elif (( rc == 10 )); then
                : # ENOENT/ENODEV expected during concurrent removal
            else
                return 1
            fi
        done
        if (( count > 0 )); then
            local total
            total="$(cat "$HWMON_COUNTS_FILE" 2>/dev/null || echo 0)"
            printf '%s\n' "$((total + count))" > "$HWMON_COUNTS_FILE"
        fi
        sleep 0.01
    done
    return 0
}

worker_misc_writer() {
    local deadline=$((SECONDS + WORKER_TIMEOUT))
    local dev rc attempts successes enodev enoent
    attempts=0
    successes=0
    enodev=0
    enoent=0
    while (( SECONDS < deadline )); do
        dev="$(cat "$CURRENT_DEVICE_FILE" 2>/dev/null || true)"
        [[ -n "$dev" ]] || { sleep 0.01; continue; }
        [[ -e "$dev" ]] || { sleep 0.01; continue; }
        if python3 -c "$MISC_WRITE_PY" "$dev" 2>/dev/null; then
            rc=0
        else
            rc=$?
        fi
        (( ++attempts ))
        case "$rc" in
            0) (( ++successes )) ;;
            11) (( ++enodev )) ;;
            10) (( ++enoent )) ;;
            *) return 1 ;;
        esac
        printf '%s %s %s\n' "$attempts" "$successes" "$((enodev + enoent))" > "$MISC_COUNTS_FILE"
        sleep 0.01
    done
    return 0
}

worker_lifecycle &
pids+=("$!")
worker_hwmon_reader &
pids+=("$!")
worker_misc_writer &
pids+=("$!")

local_failed=0
for pid in "${pids[@]}"; do
    wait "$pid" || local_failed=1
done
(( local_failed == 0 )) || die "Concurrent stress worker failed"
pids=()

# Verify workers actually did work.
hwmon_reads="$(cat "$HWMON_COUNTS_FILE" 2>/dev/null || echo 0)"
(( hwmon_reads > 0 )) || die "hwmon reader never successfully read a stress sensor"

read -r misc_attempts misc_successes misc_disappeared < "$MISC_COUNTS_FILE"
(( misc_attempts > 0 )) || die "misc writer never exercised a stress device"
echo "Concurrent stress: OK ($STRESS_ITERS iters, hwmon_reads=$hwmon_reads, misc: $misc_successes ok / $misc_disappeared disappeared)"

check_dmesg_since "$dmesg_before_stress" "concurrent stress"

# ---------------------------------------------------------------------------
# 6. Unload/reload stress
# ---------------------------------------------------------------------------
echo "--- 6. Unload/reload ($RELOAD_CYCLES cycles) ---"
dmesg_before_reload="$(dmesg_line_count)"
for i in $(seq 1 "$RELOAD_CYCLES"); do
    if ! rmmod virt_temp; then
        die "Unload cycle $i failed (leftover reference?)"
    fi
    wait_for_gone "$CONFIG_ROOT" 10
    if ! insmod "$MODULE_KO"; then
        die "Load cycle $i failed"
    fi
    wait_for_path "$CONFIG_ROOT" 10
done
check_dmesg_since "$dmesg_before_reload" "unload/reload"
echo "Unload/reload: OK ($RELOAD_CYCLES cycles)"

# ---------------------------------------------------------------------------
# 7. Final dmesg check + strict cleanup verification
# ---------------------------------------------------------------------------
check_dmesg_since "$dmesg_before" "entire stress test"
echo "dmesg: clean"

# Strict: no sensor logged during the stress may still exist.
while IFS= read -r sensor; do
    [[ -e "$sensor" ]] && die "Configfs sensor still exists: $sensor"
done < "$SENSOR_LOG_FILE"

# Also verify the deterministic sensors from scenarios 1-4 are gone.
for sensor in "${created_sensors[@]}"; do
    [[ ! -e "$sensor" ]] || die "Configfs sensor still exists: $sensor"
done

# Unload the module since we loaded it.
rmmod virt_temp
wait_for_gone "$CONFIG_ROOT" 10

echo "=== virt-temp stress test PASSED ==="
