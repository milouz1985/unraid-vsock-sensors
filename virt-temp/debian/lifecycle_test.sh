#!/usr/bin/env bash
set -euo pipefail

repo_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd)"
test_root="$(mktemp -d)"
trap 'rm -rf -- "$test_root"' EXIT
mkdir -p "$test_root"/{bin,dkms/virt-temp/old,src/virt-temp-old,src/virt-temp-new,modules/A/build,modules/B/build,saved}
touch "$test_root/src/virt-temp-old/dkms.conf" "$test_root/src/virt-temp-old/virt-temp.c"
touch "$test_root/src/virt-temp-new/dkms.conf" "$test_root/default" "$test_root/device" "$test_root/proc_modules"
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
printf 'systemctl %s\n' "$1" >> "$TEST_EVENTS"
[ "$1" != is-active ]
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

render_script() {
    local script=$1 version=$2 output=$3
    sed \
        -e "s|@VERSION@|$version|g" \
        -e "s|/var/lib/unraid-vsock-sensors/dkms-sources|$test_root/saved|g" \
        -e "s|/var/lib/dkms|$test_root/dkms|g" \
        -e "s|/usr/src|$test_root/src|g" \
        -e "s|/lib/modules|$test_root/modules|g" \
        -e "s|/etc/default/unraid-vsock-hwmon|$test_root/config|g" \
        -e "s|/usr/share/unraid-vsock-sensors-hwmon/unraid-vsock-hwmon.default|$test_root/default|g" \
        -e "s|/proc/modules|$test_root/proc_modules|g" \
        -e "s|/usr/local/sbin/uninstall-unraid-vsock-hwmon|$test_root/absent-installer|g" \
        -e "s|-c /dev/virt-temp|-f $test_root/device|g" \
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
sh "$test_root/postinst" configure old
printf 'build A\nbuild B\ninstall A\ninstall B\nsystemctl is-active\nsystemctl stop\nmodprobe virt_temp\nsystemctl daemon-reload\nsystemctl is-enabled\ndeb-systemd-invoke start\nremove old\n' > "$test_root/expected"
cmp "$TEST_EVENTS" "$test_root/expected"
[[ ! -e "$test_root/saved/virt-temp-old" && ! -d "$test_root/dkms/virt-temp/old" ]]
echo "DKMS upgrade ordering: OK"

# ---------------------------------------------------------------------------
# apt remove executed directly after a failed DKMS upgrade (no recovery
# version in between). The old version is still registered with its preserved
# sources while the new version is registered but half-configured. prerm
# remove must delete every DKMS version and its backup sources, while the
# /etc/default configuration and the hwmon cache must survive.
# ---------------------------------------------------------------------------
rm -rf -- "$test_root/dkms/virt-temp"
mkdir -p "$test_root/dkms/virt-temp/old" "$test_root/dkms/virt-temp/new"
mkdir -p "$test_root/saved/virt-temp-old"
touch "$test_root/saved/virt-temp-old/dkms.conf" "$test_root/saved/virt-temp-old/virt-temp.c"
ln -s "$test_root/saved/virt-temp-old" "$test_root/dkms/virt-temp/old/source"
ln -s "$test_root/src/virt-temp-new" "$test_root/dkms/virt-temp/new/source"
: > "$TEST_EVENTS"

render_script prerm new "$test_root/prerm_new"
sh "$test_root/prerm_new" remove
# The two DKMS removes happen in glob order; normalise before comparing.
{
    grep -F 'systemctl stop' "$TEST_EVENTS"
    sort <(grep -F 'remove ' "$TEST_EVENTS")
} > "$test_root/actual"
{
    printf 'systemctl stop\n'
    printf 'remove new\nremove old\n' | sort
} > "$test_root/expected"
cmp "$test_root/actual" "$test_root/expected"
[[ ! -d "$test_root/dkms/virt-temp/old" ]]
[[ ! -d "$test_root/dkms/virt-temp/new" ]]
[[ ! -e "$test_root/saved/virt-temp-old" ]]
[[ ! -d "$test_root/saved" ]]
echo "DKMS remove after failed upgrade: OK"
