#!/usr/bin/env bash
set -euo pipefail

repo_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd)"
test_root="$(mktemp -d)"
trap 'rm -rf -- "$test_root"' EXIT
mkdir -p "$test_root"/{bin,dkms/virt-temp/old,src/virt-temp-old,src/virt-temp-new,modules/A/build,modules/B/build,saved,configfs/disk,configfs/hba}
touch "$test_root/src/virt-temp-old/dkms.conf" "$test_root/src/virt-temp-old/virt-temp.c"
touch "$test_root/src/virt-temp-new/dkms.conf" "$test_root/default" "$test_root/proc_modules"
ln -s "$test_root/src/virt-temp-old" "$test_root/dkms/virt-temp/old/source"

export TEST_DKMS_DIR="$test_root/dkms"
export TEST_SOURCE_DIR="$test_root/src"
export TEST_EVENTS="$test_root/events"
cat > "$test_root/bin/dkms" <<'SH'
#!/bin/sh
command=$1
shift
version=""
kernel=""
while [ "$#" -gt 0 ]; do
    case "$1" in
        -v) version=$2; shift ;;
        -k) kernel=$2; shift ;;
    esac
    shift
done
case "$command" in
    status)
        if [ -L "$TEST_DKMS_DIR/virt-temp/$version/source" ]; then
            printf 'virt-temp/%s: installed\n' "$version"
        fi
        ;;
    add)
        printf 'add %s\n' "$version" >> "$TEST_EVENTS"
        mkdir -p "$TEST_DKMS_DIR/virt-temp/$version"
        ln -s "$TEST_SOURCE_DIR/virt-temp-$version" "$TEST_DKMS_DIR/virt-temp/$version/source"
        ;;
    build)
        printf 'build %s\n' "$kernel" >> "$TEST_EVENTS"
        [ "${TEST_FAIL_KERNEL:-}" != "$kernel" ]
        ;;
    install) printf 'install %s\n' "$kernel" >> "$TEST_EVENTS" ;;
    remove)
        printf 'remove %s\n' "$version" >> "$TEST_EVENTS"
        rm -rf -- "$TEST_DKMS_DIR/virt-temp/$version"
        ;;
esac
SH
cat > "$test_root/bin/systemctl" <<'SH'
#!/bin/sh
printf 'systemctl %s\n' "$*" >> "$TEST_EVENTS"
case "$1" in
    is-active)
        exit 1
        ;;
esac
SH
cat > "$test_root/bin/deb-systemd-invoke" <<'SH'
#!/bin/sh
printf 'deb-systemd-invoke %s\n' "$1" >> "$TEST_EVENTS"
SH
cat > "$test_root/bin/modprobe" <<'SH'
#!/bin/sh
printf 'modprobe %s\n' "$*" >> "$TEST_EVENTS"
SH
chmod +x "$test_root/bin/"*
export PATH="$test_root/bin:$PATH"

restart_command="$(sed -n 's/^ExecStart=//p' "$repo_dir/virt-temp/unraid-vsock-hwmon-restart@.service")"
for instance in foo my-fan coolercontrold; do
    expanded_command=${restart_command//%i/$instance}
    [[ "$expanded_command" == "/usr/bin/systemctl try-restart --no-block -- $instance.service" ]]
    [[ "$expanded_command" != *.service.service ]]
done
echo "Restart template expansion: OK"

render_script() {
    local script=$1 version=$2 output=$3
    sed \
        -e "s|@VERSION@|$version|g" \
        -e "s|/var/lib/unraid-vsock-sensors/dkms-sources|$test_root/saved|g" \
        -e "s|/var/lib/unraid-vsock-sensors|$test_root/state|g" \
        -e "s|/var/lib/dkms|$test_root/dkms|g" \
        -e "s|/usr/src|$test_root/src|g" \
        -e "s|/lib/modules|$test_root/modules|g" \
        -e "s|/etc/default/unraid-vsock-hwmon|$test_root/config|g" \
        -e "s|/etc/systemd/system|$test_root/systemd|g" \
        -e "s|/run/unraid-vsock-sensors|$test_root/runtime|g" \
        -e "s|/usr/share/unraid-vsock-sensors-hwmon/unraid-vsock-hwmon.default|$test_root/default|g" \
        -e "s|/proc/modules|$test_root/proc_modules|g" \
        -e "s|/sys/kernel/config/virt_temp|$test_root/configfs|g" \
        -e "s|/usr/local/sbin/uninstall-unraid-vsock-hwmon|$test_root/absent-installer|g" \
        -e "s|kernel_release=\"\$(uname -r)\"|kernel_release=A|" \
        "$repo_dir/virt-temp/debian/$script.in" > "$output"
}

render_script prerm old "$test_root/prerm"
render_script postinst new "$test_root/postinst"
sh "$test_root/prerm" upgrade new
[[ -f "$test_root/saved/virt-temp-old/dkms.conf" ]]
[[ "$(readlink "$test_root/dkms/virt-temp/old/source")" == "$test_root/saved/virt-temp-old" ]]
[[ ! -e "$TEST_EVENTS" ]] || { echo "prerm upgrade invoked DKMS remove" >&2; exit 1; }

export TEST_FAIL_KERNEL=B
if sh "$test_root/postinst" configure old; then
    echo "postinst accepted a failed second build" >&2; exit 1
fi
printf 'add new\nbuild A\nbuild B\n' > "$test_root/expected"
cmp "$TEST_EVENTS" "$test_root/expected"
[[ -L "$test_root/dkms/virt-temp/old/source" ]]

unset TEST_FAIL_KERNEL
: > "$TEST_EVENTS"
printf 'UNRAID_VSOCK_CID=3\nUNRAID_VSOCK_PORT=990\n' > "$test_root/config"
cp -- "$test_root/config" "$test_root/expected_config"
sh "$test_root/postinst" configure old
printf 'build A\nbuild B\ninstall A\ninstall B\nsystemctl is-active --quiet unraid-vsock-hwmon.service\nsystemctl stop unraid-vsock-hwmon-topology.path\nsystemctl stop unraid-vsock-hwmon.service\nmodprobe virt_temp\nsystemctl daemon-reload\nsystemctl is-enabled --quiet unraid-vsock-hwmon.service\ndeb-systemd-invoke start\nremove old\n' > "$test_root/expected"
cmp "$TEST_EVENTS" "$test_root/expected"
cmp "$test_root/config" "$test_root/expected_config"
[[ ! -e "$test_root/saved/virt-temp-old" && ! -d "$test_root/dkms/virt-temp/old" ]]
echo "DKMS upgrade preservation and ordering: OK"
