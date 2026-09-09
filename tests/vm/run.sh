#!/usr/bin/env bash
set -Eeuo pipefail
SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
REPO_DIR="$(cd -- "$SCRIPT_DIR/../.." && pwd)"

usage() {
    echo "Usage: $0 [--keep] (run from the development checkout)"
}

if [[ "${1:-}" == --help || "${1:-}" == -h ]]; then
    usage
    exit 0
fi
# shellcheck source=/dev/null
[[ ! -f "$SCRIPT_DIR/template.env" ]] || source "$SCRIPT_DIR/template.env"
# shellcheck source=common.sh
source "$SCRIPT_DIR/common.sh"

PVE_HOST="${PVE_HOST:-}"
PVE_SSH_USER="${PVE_SSH_USER:-root}"
TEMPLATE_VMID="${TEMPLATE_VMID:-${VMID:-9000}}"
VMID="${TEST_VMID:-9900}"
PVE_STORAGE="${PVE_STORAGE:-${STORAGE:-zfs-pve}}"
GUEST_USER="${GUEST_USER:-uvss-test}"
GUEST_SSH_KEY="${GUEST_SSH_KEY:-${HOME}/.ssh/id_ed25519}"
BOOT_TIMEOUT="${BOOT_TIMEOUT:-300}"
CLOUD_INIT_TIMEOUT="${CLOUD_INIT_TIMEOUT:-600}"
TEST_TIMEOUT="${TEST_TIMEOUT:-1800}"
TEST_DISK_SIZE_GIB="${TEST_DISK_SIZE_GIB:-1}"
VM_TEST_SUITE="${VM_TEST_SUITE:-all}"
KEEP="${TEST_VM_KEEP:-0}"
if [[ "${1:-}" == --keep ]]; then KEEP=1; shift; fi
(( $# == 0 )) || { usage >&2; exit 2; }

for cmd in ssh rsync python3 git sha256sum; do
    command -v "$cmd" >/dev/null || die "Required command not found: $cmd"
done
for name in TEMPLATE_VMID VMID BOOT_TIMEOUT CLOUD_INIT_TIMEOUT TEST_TIMEOUT TEST_DISK_SIZE_GIB; do
    positive_integer "$name" "${!name}"
done
[[ "$KEEP" == 0 || "$KEEP" == 1 ]] || die "TEST_VM_KEEP must be 0 or 1"
[[ "$VM_TEST_SUITE" == core || "$VM_TEST_SUITE" == package || "$VM_TEST_SUITE" == all ]] ||
    die "VM_TEST_SUITE must be core, package or all"
[[ -n "$PVE_HOST" ]] || die "Set PVE_HOST in tests/vm/template.env"
[[ -n "$PVE_SSH_USER" && -n "$PVE_STORAGE" && -n "$GUEST_USER" ]] ||
    die "PVE_SSH_USER, PVE_STORAGE and GUEST_USER must not be empty"
[[ -r "$GUEST_SSH_KEY" ]] || die "Guest SSH private key not readable: $GUEST_SSH_KEY"
[[ "$VMID" != "$TEMPLATE_VMID" ]] || die "TEST_VMID must differ from TEMPLATE_VMID"

PVE_TARGET="${PVE_SSH_USER}@${PVE_HOST}"
PVE_SSH=(ssh -o BatchMode=yes -o ConnectTimeout=10 -- "$PVE_TARGET")
pve() {
    "${PVE_SSH[@]}" "$@"
}

shell_quote() {
    local value="$1"
    printf "'%s'" "${value//\'/\'\\\'\'}"
}

WORK_DIR="$(mktemp -d /tmp/uvss-test.XXXXXX)"
CREATED=0
PVE_LOCK_PID=""
GUEST_IP=""
cleanup() {
    local rc=$?
    trap - EXIT
    if [[ -n "${PVE_LOCK_INPUT_FD:-}" ]]; then
        exec {PVE_LOCK_INPUT_FD}>&- || true
    fi
    if [[ -n "$PVE_LOCK_PID" ]]; then
        kill "$PVE_LOCK_PID" 2>/dev/null || true
        wait "$PVE_LOCK_PID" 2>/dev/null || true
    fi
    rm -rf -- "$WORK_DIR"
    if (( CREATED )); then
        echo "VM $VMID retained on $PVE_HOST." >&2
        echo "Inspect with: ssh $PVE_TARGET qm terminal $VMID" >&2
        if [[ -n "$GUEST_IP" ]]; then
            echo "Guest SSH: ssh -i $GUEST_SSH_KEY ${GUEST_USER}@${GUEST_IP}" >&2
        fi
    fi
    exit "$rc"
}
trap cleanup EXIT

echo "Connecting to Proxmox host $PVE_TARGET"
pve true || die "SSH authentication to $PVE_TARGET failed; authorize the key or load an encrypted key with: ssh-add $GUEST_SSH_KEY"
PVE_KERNEL_RELEASE="${PVE_KERNEL_RELEASE:-$(pve uname -r)}"
resolve_pve_kernel

# Keep this SSH process alive for the complete run. The locks therefore live
# on the Proxmox node that owns both VMIDs, not on the development machine.
lock_command="exec 8>/run/lock/uvss-vm-${TEMPLATE_VMID}.lock
flock -s -n 8 || exit 73
exec 9>/run/lock/uvss-vm-${VMID}.lock
flock -x -n 9 || exit 74
printf 'UVSS_LOCK_READY\\n'
cat >/dev/null"
coproc UVSS_PVE_LOCK { pve bash -c "$(shell_quote "$lock_command")"; }
PVE_LOCK_PID="$UVSS_PVE_LOCK_PID"
PVE_LOCK_INPUT_FD="${UVSS_PVE_LOCK[1]}"
if ! IFS= read -r -t 20 lock_ready <&"${UVSS_PVE_LOCK[0]}"; then
    if wait "$PVE_LOCK_PID"; then lock_rc=0; else lock_rc=$?; fi
    PVE_LOCK_PID=""
    case "$lock_rc" in
        73) die "Template $TEMPLATE_VMID is currently being rebuilt" ;;
        74) die "Another UVSS operation is using VMID $VMID" ;;
        *) die "Unable to acquire Proxmox VM locks (SSH exit $lock_rc)" ;;
    esac
fi
[[ "$lock_ready" == UVSS_LOCK_READY ]] || die "Unexpected response from Proxmox lock session"

template_config="$(pve qm config "$TEMPLATE_VMID")"
grep -qx 'template: 1' <<< "$template_config" || die "VM $TEMPLATE_VMID is not a template"
grep -Eq '^tags: ([^;]+;)*uvss-test-template(;|$)' <<< "$template_config" ||
    die "VM $TEMPLATE_VMID is not tagged as an UVSS test template"
if pve qm config "$VMID" >/dev/null 2>&1; then
    die "VMID $VMID already exists; choose an unused TEST_VMID"
fi
pve pvesm status | awk 'NR > 1 {print $1}' | grep -Fxq "$PVE_STORAGE" ||
    die "Proxmox storage '$PVE_STORAGE' not found"

# Include current edits and new non-ignored files without requiring a commit.
cd "$REPO_DIR"
git ls-files -z --cached --others --exclude-standard | python3 -c '
import os, sys
paths = sys.stdin.buffer.read().split(b"\0")
sys.stdout.buffer.write(b"".join(p + b"\0" for p in paths if p and os.path.lexists(p)))
' > "$WORK_DIR/files"
while IFS= read -r -d '' path; do
    sha256sum -- "$path"
done < "$WORK_DIR/files" > "$WORK_DIR/source.sha256"
SOURCE_SHA256="$(sha256sum "$WORK_DIR/source.sha256" | awk '{print $1}')"
{
    printf 'Git HEAD: %s\n' "$(git rev-parse HEAD)"
    printf 'Git status:\n'
    git status --short
    printf 'Source manifest SHA256: %s\n' "$SOURCE_SHA256"
    printf 'PVE host: %s\n' "$PVE_HOST"
    printf 'PVE kernel target: %s\n' "$PVE_KERNEL_RELEASE"
    printf 'Template image version: %s\n' "$UVSS_TEST_IMAGE_VERSION"
    printf 'Go toolchain: %s\n' "$(tr -d '[:space:]' < "$SCRIPT_DIR/go-version")"
    printf 'VM test suite: %s\n' "$VM_TEST_SUITE"
} > "$WORK_DIR/uvss-test-metadata"

echo "Cloning Proxmox template $TEMPLATE_VMID -> $VMID"
pve qm clone "$TEMPLATE_VMID" "$VMID" --name "uvss-test-$VMID" --full 0
CREATED=1
pve qm set "$VMID" --tags uvss-test-run --ciupgrade 0
echo "Adding virtual HDDs"
pve qm set "$VMID" --sata1 "${PVE_STORAGE}:${TEST_DISK_SIZE_GIB},serial=UVSSDISK1"
pve qm set "$VMID" --sata2 "${PVE_STORAGE}:${TEST_DISK_SIZE_GIB},serial=UVSSDISK2"
echo "Starting VM"
pve qm start "$VMID"

echo "Waiting for QEMU Guest Agent"
deadline=$((SECONDS + BOOT_TIMEOUT))
until pve qm guest cmd "$VMID" ping >/dev/null 2>&1; do
    (( SECONDS < deadline )) || die "Timed out waiting for QEMU Guest Agent on VM $VMID"
    sleep 3
done

echo "Discovering guest IP"
deadline=$((SECONDS + BOOT_TIMEOUT))
while (( SECONDS < deadline )); do
    network_json="$(pve qm guest cmd "$VMID" network-get-interfaces 2>/dev/null || true)"
    GUEST_IP="$(python3 -c '
import ipaddress, json, sys
try:
    interfaces = json.load(sys.stdin)
except (json.JSONDecodeError, TypeError):
    raise SystemExit(0)
for interface in interfaces:
    if interface.get("name") == "lo":
        continue
    for address in interface.get("ip-addresses", []):
        if address.get("ip-address-type") != "ipv4":
            continue
        try:
            candidate = ipaddress.ip_address(address.get("ip-address", ""))
        except ValueError:
            continue
        if not candidate.is_loopback and not candidate.is_link_local:
            print(candidate)
            raise SystemExit(0)
' <<< "$network_json")"
    [[ -n "$GUEST_IP" ]] && break
    sleep 2
done
[[ -n "$GUEST_IP" ]] || die "QEMU Guest Agent did not report a usable IPv4 address"
echo "Guest IP: $GUEST_IP"

GUEST_TARGET="${GUEST_USER}@${GUEST_IP}"
GUEST_KNOWN_HOSTS="$WORK_DIR/known_hosts"
GUEST_SSH=(ssh -i "$GUEST_SSH_KEY" -o BatchMode=yes -o ConnectTimeout=10 \
    -o StrictHostKeyChecking=accept-new -o "UserKnownHostsFile=$GUEST_KNOWN_HOSTS" -- "$GUEST_TARGET")
guest() {
    "${GUEST_SSH[@]}" "$@"
}
guest_exec() {
    local seconds="$1" command="$2" input="${3:-/dev/null}"
    guest "sudo -n /usr/bin/timeout $seconds /bin/bash -euo pipefail -c $(shell_quote "$command")" < "$input"
}

echo "Waiting for guest SSH"
deadline=$((SECONDS + BOOT_TIMEOUT))
until guest true >/dev/null 2>&1; do
    (( SECONDS < deadline )) || die "Timed out waiting for SSH on $GUEST_TARGET"
    sleep 3
done
wait_for_cloud_init
verify_guest_template

echo "Uploading working tree"
guest 'rm -rf /var/tmp/uvss-source && mkdir -p /var/tmp/uvss-source'
RSYNC_SSH="ssh -i $GUEST_SSH_KEY -o BatchMode=yes -o ConnectTimeout=10 -o StrictHostKeyChecking=accept-new -o UserKnownHostsFile=$GUEST_KNOWN_HOSTS"
rsync -a --from0 --files-from="$WORK_DIR/files" -e "$RSYNC_SSH" \
    "$REPO_DIR/" "$GUEST_TARGET:/var/tmp/uvss-source/"
rsync -a -e "$RSYNC_SSH" "$WORK_DIR/uvss-test-metadata" "$WORK_DIR/source.sha256" \
    "$GUEST_TARGET:/var/tmp/"
guest_exec 60 "test \"\$(sha256sum /var/tmp/source.sha256 | awk '{print \$1}')\" = '$SOURCE_SHA256'; cd /var/tmp/uvss-source; sha256sum -c /var/tmp/source.sha256"

echo "Running VM test suite: $VM_TEST_SUITE"
TEST_PASSED=0
if guest_exec "$TEST_TIMEOUT" "cd /var/tmp/uvss-source; bash tests/vm/guest-tests.sh '$VM_TEST_SUITE' 2>&1 | tee /var/tmp/uvss-tests.log"; then
    TEST_PASSED=1
fi

mkdir -p "$REPO_DIR/dist"
LOG_FILE="$(mktemp "$REPO_DIR/dist/vm-tests-${VMID}.XXXXXX.log")"
echo "Downloading logs"
if rsync -a -e "$RSYNC_SSH" "$GUEST_TARGET:/var/tmp/uvss-tests.log" "$LOG_FILE"; then
    echo "Complete test log saved to $LOG_FILE"
else
    echo "Unable to download /var/tmp/uvss-tests.log" >&2
fi
(( TEST_PASSED )) || die "VM tests failed"

if (( ! KEEP )); then
    echo "Shutting down VM $VMID"
    if ! pve qm shutdown "$VMID" --timeout 120; then
        echo "qm shutdown returned an error; waiting for the final VM state" >&2
    fi
    deadline=$((SECONDS + 180))
    while [[ "$(pve qm status "$VMID" 2>/dev/null)" != "status: stopped" ]]; do
        (( SECONDS < deadline )) || die "VM $VMID did not stop in time"
        sleep 2
    done
    echo "Destroying VM $VMID"
    pve qm destroy "$VMID" --purge
    CREATED=0
fi
echo "PASS"
