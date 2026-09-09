#!/usr/bin/env bash
set -Eeuo pipefail
cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.."
[[ $EUID -eq 0 && -f /etc/uvss-test-image ]] || {
    echo "Run only as root in a disposable UVSS test VM" >&2; exit 1;
}
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
uname -a
go version
make check
go test -race ./...
make -C virt-temp/module
go test -tags=integration -count=1 -timeout=120s -v -run '^TestVM' .
bash tests/vm/package-tests.sh
