// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func addFakeBlockDevice(t *testing.T, blockRoot, device string, usb bool) {
	t.Helper()
	root := filepath.Clean(filepath.Join(blockRoot, "..", ".."))
	busRoot := filepath.Join(root, "bus")
	for _, bus := range []string{"pci", "scsi", "usb"} {
		if err := os.MkdirAll(filepath.Join(busRoot, bus), 0700); err != nil {
			t.Fatal(err)
		}
	}
	physical := filepath.Join(root, "devices", "pci0000:00", "0000:00:01.0", device, "host6", "target6:0:0", "6:0:0:0")
	if usb {
		physical = filepath.Join(root, "devices", "pci0000:00", "usb2", "2-1", "2-1:1.0", device, "host6", "target6:0:0", "6:0:0:0")
	}
	if err := os.MkdirAll(physical, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(busRoot, "scsi"), filepath.Join(physical, "subsystem")); err != nil {
		t.Fatal(err)
	}
	busNode := filepath.Join(root, "devices", "pci0000:00", "0000:00:01.0")
	bus := "pci"
	if usb {
		busNode = filepath.Join(root, "devices", "pci0000:00", "usb2", "2-1", "2-1:1.0")
		bus = "usb"
	}
	if err := os.Symlink(filepath.Join(busRoot, bus), filepath.Join(busNode, "subsystem")); err != nil && !os.IsExist(err) {
		t.Fatal(err)
	}
	classNode := filepath.Join(blockRoot, device)
	if err := os.MkdirAll(classNode, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(physical, filepath.Join(classNode, "device")); err != nil {
		t.Fatal(err)
	}
}

func TestPhysicalDiskBusAndPolicy(t *testing.T) {
	tests := []struct {
		name, transport, logicalName        string
		usb, missing, broken, invalidDevice bool
		policy                              diskPolicy
		wantBus                             diskBus
		wantIncluded                        bool
	}{
		{name: "USB transport behind USB", transport: "usb", usb: true, wantBus: diskBusUSB},
		{name: "ATA passthrough behind USB", transport: "ata", usb: true, wantBus: diskBusUSB},
		{name: "SCSI SATA behind USB", transport: "scsi-SATA", usb: true, wantBus: diskBusUSB},
		{name: "internal ATA", transport: "ata", wantBus: diskBusNonUSB, wantIncluded: true},
		{name: "internal NVMe", transport: "nvme", wantBus: diskBusNonUSB, wantIncluded: true},
		{name: "USB label on non-USB hardware", transport: "usb", wantBus: diskBusNonUSB, wantIncluded: true},
		{name: "missing sysfs", transport: "usb", missing: true, wantBus: diskBusUnknown, wantIncluded: true},
		{name: "broken sysfs link", transport: "ata", broken: true, wantBus: diskBusUnknown, wantIncluded: true},
		{name: "forced USB inclusion", transport: "ata", usb: true, policy: diskPolicyInclude, wantBus: diskBusUSB, wantIncluded: true},
		{name: "forced internal exclusion", transport: "ata", policy: diskPolicyExclude, wantBus: diskBusNonUSB},
		{name: "forced exclusion before device validation", transport: "ata", missing: true, invalidDevice: true, policy: diskPolicyExclude, wantBus: diskBusUnknown},
		{name: "flash cannot be included", transport: "usb", logicalName: "flash", usb: true, policy: diskPolicyInclude, wantBus: diskBusUSB},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			environment := newDiskTestEnvironment(t, "30")
			if !test.missing && !test.broken {
				addFakeBlockDevice(t, environment.paths.sysBlockRoot, "sda", test.usb)
			} else if test.broken {
				classNode := filepath.Join(environment.paths.sysBlockRoot, "sda")
				if err := os.MkdirAll(classNode, 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink("missing", filepath.Join(classNode, "device")); err != nil {
					t.Fatal(err)
				}
			}
			name := test.logicalName
			if name == "" {
				name = "disk1"
			}
			device := "sda"
			if test.invalidDevice {
				device = "../invalid"
			}
			environment.write(t, environment.paths.disksINI,
				"["+name+"]\nid=stable_id\ndevice="+device+"\ntransport="+test.transport+"\nrotational=1\ntemp=35\n")
			policies := map[string]diskPolicy{}
			if test.policy != "" {
				policies["stable_id"] = test.policy
			}
			selector := &diskSelector{sysBlockRoot: environment.paths.sysBlockRoot, policies: policies}
			entries, err := readAssignedEntries(environment.paths.disksINI, selector, true)
			if err != nil || len(entries) != 1 {
				t.Fatalf("inventory entries = %#v, %v", entries, err)
			}
			if entries[0].bus != test.wantBus || entries[0].included != test.wantIncluded {
				t.Fatalf("selection = %#v; want bus %s, included %t", entries[0], test.wantBus, test.wantIncluded)
			}
		})
	}
}

func TestDiskCollectorAppliesPolicyChangesBeforeSMART(t *testing.T) {
	environment := newDiskTestEnvironment(t, "30")
	addFakeBlockDevice(t, environment.paths.sysBlockRoot, "sdi", true)
	environment.write(t, environment.paths.disksINI,
		"[disk1]\nid=bridge_serial\ndevice=sdi\ntransport=ata\nrotational=1\ntemp=35\n")
	collector := environment.collector()
	collector.refresh()
	readings, err := collector.snapshot()
	if err != nil || len(readings) != 0 || len(collector.state) != 0 {
		t.Fatalf("auto USB snapshot = %#v, %v; state = %#v", readings, err, collector.state)
	}
	if err := writeDiskPolicy(environment.paths.policyFile, "bridge_serial", diskPolicyInclude); err != nil {
		t.Fatal(err)
	}
	collector.refresh()
	readings, err = collector.snapshot()
	if err != nil || len(readings) != 1 || !readings[0].Unavailable || len(collector.state) != 1 {
		t.Fatalf("included USB without SMART cache = %#v, %v; state = %#v", readings, err, collector.state)
	}
	environment.report(t, "disk1", environment.now)
	collector.refresh()
	if disk := requireSingleDisk(t, collector); disk.temp != 35 || disk.unavailable {
		t.Fatalf("included USB with SMART cache = %#v", disk)
	}
	if err := writeDiskPolicy(environment.paths.policyFile, "bridge_serial", diskPolicyExclude); err != nil {
		t.Fatal(err)
	}
	collector.refresh()
	readings, err = collector.snapshot()
	if err != nil || len(readings) != 0 || len(collector.state) != 0 {
		t.Fatalf("excluded USB snapshot = %#v, %v; state = %#v", readings, err, collector.state)
	}
}

func TestDiskBusCacheTracksStableIDAndCurrentDevice(t *testing.T) {
	environment := newDiskTestEnvironment(t, "30")
	addFakeBlockDevice(t, environment.paths.sysBlockRoot, "sda", true)
	selector := &diskSelector{sysBlockRoot: environment.paths.sysBlockRoot, busCache: make(map[string]diskBus)}
	if bus := selector.detectBus("serial", "sda"); bus != diskBusUSB {
		t.Fatalf("first bus = %s, want USB", bus)
	}
	if err := os.Remove(filepath.Join(environment.paths.sysBlockRoot, "sda", "device")); err != nil {
		t.Fatal(err)
	}
	if bus := selector.detectBus("serial", "sda"); bus != diskBusUSB {
		t.Fatalf("cached bus = %s, want USB", bus)
	}
	if bus := selector.detectBus("serial", "sdb"); bus != diskBusUnknown {
		t.Fatalf("new device bus = %s, want unknown", bus)
	}
}

func TestFlashCannotReenterThroughUnassignedInventory(t *testing.T) {
	environment := newDiskTestEnvironment(t, "30")
	addFakeBlockDevice(t, environment.paths.sysBlockRoot, "sda", true)
	addFakeBlockDevice(t, environment.paths.sysBlockRoot, "sdb", true)
	environment.write(t, environment.paths.disksINI, "[flash]\nid=boot_serial\ndevice=sda\ntransport=usb\n")
	environment.write(t, environment.paths.devsINI, "[external]\nid=boot_serial\ndevice=sdb\ntransport=usb\n")
	selector := &diskSelector{
		sysBlockRoot: environment.paths.sysBlockRoot,
		policies:     map[string]diskPolicy{"boot_serial": diskPolicyInclude},
	}
	disks, err := readDiskInventory(environment.paths.disksINI, environment.paths.devsINI, selector)
	if err != nil || len(disks) != 0 {
		t.Fatalf("flash inventory = %#v, %v; want no disks", disks, err)
	}
	environment.write(t, environment.paths.disksINI, "[flash]\ndevice=sda\ntransport=usb\n")
	environment.write(t, environment.paths.devsINI, "[external]\nid=other_serial\ndevice=sda\ntransport=usb\n")
	selector.policies["other_serial"] = diskPolicyInclude
	disks, err = readDiskInventory(environment.paths.disksINI, environment.paths.devsINI, selector)
	if err != nil || len(disks) != 0 {
		t.Fatalf("flash inventory without assigned ID = %#v, %v; want no disks", disks, err)
	}
}

func TestDiskPolicyPersistenceAndCommand(t *testing.T) {
	environment := newDiskTestEnvironment(t, "30")
	addFakeBlockDevice(t, environment.paths.sysBlockRoot, "sda", true)
	addFakeBlockDevice(t, environment.paths.sysBlockRoot, "sdb", false)
	id := "WDC_ID=with,comma and spaces"
	environment.write(t, environment.paths.disksINI,
		"[disk1]\nid=\""+id+"\"\ndevice=sda\ntransport=ata\ntemp=35\n")
	encodedID := base64.StdEncoding.EncodeToString([]byte(id))
	var output bytes.Buffer
	if err := diskPolicyCommand([]string{"set", "--id-base64", encodedID, "--policy", "include",
		"--policy-file", environment.paths.policyFile}, &output); err != nil {
		t.Fatal(err)
	}
	policies, err := readDiskPolicies(environment.paths.policyFile)
	if err != nil || policies[id] != diskPolicyInclude {
		t.Fatalf("saved policies = %#v, %v", policies, err)
	}
	// A device name change leaves the stable-ID policy intact.
	environment.write(t, environment.paths.disksINI,
		"[disk1]\nid=\""+id+"\"\ndevice=sdb\ntransport=ata\ntemp=35\n")
	output.Reset()
	if err := diskPolicyCommand([]string{"list", "--disks-ini", environment.paths.disksINI,
		"--devs-ini", environment.paths.devsINI, "--sys-block-root", environment.paths.sysBlockRoot,
		"--policy-file", environment.paths.policyFile}, &output); err != nil {
		t.Fatal(err)
	}
	var rows []diskPolicyRow
	if err := json.Unmarshal(output.Bytes(), &rows); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].ID != id || rows[0].Device != "sdb" || rows[0].Policy != diskPolicyInclude || !rows[0].Included {
		t.Fatalf("disk-policy list = %#v", rows)
	}
	if err := diskPolicyCommand([]string{"set", "--id-base64", encodedID, "--policy", "auto",
		"--policy-file", environment.paths.policyFile}, &output); err != nil {
		t.Fatal(err)
	}
	policies, err = readDiskPolicies(environment.paths.policyFile)
	if err != nil || len(policies) != 0 {
		t.Fatalf("auto policies = %#v, %v; want no override", policies, err)
	}
}

func TestDiskListCanExposeInvalidDeviceForExclusion(t *testing.T) {
	environment := newDiskTestEnvironment(t, "30")
	environment.write(t, environment.paths.disksINI,
		"[disk1]\nid=stable_id\ndevice=../invalid\ntransport=ata\n")
	var output bytes.Buffer
	if err := diskPolicyCommand([]string{"list", "--disks-ini", environment.paths.disksINI,
		"--devs-ini", environment.paths.devsINI, "--sys-block-root", environment.paths.sysBlockRoot,
		"--policy-file", environment.paths.policyFile}, &output); err != nil {
		t.Fatal(err)
	}
	var rows []diskPolicyRow
	if err := json.Unmarshal(output.Bytes(), &rows); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].ID != "stable_id" || rows[0].Device != "" || !rows[0].Included {
		t.Fatalf("list with invalid device = %#v", rows)
	}
	if err := writeDiskPolicy(environment.paths.policyFile, "stable_id", diskPolicyExclude); err != nil {
		t.Fatal(err)
	}
	policies, err := readDiskPolicies(environment.paths.policyFile)
	if err != nil {
		t.Fatal(err)
	}
	disks, err := readDiskInventory(environment.paths.disksINI, environment.paths.devsINI,
		&diskSelector{sysBlockRoot: environment.paths.sysBlockRoot, policies: policies})
	if err != nil || len(disks) != 0 {
		t.Fatalf("excluded invalid device = %#v, %v", disks, err)
	}
}

func TestInvalidDiskPolicyFileDoesNotBlockCollector(t *testing.T) {
	environment := newDiskTestEnvironment(t, "30")
	environment.write(t, environment.paths.disksINI, "[disk1]\nid=internal\ndevice=sda\ntemp=35\n")
	environment.write(t, environment.paths.policyFile, `{ "internal": "invalid" }`)
	environment.report(t, "disk1", environment.now)
	collector := environment.collector()
	collector.refresh()
	if disk := requireSingleDisk(t, collector); disk.id != "internal" || disk.unavailable {
		t.Fatalf("collector with invalid policies = %#v", disk)
	}
	if err := writeDiskPolicy(environment.paths.policyFile, "internal", diskPolicyExclude); err == nil || !strings.Contains(err.Error(), "invalid disk policy") {
		t.Fatalf("overwriting invalid policy file error = %v", err)
	}
}
