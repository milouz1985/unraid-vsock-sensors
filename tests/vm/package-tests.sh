#!/usr/bin/env bash
set -Eeuo pipefail
cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.."
[[ $EUID -eq 0 && "${UVSS_VM_TEST:-}" == 1 && -f /etc/uvss-test-image ]] || {
    echo "Run only through guest-tests.sh in a disposable UVSS VM" >&2; exit 1;
}
case "$(systemd-detect-virt --vm)" in
    kvm|qemu) ;;
    *) echo "Expected a QEMU/KVM VM" >&2; exit 1 ;;
esac
kernel="$(uname -r)"
[[ "$kernel" == *-pve && "$kernel" == "$(cat /etc/uvss-test-kernel)" ]] || exit 1
package=unraid-vsock-sensors-hwmon
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
check_installed() {
    local version="$1" status magic
    [[ "$(dpkg-query -W -f='${Version}' "$package")" == "$version-1" ]]
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
    local version="$1"
    apt-get install -y --no-install-recommends "$PWD/dist/${package}_${version}-1_amd64.deb"
    check_installed "$version"
}
check_unregistered() {
    local status
    status="$(dkms status -m virt-temp -v "$1")"
    [[ -z "$status" ]]
}

echo "Building and installing the Debian package on kernel $kernel"
for version in 0.0.0-vmtest.1 0.0.0-vmtest.2; do
    make hwmon-package VERSION="$version"
done
install_version 0.0.0-vmtest.1
printf '\n# VM test: preserve this configuration across upgrades and remove\n' >> "$config"
cp -- "$config" /var/tmp/uvss-expected-config
mkdir -p -- "$(dirname -- "$cache")"
printf '{"version":1}\n' > "$cache"

echo "Checking package upgrade and DKMS replacement"
install_version 0.0.0-vmtest.2
cmp -- "$config" /var/tmp/uvss-expected-config
check_unregistered 0.0.0-vmtest.1

echo "Checking package removal preserves configuration and unloads the module"
apt-get remove -y "$package"
if systemctl is-active --quiet "$service"; then
    echo "Service is still active after package removal" >&2; exit 1
fi
[[ ! -d /sys/module/virt_temp && ! -e /dev/virt-temp ]]
[[ ! -e /usr/bin/unraid-vsock-sensors ]]
check_unregistered 0.0.0-vmtest.2
cmp -- "$config" /var/tmp/uvss-expected-config
[[ -f "$cache" ]]

echo "Checking reinstall preserves configuration, then purge removes it"
install_version 0.0.0-vmtest.2
cmp -- "$config" /var/tmp/uvss-expected-config
apt-get purge -y "$package"
[[ ! -e "$config" && ! -e "$cache" && ! -d /sys/module/virt_temp ]]
check_unregistered 0.0.0-vmtest.2
echo "Package and DKMS lifecycle checks passed on $kernel"
