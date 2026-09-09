#!/usr/bin/env bash
# Sourced by the two Proxmox entry points; VMID is their current target.

die() { echo "ERROR: $*" >&2; exit 1; }

positive_integer() {
    [[ "$2" =~ ^[1-9][0-9]{0,6}$ ]] || die "$1 must be a positive integer (at most 7 digits)"
}

resolve_pve_kernel() {
    PVE_KERNEL_RELEASE="${PVE_KERNEL_RELEASE:-$(uname -r)}"
    [[ "$PVE_KERNEL_RELEASE" =~ ^[0-9][0-9A-Za-z.+-]*-pve$ ]] ||
        die "Expected a Proxmox kernel release, got $PVE_KERNEL_RELEASE; set PVE_KERNEL_RELEASE explicitly if needed"
}

verify_guest_kernel() {
    guest_exec 30 "
        actual=\$(uname -r)
        echo \"Guest kernel: \$actual; expected: $PVE_KERNEL_RELEASE\"
        test \"\$actual\" = '$PVE_KERNEL_RELEASE'
        test \"\$(cat /etc/uvss-test-kernel)\" = '$PVE_KERNEL_RELEASE'
        test -r /lib/modules/\$actual/build/Makefile
        test -r /lib/modules/\$actual/build/Module.symvers
    "
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
messages = [message for entries in recoverable.values() for message in entries]
known = "'user' of type string is deprecated"
if any(known not in message for message in messages) or (result.returncode == 2 and not messages):
    sys.exit("Cloud-Init reported unexpected recoverable errors; inspect the output above")
if messages:
    print("WARNING: accepting the known Proxmox Cloud-Init 'user' deprecation", file=sys.stderr)
PY
}
