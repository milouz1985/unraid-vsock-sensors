// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"bytes"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"unraid-vsock-sensors/internal/sensors"
)

func TestParsePollAttributes(t *testing.T) {
	for name, test := range map[string]struct {
		data string
		want time.Duration
		err  bool
	}{
		"positive":    {data: "poll_attributes=\"30\"\n", want: 30 * time.Second},
		"zero":        {data: "poll_attributes=\"0\"\n", want: 0},
		"missing":     {data: "other=\"30\"\n", err: true},
		"negative":    {data: "poll_attributes=\"-1\"\n", err: true},
		"not numeric": {data: "poll_attributes=\"fast\"\n", err: true},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := parsePollAttributes([]byte(test.data))
			if got != test.want || (err != nil) != test.err {
				t.Fatalf("parse = %s, %v; want %s, error=%v", got, err, test.want, test.err)
			}
		})
	}
}

func TestHistoricalUnraidDevsFixtureWithoutTransport(t *testing.T) {
	disks, err := readUnassignedDisks(filepath.Join("testdata", "unraid", "historical", "devs-no-transport.ini"), unknownBusSelector(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(disks) != 1 {
		t.Fatalf("inventory = %#v; want one disk", disks)
	}

	disk := disks[0]
	if disk.transport != "" {
		t.Errorf("transport = %q; want absent", disk.transport)
	}
	if kind := (sensors.Disk{Device: disk.device, Transport: disk.transport, Rotational: disk.rotational}).Kind(); kind != sensors.DiskKindHDD {
		t.Errorf("dev1 kind = %q, want %q", kind, sensors.DiskKindHDD)
	}
}

func TestHistoricalUnraidDisksFixture(t *testing.T) {
	disks, err := readDisks(filepath.Join("testdata", "unraid", "historical", "disks-array.ini"), unknownBusSelector(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(disks) != 3 {
		t.Fatalf("inventory = %#v; want active disk and two pool devices", disks)
	}

	byName := make(map[string]unraidDisk, len(disks))
	for _, disk := range disks {
		byName[disk.name] = disk
		if disk.smartName != disk.name {
			t.Errorf("%s SMART name = %q, want logical name", disk.name, disk.smartName)
		}
	}
	if _, exists := byName["disk18"]; exists {
		t.Error("empty DISK_NP slot was included")
	}
	if _, exists := byName["parity2"]; exists {
		t.Error("empty DISK_NP_DSBL slot was included")
	}
	if !byName["disk1"].rotational || !byName["cctv_pool"].rotational || byName["unraid_files"].transport != "nvme" {
		t.Fatalf("parsed active inventory = %#v", byName)
	}
}

func TestDiskInventoryRequiresExplicitBinaryThermalFields(t *testing.T) {
	tests := []struct {
		name, rotational, spundown   string
		wantRotational, wantSpundown bool
		wantError                    string
	}{
		{name: "SSD active", rotational: "0", spundown: "0"},
		{name: "HDD standby", rotational: "1", spundown: "1", wantRotational: true, wantSpundown: true},
		{name: "missing rotational", spundown: "0", wantError: "rotational must be 0 or 1"},
		{name: "invalid rotational", rotational: "ssd", spundown: "0", wantError: "rotational must be 0 or 1"},
		{name: "missing spundown", rotational: "1", wantError: "spundown must be 0 or 1"},
		{name: "invalid spundown", rotational: "1", spundown: "yes", wantError: "spundown must be 0 or 1"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "disks.ini")
			data := "[disk1]\nid=serial\ndevice=sda\nstatus=DISK_OK\n"
			if test.rotational != "" {
				data += "rotational=" + test.rotational + "\n"
			}
			if test.spundown != "" {
				data += "spundown=" + test.spundown + "\n"
			}
			if err := os.WriteFile(path, []byte(data), 0600); err != nil {
				t.Fatal(err)
			}

			disks, err := readDisks(path, unknownBusSelector(t))
			if test.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantError) {
					t.Fatalf("readDisks() error = %v; want %q", err, test.wantError)
				}
				return
			}
			if err != nil || len(disks) != 1 || disks[0].rotational != test.wantRotational || disks[0].spundown != test.wantSpundown {
				t.Fatalf("readDisks() = %#v, %v", disks, err)
			}
		})
	}
}

func TestUnraidDiskIDSizeContract(t *testing.T) {
	if got := len(maxLengthUnraidDiskID); got != maxUnraidDiskIDSize {
		t.Fatalf("maximum-length test ID is %d bytes, want %d", got, maxUnraidDiskIDSize)
	}
	tests := []struct {
		name    string
		id      string
		wantErr bool
	}{
		{name: "78 bytes", id: maxLengthUnraidDiskID[:maxUnraidDiskIDSize-1]},
		{name: "79 bytes", id: maxLengthUnraidDiskID},
		{name: "80 bytes", id: maxLengthUnraidDiskID + "X", wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "disks.ini")
			data := fmt.Sprintf("[disk1]\nid=%q\ndevice=sdz\nstatus=DISK_OK\nrotational=1\nspundown=0\n", test.id)
			if err := os.WriteFile(path, []byte(data), 0600); err != nil {
				t.Fatal(err)
			}
			disks, err := readDisks(path, unknownBusSelector(t))
			if test.wantErr {
				if err == nil || !strings.Contains(err.Error(), "observed emhttpd limit is 79") {
					t.Fatalf("readDisks() error = %v, want maximum-ID error", err)
				}
				return
			}
			if err != nil || len(disks) != 1 || disks[0].id != test.id {
				t.Fatalf("readDisks() = %#v, %v", disks, err)
			}
		})
	}
}

func TestUnraid72MissingAssignmentSlotIsIgnored(t *testing.T) {
	disksINI := filepath.Join("testdata", "unraid", "7.2", "disks-missing-assignment.ini")
	assigned, err := readDisks(disksINI, unknownBusSelector(t))
	if err != nil || len(assigned) != 0 {
		t.Fatalf("assigned inventory = %#v, %v; want ignored DISK_NP_DSBL slot", assigned, err)
	}
}

func TestUnraid72PresentAssignmentWithoutIdentityIsInvalid(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "unraid", "7.2", "disks-missing-assignment.ini"))
	if err != nil {
		t.Fatal(err)
	}
	data = bytes.Replace(data, []byte(`status="DISK_NP_DSBL"`), []byte(`status="DISK_INVALID"`), 1)
	path := filepath.Join(t.TempDir(), "disks.ini")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := readDisks(path, unknownBusSelector(t)); err == nil || !strings.Contains(err.Error(), "has no stable ID") {
		t.Fatalf("read present disk without identity error = %v", err)
	}
}

func TestCurrentUnraidVarFixture(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "unraid", "current", "var.ini"))
	if err != nil {
		t.Fatal(err)
	}
	interval, err := parsePollAttributes(data)
	if err != nil || interval != 30*time.Second {
		t.Fatalf("poll attributes interval = %s, %v; want 30s", interval, err)
	}
}

func TestReadPollAttributesAlwaysReadsCurrentContents(t *testing.T) {
	path := filepath.Join(t.TempDir(), "var.ini")
	mtime := time.Unix(1_800_000_000, 0)
	write := func(value string) {
		t.Helper()
		if err := os.WriteFile(path, []byte("poll_attributes=\""+value+"\"\n"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, mtime, mtime); err != nil {
			t.Fatal(err)
		}
	}

	write("30")
	if interval, err := readPollAttributes(path); interval != 30*time.Second || err != nil {
		t.Fatalf("initial read = %s, %v", interval, err)
	}

	write("60")
	if interval, err := readPollAttributes(path); interval != 60*time.Second || err != nil {
		t.Fatalf("same-metadata read = %s, %v", interval, err)
	}

	write("xx")
	if interval, err := readPollAttributes(path); interval != defaultPollAttributes || err == nil {
		t.Fatalf("invalid read = %s, %v", interval, err)
	}
	write("90")
	if interval, err := readPollAttributes(path); interval != 90*time.Second || err != nil {
		t.Fatalf("read after parse error = %s, %v", interval, err)
	}

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if interval, err := readPollAttributes(path); interval != defaultPollAttributes || err == nil {
		t.Fatalf("missing read = %s, %v", interval, err)
	}
	write("45")
	if interval, err := readPollAttributes(path); interval != 45*time.Second || err != nil {
		t.Fatalf("read after file error = %s, %v", interval, err)
	}
}

func TestPollAttributesLogMessageWithoutSMARTCache(t *testing.T) {
	var output bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&output)
	t.Cleanup(func() { log.SetOutput(previous) })

	logPollAttributes(defaultPollAttributes, errors.New("invalid config"))
	if message := output.String(); !strings.Contains(message, "invalid config") || !strings.Contains(message, "stalled-poll detection") {
		t.Fatalf("fallback warning = %q", message)
	}
}

func TestPollAttributesWarnings(t *testing.T) {
	var output bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&output)
	t.Cleanup(func() { log.SetOutput(previous) })

	logPollAttributes(0, nil)
	if message := output.String(); !strings.Contains(message, "poll_attributes=0") || !strings.Contains(message, "disabled") {
		t.Fatalf("disabled warning = %q", message)
	}
	output.Reset()
	logPollAttributes(5*time.Minute, nil)
	if message := output.String(); !strings.Contains(message, "5m0s") || !strings.Contains(message, "fan control") {
		t.Fatalf("slow polling warning = %q", message)
	}
	output.Reset()
	logPollAttributes(defaultPollAttributes, errors.New("invalid config"))
	if message := output.String(); !strings.Contains(message, "invalid config") || !strings.Contains(message, "30s for stalled-poll detection") {
		t.Fatalf("fallback warning = %q", message)
	}
}

func TestDiskInventoryDeduplicatesAssignedAndUnassignedByStableID(t *testing.T) {
	environment := newDiskTestEnvironment(t, "30")
	environment.write(t, environment.paths.disksINI, "[disk1]\nid=serial\ndevice=sda\nrotational=1\nspundown=0\ntemp=35\n")
	environment.write(t, environment.paths.devsINI, "[dev1]\nid=serial\ndevice=sdb\nrotational=1\nspundown=0\ntemp=35\n")
	disks, err := readDiskInventory(environment.paths.disksINI, environment.paths.devsINI,
		&diskSelector{sysBlockRoot: environment.paths.sysBlockRoot})
	if err != nil || len(disks) != 1 || disks[0].name != "disk1" || disks[0].smartName != "disk1" {
		t.Fatalf("deduplicated inventory = %#v, %v", disks, err)
	}
}

func TestDiskInventoryRejectsDuplicateIDsWithinSource(t *testing.T) {
	for _, test := range []struct {
		name, source string
		assigned     bool
		policy       diskPolicy
		usb          bool
	}{
		{name: "assigned", source: "disks.ini", assigned: true},
		{name: "unassigned", source: "devs.ini"},
		{name: "assigned excluded by policy", source: "disks.ini", assigned: true, policy: diskPolicyExclude},
		{name: "unassigned excluded by policy", source: "devs.ini", policy: diskPolicyExclude},
		{name: "assigned USB excluded in Auto", source: "disks.ini", assigned: true, usb: true},
		{name: "unassigned USB excluded in Auto", source: "devs.ini", usb: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			environment := newDiskTestEnvironment(t, "30")
			inventory := "[disk1]\nid=serial\ndevice=sda\nrotational=1\nspundown=0\n[disk2]\nid=serial\ndevice=sdb\nrotational=1\nspundown=0\n"
			path := environment.paths.devsINI
			if test.assigned {
				path = environment.paths.disksINI
			}
			environment.write(t, path, inventory)
			if test.usb {
				addFakeBlockDevice(t, environment.paths.sysBlockRoot, "sda", true)
				addFakeBlockDevice(t, environment.paths.sysBlockRoot, "sdb", true)
			}
			selector := &diskSelector{sysBlockRoot: environment.paths.sysBlockRoot}
			if test.policy != "" {
				selector.policies = map[string]diskPolicy{"serial": test.policy}
			}
			disks, err := readDiskInventory(environment.paths.disksINI, environment.paths.devsINI, selector)
			want := "duplicate disk ID \"serial\" in " + test.source
			if disks != nil || err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("inventory = %#v, %v; want %q", disks, err, want)
			}
		})
	}
}

func TestDiskInventoryKeepsDistinctIDsWithinEachSource(t *testing.T) {
	environment := newDiskTestEnvironment(t, "30")
	environment.write(t, environment.paths.disksINI,
		"[disk1]\nid=assigned1\ndevice=sda\nrotational=1\nspundown=0\n[disk2]\nid=assigned2\ndevice=sdb\nrotational=1\nspundown=0\n")
	environment.write(t, environment.paths.devsINI,
		"[dev1]\nid=unassigned1\ndevice=sdc\nrotational=1\nspundown=0\n[dev2]\nid=unassigned2\ndevice=sdd\nrotational=1\nspundown=0\n")
	disks, err := readDiskInventory(environment.paths.disksINI, environment.paths.devsINI,
		&diskSelector{sysBlockRoot: environment.paths.sysBlockRoot})
	if err != nil || len(disks) != 4 {
		t.Fatalf("inventory = %#v, %v; want four disks", disks, err)
	}
	byID := make(map[string]unraidDisk, len(disks))
	for _, disk := range disks {
		byID[disk.id] = disk
	}
	for id, wantName := range map[string]string{
		"assigned1": "disk1", "assigned2": "disk2", "unassigned1": "dev1", "unassigned2": "dev2",
	} {
		if byID[id].name != wantName {
			t.Errorf("disk %q = %#v; want name %q", id, byID[id], wantName)
		}
	}
}

func TestDuplicateDiskIDDoesNotPublishPartialSnapshot(t *testing.T) {
	environment := newDiskTestEnvironment(t, "30")
	validInventory := "[disk1]\nid=serial1\ndevice=sda\nrotational=1\nspundown=0\ntemp=35\n" +
		"[disk2]\nid=serial2\ndevice=sdb\nrotational=1\nspundown=0\ntemp=36\n"
	environment.write(t, environment.paths.disksINI, validInventory)
	collector := environment.collector()
	collector.refresh()
	if readings, err := collector.snapshot(); err != nil || len(readings) != 2 {
		t.Fatalf("initial snapshot = %#v, %v", readings, err)
	}
	previousState := map[string]diskState{
		"serial1": collector.state["serial1"],
		"serial2": collector.state["serial2"],
	}

	environment.write(t, environment.paths.disksINI,
		"[disk1]\nid=serial1\ndevice=sda\nrotational=1\nspundown=0\ntemp=35\n"+
			"[disk2]\nid=serial1\ndevice=sdb\nrotational=1\nspundown=0\ntemp=36\n")
	collector.refresh()
	response := collectorSnapshot(collector, newTestHBACollector(time.Minute, hbaModeDisabled))
	if response.Disks != nil || !strings.Contains(response.Error, "duplicate disk ID \"serial1\" in disks.ini") {
		t.Fatalf("published snapshot = %#v; want error and no partial disks", response)
	}
	if len(collector.state) != 2 {
		t.Fatalf("previous disk history was lost: %#v", collector.state)
	}
	for id, before := range previousState {
		after := collector.state[id]
		if after.thermalState != diskThermalUnavailable || !after.lastValidAt.Equal(before.lastValidAt) ||
			after.lastSource != before.lastSource {
			t.Fatalf("disk %s state after inventory error = %#v; before=%#v", id, after, before)
		}
	}

	environment.write(t, environment.paths.disksINI, validInventory)
	collector.refresh()
	if readings, err := collector.snapshot(); err != nil || len(readings) != 2 {
		t.Fatalf("recovered snapshot = %#v, %v", readings, err)
	}
}

func TestDiskInventoryExcludesUSBFromBothSourcesBeforeSMART(t *testing.T) {
	environment := newDiskTestEnvironment(t, "30")
	addFakeBlockDevice(t, environment.paths.sysBlockRoot, "sda", false)
	addFakeBlockDevice(t, environment.paths.sysBlockRoot, "sdb", false)
	addFakeBlockDevice(t, environment.paths.sysBlockRoot, "sdi", true)
	addFakeBlockDevice(t, environment.paths.sysBlockRoot, "sdj", true)
	environment.write(t, environment.paths.disksINI, strings.TrimSpace(`
		[disk1]
		id=internal_hdd
		device=sda
		transport=ata
		rotational=1
		spundown=0
		temp=35

		[disk2]
		id=USB_external_serial
		device=sdi
		transport=" USB "
		rotational=1
		spundown=0
		temp=36
	`)+"\n")
	environment.write(t, environment.paths.devsINI, strings.TrimSpace(`
		[internal]
		id=internal_ssd
		device=sdb
		transport=ata
		rotational=0
		spundown=0
		temp=42

		[external]
		id=USB_unassigned_serial
		device=sdj
		transport=usb
		rotational=1
		spundown=0
		temp=37
	`)+"\n")

	disks, err := readDiskInventory(environment.paths.disksINI, environment.paths.devsINI,
		&diskSelector{sysBlockRoot: environment.paths.sysBlockRoot})
	if err != nil || len(disks) != 2 {
		t.Fatalf("inventory = %#v, %v; want two internal disks", disks, err)
	}
	byID := make(map[string]unraidDisk, len(disks))
	for _, disk := range disks {
		byID[disk.id] = disk
	}
	if byID["internal_hdd"].smartName != "disk1" || byID["internal_ssd"].smartName != "sdb" {
		t.Fatalf("internal inventory = %#v", byID)
	}
	if _, exists := byID["USB_external_serial"]; exists {
		t.Fatal("assigned USB disk entered the inventory")
	}
	if _, exists := byID["USB_unassigned_serial"]; exists {
		t.Fatal("unassigned USB disk entered the inventory")
	}

	collector := environment.collector()
	collector.refresh()
	readings, err := collector.snapshot()
	if err != nil || len(readings) != 2 {
		t.Fatalf("snapshot = %#v, %v; want two internal disks", readings, err)
	}
	hddCount := 0
	for _, reading := range readings {
		switch reading.ID {
		case "internal_hdd":
			if reading.Temp != 35 || reading.Unavailable || reading.Kind() != sensors.DiskKindHDD {
				t.Errorf("internal HDD reading = %#v", reading)
			}
		case "internal_ssd":
			if reading.Temp != 42 || reading.Unavailable || reading.Kind() != sensors.DiskKindSATASSD {
				t.Errorf("unassigned SATA SSD reading = %#v", reading)
			}
		default:
			t.Errorf("unexpected disk reading = %#v", reading)
		}
		if reading.Kind() == sensors.DiskKindHDD {
			hddCount++
		}
	}
	if hddCount != 1 || len(collector.state) != 2 {
		t.Fatalf("HDD count = %d, disk state = %#v; want only internal disks", hddCount, collector.state)
	}
}

func TestAutoExcludedUSBEntriesSkipStrictValidation(t *testing.T) {
	environment := newDiskTestEnvironment(t, "30")
	addFakeBlockDevice(t, environment.paths.sysBlockRoot, "sdi", true)
	addFakeBlockDevice(t, environment.paths.sysBlockRoot, "sdj", true)
	environment.write(t, environment.paths.disksINI, "[disk1]\ntransport=usb\ndevice=sdi\n")
	environment.write(t, environment.paths.devsINI, "[external]\ntransport=\" USB \"\ndevice=sdj\nrotational=invalid\nspundown=invalid\n")
	entries, err := readAssignedEntries(environment.paths.disksINI,
		&diskSelector{sysBlockRoot: environment.paths.sysBlockRoot}, true)
	if err != nil || len(entries) != 1 || entries[0].included {
		t.Fatalf("assigned USB entry = %#v, %v; want incomplete excluded disk", entries, err)
	}
	entries, err = readUnassignedEntries(environment.paths.devsINI,
		&diskSelector{sysBlockRoot: environment.paths.sysBlockRoot}, nil, nil, true)
	if err != nil || len(entries) != 1 || entries[0].included {
		t.Fatalf("unassigned USB entry = %#v, %v; want invalid excluded disk", entries, err)
	}
}

func TestReadInventorySkipsNoPhysicalDiskStatesAndKeepsDegradedDisk(t *testing.T) {
	environment := newDiskTestEnvironment(t, "30")
	environment.write(t, environment.paths.disksINI, strings.TrimSpace(`
		[disk1]
		status=DISK_NP

		[disk2]
		status=DISK_NP_DSBL

		[disk3]
		status=DISK_NP_MISSING

		[disk4]
		status=DISK_OK_NP

		[disk5]
		id=serial5
		device=sde
		status=DISK_INVALID
		rotational=1
		spundown=0
	`)+"\n")
	disks, err := readDisks(environment.paths.disksINI, &diskSelector{sysBlockRoot: environment.paths.sysBlockRoot})
	if err != nil || len(disks) != 1 || disks[0].id != "serial5" {
		t.Fatalf("inventory = %#v, %v", disks, err)
	}
}
