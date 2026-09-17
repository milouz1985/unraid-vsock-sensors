// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDiskPolicyProcessHelper(t *testing.T) {
	action := os.Getenv("UVSS_DISK_POLICY_TEST_ACTION")
	if action == "" {
		return
	}
	fmt.Fprintln(os.Stdout, "ready")
	path := os.Getenv("UVSS_DISK_POLICY_TEST_PATH")
	switch action {
	case "set":
		if err := writeDiskPolicy(path, os.Getenv("UVSS_DISK_POLICY_TEST_ID"), diskPolicyInclude); err != nil {
			t.Fatal(err)
		}
	case "reset":
		if err := resetDiskPolicies(path); err != nil {
			t.Fatal(err)
		}
	default:
		t.Fatalf("invalid helper action %q", action)
	}
}

func startDiskPolicyProcess(t *testing.T, path, action, id string) <-chan error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestDiskPolicyProcessHelper$")
	command.Env = append(os.Environ(),
		"UVSS_DISK_POLICY_TEST_ACTION="+action,
		"UVSS_DISK_POLICY_TEST_PATH="+path,
		"UVSS_DISK_POLICY_TEST_ID="+id)
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	command.Stderr = &stderr
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	ready := bufio.NewScanner(stdout)
	if !ready.Scan() || ready.Text() != "ready" {
		t.Fatalf("policy helper did not start: %q, %v; stderr: %s", ready.Text(), ready.Err(), stderr.String())
	}
	done := make(chan error, 1)
	go func() {
		for ready.Scan() {
		}
		scanErr := ready.Err()
		waitErr := command.Wait()
		if scanErr != nil {
			done <- fmt.Errorf("read policy helper output: %w", scanErr)
			return
		}
		if waitErr != nil {
			done <- fmt.Errorf("policy helper %s %s: %w; stderr: %s", action, id, waitErr, stderr.String())
			return
		}
		done <- nil
	}()
	return done
}

func requirePolicyProcessBlocked(t *testing.T, done <-chan error) {
	t.Helper()
	select {
	case err := <-done:
		t.Fatalf("policy command finished while another process held the lock: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestDiskPolicySetSerializesAcrossProcesses(t *testing.T) {
	path := filepath.Join(t.TempDir(), "disk-policies.json")
	lock, err := lockDiskPolicies(path)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	first := startDiskPolicyProcess(t, path, "set", "disk1")
	second := startDiskPolicyProcess(t, path, "set", "disk2")
	requirePolicyProcessBlocked(t, first)
	requirePolicyProcessBlocked(t, second)
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	if err := <-second; err != nil {
		t.Fatal(err)
	}
	policies, err := readDiskPolicies(path)
	if err != nil || len(policies) != 2 || policies["disk1"] != diskPolicyInclude || policies["disk2"] != diskPolicyInclude {
		t.Fatalf("concurrent policies = %#v, %v", policies, err)
	}
}

func TestDiskPolicyResetSharesSetLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "disk-policies.json")
	if err := writeDiskPolicy(path, "disk1", diskPolicyInclude); err != nil {
		t.Fatal(err)
	}
	lock, err := lockDiskPolicies(path)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	reset := startDiskPolicyProcess(t, path, "reset", "")
	requirePolicyProcessBlocked(t, reset)
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
	if err := <-reset; err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("policy file after reset: %v; want absent", err)
	}
	if _, err := os.Stat(path + ".lock"); err != nil {
		t.Fatalf("persistent policy lock after reset: %v", err)
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
	if err := writeDiskPolicy(environment.paths.policyFile, "bridge_serial", diskPolicyInclude); err != nil {
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
	if err := writeDiskPolicy(environment.paths.policyFile, "bridge_serial", diskPolicyExclude); err != nil {
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
		"[disk1]\nid=\""+id+"\"\ndevice=sdb\ntransport=ata\nrotational=1\nspundown=0\ntemp=35\n")
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

func TestDiskListCanExposeIncompleteEntryForExclusion(t *testing.T) {
	environment := newDiskTestEnvironment(t, "30")
	environment.write(t, environment.paths.disksINI,
		"[disk1]\nid=stable_id\ndevice=../invalid\ntransport=ata\nrotational=invalid\n")
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
	if len(collector.state) != 1 {
		t.Fatalf("collector state with invalid policies = %#v; want internal disk only", collector.state)
	}
	if status := collector.status(); !strings.Contains(status.policyError, "invalid disk policy") {
		t.Fatalf("collector policy error = %q", status.policyError)
	}
	listArgs := []string{"list", "--disks-ini", environment.paths.disksINI,
		"--devs-ini", environment.paths.devsINI, "--sys-block-root", environment.paths.sysBlockRoot,
		"--policy-file", environment.paths.policyFile}
	var output bytes.Buffer
	if err := diskPolicyCommand(listArgs, &output); err == nil || !strings.Contains(err.Error(), "invalid disk policy") {
		t.Fatalf("disks list with invalid policies error = %v", err)
	}
	if output.Len() != 0 {
		t.Fatalf("disks list wrote output despite invalid policies: %q", output.String())
	}
	if err := diskPolicyCommand([]string{"validate", "--policy-file", environment.paths.policyFile}, &output); err == nil {
		t.Fatal("disks validate accepted invalid policies")
	}
	if err := writeDiskPolicy(environment.paths.policyFile, "internal", diskPolicyExclude); err == nil || !strings.Contains(err.Error(), "invalid disk policy") {
		t.Fatalf("overwriting invalid policy file error = %v", err)
	}
	encodedID := base64.StdEncoding.EncodeToString([]byte("internal"))
	if err := diskPolicyCommand([]string{"set", "--id-base64", encodedID, "--policy", "exclude",
		"--policy-file", environment.paths.policyFile}, &output); err == nil {
		t.Fatal("disks set overwrote invalid policies")
	}
	if err := diskPolicyCommand([]string{"reset", "--policy-file", environment.paths.policyFile}, &output); err != nil {
		t.Fatalf("disks reset: %v", err)
	}
	if _, err := os.Stat(environment.paths.policyFile); !os.IsNotExist(err) {
		t.Fatalf("policy file after reset: %v; want absent", err)
	}
	if err := diskPolicyCommand([]string{"reset", "--policy-file", environment.paths.policyFile}, &output); err != nil {
		t.Fatalf("disks reset with absent file: %v", err)
	}
	if err := diskPolicyCommand([]string{"validate", "--policy-file", environment.paths.policyFile}, &output); err != nil {
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
	if err := resetDiskPolicies(path); err == nil {
		t.Fatal("reset removed a directory instead of a policy file")
	}
	if info, err := os.Stat(path); err != nil || !info.IsDir() {
		t.Fatalf("policy directory after failed reset = %v, %v", info, err)
	}
}
