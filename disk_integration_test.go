//go:build linux && integration

package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestVMQEMUSMARTCollector(t *testing.T) {
	if os.Geteuid() != 0 || os.Getenv("UVSS_VM_TEST") != "1" {
		t.Fatal("run through tests/vm/guest-tests.sh in a disposable VM")
	}
	for _, marker := range []string{"/etc/uvss-test-image", "/etc/uvss-test-image-version"} {
		if _, err := os.Stat(marker); err != nil {
			t.Fatalf("missing test image marker %s: %v", marker, err)
		}
	}

	devices := []struct {
		name, path, serial string
	}{
		{name: "disk1", path: "/dev/sdb", serial: "UVSSDISK1"},
		{name: "disk2", path: "/dev/sdc", serial: "UVSSDISK2"},
	}
	for _, disk := range devices {
		info, err := os.Stat(disk.path)
		if err != nil || info.Mode()&os.ModeDevice == 0 || info.Mode()&os.ModeCharDevice != 0 {
			t.Fatalf("expected QEMU SATA block device %s: %v", disk.path, err)
		}
		rotational, err := os.ReadFile("/sys/class/block/" + filepath.Base(disk.path) + "/queue/rotational")
		if err != nil || strings.TrimSpace(string(rotational)) != "1" {
			t.Fatalf("%s ROTA = %q, err=%v; want 1", disk.path, rotational, err)
		}
		serial, err := os.ReadFile("/sys/class/block/" + filepath.Base(disk.path) + "/device/serial")
		if err != nil || strings.TrimSpace(string(serial)) != disk.serial {
			t.Fatalf("%s serial = %q, err=%v; want %s", disk.path, serial, err, disk.serial)
		}
	}

	shim := `#!/usr/bin/env bash
set -euo pipefail
case "$1" in
    disk1) device=/dev/sdb ;;
    disk2) device=/dev/sdc ;;
    *) echo "unknown UVSS test disk: $1" >&2; exit 2 ;;
esac
read -r -a options <<< "$2"
exec /usr/sbin/smartctl "${options[@]}" "$device"
`
	if _, err := os.Stat(smartctlTypePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("refusing to replace existing %s", smartctlTypePath)
	}
	if err := os.WriteFile(smartctlTypePath, []byte(shim), 0755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(smartctlTypePath) })

	for _, disk := range devices {
		cmd := exec.Command("/usr/sbin/smartctl", "--json", "-i", "-A", disk.path)
		output, runErr := cmd.Output()
		var report struct {
			SmartSupport struct {
				Available bool `json:"available"`
				Enabled   bool `json:"enabled"`
			} `json:"smart_support"`
		}
		if err := json.Unmarshal(output, &report); err != nil {
			t.Fatalf("decode real SMART JSON for %s: %v; command error=%v", disk.path, err, runErr)
		}
		if !report.SmartSupport.Available || !report.SmartSupport.Enabled {
			t.Fatalf("SMART support for %s = %#v", disk.path, report.SmartSupport)
		}
		temperature, standby, err := parseSMARTTemperature(disk.name, output, runErr)
		if err != nil || standby || temperature != 31 {
			t.Fatalf("real SMART temperature for %s = %v, standby=%v, err=%v; want 31", disk.path, temperature, standby, err)
		}
	}

	directory := t.TempDir()
	inventory := filepath.Join(directory, "disks.ini")
	ini := `["disk1"]
id="UVSSDISK1"
device="sdb"
status="DISK_OK"
rotational="1"
transport="ata"
spundown="0"

["disk2"]
id="UVSSDISK2"
device="sdc"
status="DISK_OK"
rotational="1"
transport="ata"
spundown="0"
`
	if err := os.WriteFile(inventory, []byte(ini), 0600); err != nil {
		t.Fatal(err)
	}
	collector := newDiskCollector(inventory, time.Minute)
	collector.refresh(context.Background())
	readings, err := collector.snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if len(readings) != 2 {
		t.Fatalf("collector returned %d disks: %#v", len(readings), readings)
	}
	for index, disk := range readings {
		want := devices[index]
		if disk.Name != want.name || disk.Device != strings.TrimPrefix(want.path, "/dev/") ||
			disk.ID != want.serial || disk.Transport != "ata" || !disk.Rotational ||
			disk.Unavailable || disk.Temp != 31 {
			t.Fatalf("collector disk %d = %#v", index, disk)
		}
	}
}
