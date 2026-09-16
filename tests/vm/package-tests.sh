#!/usr/bin/env bash
set -Eeuo pipefail
cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.."
repo_root=$PWD
# shellcheck source=common.sh
source tests/vm/common.sh
[[ $EUID -eq 0 && "${UVSS_VM_TEST:-}" == 1 ]] || {
    echo "Run only through guest-tests.sh in a disposable UVSS VM" >&2; exit 1;
}
verify_local_test_image
case "$(systemd-detect-virt --vm)" in
    kvm|qemu) ;;
    *) echo "Expected a QEMU/KVM VM" >&2; exit 1 ;;
esac
kernel="$(uname -r)"
[[ "$kernel" == *-pve && "$kernel" == "$(cat /etc/uvss-test-kernel)" ]] || exit 1
phase="${1:-}"
package=unraid-vsock-sensors-hwmon
debian_revision="${DEBIAN_REVISION:-1}"
service=unraid-vsock-hwmon.service
config=/etc/default/unraid-vsock-hwmon
cache=/var/lib/unraid-vsock-sensors/hwmon-inventory.json
saved_sources=/var/lib/unraid-vsock-sensors/dkms-sources
checkpoint=/var/tmp/uvss-package-pre-reboot
trap 'journalctl -u "$service" -n 80 --no-pager >&2' ERR

# The receiver can listen locally without adding a VSOCK device to the host.
# This checks service startup, not guest-to-host transport or sensor traffic.
modprobe vsock_loopback
export DEBIAN_FRONTEND=noninteractive
export GOTOOLCHAIN=local
debian_version() {
    VERSION="$1" "$repo_root/version.sh" --debian
}
package_path() {
    local version="$1" debian
    debian="$(debian_version "$version")"
    printf '%s/dist/%s_%s-%s_amd64.deb\n' \
        "$repo_root" "$package" "$debian" "$debian_revision"
}
check_installed() {
    local version="$1" debian status magic
    debian="$(debian_version "$version")"
    [[ "$(dpkg-query -W -f='${Version}' "$package")" == "$debian-$debian_revision" ]]
    status="$(dkms status -m virt-temp -v "$version" -k "$kernel")"
    [[ "$status" == *': installed'* ]]
    magic="$(modinfo -F vermagic virt_temp)"
    [[ "${magic%% *}" == "$kernel" ]]
    [[ -c /dev/virt-temp ]]
    systemctl is-enabled --quiet "$service"
    # Wait past RestartSec so a process repeatedly crashing is not a success.
    sleep 3
    systemctl is-active --quiet "$service"
    [[ "$(systemctl show -p NRestarts --value "$service")" == 0 ]]
}
install_version() {
    local version="$1" package_file
    package_file="$(package_path "$version")"
    apt-get install -y --no-install-recommends "$package_file"
    check_installed "$version"
}
check_unregistered() {
    local status
    status="$(dkms status -m virt-temp -v "$1" 2>/dev/null || true)"
    [[ -z "$status" ]]
}

check_failed_upgrade() {
    local old_version="$1" failed_version="$2" debian package_status dkms_status old_status module_file source_link kernel_build target_kernel

    debian="$(debian_version "$failed_version")"
    package_status="$(dpkg-query -W -f='${Status}' "$package")"
    dkms_status="$(dkms status -m virt-temp -v "$failed_version" 2>/dev/null || true)"

    echo "State after the intentionally failed upgrade:"
    printf '  package version: %s\n' "$(dpkg-query -W -f='${Version}' "$package")"
    printf '  package status:  %s\n' "$package_status"
    printf '  old DKMS:        %s\n' \
        "$(dkms status -m virt-temp -v "$old_version" 2>/dev/null || true)"
    printf '  failed DKMS:     %s\n' "$dkms_status"
    printf '  loaded module:   %s\n' "$(grep '^virt_temp ' /proc/modules || true)"
    printf '  service:         %s\n' "$(systemctl is-active "$service" 2>/dev/null || true)"
    dpkg --audit || true

    [[ "$(dpkg-query -W -f='${Version}' "$package")" == "$debian-$debian_revision" ]]
    [[ "$package_status" == "install ok half-configured" ]]
    for kernel_build in /lib/modules/*/build; do
        [[ -d "$kernel_build" ]] || continue
        target_kernel="${kernel_build#/lib/modules/}"
        target_kernel="${target_kernel%/build}"
        old_status="$(dkms status -m virt-temp -v "$old_version" -k "$target_kernel")"
        [[ "$old_status" == *': installed'* ]]
        # Verify the module file installed by DKMS for every target kernel.
        module_file="$(
            find "/lib/modules/$target_kernel/updates/dkms" \
                -maxdepth 1 \
                -type f \
                -name 'virt-temp.ko*' \
                -print -quit
        )"
        [[ "$module_file" == "/lib/modules/$target_kernel/"* && -f "$module_file" ]]
    done
    [[ "$dkms_status" == *': added'* ]]
    [[ "$dkms_status" != *': installed'* ]]
    grep -Fq 'UVSS_VM_INTENTIONAL_DKMS_FAILURE' \
        "/var/lib/dkms/virt-temp/$failed_version/build/make.log"
    source_link="$(readlink "/var/lib/dkms/virt-temp/$old_version/source")"
    [[ "$source_link" == "$saved_sources/virt-temp-$old_version" ]]
    [[ -f "$source_link/dkms.conf" && -f "$source_link/virt-temp.c" ]]
    module_file="$(modinfo -n virt_temp)"
    [[ "$module_file" == "/lib/modules/$kernel/"* && -f "$module_file" ]]
    [[ "$(sha256sum "$module_file" | awk '{print $1}')" == "$(cat /var/tmp/uvss-expected-module.sha256)" ]]
    [[ "$(modinfo -F vermagic virt_temp)" == "$kernel "* ]]
    grep -q '^virt_temp ' /proc/modules
    [[ -d /sys/module/virt_temp && -c /dev/virt-temp ]]
    systemctl is-enabled --quiet "$service"
    systemctl is-active --quiet "$service"
}

# Turn a successfully built .deb into a package whose DKMS build is guaranteed
# to fail by appending a preprocessor error to the module source. The function
# is called in a command substitution, so set -e does not stop it on failure:
# every step is checked explicitly, the temporary root is always removed, a
# non-zero status is returned on error and the package path is printed only on
# success. The produced .deb is persistent and used directly by apt-get.
make_failed_package() {
    local version="$1" root source failed_package
    root="$(mktemp -d /var/tmp/uvss-failed-package.XXXXXX)" || {
        echo "make_failed_package: mktemp failed" >&2
        return 1
    }
    source="$(package_path "$version")" || {
        rm -rf -- "$root"
        echo "make_failed_package: package_path failed for $version" >&2
        return 1
    }
    failed_package="${source%.deb}-failed.deb"
    if ! dpkg-deb -R "$source" "$root"; then
        rm -rf -- "$root"
        echo "make_failed_package: dpkg-deb -R failed for $source" >&2
        return 1
    fi
    if ! printf '\n#error UVSS_VM_INTENTIONAL_DKMS_FAILURE\n' >> \
        "$root/usr/src/virt-temp-$version/virt-temp.c"; then
        rm -rf -- "$root"
        echo "make_failed_package: failed to append #error" >&2
        return 1
    fi
    if ! dpkg-deb --build --root-owner-group "$root" "$failed_package" >/dev/null; then
        rm -rf -- "$root"
        echo "make_failed_package: dpkg-deb --build failed" >&2
        return 1
    fi
    rm -rf -- "$root"
    printf '%s\n' "$failed_package"
}

first_failed_version=0.0.0-vmtest.3
second_failed_version=0.0.0-vmtest.5
final_version=0.0.0-vmtest.6
phase_pre_reboot() {
    [[ ! -d /sys/module/virt_temp && ! -e "$config" && ! -e /usr/bin/unraid-vsock-sensors ]] ||
        die "Expected a clean VM without an existing UVSS installation"
    echo "DKMS package: $(dpkg-query -W -f='${Version}' dkms)"
    echo "Building and installing the Debian package on kernel $kernel"
    for version in \
        0.0.0-vmtest.1 \
        0.0.0-vmtest.2 \
        0.0.0-vmtest.3 \
        0.0.0-vmtest.4 \
        0.0.0-vmtest.5 \
        0.0.0-vmtest.6; do
        make hwmon-package VERSION="$version"
    done

    install_version 0.0.0-vmtest.1
    timing "package: install"
    printf '\n# VM test: preserve this configuration across upgrades and remove\n' >> "$config"
    cp -- "$config" /var/tmp/uvss-expected-config
    mkdir -p -- "$(dirname -- "$cache")"
    printf '{"version":1}\n' > "$cache"

    echo "Checking package upgrade and DKMS replacement"
    install_version 0.0.0-vmtest.2
    timing "package: upgrade"
    cmp -- "$config" /var/tmp/uvss-expected-config
    check_unregistered 0.0.0-vmtest.1
    [[ ! -e "$saved_sources/virt-temp-0.0.0-vmtest.1" ]]
    module_file="$(modinfo -n virt_temp)"
    sha256sum "$module_file" | awk '{print $1}' > /var/tmp/uvss-expected-module.sha256

    echo "Checking state after an intentionally failed DKMS upgrade"
    failed_package="$(make_failed_package "$first_failed_version")"
    if apt-get install -y --no-install-recommends "$failed_package"; then
        echo "The intentionally broken DKMS upgrade unexpectedly succeeded" >&2
        exit 1
    fi
    timing "package: failed upgrade"
    check_failed_upgrade 0.0.0-vmtest.2 "$first_failed_version"
    cmp -- "$config" /var/tmp/uvss-expected-config
    [[ -f "$cache" ]]
    cat /proc/sys/kernel/random/boot_id > "$checkpoint"
    echo "Checkpoint written; host must reboot before the package suite continues"
}

# ---------------------------------------------------------------------------
# Post-reboot: the preserved old version must be bootable, then the
# half-configured package is repaired by installing a valid version directly
# (no apt remove in between).
# ---------------------------------------------------------------------------
phase_repair_after_failed_upgrade() {
    [[ -s "$checkpoint" ]] || die "Missing pre-reboot package checkpoint"
    [[ "$(cat /proc/sys/kernel/random/boot_id)" != "$(cat "$checkpoint")" ]] ||
        die "Package suite resumed without a VM reboot"
    echo "Checking boot from the preserved DKMS module before package repair"
    check_failed_upgrade 0.0.0-vmtest.2 "$first_failed_version"
    cmp -- "$config" /var/tmp/uvss-expected-config
    [[ -f "$cache" ]]
    timing "package: reboot recovery"

    echo "Repairing the half-configured package with a valid version"
    install_version 0.0.0-vmtest.4
    check_unregistered 0.0.0-vmtest.2
    check_unregistered "$first_failed_version"
    [[ ! -d "$saved_sources" ]]
    cmp -- "$config" /var/tmp/uvss-expected-config
    [[ -f "$cache" ]]
    # Refresh the expected module hash so the later .4 -> .5 broken upgrade
    # verifies preservation of the .4 module, not the stale .2 one.
    module_file="$(modinfo -n virt_temp)"
    sha256sum "$module_file" | awk '{print $1}' > /var/tmp/uvss-expected-module.sha256
    timing "package: recovery upgrade"

    echo "Creating a fresh broken upgrade to test direct removal"
    second_failed_package="$(make_failed_package "$second_failed_version")"
    if apt-get install -y --no-install-recommends "$second_failed_package"; then
        echo "The second intentionally broken DKMS upgrade unexpectedly succeeded" >&2
        exit 1
    fi
    check_failed_upgrade 0.0.0-vmtest.4 "$second_failed_version"
    timing "package: second failed upgrade"
    cat /proc/sys/kernel/random/boot_id > "$checkpoint"
    echo "Second checkpoint written; host must reboot before the removal checks"
}

# ---------------------------------------------------------------------------
# After the second reboot, apt remove of a half-configured
# package must remove every DKMS version and its backup sources while keeping
# the configuration and the cache.
# ---------------------------------------------------------------------------
phase_remove_after_failed_upgrade() {
    [[ -s "$checkpoint" ]] || die "Missing pre-reboot package checkpoint"
    [[ "$(cat /proc/sys/kernel/random/boot_id)" != "$(cat "$checkpoint")" ]] ||
        die "Removal phase resumed without a VM reboot"
    check_failed_upgrade 0.0.0-vmtest.4 "$second_failed_version"
    echo "Checking direct removal of the half-configured package"
    apt-get remove -y "$package"
    if systemctl is-active --quiet "$service"; then
        echo "Service is still active after package removal" >&2; exit 1
    fi
    [[ ! -d /sys/module/virt_temp && ! -c /dev/virt-temp ]]
    [[ ! -e /usr/bin/unraid-vsock-sensors ]]
    # A direct remove after a failed upgrade must clear every DKMS version,
    # including the preserved old one and its backup sources.
    for version in 0.0.0-vmtest.1 0.0.0-vmtest.2 "$first_failed_version" \
        0.0.0-vmtest.4 "$second_failed_version"; do
        check_unregistered "$version"
    done
    [[ ! -d "$saved_sources" ]]
    # The configuration and the hwmon cache must survive a plain removal.
    cmp -- "$config" /var/tmp/uvss-expected-config
    [[ -f "$cache" ]]
    # Reinstall a valid version so the next boot is not left without a module.
    # Use .6 (newer than .5) because apt-get would refuse a downgrade back to .4.
    install_version "$final_version"
    check_unregistered "$second_failed_version"
    [[ ! -d "$saved_sources" ]]
    timing "package: remove after failed upgrade"
    cat /proc/sys/kernel/random/boot_id > "$checkpoint"
    echo "Third checkpoint written; host must reboot before the final checks"
}

# ---------------------------------------------------------------------------
# Final phase, after the third reboot.
# ---------------------------------------------------------------------------
phase_final() {
    [[ -s "$checkpoint" ]] || die "Missing pre-reboot package checkpoint"
    [[ "$(cat /proc/sys/kernel/random/boot_id)" != "$(cat "$checkpoint")" ]] ||
        die "Final phase resumed without a VM reboot"
    echo "Checking boot from the repaired module after the final reboot"
    check_installed "$final_version"
    timing "package: final reboot recovery"

    echo "Checking package removal preserves configuration and unloads the module"
    apt-get remove -y "$package"
    if systemctl is-active --quiet "$service"; then
        echo "Service is still active after package removal" >&2; exit 1
    fi
    [[ ! -d /sys/module/virt_temp && ! -e /dev/virt-temp ]]
    [[ ! -e /usr/bin/unraid-vsock-sensors ]]
    check_unregistered "$final_version"
    cmp -- "$config" /var/tmp/uvss-expected-config
    [[ -f "$cache" ]]
    timing "package: remove"

    echo "Checking purge removes the preserved configuration"
    apt-get purge -y "$package"
    [[ ! -e "$config" && ! -e "$cache" && ! -d /sys/module/virt_temp ]]
    for version in 0.0.0-vmtest.1 0.0.0-vmtest.2 "$first_failed_version" \
        0.0.0-vmtest.4 "$second_failed_version" "$final_version"; do
        check_unregistered "$version"
    done
    [[ ! -d "$saved_sources" ]]
    rm -f -- "$checkpoint" /var/tmp/uvss-expected-module.sha256 /var/tmp/uvss-expected-config
    timing "package: purge"
    echo "Package and DKMS lifecycle checks passed on $kernel"
}

start_timing
case "$phase" in
    pre-reboot) phase_pre_reboot ;;
    post-reboot) phase_repair_after_failed_upgrade ;;
    remove-after-failed-upgrade) phase_remove_after_failed_upgrade ;;
    final) phase_final ;;
    *) die "Usage: $0 [pre-reboot|post-reboot|remove-after-failed-upgrade|final]" ;;
esac
