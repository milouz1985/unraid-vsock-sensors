// SPDX-License-Identifier: GPL-3.0-or-later
//go:build linux && integration

package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"unraid-vsock-sensors/internal/sensors"
)

type vmTestDisk struct {
	name   string
	path   string
	serial string
}

func discoverVMTestDisks(t *testing.T) []vmTestDisk {
	t.Helper()
	disks := []vmTestDisk{
		{name: "disk1", serial: "UVSSDISK1"},
		{name: "disk2", serial: "UVSSDISK2"},
	}

	for index := range disks {
		disk := &disks[index]
		byID := filepath.Join("/dev/disk/by-id", "ata-QEMU_HARDDISK_"+disk.serial)
		linkInfo, err := os.Lstat(byID)
		if errors.Is(err, os.ErrNotExist) {
			t.Fatalf("QEMU SATA disk %s not found at %s", disk.serial, byID)
		}
		if err != nil {
			t.Fatalf("inspect QEMU SATA disk link %s: %v", byID, err)
		}
		if linkInfo.Mode()&os.ModeSymlink == 0 {
			t.Fatalf("invalid QEMU SATA disk link %s: expected a symlink", byID)
		}

		devicePath, err := filepath.EvalSymlinks(byID)
		if err != nil {
			t.Fatalf("resolve QEMU SATA disk %s via %s: %v", disk.serial, byID, err)
		}
		deviceInfo, err := os.Stat(devicePath)
		if err != nil {
			t.Fatalf("inspect target %s resolved from %s: %v", devicePath, byID, err)
		}
		if deviceInfo.Mode()&os.ModeDevice == 0 || deviceInfo.Mode()&os.ModeCharDevice != 0 {
			t.Fatalf("invalid target %s resolved from %s: expected a block device", devicePath, byID)
		}

		deviceName := filepath.Base(devicePath)
		sysfsPath := filepath.Join("/sys/class/block", deviceName)
		if _, err := os.Stat(sysfsPath); err != nil {
			t.Fatalf("invalid target %s resolved from %s: missing %s: %v", devicePath, byID, sysfsPath, err)
		}
		if usb, err := isUSBBlockDevice(defaultSysBlockRoot, deviceName); err != nil || usb {
			t.Fatalf("QEMU SATA disk %s physical bus: USB=%t, error=%v; want non-USB", deviceName, usb, err)
		}
		partitionPath := filepath.Join(sysfsPath, "partition")
		if _, err := os.Stat(partitionPath); err == nil {
			t.Fatalf("invalid target %s resolved from %s: expected a whole disk, got a partition", devicePath, byID)
		} else if !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("inspect target %s resolved from %s: %v", devicePath, byID, err)
		}

		rotationalPath := filepath.Join(sysfsPath, "queue", "rotational")
		rotational, err := os.ReadFile(rotationalPath)
		if err != nil {
			t.Fatalf("read rotational value for %s resolved from %s: %v", devicePath, byID, err)
		}
		if value := strings.TrimSpace(string(rotational)); value != "1" {
			t.Fatalf("unexpected rotational value for %s resolved from %s: got %q, want 1", devicePath, byID, value)
		}

		disk.path = devicePath
		t.Logf("discovered QEMU SATA disk %s at %s via %s", disk.serial, disk.path, byID)
	}
	return disks
}

func TestVMUnraidTemperatureCollector(t *testing.T) {
	requireVMIntegrationTest(t)

	devices := discoverVMTestDisks(t)

	directory := t.TempDir()
	paths := diskDataPaths{
		disksINI:   filepath.Join(directory, "disks.ini"),
		devsINI:    filepath.Join(directory, "devs.ini"),
		varINI:     filepath.Join(directory, "var.ini"),
		policyFile: filepath.Join(directory, "disk-policies.json"),
	}
	if err := os.WriteFile(paths.devsINI, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.varINI, []byte("poll_attributes=30\n"), 0600); err != nil {
		t.Fatal(err)
	}

	var inventory strings.Builder
	for _, disk := range devices {
		fmt.Fprintf(&inventory, `[%q]
id=%q
device=%q
status="DISK_OK"
rotational="1"
transport="ata"
spundown="0"
temp="31"

	`, disk.name, disk.serial, filepath.Base(disk.path))
	}
	if err := os.WriteFile(paths.disksINI, []byte(inventory.String()), 0600); err != nil {
		t.Fatal(err)
	}

	now := time.Now()
	collector := newDiskCollector(paths)
	collector.now = func() time.Time { return now }
	collector.refresh()
	readings, err := collector.snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if len(readings) != 2 {
		t.Fatalf("collector returned %d disks: %#v", len(readings), readings)
	}
	for index, disk := range readings {
		want := devices[index]
		if disk.Name != want.name || disk.Device != filepath.Base(want.path) ||
			disk.ID != want.serial || disk.Transport != "ata" || !disk.Rotational ||
			disk.Unavailable || disk.Temp != 31 {
			t.Fatalf("collector disk %d = %#v", index, disk)
		}
	}

	device := loadTestVirtTemp(t)
	publisher := &hwmonPublisher{cachePath: filepath.Join(directory, "hwmon-inventory.json")}
	publish := func(disks []sensors.Disk, wantChanged bool) {
		t.Helper()
		changed, err := publisher.publish(device, sensors.Response{
			Disks: disks,
			HBAs:  []sensors.HBA{},
		})
		if err != nil || changed != wantChanged {
			t.Fatalf("publish disks: changed=%v, want %v; err=%v", changed, wantChanged, err)
		}
	}

	t.Log("publish Unraid temperatures through virt_temp")
	publish(readings, true)
	for _, disk := range devices {
		requireVMHWMonTemp(t, "disk", "disk:"+disk.serial, "31000")
	}
	requireVMHWMonTemp(t, "disk", "disk:group:hdd", "31000")

}
