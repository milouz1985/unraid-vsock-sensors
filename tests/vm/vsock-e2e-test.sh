#!/usr/bin/env bash
set -Eeuo pipefail
# Real guest -> host VSOCK acceptance, entirely inside the disposable PVE VM.
cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.."
# shellcheck source=common.sh
source tests/vm/common.sh
[[ $EUID -eq 0 && "${UVSS_VM_TEST:-}" == 1 ]] || die "Run through the disposable VM suite"
verify_local_test_image
case "$(systemd-detect-virt --vm)" in
    kvm|qemu) ;;
    *) die "Expected a QEMU/KVM outer VM" ;;
esac
[[ ! -d /sys/module/virt_temp ]] || die "virt_temp already loaded; previous suite did not clean up"
! systemctl is-active --quiet unraid-vsock-hwmon.service || die "Receiver service already active"
for command in qemu-system-x86_64 busybox cpio modprobe zstd xz; do
    command -v "$command" >/dev/null || die "Template dependency missing: $command; rebuild template"
done
[[ -c /dev/kvm ]] || die "Nested KVM unavailable: /dev/kvm is missing (no TCG fallback)"
qemu-system-x86_64 -device help > /var/tmp/uvss-vsock-devices.log
grep -q 'vhost-vsock-pci' /var/tmp/uvss-vsock-devices.log || die "QEMU lacks vhost-vsock-pci"
readelf -l /usr/bin/busybox > /var/tmp/uvss-vsock-busybox.log
! grep -q INTERP /var/tmp/uvss-vsock-busybox.log || die "Template BusyBox must be static"

readonly CONFIG_ITEM=/sys/kernel/config/virt_temp/disk/76736f636b2d653265
readonly DEVICE=/dev/virt-temp/6469736b3a76736f636b2d653265
readonly INNER_CID=42
KERNEL="$(uname -r)"
PORT="$(sed -n 's/^Environment=UNRAID_VSOCK_PORT=//p' virt-temp/unraid-vsock-hwmon.service)"
readonly KERNEL PORT
positive_integer PORT "$PORT"
readonly ERROR_PATTERN='BUG:|WARNING:|KASAN:|KCSAN:|UBSAN:|use-after-free|general protection fault|kernel BUG|Oops:|refcount_t:|hung task|lockdep'
WORK_DIR="$(mktemp -d /var/tmp/uvss-vsock.XXXXXX)"
readonly WORK_DIR
readonly CONSOLE_LOG=/var/tmp/uvss-vsock-console.log
readonly RECEIVER_LOG=/var/tmp/uvss-vsock-receiver.log
: > "$CONSOLE_LOG"
: > "$RECEIVER_LOG"
dmesg_before="$(dmesg | wc -l)"
receiver_pid=""
qemu_pid=""
loaded_virt_temp=0
loaded_vhost=0
mounted_configfs=0
owned_runtime=0
hwmon_path=""

stop_process() {
    local pid="$1" label="$2" deadline=$((SECONDS + 10))
    kill -TERM "$pid" 2>/dev/null || true
    while kill -0 "$pid" 2>/dev/null; do
        if (( SECONDS >= deadline )); then
            echo "$label did not stop within 10s; killing it" >&2
            kill -KILL "$pid" 2>/dev/null || true
            wait "$pid" 2>/dev/null || true
            return 1
        fi
        sleep 0.05
    done
    wait "$pid"
}

cleanup() {
    local rc=$? cleanup_rc=0 line=""
    trap - EXIT
    set +e
    if [[ -n "$qemu_pid" ]]; then
        # Interrupted runs are already red; termination must still be bounded.
        stop_process "$qemu_pid" QEMU || cleanup_rc=1
    fi
    if [[ -n "${serial_output:-}" && -e "/proc/$$/fd/$serial_output" ]]; then
        while IFS= read -r -t 0.1 line <&"$serial_output"; do
            printf '%s\n' "${line//$'\r'/}" >> "$CONSOLE_LOG"
        done
        [[ -z "$line" ]] || printf '%s' "${line//$'\r'/}" >> "$CONSOLE_LOG"
    fi
    if [[ -n "$receiver_pid" ]]; then
        stop_process "$receiver_pid" receiver || cleanup_rc=1
    fi
    if (( loaded_virt_temp )); then
        if [[ -d "$CONFIG_ITEM" ]]; then rmdir "$CONFIG_ITEM" || cleanup_rc=1; fi
        [[ ! -e "$CONFIG_ITEM" && ! -e "$DEVICE" ]] || cleanup_rc=1
        if [[ -n "$hwmon_path" && -e "$hwmon_path" ]]; then cleanup_rc=1; fi
        rmmod virt_temp || cleanup_rc=1
        [[ ! -d /sys/module/virt_temp ]] || cleanup_rc=1
    fi
    if (( loaded_vhost )); then rmmod vhost_vsock || cleanup_rc=1; fi
    if (( mounted_configfs )); then umount /sys/kernel/config || cleanup_rc=1; fi
    if (( owned_runtime )); then
        rm -f /run/unraid-vsock-sensors/topology-changed
        rmdir /run/unraid-vsock-sensors || cleanup_rc=1
    fi
    dmesg | tail -n +"$((dmesg_before + 1))" > /var/tmp/uvss-vsock-dmesg.log
    if grep -E "$ERROR_PATTERN" /var/tmp/uvss-vsock-dmesg.log; then cleanup_rc=1; fi
    if grep -E 'Kernel panic|Oops|BUG:|general protection fault' "$CONSOLE_LOG"; then cleanup_rc=1; fi
    echo '--- nested serial console ---'
    cat "$CONSOLE_LOG"
    echo '--- production receiver log ---'
    cat "$RECEIVER_LOG"
    rm -rf -- "$WORK_DIR"
    if (( cleanup_rc )); then echo 'VSOCK cleanup or kernel validation failed' >&2; rc=1; fi
    if (( rc == 0 )); then echo '=== real AF_VSOCK acceptance PASSED; cleanup and dmesg clean ==='; fi
    exit "$rc"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

if [[ ! -d /sys/module/vhost_vsock ]]; then
    modprobe vhost_vsock
    loaded_vhost=1
fi
[[ -c /dev/vhost-vsock && -r /dev/vhost-vsock && -w /dev/vhost-vsock ]] ||
    die "vhost-vsock unavailable: /dev/vhost-vsock must be an accessible character device"
echo "Nested KVM prerequisites: OK; kernel=$KERNEL; guest CID=$INNER_CID; production port=$PORT"
qemu-system-x86_64 --version
grep -E '^CONFIG_(VSOCKETS|VIRTIO_VSOCKETS|VIRTIO_PCI)=' "/boot/config-$KERNEL"
make -C virt-temp/module
CGO_ENABLED=0 go build -buildvcs=false -trimpath -o "$WORK_DIR/receiver" .
CGO_ENABLED=0 go build -buildvcs=false -trimpath -ldflags='-s -w' -o "$WORK_DIR/sender" ./tests/vm/vsock-sender
if ! mountpoint -q /sys/kernel/config; then
    mount -t configfs configfs /sys/kernel/config
    mounted_configfs=1
fi
insmod "$PWD/virt-temp/module/virt-temp.ko"
loaded_virt_temp=1
[[ ! -e /run/unraid-vsock-sensors ]] || die "Runtime directory already present; refusing to disturb it"
mkdir /run/unraid-vsock-sensors
owned_runtime=1
"$WORK_DIR/receiver" hwmon --cid "$INNER_CID" --port "$PORT" --cache "$WORK_DIR/inventory.json" > "$RECEIVER_LOG" 2>&1 &
receiver_pid=$!

root="$WORK_DIR/initramfs"
mkdir -p "$root"/{bin,modules,dev,proc,sys}
cp /usr/bin/busybox "$root/bin/busybox"
ln -s busybox "$root/bin/sh"
cp "$WORK_DIR/sender" "$root/uvss-vsock-sender"
mknod "$root/dev/console" c 5 1
printf '%s\n' "$PORT" > "$root/port"
: > "$root/module-order"
# modprobe derives dependencies and ordering from the exact booted kernel.
modprobe --show-depends virtio_pci > "$WORK_DIR/dependencies"
modprobe --show-depends vmw_vsock_virtio_transport >> "$WORK_DIR/dependencies"
echo 'Inner module dependencies:'
cat "$WORK_DIR/dependencies"
declare -A copied_modules=()
while read -r action path _; do
    [[ "$action" == insmod ]] || continue
    [[ ! -v "copied_modules[$path]" ]] || continue
    copied_modules["$path"]=1
    module="$(basename "$path")"
    module="${module%.zst}"
    module="${module%.xz}"
    case "$path" in
        *.zst) zstd -dc "$path" > "$root/modules/$module" ;;
        *.xz) xz -dc "$path" > "$root/modules/$module" ;;
        *.ko) cp "$path" "$root/modules/$module" ;;
        *) die "Unsupported module format: $path" ;;
    esac
    printf '/modules/%s\n' "$module" >> "$root/module-order"
done < "$WORK_DIR/dependencies"
cat > "$root/init" <<'INIT'
#!/bin/sh
export PATH=/bin
bb=/bin/busybox
fail() { echo "UVSS-INNER-ERROR: $*"; exec "$bb" poweroff -f; }
"$bb" mount -t proc proc /proc || fail proc
"$bb" mount -t sysfs sysfs /sys || fail sysfs
"$bb" mount -t devtmpfs devtmpfs /dev || fail devtmpfs
exec </dev/ttyS0 >/dev/ttyS0 2>&1
"$bb" stty -echo || fail stty
while read -r module; do "$bb" insmod "$module" || fail "insmod $module"; done < /module-order
port="$($bb cat /port)"
echo "UVSS-INNER-KERNEL: $($bb uname -r)"
echo UVSS-INNER-READY
while read -r command; do
    case "$command" in
        VALID1) scenario=valid-1 ;;
        INVALID) scenario=invalid ;;
        VALID2) scenario=valid-2 ;;
        QUIT) echo UVSS-INNER-BYE; exec "$bb" poweroff -f ;;
        *) fail "unknown command $command" ;;
    esac
    "$bb" timeout -s KILL 8 /uvss-vsock-sender --port "$port" "$scenario" || fail "sender $command"
    echo "UVSS-DONE-$command"
done
fail 'serial EOF'
INIT
chmod +x "$root/init"
(cd "$root"; find . -print0 | cpio --null -o --format=newc --quiet) > "$WORK_DIR/inner.cpio"

wait_serial_marker() {
    local marker="$1" timeout_s="$2" line remaining deadline
    deadline=$((SECONDS + timeout_s))
    while (( SECONDS < deadline )); do
        remaining=$((deadline - SECONDS))
        if ! IFS= read -r -t "$remaining" line <&"$serial_output"; then
            [[ -z "$line" ]] || printf '%s' "${line//$'\r'/}" >> "$CONSOLE_LOG"
            die "Inner console closed or timed out waiting for $marker"
        fi
        line="${line//$'\r'/}"
        printf '%s\n' "$line" >> "$CONSOLE_LOG"
        case "$line" in
            *UVSS-INNER-ERROR*|*UVSS-SENDER-ERROR*|*'Kernel panic'*|*Oops*|*'BUG:'*|*'general protection fault'*)
                die "Inner guest failure: $line" ;;
        esac
        [[ "$line" != "$marker" ]] || return 0
    done
    die "Inner console timeout waiting for $marker"
}

receiver_alive() { kill -0 "$receiver_pid" 2>/dev/null || die "Production receiver exited"; }
wait_receiver_log() {
    local pattern="$1" deadline=$((SECONDS + 10))
    until grep -Fq "$pattern" "$RECEIVER_LOG"; do
        receiver_alive
        (( SECONDS < deadline )) || die "Receiver did not report: $pattern"
        sleep 0.05
    done
}

assert_sensor() {
    local expected="$1" deadline=$((SECONDS + 10)) path
    local matches=() items=() devices=()
    shopt -s nullglob
    while :; do
        receiver_alive
        matches=()
        for path in /sys/class/hwmon/hwmon*; do
            [[ -f "$path/temp1_label" ]] || continue
            if [[ "$(cat "$path/temp1_label")" == 'VSOCK E2E' ]]; then matches+=("$path"); fi
        done
        if (( ${#matches[@]} == 1 )) && [[ "$(cat "${matches[0]}/temp1_input")" == "$expected" ]]; then break; fi
        (( SECONDS < deadline )) || die "Expected exactly one VSOCK E2E hwmon at $expected mC"
        sleep 0.05
    done
    items=(/sys/kernel/config/virt_temp/disk/* /sys/kernel/config/virt_temp/hba/*)
    devices=(/dev/virt-temp/*)
    [[ ${#items[@]} == 1 && "${items[0]}" == "$CONFIG_ITEM" ]] || die "Unexpected configfs topology: ${items[*]}"
    [[ ${#devices[@]} == 1 && "${devices[0]}" == "$DEVICE" && -c "$DEVICE" ]] || die "Unexpected miscdevice topology: ${devices[*]}"
    [[ "$(cat "$CONFIG_ITEM/label")" == 'VSOCK E2E' ]] || die "Wrong configfs label"
    [[ "$(cat "${matches[0]}/name")" == unraid_vsock_e2e ]] || die "Wrong hwmon name"
    if [[ -n "$hwmon_path" ]]; then [[ "$hwmon_path" == "${matches[0]}" ]] || die "Hwmon duplicated or re-created on reconnect"; fi
    hwmon_path="${matches[0]}"
    echo "Sensor verified: $CONFIG_ITEM -> $DEVICE -> $hwmon_path; label=VSOCK E2E; temp=$expected"
}

wait_receiver_log "receiving Unraid snapshots on VSOCK port $PORT"
coproc INNER_QEMU {
    exec qemu-system-x86_64 -machine accel=kvm -cpu host -m 256 -smp 1 \
        -nodefaults -no-reboot -nic none -display none -monitor none -serial stdio \
        -kernel "/boot/vmlinuz-$KERNEL" -initrd "$WORK_DIR/inner.cpio" \
        -append 'console=ttyS0 rdinit=/init panic=1 loglevel=4' \
        -device "vhost-vsock-pci,guest-cid=$INNER_CID" 2>&1
}
qemu_pid="$INNER_QEMU_PID"
# Duplicated descriptors survive Bash's automatic removal of coproc variables.
exec {serial_input}>&"${INNER_QEMU[1]}"
exec {serial_output}<&"${INNER_QEMU[0]}"
wait_serial_marker UVSS-INNER-READY 60
printf 'VALID1\n' >&"$serial_input"
wait_serial_marker UVSS-DONE-VALID1 15
assert_sensor 42000
printf 'INVALID\n' >&"$serial_input"
wait_serial_marker UVSS-DONE-INVALID 15
wait_receiver_log 'decode stream snapshot:'
assert_sensor 42000
echo 'Invalid JSON observed by production receiver; daemon and sensor survive'
printf 'VALID2\n' >&"$serial_input"
wait_serial_marker UVSS-DONE-VALID2 15
assert_sensor 43000
printf 'QUIT\n' >&"$serial_input"
wait_serial_marker UVSS-INNER-BYE 10
deadline=$((SECONDS + 10))
# Drain the remaining console through EOF, including any shutdown panic.
while IFS= read -r -t "$((deadline - SECONDS > 0 ? deadline - SECONDS : 1))" line <&"$serial_output"; do
    printf '%s\n' "${line//$'\r'/}" >> "$CONSOLE_LOG"
    (( SECONDS < deadline )) || die "Inner guest did not shut down"
done
[[ -z "$line" ]] || printf '%s' "${line//$'\r'/}" >> "$CONSOLE_LOG"
while kill -0 "$qemu_pid" 2>/dev/null; do
    (( SECONDS < deadline )) || die "QEMU did not exit after QUIT"
    sleep 0.05
done
wait "$qemu_pid" || die "QEMU returned nonzero after QUIT"
qemu_pid=""
exec {serial_input}>&-
exec {serial_output}<&-
receiver_alive
echo 'Fresh connections VALID1 -> INVALID -> VALID2 completed over real AF_VSOCK'
