// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

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
				"["+name+"]\nid=stable_id\ndevice="+device+"\ntransport="+test.transport+"\nrotational=1\nspundown=0\ntemp=35\n")
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
			check := func(wantIncluded bool) {
				t.Helper()
				collector.refresh()
				readings, err := collector.snapshot()
				wantCount := 0
				if wantIncluded {
					wantCount = 1
				}
				if err != nil || len(readings) != wantCount || len(collector.state) != wantCount {
					t.Fatalf("snapshot = %#v, %v; state = %#v; want %d disks", readings, err, collector.state, wantCount)
				}
				if wantIncluded && (readings[0].ID != "stable_serial" || readings[0].Temp != 35 || readings[0].Unavailable) {
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

func TestDiskPolicyPersistenceAndCommand(t *testing.T) {
	environment := newDiskTestEnvironment(t, "30")
	addFakeBlockDevice(t, environment.paths.sysBlockRoot, "sda", true)
	addFakeBlockDevice(t, environment.paths.sysBlockRoot, "sdb", false)
	id := "WDC_ID=with,comma and spaces"
	environment.write(t, environment.paths.disksINI,
		"[disk1]\nid=\""+id+"\"\ndevice=sda\ntransport=ata\nrotational=1\nspundown=0\ntemp=35\n")
	refresh := make(chan struct{}, 1)
	server := startControlServerForTest(t, environment, refresh)
	encodedID := base64.StdEncoding.EncodeToString([]byte(id))
	var output bytes.Buffer
	if err := diskPolicyCommand([]string{"set", "--id-base64", encodedID, "--policy", "include",
		"--control-socket", server.socketPath}, &output); err != nil {
		t.Fatal(err)
	}
	policies, err := readDiskPolicies(environment.paths.policyFile)
	if err != nil || policies[id] != diskPolicyInclude {
		t.Fatalf("saved policies = %#v, %v", policies, err)
	}
	// A device name change leaves the stable-ID policy intact. The management
	// inventory is read on demand by the control server, so no thermal refresh
	// is needed to observe the new device name.
	environment.write(t, environment.paths.disksINI,
		"[disk1]\nid=\""+id+"\"\ndevice=sdb\ntransport=ata\nrotational=1\nspundown=0\ntemp=35\n")
	output.Reset()
	if err := diskPolicyCommand([]string{"list", "--control-socket", server.socketPath}, &output); err != nil {
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
		"--control-socket", server.socketPath}, &output); err != nil {
		t.Fatal(err)
	}
	policies, err = readDiskPolicies(environment.paths.policyFile)
	if err != nil || len(policies) != 0 {
		t.Fatalf("auto policies = %#v, %v; want no override", policies, err)
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
	entries, err := readDiskInventoryEntries(environment.paths.disksINI, environment.paths.devsINI, selector, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].disk.id != "stable_id" || entries[0].disk.device != "" || !entries[0].included {
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

func TestInvalidDiskPolicyFileDoesNotBlockCollector(t *testing.T) {
	environment := newDiskTestEnvironment(t, "30")
	addFakeBlockDevice(t, environment.paths.sysBlockRoot, "sda", false)
	addFakeBlockDevice(t, environment.paths.sysBlockRoot, "sdi", true)
	environment.write(t, environment.paths.disksINI,
		"[disk1]\nid=internal\ndevice=sda\nrotational=1\nspundown=0\ntemp=35\n[disk2]\nid=external\ndevice=sdi\ntransport=ata\nrotational=1\nspundown=0\ntemp=36\n")
	environment.write(t, environment.paths.policyFile, `{ "internal": "invalid" }`)
	refresh := make(chan struct{}, 1)
	collector := environment.collector()
	collector.refresh()
	if disk := requireSingleDisk(t, collector); disk.id != "internal" || disk.unavailable {
		t.Fatalf("collector with invalid policies = %#v", disk)
	}
	if len(collector.state) != 1 {
		t.Fatalf("collector state with invalid policies = %#v; want internal disk only", collector.state)
	}
	if status := collector.status(); !strings.Contains(status.policyError, "disk policies file is invalid") {
		t.Fatalf("collector policy error = %q", status.policyError)
	}
	server := startControlServerForTest(t, environment, refresh)
	listArgs := []string{"list", "--control-socket", server.socketPath}
	var output bytes.Buffer
	if err := diskPolicyCommand(listArgs, &output); err == nil || !strings.Contains(err.Error(), "disk policies file is invalid") {
		t.Fatalf("disks list with invalid policies error = %v", err)
	}
	if output.Len() != 0 {
		t.Fatalf("disks list wrote output despite invalid policies: %q", output.String())
	}
	if err := diskPolicyCommand([]string{"validate", "--control-socket", server.socketPath}, &output); err == nil {
		t.Fatal("disks validate accepted invalid policies")
	}
	if err := newDiskPolicyStore(environment.paths.policyFile).Set("internal", diskPolicyExclude); err == nil || !strings.Contains(err.Error(), "disk policies file is invalid") {
		t.Fatalf("overwriting invalid policy file error = %v", err)
	}
	encodedID := base64.StdEncoding.EncodeToString([]byte("internal"))
	if err := diskPolicyCommand([]string{"set", "--id-base64", encodedID, "--policy", "exclude",
		"--control-socket", server.socketPath}, &output); err == nil {
		t.Fatal("disks set overwrote invalid policies")
	}
	if err := diskPolicyCommand([]string{"reset", "--control-socket", server.socketPath}, &output); err != nil {
		t.Fatalf("disks reset: %v", err)
	}
	if _, err := os.Stat(environment.paths.policyFile); !os.IsNotExist(err) {
		t.Fatalf("policy file after reset: %v; want absent", err)
	}
	if err := diskPolicyCommand([]string{"reset", "--control-socket", server.socketPath}, &output); err != nil {
		t.Fatalf("disks reset with absent file: %v", err)
	}
	// A successful reset clears the policy file. The control server reads the
	// policy file on demand, so validate and list succeed immediately without
	// a thermal refresh.
	if err := diskPolicyCommand([]string{"validate", "--control-socket", server.socketPath}, &output); err != nil {
		t.Fatalf("disks validate after reset: %v", err)
	}
	output.Reset()
	if err := diskPolicyCommand(listArgs, &output); err != nil {
		t.Fatalf("disks list after reset: %v", err)
	}
	var rows []diskPolicyRow
	if err := json.Unmarshal(output.Bytes(), &rows); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].Policy != diskPolicyAuto || rows[1].Policy != diskPolicyAuto {
		t.Fatalf("disks list after reset = %#v; want Auto for all disks", rows)
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
