// SPDX-License-Identifier: GPL-3.0-or-later
//go:build linux && integration

package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// This test owns the module for its entire lifetime. It must never run on the
// Proxmox host that supplies production fan-control sensors.
func TestVMHWMon(t *testing.T) {
	device := loadTestVirtTemp(t)
	disks, hbas := hwmonInventory{}, hwmonInventory{}
	diskSamples := []hwmonSample{hwmonTestSample("disk:vm-a", "VM disk A", 34)}
	hbaSamples := []hwmonSample{hwmonTestSample("hba:vm-a", "VM HBA A", 48)}
	publish := func(namespace string, inventory *hwmonInventory, samples []hwmonSample, wantChanged bool) {
		t.Helper()
		changed, err := publishHWMonFamily(device, namespace, inventory, samples)
		if err != nil || changed != wantChanged {
			t.Fatalf("publish %s: changed=%v, want %v; err=%v", namespace, changed, wantChanged, err)
		}
	}
	t.Log("configure disk/HBA families and read real sysfs temperatures")
	publish("disk", &disks, diskSamples, true)
	publish("hba", &hbas, hbaSamples, true)
	requireVMHWMonTemp(t, "disk", "disk:vm-a", "34000")
	requireVMHWMonTemp(t, "hba", "hba:vm-a", "48000")
	if got := readVMHWMonAttribute(t, "disk", "disk:vm-a", "name"); got != "unraid_vm_disk_a" {
		t.Fatalf("disk hwmon name = %q, want unraid_vm_disk_a", got)
	}
	if got := readVMHWMonAttribute(t, "hba", "hba:vm-a", "name"); got != "unraid_vm_hba_a" {
		t.Fatalf("HBA hwmon name = %q, want unraid_vm_hba_a", got)
	}

	t.Log("a maximum-length Unraid disk ID crosses the real kernel boundary")
	maximumDiskID := "disk:" + strings.Repeat("a", maxUnraidDiskIDSize)
	maximumSamples := []hwmonSample{
		diskSamples[0],
		hwmonTestSample(maximumDiskID, "Maximum ID", 39),
	}
	publish("disk", &disks, maximumSamples, true)
	requireVMHWMonTemp(t, "disk", maximumDiskID, "39000")
	if got := readVMHWMonAttribute(t, "disk", maximumDiskID, "temp1_label"); got != "Maximum ID" {
		t.Fatalf("maximum-length ID label = %q, want Maximum ID", got)
	}
	publish("disk", &disks, diskSamples, true)

	t.Log("a signed atypical temperature crosses the real kernel boundary")
	hbaSamples[0].temperature = -40
	publish("hba", &hbas, hbaSamples, false)
	requireVMHWMonTemp(t, "hba", "hba:vm-a", "-40000")
	hbaSamples[0].temperature = 48

	t.Log("a label change reconfigures the family")
	diskSamples[0].temperature = 35.125
	diskSamples[0].sensor.label = "New label"
	publish("disk", &disks, diskSamples, true)
	requireVMHWMonTemp(t, "disk", "disk:vm-a", "35125")
	if readVMHWMonAttribute(t, "disk", "disk:vm-a", "temp1_label") != "New label" {
		t.Fatal("reconfiguration did not apply the new label")
	}

	t.Log("a session closed without commit has no effect")
	staged, err := os.OpenFile(device, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, writeErr := staged.WriteString("sample\tdisk:vm-a\t99000\tIgnored\n")
	if err := errors.Join(writeErr, staged.Close()); err != nil {
		t.Fatal(err)
	}
	requireVMHWMonTemp(t, "disk", "disk:vm-a", "35125")

	t.Log("a real ENOSPC write error is propagated without changing inventory")
	before := append([]hwmonSensor(nil), disks.sensors...)
	changed, err := publishHWMonFamily("/dev/full", "disk", &disks, diskSamples)
	if changed || !errors.Is(err, unix.ENOSPC) || strings.Contains(err.Error(), "reconfigure") || !reflect.DeepEqual(before, disks.sensors) {
		t.Fatalf("changed=%v, err=%v, inventory=%v", changed, err, disks.sensors)
	}
	requireVMHWMonTemp(t, "disk", "disk:vm-a", "35125")

	t.Log("reload loses kernel inventory; a real ESTALE triggers reconfiguration")
	reloadTestVirtTemp(t)
	if err := writeHWMonSamples(device, "disk", "commit", diskSamples); !errors.Is(err, unix.ESTALE) {
		t.Fatalf("commit after reload = %v, want ESTALE", err)
	}
	publish("disk", &disks, diskSamples, true)
	publish("hba", &hbas, hbaSamples, true)
	requireVMHWMonTemp(t, "disk", "disk:vm-a", "35125")
	requireVMHWMonTemp(t, "hba", "hba:vm-a", "48000")
	if readVMHWMonAttribute(t, "disk", "disk:vm-a", "temp1_label") != "New label" {
		t.Fatal("reconfigure did not apply the current label")
	}

	t.Log("topology removal leaves the other family intact")
	publish("disk", &disks, nil, true)
	if len(vmHWMonPaths(t, "disk", "disk:vm-a")) != 0 {
		t.Fatal("removed disk still exists in sysfs")
	}
	requireVMHWMonTemp(t, "hba", "hba:vm-a", "48000")
	publish("disk", &disks, diskSamples, true)
	publish("hba", &hbas, nil, true)
	if len(vmHWMonPaths(t, "hba", "hba:vm-a")) != 0 {
		t.Fatal("removed HBA still exists in sysfs")
	}
	requireVMHWMonTemp(t, "disk", "disk:vm-a", "35125")
	publish("hba", &hbas, hbaSamples, true)

	t.Log("kernel timeout reaches failsafe, then fresh data recovers")
	if err := os.WriteFile("/sys/module/virt_temp/parameters/stale_timeout", []byte("1\n"), 0600); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for readVMHWMonTemp(t, "disk", "disk:vm-a") != "100000" {
		if time.Now().After(deadline) {
			t.Fatal("kernel did not apply stale failsafe")
		}
		time.Sleep(50 * time.Millisecond)
	}
	publish("disk", &disks, diskSamples, false)
	requireVMHWMonTemp(t, "disk", "disk:vm-a", "35125")

	t.Log("cached topology is restored through the real module at failsafe")
	cachePath := filepath.Join(t.TempDir(), "hwmon-inventory.json")
	cache := cachedHWMonInventory{
		Version: 1,
		Disks: &cachedHWMonFamily{Sensors: []cachedHWMonSensor{
			{ID: "disk:cached", Label: "Cached disk"},
		}},
		HBAs: &cachedHWMonFamily{Sensors: []cachedHWMonSensor{
			{ID: "hba:cached", Label: "Cached HBA"},
		}},
	}
	data, err := json.Marshal(cache)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cachePath, data, 0600); err != nil {
		t.Fatal(err)
	}
	reloadTestVirtTemp(t)
	restored := &hwmonPublisher{cachePath: cachePath}
	if err := restored.restore(device); err != nil {
		t.Fatal(err)
	}
	requireVMHWMonTemp(t, "disk", "disk:cached", "100000")
	requireVMHWMonTemp(t, "hba", "hba:cached", "100000")
	if got := readVMHWMonAttribute(t, "disk", "disk:cached", "name"); got != "unraid_cached_disk" {
		t.Fatalf("restored disk hwmon name = %q, want unraid_cached_disk", got)
	}
	if got := readVMHWMonAttribute(t, "hba", "hba:cached", "name"); got != "unraid_cached_hba" {
		t.Fatalf("restored HBA hwmon name = %q, want unraid_cached_hba", got)
	}
}
