#!/usr/bin/env bash
set -Eeuo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"

if [[ -f "${SCRIPT_DIR}/template.env" ]]; then
    # shellcheck disable=SC1091
    source "${SCRIPT_DIR}/template.env"
fi

VMID="${VMID:-9000}"
NAME="${NAME:-uvss-debian13-test-template}"
STORAGE="${STORAGE:-zfs-pve}"
BRIDGE="${BRIDGE:-vmbr0}"

DEBIAN_VERSION="${DEBIAN_VERSION:-13}"
DEBIAN_CODENAME="${DEBIAN_CODENAME:-trixie}"
GO_VERSION="$(tr -d '[:space:]' < "$SCRIPT_DIR/go-version")"
PVE_KEYRING_FILE="${PVE_KEYRING_FILE:-/usr/share/keyrings/proxmox-archive-keyring.gpg}"
PVE_REPO_COMPONENT="${PVE_REPO_COMPONENT:-pve-no-subscription}"

CI_USER="${CI_USER:-uvss-test}"
SSH_PUBLIC_KEY_FILE="${SSH_PUBLIC_KEY_FILE:-${HOME}/.ssh/id_ed25519.pub}"

DISK_SIZE="${DISK_SIZE:-16G}"
CORES="${CORES:-2}"
MEMORY="${MEMORY:-2048}"

IPCONFIG0="${IPCONFIG0:-ip=dhcp}"
NAMESERVER="${NAMESERVER-}"
SEARCHDOMAIN="${SEARCHDOMAIN-}"

BOOT_TIMEOUT="${BOOT_TIMEOUT:-300}"
CLOUD_INIT_TIMEOUT="${CLOUD_INIT_TIMEOUT:-600}"

CACHE_DIR="${CACHE_DIR:-/var/cache/uvss-template}"
IMAGE_NAME="${IMAGE_NAME:-debian-${DEBIAN_VERSION}-generic-amd64.qcow2}"
IMAGE_BASE_URL="${IMAGE_BASE_URL:-https://cloud.debian.org/images/cloud/${DEBIAN_CODENAME}/latest}"
IMAGE_URL="${IMAGE_URL:-${IMAGE_BASE_URL}/${IMAGE_NAME}}"
SHA512_URL="${SHA512_URL:-${IMAGE_BASE_URL}/SHA512SUMS}"

GO_ARCHIVE="go${GO_VERSION}.linux-amd64.tar.gz"
GO_URL="https://go.dev/dl/${GO_ARCHIVE}"

REPLACE=0
WORK_DIR=""

usage() {
    cat <<EOF
Usage: $0 [--replace]

Build a Debian template booting the target Proxmox kernel for UVSS tests.

  --replace   Destroy VM/template ${VMID} first if it already exists.

Important:
  This version pre-bakes qemu-guest-agent and test dependencies into the
  Debian QCOW2 with virt-customize BEFORE the first boot. Cloud-Init is then
  used only for per-instance identity/network/user configuration.

Configuration:
  ${SCRIPT_DIR}/template.env
EOF
}

while [[ $# -gt 0 ]]; do
    case "$1" in
        --replace) REPLACE=1 ;;
        -h|--help) usage; exit 0 ;;
        *) echo "Unknown argument: $1" >&2; usage >&2; exit 2 ;;
    esac
    shift
done

log() {
    printf '\n==> %s\n' "$*"
}

die() {
    printf '\nERROR: %s\n' "$*" >&2
    exit 1
}

require_cmd() {
    command -v "$1" >/dev/null 2>&1 ||
        die "Required command not found: $1"
}

vm_exists() {
    qm config "$VMID" >/dev/null 2>&1
}

vm_status() {
    qm status "$VMID" 2>/dev/null | awk '{print $2}'
}

cleanup() {
    if [[ -n "${WORK_DIR:-}" && -d "$WORK_DIR" ]]; then
        rm -rf "$WORK_DIR"
    fi
}

on_exit() {
    local rc=$?
    trap - EXIT
    set +e
    cleanup

    if (( rc != 0 )); then
        echo >&2
        echo "Build failed with exit code ${rc}." >&2

        if vm_exists; then
            echo "VM ${VMID} is intentionally left in place for diagnosis." >&2
            echo "Useful commands:" >&2
            echo "  qm status ${VMID}" >&2
            echo "  qm terminal ${VMID}" >&2
            echo "  qm guest cmd ${VMID} ping" >&2
            echo "  qm guest exec ${VMID} -- /bin/bash -lc 'cloud-init status --long'" >&2
            echo "  qm guest exec ${VMID} -- /bin/bash -lc 'tail -n 200 /var/log/cloud-init-output.log'" >&2
        fi
    fi

    exit "$rc"
}

trap on_exit EXIT

[[ $EUID -eq 0 ]] || die "Run this script as root on the Proxmox node"

for cmd in qm pvesm curl qemu-img sha512sum sha256sum virt-customize virt-resize virt-filesystems python3 awk sed flock; do
    require_cmd "$cmd"
done

# shellcheck source=common.sh
source "$SCRIPT_DIR/common.sh"
resolve_pve_kernel
[[ "$DEBIAN_VERSION" == 13 && "$DEBIAN_CODENAME" == trixie ]] ||
    die "This Proxmox kernel template currently supports Debian 13 / trixie"
[[ "$PVE_REPO_COMPONENT" == pve-no-subscription || "$PVE_REPO_COMPONENT" == pve-test ]] ||
    die "PVE_REPO_COMPONENT must be pve-no-subscription or pve-test"
[[ -r "$PVE_KEYRING_FILE" ]] || die "Proxmox archive keyring not readable: $PVE_KEYRING_FILE"
for name in VMID CORES MEMORY BOOT_TIMEOUT CLOUD_INIT_TIMEOUT; do
    positive_integer "$name" "${!name}"
done
[[ "$GO_VERSION" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] || die "Invalid GO_VERSION"
exec 9>"/run/lock/uvss-vm-${VMID}.lock"
flock -x -n 9 || die "The template VMID $VMID is being built or used by a test run"
if vm_exists; then
    (( REPLACE )) || die "VMID $VMID already exists; use --replace for a UVSS template"
    qm config "$VMID" | grep -Eq '^tags: ([^;]+;)*uvss-test-template(;|$)' ||
        die "Refusing to replace VM $VMID without the uvss-test-template tag"
fi

[[ -r "$SSH_PUBLIC_KEY_FILE" ]] ||
    die "SSH public key not readable: ${SSH_PUBLIC_KEY_FILE}"

grep -Eq '^(ssh-(ed25519|rsa)|ecdsa-sha2-)' "$SSH_PUBLIC_KEY_FILE" ||
    die "${SSH_PUBLIC_KEY_FILE} does not look like an OpenSSH public key"

pvesm status | awk 'NR > 1 {print $1}' | grep -Fxq "$STORAGE" ||
    die "Proxmox storage '${STORAGE}' not found"

mkdir -p "$CACHE_DIR"
exec 7>"${CACHE_DIR}/build.lock"
flock -n 7 || die "Another template build is using $CACHE_DIR"
WORK_DIR="$(mktemp -d /var/tmp/uvss-template.XXXXXX)"

SOURCE_IMAGE="${CACHE_DIR}/${IMAGE_NAME}"
SUMS_FILE="${CACHE_DIR}/SHA512SUMS-${DEBIAN_CODENAME}"
CUSTOM_IMAGE="${WORK_DIR}/${IMAGE_NAME}"
GO_TARBALL="${WORK_DIR}/${GO_ARCHIVE}"

log "Downloading Debian ${DEBIAN_VERSION} generic image"
curl -fL \
    --retry 4 \
    --retry-delay 2 \
    --connect-timeout 20 \
    -o "${SOURCE_IMAGE}.tmp" \
    "$IMAGE_URL"
mv -f "${SOURCE_IMAGE}.tmp" "$SOURCE_IMAGE"

log "Verifying Debian SHA512"
curl -fL \
    --retry 4 \
    --retry-delay 2 \
    --connect-timeout 20 \
    -o "$SUMS_FILE" \
    "$SHA512_URL"

expected_debian_sha="$(
    awk -v file="$IMAGE_NAME" '
        $2 == file || $2 == "*" file || $2 == "./" file || $2 == "*" "./" file {
            print $1
            exit
        }
    ' "$SUMS_FILE"
)"

[[ -n "$expected_debian_sha" ]] ||
    die "Could not find ${IMAGE_NAME} in SHA512SUMS"

printf '%s  %s\n' "$expected_debian_sha" "$SOURCE_IMAGE" | sha512sum -c -

qemu-img info "$SOURCE_IMAGE" |
    grep -q '^file format: qcow2$' ||
    die "Downloaded Debian image is not QCOW2"

log "Expanding the root filesystem before installing dependencies"
root_partition="$(virt-filesystems -a "$SOURCE_IMAGE" --filesystems --long --no-title | awk '$3 == "ext4" { print $1 }')"
[[ "$root_partition" =~ ^/dev/sd[a-z][0-9]+$ ]] || die "Expected one ext4 root filesystem, got: $root_partition"
qemu-img create -f qcow2 "$CUSTOM_IMAGE" "$DISK_SIZE"
virt-resize --expand "$root_partition" "$SOURCE_IMAGE" "$CUSTOM_IMAGE"

log "Downloading Go ${GO_VERSION}"
curl -fL \
    --retry 4 \
    --retry-delay 2 \
    --connect-timeout 20 \
    -o "$GO_TARBALL" \
    "$GO_URL"

log "Obtaining official Go SHA256"
GO_SHA256="$(
    curl -fsSL 'https://go.dev/dl/?mode=json&include=all' |
        python3 -c '
import json
import sys

filename = sys.argv[1]
data = json.load(sys.stdin)
for release in data:
    for item in release.get("files", []):
        if item.get("filename") == filename:
            print(item["sha256"])
            raise SystemExit(0)
raise SystemExit(1)
' "$GO_ARCHIVE"
)"

[[ "$GO_SHA256" =~ ^[0-9a-f]{64}$ ]] ||
    die "Unable to obtain a valid SHA256 for ${GO_ARCHIVE}"

printf '%s  %s\n' "$GO_SHA256" "$GO_TARBALL" | sha256sum -c -

log "Installing Proxmox kernel $PVE_KERNEL_RELEASE and test dependencies into the image"
virt-customize \
    -a "$CUSTOM_IMAGE" \
    --install 'qemu-guest-agent,openssh-server,ca-certificates,curl,git,rsync,jq,python3,smartmontools,lm-sensors,hdparm,pciutils,usbutils,lsscsi,build-essential,make,gcc,pkg-config,kmod,php-cli,dkms' \
    --upload "${PVE_KEYRING_FILE}:/usr/share/keyrings/uvss-proxmox.gpg" \
    --write "/etc/apt/sources.list.d/uvss-proxmox.sources:Types: deb
URIs: http://download.proxmox.com/debian/pve
Suites: trixie
Components: ${PVE_REPO_COMPONENT}
Signed-By: /usr/share/keyrings/uvss-proxmox.gpg
" \
    --run-command "apt-get update && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends proxmox-kernel-${PVE_KERNEL_RELEASE}-signed proxmox-headers-${PVE_KERNEL_RELEASE} proxmox-default-headers" \
    --write "/etc/uvss-test-kernel:${PVE_KERNEL_RELEASE}
" \
    --mkdir /etc/default/grub.d \
    --write "/etc/default/grub.d/99-uvss-test-kernel.cfg:GRUB_DEFAULT=0
GRUB_TOP_LEVEL=/boot/vmlinuz-${PVE_KERNEL_RELEASE}
" \
    --run-command 'update-grub' \
    --write '/etc/uvss-test-image:Disposable UVSS integration test image' \
    --write "/etc/uvss-test-image-version:${UVSS_TEST_IMAGE_VERSION}
" \
    --upload "${GO_TARBALL}:/tmp/${GO_ARCHIVE}" \
    --run-command "rm -rf /usr/local/go && tar -C /usr/local -xzf /tmp/${GO_ARCHIVE}" \
    --run-command 'ln -sf /usr/local/go/bin/go /usr/local/bin/go' \
    --run-command 'ln -sf /usr/local/go/bin/gofmt /usr/local/bin/gofmt' \
    --run-command "rm -f /tmp/${GO_ARCHIVE}" \
    --write '/etc/modules-load.d/uvss-test.conf:drivetemp
vsock_loopback
' \
    --run-command 'rm -f /etc/ssh/ssh_host_*' \
    --truncate '/etc/machine-id'

if vm_exists; then
    if (( REPLACE == 0 )); then
        die "VMID ${VMID} already exists. Re-run with --replace to destroy and rebuild it."
    fi

    log "Destroying existing VM/template ${VMID} - DESTRUCTIVE"
    if [[ "$(vm_status)" == "running" ]]; then
        qm shutdown "$VMID" --timeout 60 || qm stop "$VMID"
    fi
    qm config "$VMID" | grep -Eq '^tags: ([^;]+;)*uvss-test-template(;|$)' ||
        die "Refusing to replace VM $VMID without the uvss-test-template tag"
    qm destroy "$VMID" --purge
fi

log "Creating VM ${VMID}"
qm create "$VMID" \
    --name "$NAME" \
    --ostype l26 \
    --cpu host \
    --cores "$CORES" \
    --memory "$MEMORY" \
    --scsihw virtio-scsi-single \
    --net0 "virtio,bridge=${BRIDGE}" \
    --serial0 socket \
    --vga serial0 \
    --agent enabled=1 \
    --tags uvss-test-template

log "Importing customized Debian image into ${STORAGE}"
qm importdisk "$VMID" "$CUSTOM_IMAGE" "$STORAGE"

imported_volume="$(
    qm config "$VMID" |
        sed -n 's/^unused0: \([^,]*\).*/\1/p' |
        head -n1
)"

[[ -n "$imported_volume" ]] ||
    die "Unable to determine imported volume from unused0"

log "Attaching root disk ${imported_volume}"
qm set "$VMID" --scsi0 "${imported_volume},discard=on,ssd=1"
qm set "$VMID" --boot order=scsi0

log "Adding standard Proxmox Cloud-Init configuration"
qm set "$VMID" --ide2 "${STORAGE}:cloudinit"
qm set "$VMID" --ciuser "$CI_USER"
qm set "$VMID" --ciupgrade 0
qm set "$VMID" --sshkeys "$SSH_PUBLIC_KEY_FILE"
qm set "$VMID" --ipconfig0 "$IPCONFIG0"

if [[ -n "$NAMESERVER" ]]; then
    qm set "$VMID" --nameserver "$NAMESERVER"
fi

if [[ -n "$SEARCHDOMAIN" ]]; then
    qm set "$VMID" --searchdomain "$SEARCHDOMAIN"
fi

qm cloudinit update "$VMID"

log "Starting validation boot"
qm start "$VMID"

log "Waiting for QEMU Guest Agent - it is already baked into the image"
deadline=$((SECONDS + BOOT_TIMEOUT))
last_notice=-1
while (( SECONDS < deadline )); do
    if [[ "$(vm_status || true)" != "running" ]]; then
        die "VM ${VMID} stopped while waiting for QEMU Guest Agent"
    fi

    if qm guest cmd "$VMID" ping >/dev/null 2>&1; then
        echo "QEMU Guest Agent is ready."
        break
    fi

    elapsed=$((BOOT_TIMEOUT - (deadline - SECONDS)))
    if (( elapsed / 15 != last_notice )); then
        printf '  waiting... %ss\n' "$elapsed"
        last_notice=$((elapsed / 15))
    fi
    sleep 3
done

qm guest cmd "$VMID" ping >/dev/null 2>&1 ||
    die "Timeout waiting for QEMU Guest Agent on VM ${VMID}"

log "Waiting for Cloud-Init"
wait_for_cloud_init

log "Validating template environment"
verify_guest_template
guest_exec 60 "grep -Eq '^CONFIG_HWMON=(y|m)$' /boot/config-\$(uname -r)"
guest_exec 60 "grep -Eq '^CONFIG_SENSORS_DRIVETEMP=m$' /boot/config-\$(uname -r)"
guest_exec 60 "grep -qw 'hwmon_device_register_with_info' /lib/modules/\$(uname -r)/build/Module.symvers"
guest_exec 60 "grep -qw 'hwmon_device_unregister' /lib/modules/\$(uname -r)/build/Module.symvers"
guest_exec 60 "/usr/sbin/modprobe drivetemp"
guest_exec 60 "/usr/sbin/modinfo drivetemp >/dev/null"
guest_exec 60 "/usr/sbin/modprobe vsock_loopback"
guest_exec 60 "python3 -c 'import socket; s = socket.socket(socket.AF_VSOCK, socket.SOCK_STREAM); s.bind((socket.VMADDR_CID_ANY, 990)); s.listen(); s.close()'"
guest_exec 60 "/usr/local/bin/go version | grep -q 'go${GO_VERSION} '"
guest_exec 60 "command -v gcc >/dev/null"
guest_exec 60 "command -v smartctl >/dev/null"
guest_exec 60 "command -v sensors >/dev/null"
guest_exec 60 "systemctl is-active --quiet qemu-guest-agent"

log "Cleaning guest identity for cloning"
guest_exec 60 "
    cloud-init clean --logs --machine-id
    rm -f /etc/ssh/ssh_host_*
    sync
"

log "Shutting down validation VM"
qm shutdown "$VMID" --timeout 120

wait_for_vm_stopped 180

log "Converting VM ${VMID} to template"
qm template "$VMID"

log "Template successfully created"
qm config "$VMID"

cat <<EOF

Template ${VMID} is ready.

Run project tests from the configured development checkout:
  make test-vm

Future rebuild:
  bash ${SCRIPT_DIR}/build-template.sh --replace
EOF
