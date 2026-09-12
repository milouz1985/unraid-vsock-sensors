// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"unraid-vsock-sensors/internal/sensors"
)

// Representative anonymized SCSI identity at the 79-byte length observed in
// Unraid tests. Its model_serial shape matters; the original value does not.
const maxLengthUnraidDiskID = "SEAGATE_EXOS_X24_ST24000NM002H-3KS133_ANONYMIZED_SERIAL_00000000000000000000000"

type diskTestEnvironment struct {
	paths diskDataPaths
	now   time.Time
}

func newDiskTestEnvironment(t *testing.T, pollAttributes string) *diskTestEnvironment {
	t.Helper()
	root := t.TempDir()
	environment := &diskTestEnvironment{
		paths: diskDataPaths{
			disksINI: filepath.Join(root, "disks.ini"), devsINI: filepath.Join(root, "devs.ini"),
			smartDir: filepath.Join(root, "smart"), varINI: filepath.Join(root, "var.ini"),
			sysBlockRoot: filepath.Join(root, "class", "block"),
			policyFile:   filepath.Join(root, "disk-policies.json"),
		},
		now: time.Unix(1_800_000_000, 0),
	}
	if err := os.Mkdir(environment.paths.smartDir, 0700); err != nil {
		t.Fatal(err)
	}
	environment.write(t, environment.paths.disksINI, "[flash]\ndevice=sdz\n")
	environment.write(t, environment.paths.devsINI, "")
	environment.write(t, environment.paths.varINI, "poll_attributes=\""+pollAttributes+"\"\n")
	return environment
}

func unknownBusSelector(t *testing.T) *diskSelector {
	t.Helper()
	return &diskSelector{sysBlockRoot: filepath.Join(t.TempDir(), "class", "block")}
}

func (environment *diskTestEnvironment) write(t *testing.T, path, data string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
}

func (environment *diskTestEnvironment) report(t *testing.T, name string, mtime time.Time) {
	t.Helper()
	path := filepath.Join(environment.paths.smartDir, name)
	environment.write(t, path, "cached SMART report\n")
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatal(err)
	}
}

func (environment *diskTestEnvironment) collector() *diskCollector {
	collector := newDiskCollector(environment.paths)
	collector.now = func() time.Time { return environment.now }
	return collector
}

func requireSingleDisk(t *testing.T, collector *diskCollector) sensorsDisk {
	t.Helper()
	readings, err := collector.snapshot()
	if err != nil || len(readings) != 1 {
		t.Fatalf("snapshot = %#v, %v; want one disk", readings, err)
	}
	return sensorsDisk{readings[0].ID, readings[0].Name, readings[0].Device, readings[0].Temp, readings[0].Unavailable}
}

// sensorsDisk keeps assertions concise without hiding the public snapshot fields.
type sensorsDisk struct {
	id, name, device string
	temp             float64
	unavailable      bool
}

func TestAssignedDiskUsesFreshUnraidTemperatureAndLogicalSMARTName(t *testing.T) {
	environment := newDiskTestEnvironment(t, "30")
	environment.write(t, environment.paths.disksINI, strings.TrimSpace(`
		["disk1"]
		id="WDC_stable_serial"
		device="sda"
		status="DISK_OK"
		rotational="1"
		transport="ata"
		spundown="0"
		temp="35"
	`)+"\n")
	environment.report(t, "disk1", environment.now)

	collector := environment.collector()
	collector.refresh()
	disk := requireSingleDisk(t, collector)
	if disk.id != "WDC_stable_serial" || disk.name != "disk1" || disk.device != "sda" ||
		disk.temp != 35 || disk.unavailable {
		t.Fatalf("assigned disk = %#v", disk)
	}
	if _, err := os.Stat(filepath.Join(environment.paths.smartDir, "sda")); !os.IsNotExist(err) {
		t.Fatalf("assigned disk unexpectedly required smart/sda: %v", err)
	}
}

func TestUnassignedDiskUsesDeviceSMARTNameAndStableIdentity(t *testing.T) {
	environment := newDiskTestEnvironment(t, "30")
	writeDevice := func(device string) {
		environment.write(t, environment.paths.devsINI, strings.TrimSpace(`
			["dev1"]
			name="dev1"
			id="TOSHIBA_stable_serial"
			device="`+device+`"
			rotational="1"
			transport="ata"
			spundown="0"
			temp="42"
		`)+"\n")
	}
	writeDevice("/dev/sda")
	environment.report(t, "sda", environment.now)

	collector := environment.collector()
	collector.refresh()
	first := requireSingleDisk(t, collector)
	if first.id != "TOSHIBA_stable_serial" || first.device != "sda" || first.temp != 42 || first.unavailable {
		t.Fatalf("unassigned disk = %#v", first)
	}

	writeDevice("sdb")
	environment.report(t, "sdb", environment.now)
	collector.refresh()
	second := requireSingleDisk(t, collector)
	if second.id != first.id || second.device != "sdb" || second.temp != 42 || second.unavailable {
		t.Fatalf("unassigned disk after sdX change = %#v", second)
	}
}

func TestSMARTCacheFreshnessBoundary(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	freshness := smartFreshnessWindow(30 * time.Second)
	for name, age := range map[string]time.Duration{
		"exactly at limit": freshness,
		"just past limit":  freshness + time.Nanosecond,
	} {
		t.Run(name, func(t *testing.T) {
			directory := t.TempDir()
			path := filepath.Join(directory, "disk1")
			if err := os.WriteFile(path, nil, 0600); err != nil {
				t.Fatal(err)
			}
			mtime := now.Add(-age)
			if err := os.Chtimes(path, mtime, mtime); err != nil {
				t.Fatal(err)
			}
			observations := makeDiskObservations([]unraidDisk{{
				id: "serial", name: "disk1", device: "sda", smartName: "disk1", temperature: "35",
			}}, directory, now, freshness)
			if len(observations) != 1 || (observations[0].err != nil) != (age > freshness) {
				t.Fatalf("observation at age %s = %#v", age, observations)
			}
		})
	}
}

func TestActiveDiskRejectsMissingReportAndInvalidTemperature(t *testing.T) {
	for name, test := range map[string]struct {
		temperature string
		report      bool
	}{
		"missing report":      {temperature: "35"},
		"missing temperature": {temperature: "", report: true},
		"asterisk":            {temperature: "*", report: true},
		"not numeric":         {temperature: "warm", report: true},
	} {
		t.Run(name, func(t *testing.T) {
			now := time.Unix(1_800_000_000, 0)
			directory := t.TempDir()
			if test.report {
				path := filepath.Join(directory, "disk1")
				if err := os.WriteFile(path, nil, 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.Chtimes(path, now, now); err != nil {
					t.Fatal(err)
				}
			}
			observations := makeDiskObservations([]unraidDisk{{
				id: "serial", name: "disk1", device: "sda", smartName: "disk1", temperature: test.temperature,
			}}, directory, now, time.Minute)
			readings := make(diskStateTracker).apply(observations, now, time.Minute)
			if len(readings) != 1 || !readings[0].Unavailable || readings[0].Temp != 0 {
				t.Fatalf("reading = %#v; want unavailable", readings)
			}
		})
	}
}

func TestCachedTemperatureAllowsValuesOutsideTypicalSensorRange(t *testing.T) {
	for _, test := range []struct {
		raw  string
		want float64
	}{
		{raw: "-40.125", want: -40.125},
		{raw: "151.5", want: 151.5},
	} {
		got, err := parseCachedTemperature(test.raw)
		if err != nil || got != test.want {
			t.Errorf("parseCachedTemperature(%q) = %g, %v; want %g", test.raw, got, err, test.want)
		}
	}
}

func TestSleepingDiskAllowsOldSMARTReport(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	directory := t.TempDir()
	path := filepath.Join(directory, "disk1")
	if err := os.WriteFile(path, nil, 0600); err != nil {
		t.Fatal(err)
	}
	old := now.Add(-24 * time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	observations := makeDiskObservations([]unraidDisk{{
		id: "serial", name: "disk1", device: "sda", smartName: "disk1",
		temperature: "*", spundown: true,
	}}, directory, now, time.Minute)
	readings := make(diskStateTracker).apply(observations, now, time.Minute)
	if len(readings) != 1 || readings[0].Unavailable || readings[0].Temp != 0 {
		t.Fatalf("sleeping disk = %#v", readings)
	}
}

func TestWakeGraceUnavailableAndRecovery(t *testing.T) {
	environment := newDiskTestEnvironment(t, "30")
	writeAssigned := func(spundown, temperature string) {
		environment.write(t, environment.paths.disksINI, strings.TrimSpace(`
			["disk1"]
			id="serial"
			device="sda"
			status="DISK_OK"
			rotational="1"
			transport="ata"
			spundown="`+spundown+`"
			temp="`+temperature+`"
		`)+"\n")
	}
	freshness := smartFreshnessWindow(30 * time.Second)
	writeAssigned("0", "35")
	environment.report(t, "disk1", environment.now)
	collector := environment.collector()
	collector.refresh()
	if disk := requireSingleDisk(t, collector); disk.temp != 35 || disk.unavailable {
		t.Fatalf("initial disk = %#v", disk)
	}

	writeAssigned("1", "*")
	environment.report(t, "disk1", environment.now.Add(-time.Hour))
	collector.refresh()
	if disk := requireSingleDisk(t, collector); disk.temp != 0 || disk.unavailable {
		t.Fatalf("sleeping disk = %#v", disk)
	}

	writeAssigned("0", "36")
	collector.refresh()
	if disk := requireSingleDisk(t, collector); disk.temp != 35 || disk.unavailable {
		t.Fatalf("disk during wake grace = %#v", disk)
	}

	environment.now = environment.now.Add(freshness)
	collector.refresh()
	if disk := requireSingleDisk(t, collector); disk.temp != 0 || !disk.unavailable {
		t.Fatalf("disk after wake grace = %#v", disk)
	}

	environment.report(t, "disk1", environment.now)
	collector.refresh()
	if disk := requireSingleDisk(t, collector); disk.temp != 36 || disk.unavailable {
		t.Fatalf("recovered disk = %#v", disk)
	}
}

func TestInventoryFailureCountsTowardDiskGracePeriod(t *testing.T) {
	environment := newDiskTestEnvironment(t, "30")
	inventory := strings.TrimSpace(`
		["disk1"]
		id="serial"
		device="sda"
		status="DISK_OK"
		rotational="1"
		transport="ata"
		spundown="0"
		temp="35"
	`) + "\n"
	environment.write(t, environment.paths.disksINI, inventory)
	environment.report(t, "disk1", environment.now)

	collector := environment.collector()
	collector.refresh()
	if disk := requireSingleDisk(t, collector); disk.temp != 35 || disk.unavailable {
		t.Fatalf("initial disk = %#v", disk)
	}

	if err := os.Remove(environment.paths.disksINI); err != nil {
		t.Fatal(err)
	}
	collector.refresh()
	if _, err := collector.snapshot(); err == nil {
		t.Fatal("broken inventory did not make the disk snapshot unavailable")
	}

	environment.now = environment.now.Add(smartFreshnessWindow(30*time.Second) + time.Second)
	environment.write(t, environment.paths.disksINI, inventory)
	collector.refresh()
	if disk := requireSingleDisk(t, collector); disk.temp != 0 || !disk.unavailable {
		t.Fatalf("disk after prolonged inventory failure = %#v", disk)
	}
}

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
			data := fmt.Sprintf("[disk1]\nid=%q\ndevice=sdz\nstatus=DISK_OK\n", test.id)
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

func TestUnraid72MissingAssignmentFixtures(t *testing.T) {
	disksINI := filepath.Join("testdata", "unraid", "7.2", "disks-missing-assignment.ini")
	devsINI := filepath.Join("testdata", "unraid", "7.2", "devs-unassigned-ata.ini")

	selector := unknownBusSelector(t)
	assigned, err := readDisks(disksINI, selector)
	if err != nil || len(assigned) != 0 {
		t.Fatalf("assigned inventory = %#v, %v; want ignored DISK_NP_DSBL slot", assigned, err)
	}
	disks, err := readDiskInventory(disksINI, devsINI, selector)
	if err != nil || len(disks) != 1 {
		t.Fatalf("merged inventory = %#v, %v; want unassigned disk", disks, err)
	}
	disk := disks[0]
	if disk.id != "TOSHIBA_MG09ACA18TE_ANON0001" || disk.device != "sdc" || disk.transport != "ata" {
		t.Fatalf("unassigned disk = %#v", disk)
	}
	if kind := (sensors.Disk{Device: disk.device, Transport: disk.transport, Rotational: disk.rotational}).Kind(); kind != sensors.DiskKindSATASSD {
		t.Errorf("unassigned ATA disk kind = %q, want %q", kind, sensors.DiskKindSATASSD)
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

func TestSMARTFreshnessMargin(t *testing.T) {
	for poll, want := range map[time.Duration]time.Duration{
		30 * time.Second:  40 * time.Second,
		60 * time.Second:  72 * time.Second,
		300 * time.Second: 360 * time.Second,
	} {
		if got := smartFreshnessWindow(poll); got != want {
			t.Fatalf("freshness for %s = %s, want %s", poll, got, want)
		}
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
	if message := output.String(); !strings.Contains(message, "invalid config") || !strings.Contains(message, "30s fallback") {
		t.Fatalf("fallback warning = %q", message)
	}
}

func TestDiskInventoryDeduplicatesAssignedAndUnassignedByStableID(t *testing.T) {
	environment := newDiskTestEnvironment(t, "30")
	environment.write(t, environment.paths.disksINI, "[disk1]\nid=serial\ndevice=sda\ntemp=35\n")
	environment.write(t, environment.paths.devsINI, "[dev1]\nid=serial\ndevice=sdb\ntemp=35\n")
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
			inventory := "[disk1]\nid=serial\ndevice=sda\n[disk2]\nid=serial\ndevice=sdb\n"
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
		"[disk1]\nid=assigned1\ndevice=sda\n[disk2]\nid=assigned2\ndevice=sdb\n")
	environment.write(t, environment.paths.devsINI,
		"[dev1]\nid=unassigned1\ndevice=sdc\n[dev2]\nid=unassigned2\ndevice=sdd\n")
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
	validInventory := "[disk1]\nid=serial1\ndevice=sda\ntemp=35\n" +
		"[disk2]\nid=serial2\ndevice=sdb\ntemp=36\n"
	environment.write(t, environment.paths.disksINI, validInventory)
	environment.report(t, "disk1", environment.now)
	environment.report(t, "disk2", environment.now)
	collector := environment.collector()
	collector.refresh()
	if readings, err := collector.snapshot(); err != nil || len(readings) != 2 {
		t.Fatalf("initial snapshot = %#v, %v", readings, err)
	}

	environment.write(t, environment.paths.disksINI,
		"[disk1]\nid=serial1\ndevice=sda\ntemp=35\n"+
			"[disk2]\nid=serial1\ndevice=sdb\ntemp=36\n")
	collector.refresh()
	response := collectorSnapshot(collector, newTestHBACollector(time.Minute, hbaModeDisabled))
	if response.Disks != nil || !strings.Contains(response.Error, "duplicate disk ID \"serial1\" in disks.ini") {
		t.Fatalf("published snapshot = %#v; want error and no partial disks", response)
	}
	if len(collector.state) != 2 || !collector.state["serial1"].hasValid || !collector.state["serial2"].hasValid {
		t.Fatalf("previous disk state was lost: %#v", collector.state)
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
	environment.report(t, "disk1", environment.now)
	environment.report(t, "sdb", environment.now)

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

func TestUSBEntriesSkipIdentityValidation(t *testing.T) {
	environment := newDiskTestEnvironment(t, "30")
	addFakeBlockDevice(t, environment.paths.sysBlockRoot, "sdi", true)
	addFakeBlockDevice(t, environment.paths.sysBlockRoot, "sdj", true)
	environment.write(t, environment.paths.disksINI, "[disk1]\ntransport=usb\ndevice=sdi\n")
	environment.write(t, environment.paths.devsINI, "[external]\ntransport=\" USB \"\ndevice=sdj\n")
	entries, err := readAssignedEntries(environment.paths.disksINI,
		&diskSelector{sysBlockRoot: environment.paths.sysBlockRoot}, true)
	if err != nil || len(entries) != 1 || entries[0].included {
		t.Fatalf("assigned USB entry = %#v, %v; want excluded disk without identity error", entries, err)
	}
	entries, err = readUnassignedEntries(environment.paths.devsINI,
		&diskSelector{sysBlockRoot: environment.paths.sysBlockRoot}, nil, nil, true)
	if err != nil || len(entries) != 1 || entries[0].included {
		t.Fatalf("unassigned USB entry = %#v, %v; want excluded disk without identity error", entries, err)
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
	`)+"\n")
	disks, err := readDisks(environment.paths.disksINI, &diskSelector{sysBlockRoot: environment.paths.sysBlockRoot})
	if err != nil || len(disks) != 1 || disks[0].id != "serial5" {
		t.Fatalf("inventory = %#v, %v", disks, err)
	}
}

func TestRefreshRequestsAreCoalesced(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	refresh := make(chan struct{}, 1)
	started := make(chan int32, 3)
	release := make(chan struct{})
	var count atomic.Int32
	done := make(chan struct{})
	go func() {
		runDiskRefreshLoop(ctx, refresh, time.Hour, func() {
			current := count.Add(1)
			started <- current
			if current == 1 {
				<-release
			}
		})
		close(done)
	}()
	if current := <-started; current != 1 {
		t.Fatalf("first collection = %d", current)
	}
	for range 100 {
		requestDiskRefresh(refresh)
	}
	close(release)
	if current := <-started; current != 2 {
		t.Fatalf("coalesced collection = %d", current)
	}
	if got := count.Load(); got != 2 {
		t.Fatalf("collection count = %d, want 2", got)
	}
	cancel()
	<-done
}

func TestWatchdogDetectsCacheExpirationWithoutEvent(t *testing.T) {
	environment := newDiskTestEnvironment(t, "0")
	environment.write(t, environment.paths.disksINI, "[disk1]\nid=serial\ndevice=sda\nspundown=0\ntemp=35\n")
	environment.report(t, "disk1", environment.now)

	var nowUnixNano atomic.Int64
	nowUnixNano.Store(environment.now.UnixNano())
	collector := newDiskCollector(environment.paths)
	collector.watchdog = 5 * time.Millisecond
	collector.now = func() time.Time { return time.Unix(0, nowUnixNano.Load()) }
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		collector.run(ctx, nil)
		close(done)
	}()
	eventuallyDisk(t, collector, func(disk sensorsDisk) bool { return disk.temp == 35 && !disk.unavailable })

	nowUnixNano.Add(int64(11 * time.Second))
	eventuallyDiskFailure(t, collector, "serial")
	if disk := requireSingleDisk(t, collector); disk.temp != 35 || disk.unavailable {
		t.Fatalf("disk during watchdog grace = %#v", disk)
	}
	nowUnixNano.Add(int64(10 * time.Second))
	eventuallyDisk(t, collector, func(disk sensorsDisk) bool { return disk.unavailable })
	cancel()
	<-done
}

func eventuallyDiskFailure(t *testing.T, collector *diskCollector, id string) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		collector.refreshMu.Lock()
		failedSince := collector.state[id].failedSince
		collector.refreshMu.Unlock()
		if !failedSince.IsZero() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("watchdog did not detect the stale SMART cache")
}

func eventuallyDisk(t *testing.T, collector *diskCollector, accept func(sensorsDisk) bool) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		readings, err := collector.snapshot()
		if err == nil && len(readings) == 1 {
			disk := sensorsDisk{
				readings[0].ID, readings[0].Name, readings[0].Device,
				readings[0].Temp, readings[0].Unavailable,
			}
			if accept(disk) {
				return
			}
		}
		time.Sleep(time.Millisecond)
	}
	readings, err := collector.snapshot()
	t.Fatalf("condition not reached; snapshot = %#v, %v", readings, err)
}

func TestDiskCollectorConcurrentRefreshAndSnapshot(t *testing.T) {
	environment := newDiskTestEnvironment(t, "30")
	environment.write(t, environment.paths.disksINI, "[disk1]\nid=serial\ndevice=sda\ntemp=35\n")
	environment.report(t, "disk1", environment.now)
	collector := environment.collector()
	collector.refresh()

	start := make(chan struct{})
	done := make(chan error, 2)
	go func() {
		<-start
		for range 100 {
			collector.refresh()
		}
		done <- nil
	}()
	go func() {
		<-start
		for range 100 {
			readings, err := collector.snapshot()
			if err != nil {
				done <- err
				return
			}
			if len(readings) != 1 || readings[0].ID != "serial" {
				done <- errors.New("concurrent snapshot lost the disk reading")
				return
			}
		}
		done <- nil
	}()
	close(start)
	for range 2 {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
}
