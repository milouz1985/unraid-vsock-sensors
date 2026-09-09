#!/usr/bin/env bash
set -Eeuo pipefail
SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
REPO_DIR="$(cd -- "$SCRIPT_DIR/../.." && pwd)"

if [[ "${1:-}" == --help ]]; then
    echo "Usage: $0 [--keep] (root on Proxmox; settings in tests/vm/template.env)"
    exit 0
fi
# shellcheck source=/dev/null
[[ ! -f "$SCRIPT_DIR/template.env" ]] || source "$SCRIPT_DIR/template.env"
# shellcheck source=common.sh
source "$SCRIPT_DIR/common.sh"
resolve_pve_kernel
TEMPLATE_VMID="${VMID:-9000}"
VMID="${TEST_VMID:-9900}"
BOOT_TIMEOUT="${BOOT_TIMEOUT:-300}"
CLOUD_INIT_TIMEOUT="${CLOUD_INIT_TIMEOUT:-600}"
TEST_TIMEOUT="${TEST_TIMEOUT:-1800}"
STORAGE="${STORAGE:-local-lvm}"
TEST_DISK_SIZE_GIB="${TEST_DISK_SIZE_GIB:-1}"
VM_TEST_SUITE="${VM_TEST_SUITE:-core}"
KEEP=0
if [[ "${1:-}" == --keep ]]; then KEEP=1; shift; fi
(( $# == 0 )) || die "Usage: $0 [--keep]"
[[ $EUID -eq 0 ]] || die "Run on the Proxmox node as root"
for cmd in qm python3 git tar base64 split flock; do
    command -v "$cmd" >/dev/null || die "Required command not found: $cmd"
done
for name in TEMPLATE_VMID VMID BOOT_TIMEOUT CLOUD_INIT_TIMEOUT TEST_TIMEOUT TEST_DISK_SIZE_GIB; do
    positive_integer "$name" "${!name}"
done
[[ "$VM_TEST_SUITE" == core || "$VM_TEST_SUITE" == package || "$VM_TEST_SUITE" == all ]] ||
    die "VM_TEST_SUITE must be core, package or all"
[[ "$VMID" != "$TEMPLATE_VMID" ]] || die "TEST_VMID must differ from VMID"
exec 8>"/run/lock/uvss-vm-${TEMPLATE_VMID}.lock"
flock -s -n 8 || die "Template $TEMPLATE_VMID is currently being rebuilt"
qm config "$TEMPLATE_VMID" | grep -qx 'template: 1' || die "VM $TEMPLATE_VMID is not a template"
qm config "$TEMPLATE_VMID" | grep -Eq '^tags: ([^;]+;)*uvss-test-template(;|$)' ||
    die "VM $TEMPLATE_VMID is not tagged as an UVSS test template"
exec 9>"/run/lock/uvss-vm-${VMID}.lock"
flock -x -n 9 || die "Another UVSS operation is using VMID $VMID"
if qm config "$VMID" >/dev/null 2>&1; then die "VMID $VMID already exists; choose an unused TEST_VMID"; fi

WORK_DIR="$(mktemp -d /var/tmp/uvss-test.XXXXXX)"
CREATED=0
cleanup() {
    local rc=$?
    rm -rf -- "$WORK_DIR"
    if (( CREATED )); then
        echo "VM $VMID retained. Log: /var/tmp/uvss-tests.log inside the guest." >&2
        echo "Inspect with: qm guest exec $VMID -- cat /var/tmp/uvss-tests.log" >&2
    fi
    exit "$rc"
}
trap cleanup EXIT

# Include current edits and new source files, excluding ignored local settings
# and build artifacts. Do not require a commit just to test a change.
cd "$REPO_DIR"
git ls-files -z --cached --others --exclude-standard | python3 -c '
import os, sys
paths = sys.stdin.buffer.read().split(b"\0")
sys.stdout.buffer.write(b"".join(p + b"\0" for p in paths if p and os.path.lexists(p)))
' > "$WORK_DIR/files"
tar --null --verbatim-files-from -czf "$WORK_DIR/source.tar.gz" -T "$WORK_DIR/files"
SOURCE_SHA256="$(sha256sum "$WORK_DIR/source.tar.gz" | awk '{print $1}')"
{
    printf 'Git HEAD: %s\n' "$(git rev-parse HEAD)"
    printf 'Git status:\n'
    git status --short
    printf 'Source SHA256: %s\n' "$SOURCE_SHA256"
    printf 'PVE kernel target: %s\n' "$PVE_KERNEL_RELEASE"
    printf 'Template image version: %s\n' "$UVSS_TEST_IMAGE_VERSION"
    printf 'Go toolchain: %s\n' "$(tr -d '[:space:]' < "$SCRIPT_DIR/go-version")"
    printf 'VM test suite: %s\n' "$VM_TEST_SUITE"
} > "$WORK_DIR/metadata"
base64 -w 0 "$WORK_DIR/source.tar.gz" > "$WORK_DIR/source.b64"
# QGA input is limited; keep each request well below its 1 MiB limit.
split -b 49152 "$WORK_DIR/source.b64" "$WORK_DIR/chunk-"

echo "Cloning template $TEMPLATE_VMID to VM $VMID"
qm clone "$TEMPLATE_VMID" "$VMID" --name "uvss-test-$VMID" --full 1
CREATED=1
qm set "$VMID" --tags uvss-test-run --ciupgrade 0
qm set "$VMID" --sata1 "${STORAGE}:${TEST_DISK_SIZE_GIB},serial=UVSSDISK1"
qm set "$VMID" --sata2 "${STORAGE}:${TEST_DISK_SIZE_GIB},serial=UVSSDISK2"
qm start "$VMID"
wait_for_guest
wait_for_cloud_init
verify_guest_template
guest_exec 30 'mkdir -p /var/tmp/uvss-source; test ! -e /var/tmp/uvss-source.tar.gz; touch /var/tmp/uvss-source.tar.gz'
for chunk in "$WORK_DIR"/chunk-*; do
    guest_exec 30 'base64 -d >> /var/tmp/uvss-source.tar.gz' "$chunk"
done
guest_exec 30 'cat > /var/tmp/uvss-test-metadata' "$WORK_DIR/metadata"
guest_exec 30 "test \"\$(sha256sum /var/tmp/uvss-source.tar.gz | awk '{print \$1}')\" = '$SOURCE_SHA256'"
guest_exec 60 'tar -xzf /var/tmp/uvss-source.tar.gz -C /var/tmp/uvss-source'
echo "Running checks and real kernel tests in VM $VMID"
TEST_PASSED=0
if guest_exec "$TEST_TIMEOUT" "cd /var/tmp/uvss-source; bash tests/vm/guest-tests.sh '$VM_TEST_SUITE' > /var/tmp/uvss-tests.log 2>&1"; then
    TEST_PASSED=1
fi
# Package/DKMS logs can exceed QGA's output limit. Save them in chunks before
# deleting a successful clone, so the complete results remain available.
mkdir -p "$REPO_DIR/dist"
LOG_FILE="$(mktemp "$REPO_DIR/dist/vm-tests-${VMID}.XXXXXX.log")"
block=0
while true; do
    chunk="$(guest_exec 30 "dd if=/var/tmp/uvss-tests.log bs=32768 skip=$block count=1 status=none | base64 -w 0")"
    [[ -n "$chunk" ]] || break
    printf '%s' "$chunk" | base64 -d >> "$LOG_FILE"
    block=$((block + 1))
done
cat "$LOG_FILE"
echo "Complete test log saved to $LOG_FILE"
(( TEST_PASSED )) || die "VM tests failed"
if (( ! KEEP )); then
    qm shutdown "$VMID" --timeout 120
    wait_for_vm_stopped 180
    qm destroy "$VMID" --purge
    CREATED=0
fi
echo "VM integration checks passed"
