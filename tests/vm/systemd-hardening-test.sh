#!/usr/bin/env bash
set -Eeuo pipefail
# Verifies the effective systemd hardening properties loaded by the running
# manager after the Debian package is installed. This complements the static
# test (virt-temp/systemd_hardening_test.sh) which checks the source file.
#
# Run inside the VM via package-tests.sh after install_version.

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

readonly SERVICE=unraid-vsock-hwmon.service
readonly UNIT_DIR=/etc/systemd/system/${SERVICE}.d
readonly MUTATION_DROPIN="${UNIT_DIR}/99-uvss-test-mutation.conf"

cleanup() {
    local rc=$?
    trap - EXIT
    if [[ -f "$MUTATION_DROPIN" ]]; then
        rm -f -- "$MUTATION_DROPIN"
        rmdir "$UNIT_DIR" 2>/dev/null || true
        systemctl daemon-reload 2>/dev/null || true
        systemctl restart "$SERVICE" 2>/dev/null || true
    fi
    exit "$rc"
}
trap cleanup EXIT

show_property() {
    systemctl show --property="$1" --value "$SERVICE"
}

assert_property() {
    local property="$1" expected="$2" actual
    actual="$(show_property "$property")"
    [[ "$actual" == "$expected" ]] ||
        die "$property=$actual, expected $expected"
}

assert_set_property() {
    local property="$1" expected="$2" actual
    actual="$(show_property "$property")"
    local expected_set actual_set
    # shellcheck disable=SC2086  # intentional word splitting for set comparison
    expected_set="$(printf '%s\n' $expected | sort)"
    # shellcheck disable=SC2086
    actual_set="$(printf '%s\n' $actual | sort)"
    [[ "$actual_set" == "$expected_set" ]] ||
        die "$property=[$actual], expected set [$expected]"
}

echo "=== Effective systemd hardening ==="

# Precondition: the service must be active under its confinement.
systemctl is-active --quiet "$SERVICE" ||
    die "$SERVICE is not active"

# Diagnostic: show what systemd actually loaded.
echo "FragmentPath: $(show_property FragmentPath)"
echo "DropInPaths:  $(show_property DropInPaths)"

# Boolean protections (contract: virt-temp/unraid-vsock-hwmon.service).
assert_property NoNewPrivileges yes
assert_property PrivateTmp yes
assert_property ProtectClock yes
assert_property ProtectControlGroups yes
assert_property ProtectHome yes
assert_property ProtectKernelLogs yes
assert_property ProtectKernelModules yes
assert_property ProtectKernelTunables yes
assert_property LockPersonality yes
assert_property MemoryDenyWriteExecute yes
assert_property RestrictNamespaces yes
assert_property RestrictRealtime yes
assert_property RestrictSUIDSGID yes

# ProtectSystem and its companion paths.
assert_property ProtectSystem strict
assert_property RuntimeDirectory unraid-vsock-sensors
assert_property RuntimeDirectoryPreserve yes
assert_property StateDirectory unraid-vsock-sensors
assert_property ReadWritePaths -/sys/kernel/config/virt_temp

# RestrictAddressFamilies: set comparison (order not guaranteed).
assert_set_property RestrictAddressFamilies AF_VSOCK

# CapabilityBoundingSet: only cap_net_bind_service is authorized.
assert_set_property CapabilityBoundingSet cap_net_bind_service
# No ambient capabilities should be set.
assert_property AmbientCapabilities ""

# SystemCallArchitectures.
assert_property SystemCallArchitectures native

# SystemCallFilter: the effective value is a fully expanded deny-list, not
# the @group shorthand from the unit file. Verify one representative syscall
# per configured group is present in the deny-list. This detects a group
# removed from the filter without depending on systemd-analyze's expansion
# (which can diverge from the kernel the unit was compiled against).
assert_syscall_filter_sentinels() {
    local filter syscall group
    filter="$(show_property SystemCallFilter)"
    [[ "$filter" == ~* ]] ||
        die "SystemCallFilter is not an effective deny-list: $filter"
    [[ -n "${filter#\~}" ]] ||
        die "SystemCallFilter deny-list is empty"
    for pair in \
        "cpu-emulation:modify_ldt" \
        "debug:ptrace" \
        "mount:mount" \
        "obsolete:_sysctl" \
        "privileged:chroot" \
        "resources:setrlimit"; do
        group="${pair%%:*}"
        syscall="${pair#*:}"
        if ! printf '%s\n' "$filter" | tr ' ' '\n' | grep -qx -- "$syscall"; then
            die "SystemCallFilter missing sentinel $syscall (group @$group)"
        fi
    done
}

assert_syscall_filter_sentinels

# Module must be loaded (ExecStartPre ran outside the confinement).
grep -q '^virt_temp ' /proc/modules ||
    die "virt_temp module is not loaded"

echo "=== Effective systemd hardening PASSED ==="
