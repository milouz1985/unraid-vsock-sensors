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
package=unraid-vsock-sensors-hwmon
debian_revision="${DEBIAN_REVISION:-1}"
service=unraid-vsock-hwmon.service
config=/etc/default/unraid-vsock-hwmon
cache=/var/lib/unraid-vsock-sensors/hwmon-inventory.json
[[ ! -d /sys/module/virt_temp && ! -e "$config" && ! -e /usr/bin/unraid-vsock-sensors ]] || {
    echo "Expected a clean VM without an existing UVSS installation" >&2; exit 1;
}
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
    local old_version="$1" failed_version="$2" debian package_status dkms_status

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
    check_unregistered "$old_version"
    [[ "$dkms_status" == *': added'* ]]
    grep -Fq 'UVSS_VM_INTENTIONAL_DKMS_FAILURE' \
        "/var/lib/dkms/virt-temp/$failed_version/build/make.log"
    [[ -d /sys/module/virt_temp && -c /dev/virt-temp ]]
    systemctl is-active --quiet "$service"
}

echo "Building and installing the Debian package on kernel $kernel"
start_timing
for version in \
    0.0.0-vmtest.1 \
    0.0.0-vmtest.2 \
    0.0.0-vmtest.3 \
    0.0.0-vmtest.4; do
    make hwmon-package VERSION="$version"
done

failed_version=0.0.0-vmtest.3
failed_root="$(mktemp -d /var/tmp/uvss-failed-package.XXXXXX)"
failed_source="$(package_path "$failed_version")"
failed_package="${failed_source%.deb}-failed.deb"
trap 'rm -rf -- "$failed_root"' EXIT
dpkg-deb -R "$failed_source" "$failed_root"
printf '\n#error UVSS_VM_INTENTIONAL_DKMS_FAILURE\n' >> \
    "$failed_root/usr/src/virt-temp-$failed_version/virt-temp.c"
dpkg-deb --build --root-owner-group "$failed_root" "$failed_package" >/dev/null
timing "package: builds"
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

echo "Checking state after an intentionally failed DKMS upgrade"
if apt-get install -y --no-install-recommends "$failed_package"; then
    echo "The intentionally broken DKMS upgrade unexpectedly succeeded" >&2
    exit 1
fi
timing "package: failed upgrade"
check_failed_upgrade 0.0.0-vmtest.2 "$failed_version"
cmp -- "$config" /var/tmp/uvss-expected-config
[[ -f "$cache" ]]

echo "Checking recovery by upgrading the half-configured package"
install_version 0.0.0-vmtest.4
timing "package: recovery upgrade"
check_unregistered 0.0.0-vmtest.2
check_unregistered "$failed_version"
cmp -- "$config" /var/tmp/uvss-expected-config
[[ -f "$cache" ]]

echo "Checking package removal preserves configuration and unloads the module"
apt-get remove -y "$package"
if systemctl is-active --quiet "$service"; then
    echo "Service is still active after package removal" >&2; exit 1
fi
[[ ! -d /sys/module/virt_temp && ! -e /dev/virt-temp ]]
[[ ! -e /usr/bin/unraid-vsock-sensors ]]
check_unregistered 0.0.0-vmtest.4
cmp -- "$config" /var/tmp/uvss-expected-config
[[ -f "$cache" ]]
timing "package: remove"

echo "Checking purge removes the preserved configuration"
apt-get purge -y "$package"
[[ ! -e "$config" && ! -e "$cache" && ! -d /sys/module/virt_temp ]]
check_unregistered 0.0.0-vmtest.4
timing "package: purge"
echo "Package and DKMS lifecycle checks passed on $kernel"
