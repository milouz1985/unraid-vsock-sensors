// SPDX-License-Identifier: GPL-3.0-or-later
//go:build linux && integration

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
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
		devicePath := filepath.Join("/dev", filepath.Base(sysfsPath))
		output, runErr := exec.Command("/usr/sbin/smartctl", "--json", "-i", devicePath).Output()
		var identity struct {
			SerialNumber string `json:"serial_number"`
			Smartctl     struct {
				ExitStatus *int `json:"exit_status"`
			} `json:"smartctl"`
		}
		if err := json.Unmarshal(output, &identity); err != nil {
			continue
		}
		if identity.SerialNumber != "" {
			observed = append(observed, fmt.Sprintf("%s=%s", devicePath, identity.SerialNumber))
		}
		if _, ok := wantedSerials[identity.SerialNumber]; !ok {
			continue
		}
		if runErr != nil && (identity.Smartctl.ExitStatus == nil || *identity.Smartctl.ExitStatus&smartctlCommandErrorMask != 0) {
			t.Fatalf("identify test disk %s at %s: %v", identity.SerialNumber, devicePath, runErr)
		}
		if previous := found[identity.SerialNumber]; previous != "" {
			t.Fatalf("serial %s found on both %s and %s", identity.SerialNumber, previous, devicePath)
		}
		found[identity.SerialNumber] = devicePath
	}

	for index := range wanted {
		wanted[index].path = found[wanted[index].serial]
		if wanted[index].path == "" {
			t.Fatalf("QEMU SATA disk %s not found through SMART; observed: %v", wanted[index].serial, observed)
		}
		t.Logf("discovered QEMU SATA disk %s at %s", wanted[index].serial, wanted[index].path)
	}
	return wanted
}

func TestVMQEMUSMARTCollector(t *testing.T) {
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

	failureMarker := filepath.Join(t.TempDir(), "fail-"+devices[0].name)
	var shim strings.Builder
	shim.WriteString(`#!/usr/bin/env bash
set -euo pipefail
case "$1" in
`)
	for _, disk := range devices {
		fmt.Fprintf(&shim, "    %s) device=%q ;;\n", disk.name, disk.path)
	}
	fmt.Fprintf(&shim, `
    *) echo "unknown UVSS test disk: $1" >&2; exit 2 ;;
esac
if [[ "$1" == %q && -e %q ]]; then
    printf '{"smartctl":{"exit_status":2}}\n'
    echo "smartctl: simulated device read failure" >&2
    exit 2
fi
read -r -a options <<< "$2"
exec /usr/sbin/smartctl "${options[@]}" "$device"
`, devices[0].name, failureMarker)
	if _, err := os.Stat(smartctlTypePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("refusing to replace existing %s", smartctlTypePath)
	}
	if err := os.WriteFile(smartctlTypePath, []byte(shim.String()), 0755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(smartctlTypePath) })

	for _, disk := range devices {
		cmd := exec.Command("/usr/sbin/smartctl", "--json", "-i", "-A", disk.path)
		output, runErr := cmd.Output()
		var report struct {
			SerialNumber string `json:"serial_number"`
			SmartSupport struct {
				Available bool `json:"available"`
				Enabled   bool `json:"enabled"`
			} `json:"smart_support"`
		}
		if err := json.Unmarshal(output, &report); err != nil {
			t.Fatalf("decode real SMART JSON for %s: %v; command error=%v", disk.path, err, runErr)
		}
		if report.SerialNumber != disk.serial {
			t.Fatalf("SMART serial for %s = %q, want %s", disk.path, report.SerialNumber, disk.serial)
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
	var ini strings.Builder
	for _, disk := range devices {
		fmt.Fprintf(&ini, `[%q]
id=%q
device=%q
status="DISK_OK"
rotational="1"
transport="ata"
spundown="0"

`, disk.name, disk.serial, filepath.Base(disk.path))
	}
	if err := os.WriteFile(inventory, []byte(ini.String()), 0600); err != nil {
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

	t.Log("publish real SMART temperatures through virt_temp")
	publish(readings, true)
	for _, disk := range devices {
		requireVMHWMonTemp(t, "disk", "disk:"+disk.serial, "31000")
	}
	requireVMHWMonTemp(t, "disk", "disk:group:hdd", "31000")

	t.Log("a persistent SMART failure lets the failed disk and its group expire")
	if err := os.WriteFile("/sys/module/virt_temp/parameters/stale_timeout", []byte("1\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(failureMarker, nil, 0600); err != nil {
		t.Fatal(err)
	}
	collector.refresh(context.Background())
	failedReadings, err := collector.snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if len(failedReadings) != 2 || !failedReadings[0].Unavailable || failedReadings[0].Temp != 0 ||
		failedReadings[1].Unavailable || failedReadings[1].Temp != 31 {
		t.Fatalf("collector readings after SMART failure = %#v", failedReadings)
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

	t.Log("a successful SMART read recovers disk and group temperatures")
	if err := os.Remove(failureMarker); err != nil {
		t.Fatal(err)
	}
	collector.refresh(context.Background())
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
