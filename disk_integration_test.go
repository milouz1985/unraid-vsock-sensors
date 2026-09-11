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
	wanted := []vmTestDisk{
		{name: "disk1", serial: "UVSSDISK1"},
		{name: "disk2", serial: "UVSSDISK2"},
	}
	wantedSerials := make(map[string]struct{}, len(wanted))
	for _, disk := range wanted {
		wantedSerials[disk.serial] = struct{}{}
	}

	blockDevices, err := filepath.Glob("/sys/class/block/sd*")
	if err != nil {
		t.Fatal(err)
	}
	found := make(map[string]string, len(wanted))
	var observed []string
	for _, sysfsPath := range blockDevices {
		if _, err := os.Stat(filepath.Join(sysfsPath, "partition")); err == nil {
			continue
		} else if !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("inspect block device %s: %v", sysfsPath, err)
		}
		serialBytes, err := os.ReadFile(filepath.Join(sysfsPath, "device", "serial"))
		if err != nil {
			continue
		}
		serial := strings.TrimSpace(string(serialBytes))
		devicePath := filepath.Join("/dev", filepath.Base(sysfsPath))
		if serial != "" {
			observed = append(observed, fmt.Sprintf("%s=%s", devicePath, serial))
		}
		if _, ok := wantedSerials[serial]; !ok {
			continue
		}
		if previous := found[serial]; previous != "" {
			t.Fatalf("serial %s found on both %s and %s", serial, previous, devicePath)
		}
		found[serial] = devicePath
	}

	for index := range wanted {
		wanted[index].path = found[wanted[index].serial]
		if wanted[index].path == "" {
			t.Fatalf("QEMU SATA disk %s not found through sysfs; observed: %v", wanted[index].serial, observed)
		}
		t.Logf("discovered QEMU SATA disk %s at %s", wanted[index].serial, wanted[index].path)
	}
	return wanted
}

func TestVMUnraidSMARTCacheCollector(t *testing.T) {
	requireVMIntegrationTest(t)

	devices := discoverVMTestDisks(t)
	for _, disk := range devices {
		info, err := os.Stat(disk.path)
		if err != nil || info.Mode()&os.ModeDevice == 0 || info.Mode()&os.ModeCharDevice != 0 {
			t.Fatalf("expected QEMU SATA block device %s: %v", disk.path, err)
		}
		rotational, err := os.ReadFile("/sys/class/block/" + filepath.Base(disk.path) + "/queue/rotational")
		if err != nil || strings.TrimSpace(string(rotational)) != "1" {
			t.Fatalf("%s ROTA = %q, err=%v; want 1", disk.path, rotational, err)
		}
	}

	directory := t.TempDir()
	paths := diskDataPaths{
		disksINI: filepath.Join(directory, "disks.ini"),
		devsINI:  filepath.Join(directory, "devs.ini"),
		smartDir: filepath.Join(directory, "smart"),
		varINI:   filepath.Join(directory, "var.ini"),
	}
	if err := os.Mkdir(paths.smartDir, 0700); err != nil {
		t.Fatal(err)
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
		if err := os.WriteFile(filepath.Join(paths.smartDir, disk.name), []byte("QEMU SMART cache\n"), 0600); err != nil {
			t.Fatal(err)
		}
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

	t.Log("publish Unraid-cached temperatures through virt_temp")
	publish(readings, true)
	for _, disk := range devices {
		requireVMHWMonTemp(t, "disk", "disk:"+disk.serial, "31000")
	}
	requireVMHWMonTemp(t, "disk", "disk:group:hdd", "31000")

	t.Log("an expired Unraid SMART cache lets the disk and its group reach failsafe")
	if err := os.WriteFile("/sys/module/virt_temp/parameters/stale_timeout", []byte("1\n"), 0600); err != nil {
		t.Fatal(err)
	}
	freshness := smartFreshnessWindow(30 * time.Second)
	old := now.Add(-freshness - time.Second)
	if err := os.Chtimes(filepath.Join(paths.smartDir, devices[0].name), old, old); err != nil {
		t.Fatal(err)
	}
	collector.refresh()
	now = now.Add(freshness)
	if err := os.Chtimes(filepath.Join(paths.smartDir, devices[1].name), now, now); err != nil {
		t.Fatal(err)
	}
	collector.refresh()
	failedReadings, err := collector.snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if len(failedReadings) != 2 || !failedReadings[0].Unavailable || failedReadings[0].Temp != 0 ||
		failedReadings[1].Unavailable || failedReadings[1].Temp != 31 {
		t.Fatalf("collector readings after cache expiry = %#v", failedReadings)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		publish(failedReadings, false)
		failedTemp := readVMHWMonTemp(t, "disk", "disk:"+devices[0].serial)
		groupTemp := readVMHWMonTemp(t, "disk", "disk:group:hdd")
		if failedTemp == "100000" && groupTemp == "100000" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("failed disk/group did not expire: disk=%s group=%s", failedTemp, groupTemp)
		}
		time.Sleep(100 * time.Millisecond)
	}
	requireVMHWMonTemp(t, "disk", "disk:"+devices[1].serial, "31000")

	t.Log("a fresh Unraid cache recovers disk and group temperatures")
	if err := os.Chtimes(filepath.Join(paths.smartDir, devices[0].name), now, now); err != nil {
		t.Fatal(err)
	}
	collector.refresh()
	recoveredReadings, err := collector.snapshot()
	if err != nil {
		t.Fatal(err)
	}
	for _, disk := range recoveredReadings {
		if disk.Unavailable || disk.Temp != 31 {
			t.Fatalf("collector did not recover: %#v", recoveredReadings)
		}
	}
	publish(recoveredReadings, false)
	for _, disk := range devices {
		requireVMHWMonTemp(t, "disk", "disk:"+disk.serial, "31000")
	}
	requireVMHWMonTemp(t, "disk", "disk:group:hdd", "31000")
}
