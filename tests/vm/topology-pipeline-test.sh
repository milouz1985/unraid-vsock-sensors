#!/usr/bin/env bash
set -Eeuo pipefail
# Tests the real systemd pipeline:
#   /run/unraid-vsock-sensors/topology-changed
#     -> unraid-vsock-hwmon-topology.path (PathChanged)
#     -> unraid-vsock-hwmon-topology.service (oneshot)
#     -> unraid-vsock-hwmon-restart@<consumer>.service (try-restart)
#     -> <consumer>.service (counter increment)
#
# Requires: package installed, systemd, root.
# Run inside the VM via package-tests.sh.

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

readonly SUBSCRIBER_UNIT=unraid-vsock-test-subscriber.service
readonly SUBSCRIBER_INSTANCE="unraid-vsock-hwmon-restart@unraid-vsock-test-subscriber.service"
readonly SUBSCRIBER_LINK="/etc/systemd/system/unraid-vsock-hwmon-topology.service.wants/${SUBSCRIBER_INSTANCE}"
readonly COUNTER_FILE=/run/unraid-vsock-sensors/test-subscriber-count
readonly TOPOLOGY_EVENT=/run/unraid-vsock-sensors/topology-changed
readonly RUNTIME_DIR=/run/unraid-vsock-sensors
readonly TOPOLOGY_PATH_UNIT=unraid-vsock-hwmon-topology.path

cleanup() {
    local rc=$?
    trap - EXIT
    systemctl disable --now "$SUBSCRIBER_INSTANCE" 2>/dev/null || true
    systemctl stop "$SUBSCRIBER_UNIT" 2>/dev/null || true
    rm -f -- "/etc/systemd/system/${SUBSCRIBER_UNIT}" 2>/dev/null || true
    rm -f -- "$SUBSCRIBER_LINK" 2>/dev/null || true
    rm -f -- "$COUNTER_FILE" 2>/dev/null || true
    systemctl daemon-reload 2>/dev/null || true
    exit "$rc"
}
trap cleanup EXIT

read_counter() {
    cat "$COUNTER_FILE" 2>/dev/null || echo 0
}

wait_for_counter() {
    local expected="$1" deadline=$((SECONDS + 15))
    while :; do
        local current
        current="$(read_counter)"
        [[ "$current" == "$expected" ]] && return 0
        (( SECONDS < deadline )) || {
            echo "Counter is $current, expected $expected" >&2
            return 1
        }
        sleep 0.2
    done
}

trigger_topology() {
    : > "$TOPOLOGY_EVENT"
}

echo "=== Topology pipeline test ==="

# Precondition: the real packaged units must exist and the .path must be active.
test -f /usr/lib/systemd/system/unraid-vsock-hwmon-topology.path
test -f /usr/lib/systemd/system/unraid-vsock-hwmon-topology.service
test -f /usr/lib/systemd/system/unraid-vsock-hwmon-restart@.service
[[ "$(systemctl show -p ActiveState --value "$TOPOLOGY_PATH_UNIT")" == active ]] ||
    die "Topology path unit is not active"

# Create the fake subscriber service.
cat > "/etc/systemd/system/${SUBSCRIBER_UNIT}" <<EOF
[Unit]
Description=UVSS test subscriber (increments a counter)

[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=/bin/sh -c 'count=\$(cat ${COUNTER_FILE} 2>/dev/null || echo 0); echo \$((count + 1)) > ${COUNTER_FILE}'
EOF

systemctl daemon-reload

# Start the subscriber so try-restart can find it active.
systemctl start "$SUBSCRIBER_UNIT"
systemctl is-active --quiet "$SUBSCRIBER_UNIT" ||
    die "Test subscriber failed to start"

# Enable the subscription via the real template instance.
systemctl enable "$SUBSCRIBER_INSTANCE"
systemctl is-enabled --quiet "$SUBSCRIBER_INSTANCE" ||
    die "Restart subscription instance could not be enabled"

# Ensure the runtime directory exists.
mkdir -p -- "$RUNTIME_DIR"

# Read the initial counter.
local_before="$(read_counter)"
echo "Initial counter: $local_before"

# First topology event: N -> N+1.
trigger_topology
wait_for_counter $((local_before + 1)) ||
    die "First topology event did not increment the counter"
echo "After first event: $((local_before + 1))"

# Second topology event: N+1 -> N+2.
trigger_topology
wait_for_counter $((local_before + 2)) ||
    die "Second topology event did not increment the counter"
echo "After second event: $((local_before + 2))"

# Negative control: wait 3s without an event, counter must not change.
sleep 3
local_after_negative="$(read_counter)"
[[ "$local_after_negative" == "$((local_before + 2))" ]] ||
    die "Counter changed without a topology event: $local_after_negative"
echo "No false trigger: counter stable at $local_after_negative"

echo "=== Topology pipeline test PASSED ==="
