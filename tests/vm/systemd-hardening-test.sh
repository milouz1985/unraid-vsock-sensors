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
readonly FILTER_GROUPS='@cpu-emulation @debug @mount @obsolete @privileged @resources'

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

# systemd-analyze lists nested groups and syscalls from multiple architectures.
# Expand recursively; @known is not a native-architecture applicability oracle.
expand_syscall_group() {
    local group="$1" expansion member rest
    expansion="$(systemd-analyze syscall-filter "$group")" ||
        die "Cannot expand syscall group $group"
    while read -r member rest; do
        case "$member" in
            ""|\#*|"$group") ;;
            @*) expand_syscall_group "$member" ;;
            *) printf '%s\n' "$member" ;;
        esac
    done <<< "$expansion"
}

filter_directives_in() {
    awk '
        /^\[/ { service = ($0 == "[Service]") }
        service && /^SystemCallFilter=/ { sub(/^SystemCallFilter=/, ""); print }
    ' "$1"
}

filter_group_set() {
    local filter="$1"
    [[ "$filter" == ~* ]] || die "Declared SystemCallFilter is not a deny-list: $filter"
    printf '%s\n' "${filter#\~}" | tr ' ' '\n' | sed '/^$/d' | sort -u
}

expected_groups="$(printf '%s\n' "$FILTER_GROUPS" | tr ' ' '\n' | sort -u)"
expected_syscalls="$(
    for group in $FILTER_GROUPS; do expand_syscall_group "$group"; done
)" || die "Cannot expand configured syscall groups"
expected_syscalls="$(printf '%s\n' "$expected_syscalls" | sort -u)"

assert_syscall_filter() {
    local filter blocked unexpected group members contribution
    local fragment declared dropins dropin directive
    filter="$(show_property SystemCallFilter)"
    [[ "$filter" == ~* ]] || die "SystemCallFilter is not an effective deny-list: $filter"
    blocked="$(printf '%s\n' "${filter#\~}" | tr ' ' '\n' | sed '/^$/d' | sort -u)"
    [[ -n "$blocked" ]] || die "SystemCallFilter deny-list is empty"
    unexpected="$(comm -23 <(printf '%s\n' "$blocked") <(printf '%s\n' "$expected_syscalls"))"
    [[ -z "$unexpected" ]] || die "SystemCallFilter blocks unexpected syscalls: $unexpected"

    # Check the package's fragment, then account separately for filter drop-ins.
    fragment="$(show_property FragmentPath)"
    [[ -r "$fragment" ]] || die "Installed unit fragment is not readable: $fragment"
    declared="$(filter_directives_in "$fragment")"
    [[ "$(filter_group_set "$declared")" == "$expected_groups" ]] ||
        die "Installed SystemCallFilter groups differ from the contract: $declared"
    dropins="$(show_property DropInPaths)"
    for dropin in $dropins; do
        [[ -r "$dropin" ]] || die "Unit drop-in is not readable: $dropin"
        while IFS= read -r directive; do
            if [[ -z "$directive" ]]; then
                declared=""
            elif [[ "$directive" == ~* ]]; then
                declared="~${declared#\~} ${directive#\~}"
            else
                die "SystemCallFilter drop-in changes the deny-list policy: $dropin"
            fi
        done < <(filter_directives_in "$dropin")
    done
    [[ "$(filter_group_set "$declared")" == "$expected_groups" ]] ||
        die "Effective declared SystemCallFilter groups differ from the contract: $declared"

    # Prove contribution dynamically, without requiring any fixed syscall name.
    for group in $FILTER_GROUPS; do
        members="$(expand_syscall_group "$group" | sort -u)" ||
            die "Cannot expand syscall group $group"
        [[ -n "$members" ]] || { echo "$group: no local members"; continue; }
        contribution="$(comm -12 <(printf '%s\n' "$members") <(printf '%s\n' "$blocked"))"
        [[ -n "$contribution" ]] || die "SystemCallFilter has no effective contribution from $group"
        echo "$group: $(printf '%s\n' "$contribution" | wc -l) effective syscalls"
    done
}

assert_syscall_filter

# Module must be loaded (ExecStartPre ran outside the confinement).
grep -q '^virt_temp ' /proc/modules ||
    die "virt_temp module is not loaded"

echo "=== Effective systemd hardening PASSED ==="
