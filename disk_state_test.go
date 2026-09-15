// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
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
	environment.write(t, environment.paths.disksINI, "[flash]\ndevice=sdz\nrotational=0\nspundown=0\n")
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

func (c *diskCollector) refresh() {
	c.refreshWithContext(context.Background())
}

func readDisks(disksINIPath string, selector *diskSelector) ([]unraidDisk, error) {
	entries, err := readAssignedEntries(disksINIPath, selector, true)
	if err != nil {
		return nil, err
	}
	return includedDisks(entries), nil
}

func readUnassignedDisks(devsINIPath string, selector *diskSelector) ([]unraidDisk, error) {
	entries, err := readUnassignedEntries(devsINIPath, selector, nil, nil, true)
	if err != nil {
		return nil, err
	}
	return includedDisks(entries), nil
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

func TestDiskSnapshotUsesRuntimeOrderAndReturnsCopy(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	collector := newDiskCollector(diskDataPaths{})
	collector.now = func() time.Time { return now }
	collector.err = nil
	collector.updatedAt = now
	collector.lastSuccessfulSnapshot = []diskRuntimeDisk{
		{reading: sensors.Disk{ID: "second", Temp: 52}, hasReading: true},
		{reading: sensors.Disk{ID: "first", Temp: 41}, hasReading: true},
	}

	readings, err := collector.snapshot()
	if err != nil || len(readings) != 2 || readings[0].ID != "second" || readings[1].ID != "first" {
		t.Fatalf("snapshot order = %#v, %v", readings, err)
	}
	readings[0].Temp = 99
	again, err := collector.snapshot()
	if err != nil || again[0].Temp != 52 {
		t.Fatalf("caller mutated runtime snapshot: %#v, %v", again, err)
	}
}

func TestDiskSnapshotPreservesSuccessfulEmptyInventory(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	collector := newDiskCollector(diskDataPaths{})
	collector.now = func() time.Time { return now }
	collector.err = nil
	collector.updatedAt = now
	collector.lastSuccessfulSnapshot = []diskRuntimeDisk{}
	readings, err := collector.snapshot()
	if err != nil || readings == nil || len(readings) != 0 {
		t.Fatalf("empty snapshot = %#v, %v", readings, err)
	}
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
			if age <= freshness {
				tracker := make(diskStateTracker)
				tracker.apply(observations, now, time.Minute)
				if got := tracker["serial"].lastValidAt; !got.Equal(mtime) {
					t.Fatalf("last valid timestamp = %s; want cache mtime %s", got, mtime)
				}
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
		{raw: "0", want: 0},
		{raw: "-40.125", want: -40.125},
		{raw: "151.5", want: 151.5},
	} {
		got, err := parseCachedTemperature(test.raw)
		if err != nil || got != test.want {
			t.Errorf("parseCachedTemperature(%q) = %g, %v; want %g", test.raw, got, err, test.want)
		}
	}
}

func TestZeroCachedTemperatureIsValidReading(t *testing.T) {
	environment := newDiskTestEnvironment(t, "30")
	environment.write(t, environment.paths.disksINI, "[disk1]\nid=serial\ndevice=sda\nrotational=1\nspundown=0\ntemp=0\n")
	environment.report(t, "disk1", environment.now)
	collector := environment.collector()
	collector.refresh()
	disk := requireSingleDisk(t, collector)
	if disk.temp != 0 || disk.unavailable || collector.state["serial"].thermalState != diskThermalValid {
		t.Fatalf("zero-degree reading = %#v; state=%#v", disk, collector.state["serial"])
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
	tracker := make(diskStateTracker)
	readings := tracker.apply(observations, now, time.Minute)
	if len(readings) != 1 || readings[0].Unavailable || readings[0].Temp != 0 {
		t.Fatalf("sleeping disk = %#v", readings)
	}
	readings = tracker.apply(observations, now.Add(time.Hour), time.Minute)
	if readings[0].Unavailable || readings[0].Temp != 0 || tracker["serial"].thermalState != diskThermalStandby {
		t.Fatalf("disk remaining in standby = %#v; state=%#v", readings, tracker["serial"])
	}
}

func TestWakeGraceUsesSyntheticZeroUntilFreshTemperature(t *testing.T) {
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

	environment.now = environment.now.Add(2 * time.Hour)
	collector.noteEmhttpPoll()
	writeAssigned("0", "*")
	collector.refresh()
	if disk := requireSingleDisk(t, collector); disk.temp != 0 || disk.unavailable {
		t.Fatalf("disk during wake grace = %#v", disk)
	}
	wakeStartedAt := collector.state["serial"].wakeStartedAt
	if collector.state["serial"].thermalState != diskThermalWaking || !wakeStartedAt.Equal(environment.now) {
		t.Fatalf("wake state = %#v", collector.state["serial"])
	}
	if source := collector.smartSource.status().source; source != diskSourceEmhttpd {
		t.Fatalf("invalid per-disk cache changed healthy emhttpd source to %q", source)
	}

	environment.now = environment.now.Add(30 * time.Second)
	collector.noteEmhttpPoll()
	collector.refresh()
	if disk := requireSingleDisk(t, collector); disk.temp != 0 || disk.unavailable {
		t.Fatalf("disk before fresh post-wake sample = %#v", disk)
	}
	if !collector.state["serial"].wakeStartedAt.Equal(wakeStartedAt) {
		t.Fatal("invalid refresh restarted the wake grace")
	}

	writeAssigned("0", "28")
	environment.report(t, "disk1", environment.now)
	collector.refresh()
	if disk := requireSingleDisk(t, collector); disk.temp != 28 || disk.unavailable {
		t.Fatalf("recovered disk = %#v", disk)
	}
	if state := collector.state["serial"]; state.thermalState != diskThermalValid || !state.wakeStartedAt.IsZero() {
		t.Fatalf("state after fresh post-wake sample = %#v", state)
	}
}

func TestWakeGraceExpiresWithoutFreshTemperature(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	wakeGrace := 30*time.Second + diskWakeMargin
	disk := unraidDisk{id: "serial", name: "disk1", device: "sda"}
	tracker := make(diskStateTracker)
	tracker.apply([]diskObservation{{disk: disk, standby: true, source: diskSourceEmhttpd}}, now, wakeGrace)
	readings := tracker.apply([]diskObservation{{disk: disk, source: diskSourceEmhttpd, err: errors.New("temperature pending")}}, now.Add(time.Hour), wakeGrace)
	if len(readings) != 1 || readings[0].Temp != 0 || readings[0].Unavailable || tracker["serial"].thermalState != diskThermalWaking {
		t.Fatalf("initial wake reading = %#v; state=%#v", readings, tracker["serial"])
	}
	wakeStartedAt := tracker["serial"].wakeStartedAt
	readings = tracker.apply([]diskObservation{{disk: disk, source: diskSourceEmhttpd, err: errors.New("temperature pending")}}, wakeStartedAt.Add(wakeGrace), wakeGrace)
	if len(readings) != 1 || readings[0].Temp != 0 || !readings[0].Unavailable || tracker["serial"].thermalState != diskThermalUnavailable {
		t.Fatalf("expired wake reading = %#v; state=%#v", readings, tracker["serial"])
	}
}

func TestActiveObservationFailureDoesNotRetainPreviousTemperature(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	disk := unraidDisk{id: "serial", name: "disk1", device: "sda"}
	tracker := make(diskStateTracker)
	tracker.apply([]diskObservation{{disk: disk, temperature: 35, source: diskSourceEmhttpd}}, now, time.Minute)
	readings := tracker.apply([]diskObservation{{disk: disk, source: diskSourceEmhttpd, err: errors.New("invalid cache")}}, now.Add(time.Second), time.Minute)
	if len(readings) != 1 || readings[0].Temp != 0 || !readings[0].Unavailable || tracker["serial"].thermalState != diskThermalUnavailable {
		t.Fatalf("invalid active reading = %#v; state=%#v", readings, tracker["serial"])
	}
}

func TestWakeGraceReturnsToStandbyAndRestartsOnNextWake(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	disk := unraidDisk{id: "serial", name: "disk1", device: "sda"}
	tracker := make(diskStateTracker)
	tracker.apply([]diskObservation{{disk: disk, standby: true, source: diskSourceEmhttpd}}, now, time.Minute)
	tracker.apply([]diskObservation{{disk: disk, source: diskSourceEmhttpd, err: errors.New("temperature pending")}}, now.Add(time.Second), time.Minute)
	firstWake := tracker["serial"].wakeStartedAt
	readings := tracker.apply([]diskObservation{{disk: disk, standby: true, source: diskSourceEmhttpd}}, now.Add(2*time.Second), time.Minute)
	if readings[0].Temp != 0 || readings[0].Unavailable || tracker["serial"].thermalState != diskThermalStandby || !tracker["serial"].wakeStartedAt.IsZero() {
		t.Fatalf("returned standby reading = %#v; state=%#v", readings, tracker["serial"])
	}
	tracker.apply([]diskObservation{{disk: disk, source: diskSourceEmhttpd, err: errors.New("temperature pending")}}, now.Add(3*time.Second), time.Minute)
	if state := tracker["serial"]; state.thermalState != diskThermalWaking || !state.wakeStartedAt.After(firstWake) {
		t.Fatalf("second wake state = %#v", state)
	}
}

func TestWakeStateDoesNotTransferToReplacement(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	oldDisk := unraidDisk{id: "old", name: "disk1", device: "sda"}
	newDisk := unraidDisk{id: "new", name: "disk1", device: "sda"}
	tracker := make(diskStateTracker)
	tracker.apply([]diskObservation{{disk: oldDisk, standby: true, source: diskSourceEmhttpd}}, now, time.Minute)
	tracker.apply([]diskObservation{{disk: oldDisk, source: diskSourceEmhttpd, err: errors.New("temperature pending")}}, now.Add(time.Second), time.Minute)
	readings := tracker.apply([]diskObservation{{disk: newDisk, source: diskSourceEmhttpd, err: errors.New("temperature pending")}}, now.Add(2*time.Second), time.Minute)
	if _, exists := tracker["old"]; exists || tracker["new"].thermalState != diskThermalUnavailable ||
		len(readings) != 1 || !readings[0].Unavailable {
		t.Fatalf("replacement reading = %#v; states=%#v", readings, tracker)
	}
}

func TestInventoryFailureBreaksStandbyWakeContinuity(t *testing.T) {
	for _, scenario := range []struct {
		name            string
		spundown        string
		temperature     string
		wantTemperature float64
		wantUnavailable bool
		wantState       diskThermalState
		thenFresh       bool
	}{
		{
			name: "active without temperature", spundown: "0", temperature: "*",
			wantUnavailable: true, wantState: diskThermalUnavailable, thenFresh: true,
		},
		{
			name: "active with fresh temperature", spundown: "0", temperature: "28",
			wantTemperature: 28, wantState: diskThermalValid,
		},
		{
			name: "still standby", spundown: "1", temperature: "*",
			wantState: diskThermalStandby,
		},
	} {
		t.Run(scenario.name, func(t *testing.T) {
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

			writeAssigned("0", "35")
			environment.report(t, "disk1", environment.now)
			collector := environment.collector()
			collector.refresh()
			writeAssigned("1", "*")
			collector.refresh()
			beforeFailure := collector.state["serial"]
			if beforeFailure.thermalState != diskThermalStandby {
				t.Fatalf("precondition state = %#v", beforeFailure)
			}

			if err := os.Remove(environment.paths.disksINI); err != nil {
				t.Fatal(err)
			}
			collector.refresh()
			lost := collector.state["serial"]
			if lost.thermalState != diskThermalUnavailable || !lost.wakeStartedAt.IsZero() ||
				!lost.lastValidAt.Equal(beforeFailure.lastValidAt) || lost.lastSource != beforeFailure.lastSource {
				t.Fatalf("state after inventory failure = %#v; before=%#v", lost, beforeFailure)
			}
			if readings, err := collector.snapshot(); err == nil || readings != nil {
				t.Fatalf("inventory failure snapshot = %#v, %v", readings, err)
			}

			environment.now = environment.now.Add(2 * time.Hour)
			collector.noteEmhttpPoll()
			collector.refresh()
			writeAssigned(scenario.spundown, scenario.temperature)
			if scenario.temperature == "28" {
				environment.report(t, "disk1", environment.now)
			}
			collector.refresh()
			disk := requireSingleDisk(t, collector)
			if disk.temp != scenario.wantTemperature || disk.unavailable != scenario.wantUnavailable ||
				collector.state["serial"].thermalState != scenario.wantState {
				t.Fatalf("reading after inventory recovery = %#v; state=%#v", disk, collector.state["serial"])
			}

			if scenario.thenFresh {
				writeAssigned("0", "28")
				environment.report(t, "disk1", environment.now)
				collector.refresh()
				disk = requireSingleDisk(t, collector)
				if disk.temp != 28 || disk.unavailable || collector.state["serial"].thermalState != diskThermalValid {
					t.Fatalf("fresh reading after unavailable = %#v; state=%#v", disk, collector.state["serial"])
				}
			}
		})
	}
}

func TestInventoryFailureDoesNotPublishStaleDiskAfterRecovery(t *testing.T) {
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
