#!/usr/bin/env bash
# Shared by the Proxmox entry points and the scripts executed in the guest.

readonly UVSS_TEST_IMAGE_VERSION=2

die() { echo "ERROR: $*" >&2; exit 1; }

start_timing() {
    LAST_TIMING_MS="$(date +%s%3N)"
}

timing() {
    local now elapsed
    now="$(date +%s%3N)"
    elapsed=$((now - LAST_TIMING_MS))
    printf 'TIMING %-25s %6d ms\n' "$1" "$elapsed"
    LAST_TIMING_MS="$now"
}

positive_integer() {
    [[ "$2" =~ ^[1-9][0-9]{0,6}$ ]] || die "$1 must be a positive integer (at most 7 digits)"
}

resolve_pve_kernel() {
    PVE_KERNEL_RELEASE="${PVE_KERNEL_RELEASE:-$(uname -r)}"
    [[ "$PVE_KERNEL_RELEASE" =~ ^[0-9][0-9A-Za-z.+-]*-pve$ ]] ||
        die "Expected a Proxmox kernel release, got $PVE_KERNEL_RELEASE; set PVE_KERNEL_RELEASE explicitly if needed"
}

verify_guest_template() {
    guest_exec 30 "
        actual=\$(uname -r)
        test -r /etc/uvss-test-image-version || {
            echo 'Template image version marker missing; rebuild the configured template' >&2
            exit 1
        }
        image_version=\$(cat /etc/uvss-test-image-version)
        echo \"Guest kernel: \$actual; expected: $PVE_KERNEL_RELEASE; image version: \$image_version\"
        test -f /etc/uvss-test-image
        test \"\$image_version\" = '$UVSS_TEST_IMAGE_VERSION' || {
            echo 'Template image version mismatch; expected $UVSS_TEST_IMAGE_VERSION' >&2
            exit 1
        }
        test \"\$actual\" = '$PVE_KERNEL_RELEASE'
        test \"\$(cat /etc/uvss-test-kernel)\" = '$PVE_KERNEL_RELEASE'
        test -r /lib/modules/\$actual/build/Makefile
        test -r /lib/modules/\$actual/build/Module.symvers
    "
}

verify_local_test_image() {
    [[ -f /etc/uvss-test-image ]] || die "Missing UVSS test image marker"
    [[ "$(cat /etc/uvss-test-image-version 2>/dev/null)" == "$UVSS_TEST_IMAGE_VERSION" ]] ||
        die "UVSS test image version mismatch; rebuild the template"
}

wait_for_vm_stopped() {
    local seconds="$1"
    local deadline=$((SECONDS + seconds))
    while [[ "$(qm status "$VMID" 2>/dev/null)" != "status: stopped" ]]; do
        (( SECONDS < deadline )) || die "VM $VMID did not stop in time"
        sleep 2
    done
}

# qm can return a PID without an exit code when its own timeout expires.
# Bound the command in the guest first, and require a completed JSON result.
guest_exec() {
    local seconds="$1" command="$2" input="${3:-/dev/null}" result
    result="$(qm guest exec "$VMID" --timeout "$((seconds + 15))" --pass-stdin 1 \
        -- /usr/bin/timeout "$seconds" /bin/bash -euo pipefail -c "$command" < "$input")" || return
    python3 -c '
import json, sys
result = json.load(sys.stdin)
for key, stream in (("out-data", sys.stdout), ("err-data", sys.stderr)):
    stream.write(result.get(key, ""))
if not result.get("exited") or "exitcode" not in result:
    sys.exit("Guest command did not complete: " + repr(result))
if result.get("out-truncated") or result.get("err-truncated"):
    print("Guest output truncated; consult /var/tmp/uvss-tests.log in the VM", file=sys.stderr)
sys.exit(0 if result["exitcode"] == 0 else 1)
' <<< "$result"
}

wait_for_guest() {
    local deadline=$((SECONDS + BOOT_TIMEOUT))
    until qm guest cmd "$VMID" ping >/dev/null 2>&1; do
        (( SECONDS < deadline )) || die "Timed out waiting for QEMU Guest Agent on VM $VMID"
        sleep 3
    done
}

# Only the known Proxmox user-data deprecation is tolerated. Other guest
# commands (including the tests) must still return zero to succeed.
wait_for_cloud_init() {
    guest_exec "$CLOUD_INIT_TIMEOUT" 'python3 -' /dev/stdin <<'PY'
import json
import subprocess
import sys

result = subprocess.run(
    ["cloud-init", "status", "--wait", "--format=json"],
    capture_output=True, text=True,
)
sys.stdout.write(result.stdout)
sys.stderr.write(result.stderr)
if result.returncode not in (0, 2):
    sys.exit("Cloud-Init failed with exit code " + str(result.returncode))
status = json.loads(result.stdout)
if status.get("status") != "done" or status.get("errors"):
    sys.exit("Cloud-Init did not finish successfully")
recoverable = status.get("recoverable_errors", {})
if set(recoverable) - {"DEPRECATED"}:
    sys.exit("Cloud-Init reported an unexpected recoverable error category")
messages = recoverable.get("DEPRECATED", [])
known = (
    "'user' of type string is deprecated in 22.2 and scheduled to be removed "
    "in 27.2. Use 'users' list instead."
)
if any(message != known for message in messages) or (result.returncode == 2 and not messages):
    sys.exit("Cloud-Init reported unexpected recoverable errors; inspect the output above")
if messages:
    print("WARNING: accepting the known Proxmox Cloud-Init 'user' deprecation", file=sys.stderr)
PY
}
