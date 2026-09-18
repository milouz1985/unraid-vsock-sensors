// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestDiskPolicyStoreSkipsNoopWrites(t *testing.T) {
	path := filepath.Join(t.TempDir(), "disk-policies.json")
	store := newDiskPolicyStore(path)
	if err := store.Set("disk1", diskPolicyInclude); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	if err := store.Set("disk1", diskPolicyInclude); err != nil {
		t.Fatal(err)
	}
	afterSamePolicy, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before, afterSamePolicy) {
		t.Fatal("setting an unchanged policy rewrote disk-policies.json")
	}

	if err := store.Set("missing", diskPolicyAuto); err != nil {
		t.Fatal(err)
	}
	afterMissingAuto, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before, afterMissingAuto) {
		t.Fatal("setting Auto for an absent policy rewrote disk-policies.json")
	}
}

func TestDiskPolicyStoreSerializesConcurrentSets(t *testing.T) {
	path := filepath.Join(t.TempDir(), "disk-policies.json")
	store := newDiskPolicyStore(path)
	var wg sync.WaitGroup
	for id := range 20 {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			if err := store.Set(fmt.Sprintf("disk%d", id), diskPolicyInclude); err != nil {
				t.Errorf("set disk%d: %v", id, err)
			}
		}(id)
	}
	wg.Wait()
	policies, err := readDiskPolicies(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(policies) != 20 {
		t.Fatalf("concurrent sets = %d policies, want 20", len(policies))
	}
	for id := range 20 {
		if policies[fmt.Sprintf("disk%d", id)] != diskPolicyInclude {
			t.Fatalf("policy disk%d = %q, want include", id, policies[fmt.Sprintf("disk%d", id)])
		}
	}
}

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
		wantSelected                        bool
	}{
		{name: "USB transport behind USB", transport: "usb", usb: true, wantBus: diskBusUSB},
		{name: "ATA passthrough behind USB", transport: "ata", usb: true, wantBus: diskBusUSB},
		{name: "SCSI SATA behind USB", transport: "scsi-SATA", usb: true, wantBus: diskBusUSB},
		{name: "internal ATA", transport: "ata", wantBus: diskBusNonUSB, wantSelected: true},
		{name: "internal NVMe", transport: "nvme", wantBus: diskBusNonUSB, wantSelected: true},
		{name: "USB label on non-USB hardware", transport: "usb", wantBus: diskBusNonUSB, wantSelected: true},
		{name: "missing sysfs", transport: "usb", missing: true, wantBus: diskBusUnknown, wantSelected: true},
		{name: "broken sysfs link", transport: "ata", broken: true, wantBus: diskBusUnknown, wantSelected: true},
		{name: "forced USB inclusion", transport: "ata", usb: true, policy: diskPolicyInclude, wantBus: diskBusUSB, wantSelected: true},
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
				"["+name+"]\nid=stable_id\ndevice="+device+"\ntransport="+test.transport+"\nrotational=1\nspundown=0\ntemp=35\n")
			policies := map[string]diskPolicy{}
			if test.policy != "" {
				policies["stable_id"] = test.policy
			}
			selector := &diskSelector{sysBlockRoot: environment.paths.sysBlockRoot, policies: policies}
			entries, err := readAssignedEntries(environment.paths.disksINI, selector)
			if err != nil || len(entries) != 1 {
				t.Fatalf("inventory entries = %#v, %v", entries, err)
			}
			if entries[0].bus != test.wantBus || entries[0].selected != test.wantSelected {
				t.Fatalf("selection = %#v; want bus %s, selected %t", entries[0], test.wantBus, test.wantSelected)
			}
		})
	}
}

func TestDiskCollectorAppliesPolicyChangesBeforeSMART(t *testing.T) {
	environment := newDiskTestEnvironment(t, "30")
	addFakeBlockDevice(t, environment.paths.sysBlockRoot, "sdi", true)
	environment.write(t, environment.paths.disksINI,
		"[disk1]\nid=bridge_serial\ndevice=sdi\ntransport=ata\nrotational=1\nspundown=0\ntemp=35\n")
	collector := environment.collector()
	collector.refresh()
	readings, err := collector.snapshot()
	if err != nil || len(readings) != 0 || len(collector.state) != 0 {
		t.Fatalf("auto USB snapshot = %#v, %v; state = %#v", readings, err, collector.state)
	}
	if err := newDiskPolicyStore(environment.paths.policyFile).Set("bridge_serial", diskPolicyInclude); err != nil {
		t.Fatal(err)
	}
	collector.refresh()
	readings, err = collector.snapshot()
	if err != nil || len(readings) != 1 || len(collector.state) != 1 {
		t.Fatalf("included USB = %#v, %v; state = %#v", readings, err, collector.state)
	}
	if disk := requireSingleDisk(t, collector); disk.temp != 35 || disk.unavailable {
		t.Fatalf("included USB = %#v", disk)
	}
	if err := newDiskPolicyStore(environment.paths.policyFile).Set("bridge_serial", diskPolicyExclude); err != nil {
		t.Fatal(err)
	}
	collector.refresh()
	readings, err = collector.snapshot()
	if err != nil || len(readings) != 0 || len(collector.state) != 0 {
		t.Fatalf("excluded USB snapshot = %#v, %v; state = %#v", readings, err, collector.state)
	}
}

func TestDiskBusFollowsSysfsTopologyWithSameIDAndDevice(t *testing.T) {
	for _, test := range []struct {
		name       string
		initialUSB bool
	}{
		{name: "USB to SATA", initialUSB: true},
		{name: "SATA to USB", initialUSB: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			environment := newDiskTestEnvironment(t, "30")
			addFakeBlockDevice(t, environment.paths.sysBlockRoot, "sda", test.initialUSB)
			environment.write(t, environment.paths.disksINI,
				"[disk1]\nid=stable_serial\ndevice=sda\ntransport=ata\nrotational=1\nspundown=0\ntemp=35\n")
			collector := environment.collector()
			check := func(wantSelected bool) {
				t.Helper()
				collector.refresh()
				readings, err := collector.snapshot()
				wantCount := 0
				if wantSelected {
					wantCount = 1
				}
				if err != nil || len(readings) != wantCount || len(collector.state) != wantCount {
					t.Fatalf("snapshot = %#v, %v; state = %#v; want %d disks", readings, err, collector.state, wantCount)
				}
				if wantSelected && (readings[0].ID != "stable_serial" || readings[0].Temp != 35 || readings[0].Unavailable) {
					t.Fatalf("included disk = %#v", readings[0])
				}
			}
			check(!test.initialUSB)
			if err := os.Remove(filepath.Join(environment.paths.sysBlockRoot, "sda", "device")); err != nil {
				t.Fatal(err)
			}
			addFakeBlockDevice(t, environment.paths.sysBlockRoot, "sda", !test.initialUSB)
			check(test.initialUSB)
		})
	}
}

func TestFlashCannotReenterThroughUnassignedInventory(t *testing.T) {
	environment := newDiskTestEnvironment(t, "30")
	addFakeBlockDevice(t, environment.paths.sysBlockRoot, "sda", true)
	addFakeBlockDevice(t, environment.paths.sysBlockRoot, "sdb", true)
	environment.write(t, environment.paths.disksINI, "[flash]\nid=boot_serial\ndevice=sda\ntransport=usb\nrotational=0\nspundown=0\n")
	environment.write(t, environment.paths.devsINI, "[external]\nid=boot_serial\ndevice=sdb\ntransport=usb\nrotational=0\nspundown=0\n")
	selector := &diskSelector{
		sysBlockRoot: environment.paths.sysBlockRoot,
		policies:     map[string]diskPolicy{"boot_serial": diskPolicyInclude},
	}
	disks, err := readDiskInventory(environment.paths.disksINI, environment.paths.devsINI, selector)
	if err != nil || len(disks) != 0 {
		t.Fatalf("flash inventory = %#v, %v; want no disks", disks, err)
	}
	environment.write(t, environment.paths.disksINI, "[flash]\ndevice=sda\ntransport=usb\nrotational=0\nspundown=0\n")
	environment.write(t, environment.paths.devsINI, "[external]\nid=other_serial\ndevice=sda\ntransport=usb\nrotational=0\nspundown=0\n")
	selector.policies["other_serial"] = diskPolicyInclude
	disks, err = readDiskInventory(environment.paths.disksINI, environment.paths.devsINI, selector)
	if err != nil || len(disks) != 0 {
		t.Fatalf("flash inventory without assigned ID = %#v, %v; want no disks", disks, err)
	}
}

func TestDiskListCanExposeIncompleteEntryForExclusion(t *testing.T) {
	environment := newDiskTestEnvironment(t, "30")
	// An active disk whose device cannot be resolved is exposed with an empty
	// device so it can still be excluded. The daemon's strict validation is
	// exercised separately; here the raw inventory entry is inspected.
	environment.write(t, environment.paths.disksINI,
		"[disk1]\nid=stable_id\ndevice=../invalid\ntransport=ata\nrotational=1\nspundown=0\n")
	selector := &diskSelector{sysBlockRoot: environment.paths.sysBlockRoot, policies: map[string]diskPolicy{}}
	entries, err := readDiskInventoryEntries(environment.paths.disksINI, environment.paths.devsINI, selector)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].disk.id != "stable_id" || entries[0].disk.device != "" || !entries[0].selected {
		t.Fatalf("incomplete entry = %#v", entries)
	}
	// Excluding the disk makes the strict inventory accept it again.
	if err := newDiskPolicyStore(environment.paths.policyFile).Set("stable_id", diskPolicyExclude); err != nil {
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

func TestDiskCollectorKeepsLastValidPoliciesOnFileError(t *testing.T) {
	environment := newDiskTestEnvironment(t, "30")
	addFakeBlockDevice(t, environment.paths.sysBlockRoot, "sda", false)
	addFakeBlockDevice(t, environment.paths.sysBlockRoot, "sdi", true)
	environment.write(t, environment.paths.disksINI,
		"[disk1]\nid=internal\ndevice=sda\nrotational=1\nspundown=0\ntemp=35\n[disk2]\nid=external\ndevice=sdi\ntransport=ata\nrotational=1\nspundown=0\ntemp=36\n")
	environment.write(t, environment.paths.policyFile, `{"internal":"exclude","external":"include"}`)

	collector := environment.collector()
	collector.refresh()
	if disk := requireSingleDisk(t, collector); disk.id != "external" || disk.unavailable {
		t.Fatalf("collector with valid policies = %#v; want external disk", disk)
	}

	// A malformed update must be reported without changing the effective disk
	// selection. Falling back to Auto here would drop the USB disk and include
	// the internal disk instead.
	environment.write(t, environment.paths.policyFile, `{"internal":"invalid"}`)
	environment.now = environment.now.Add(time.Second)
	collector.refresh()
	if disk := requireSingleDisk(t, collector); disk.id != "external" || disk.unavailable {
		t.Fatalf("collector after invalid policies = %#v; want last-known-good external disk", disk)
	}
	if status := collector.status(); !strings.Contains(status.policyError, "disk policies file is invalid") {
		t.Fatalf("collector policy error = %q", status.policyError)
	}

	// An absent policy file is a valid empty configuration, not another read
	// failure. A reset must therefore replace the cached policies with Auto.
	if err := os.Remove(environment.paths.policyFile); err != nil {
		t.Fatal(err)
	}
	environment.now = environment.now.Add(time.Second)
	collector.refresh()
	if disk := requireSingleDisk(t, collector); disk.id != "internal" || disk.unavailable {
		t.Fatalf("collector after policy reset = %#v; want Auto-selected internal disk", disk)
	}
	if status := collector.status(); status.policyError != "" {
		t.Fatalf("collector policy error after reset = %q; want empty", status.policyError)
	}
}

func TestInvalidDiskPolicyFileDoesNotBlockCollector(t *testing.T) {
	environment := newDiskTestEnvironment(t, "30")
	addFakeBlockDevice(t, environment.paths.sysBlockRoot, "sda", false)
	addFakeBlockDevice(t, environment.paths.sysBlockRoot, "sdi", true)
	environment.write(t, environment.paths.disksINI,
		"[disk1]\nid=internal\ndevice=sda\nrotational=1\nspundown=0\ntemp=35\n[disk2]\nid=external\ndevice=sdi\ntransport=ata\nrotational=1\nspundown=0\ntemp=36\n")
	environment.write(t, environment.paths.policyFile, `{ "internal": "invalid" }`)
	collector := environment.collector()
	collector.refresh()
	if disk := requireSingleDisk(t, collector); disk.id != "internal" || disk.unavailable {
		t.Fatalf("collector with invalid policies = %#v", disk)
	}
	if status := collector.status(); !strings.Contains(status.policyError, "disk policies file is invalid") {
		t.Fatalf("collector policy error = %q", status.policyError)
	}
	store := newDiskPolicyStore(environment.paths.policyFile)
	if err := store.Set("internal", diskPolicyExclude); err == nil || !strings.Contains(err.Error(), "disk policies file is invalid") {
		t.Fatalf("overwriting invalid policy file error = %v", err)
	}
	if err := store.Reset(); err != nil {
		t.Fatalf("reset invalid policies: %v", err)
	}
	if _, err := os.Stat(environment.paths.policyFile); !os.IsNotExist(err) {
		t.Fatalf("policy file after reset: %v; want absent", err)
	}
}

func TestDiskPoliciesResetRejectsDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "disk-policies.json")
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	if err := newDiskPolicyStore(path).Reset(); err == nil {
		t.Fatal("reset removed a directory instead of a policy file")
	}
	if info, err := os.Stat(path); err != nil || !info.IsDir() {
		t.Fatalf("policy directory after failed reset = %v, %v", info, err)
	}
}
