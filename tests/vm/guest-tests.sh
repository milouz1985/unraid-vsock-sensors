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
[[ "$suite" == core || "$suite" == package || "$suite" == all ]] ||
    die "Usage: $0 [core|package|all]"
case "$(systemd-detect-virt --vm)" in
    kvm|qemu) ;;
    *) echo "Expected a QEMU/KVM VM" >&2; exit 1 ;;
esac
kernel="$(uname -r)"
[[ "$kernel" == *-pve && "$kernel" == "$(cat /etc/uvss-test-kernel)" ]] || {
    echo "VM must boot the Proxmox kernel recorded by the builder" >&2; exit 1;
}
[[ ! -d /sys/module/virt_temp ]] || {
    echo "virt_temp is already loaded; refusing to disturb it" >&2; exit 1;
}
export PATH="/usr/local/go/bin:$PATH"
export VERSION=0.0.0-vmtest
export UVSS_VM_TEST=1
export GOTOOLCHAIN=local
cat /var/tmp/uvss-test-metadata
uname -a
go version
if [[ "$suite" == core || "$suite" == all ]]; then
    make vet check-scripts
    go test -race ./...
    make -C virt-temp/module
    go test -tags=integration -count=1 -timeout=120s -v -run '^TestVM' .
fi
if [[ "$suite" == package || "$suite" == all ]]; then
    bash tests/vm/package-tests.sh
fi
