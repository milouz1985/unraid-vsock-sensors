#!/usr/bin/env bash
set -Eeuo pipefail
cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.."
# shellcheck source=common.sh
source tests/vm/common.sh
[[ $EUID -eq 0 ]] || {
    echo "Run only as root in a disposable UVSS test VM" >&2; exit 1;
}
verify_local_test_image
suite="${1:-core}"
[[ "$suite" == core || "$suite" == kernel-stress || "$suite" == vsock-e2e || "$suite" == package-pre-reboot || "$suite" == package-post-reboot || "$suite" == package-remove-after-failed-upgrade || "$suite" == package-final ]] ||
    die "Usage: $0 [core|kernel-stress|vsock-e2e|package-pre-reboot|package-post-reboot|package-remove-after-failed-upgrade|package-final]"
case "$(systemd-detect-virt --vm)" in
    kvm|qemu) ;;
    *) echo "Expected a QEMU/KVM VM" >&2; exit 1 ;;
esac
kernel="$(uname -r)"
[[ "$kernel" == *-pve && "$kernel" == "$(cat /etc/uvss-test-kernel)" ]] || {
    echo "VM must boot the Proxmox kernel recorded by the builder" >&2; exit 1;
}
if [[ "$suite" == core ]]; then
    [[ ! -d /sys/module/virt_temp ]] || {
        echo "virt_temp is already loaded; refusing to disturb it" >&2; exit 1;
    }
fi
export PATH="/usr/local/go/bin:$PATH"
export VERSION=0.0.0-vmtest
export UVSS_VM_TEST=1
export GOTOOLCHAIN=local
cat /var/tmp/uvss-test-metadata
uname -a
go version
start_timing
if [[ "$suite" == core ]]; then
    make -C virt-temp/module
    timing "core: module build"
    go mod download
    timing "core: module download"
    go test -tags=integration -count=1 -timeout=120s -v -run '^TestVM' .
    timing "core: integration tests"
fi
if [[ "$suite" == kernel-stress ]]; then
    make -C virt-temp/module
    timing "kernel-stress: module build"
    [[ ! -d /sys/module/virt_temp ]] ||
        die "virt_temp is already loaded before kernel-stress (previous suite did not clean up)"
    insmod "$PWD/virt-temp/module/virt-temp.ko"
    timing "kernel-stress: module load"
    bash tests/vm/virt-temp-stress-test.sh "$PWD/virt-temp/module/virt-temp.ko"
    timing "kernel-stress suite"
fi
if [[ "$suite" == vsock-e2e ]]; then
    bash tests/vm/vsock-e2e-test.sh
    timing "vsock-e2e suite"
fi
if [[ "$suite" == package-pre-reboot || "$suite" == package-post-reboot || "$suite" == package-remove-after-failed-upgrade || "$suite" == package-final ]]; then
    bash tests/vm/package-tests.sh "${suite#package-}"
    timing "package suite"
fi
