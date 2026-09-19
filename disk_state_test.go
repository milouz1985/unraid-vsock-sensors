// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"unraid-vsock-sensors/internal/sensors"
)

func TestDiskStateReadingsMatchThermalState(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	disk := unraidDisk{id: "serial", name: "disk1", device: "sda"}
	collectionErr := errors.New("temperature unavailable")

	for _, test := range []struct {
		name            string
		initialState    diskState
		observation     diskObservation
		wantState       diskThermalState
		wantTemperature float64
		wantUnavailable bool
	}{
		{
			name:            "valid",
			observation:     diskObservation{disk: disk, temperature: 37, source: diskSourceDirect},
			wantState:       diskThermalValid,
			wantTemperature: 37,
		},
		{
			name:        "standby",
			observation: diskObservation{disk: disk, standby: true, source: diskSourceEmhttpd},
			wantState:   diskThermalStandby,
		},
		{
			name:         "waking",
			initialState: diskState{thermalState: diskThermalStandby},
			observation:  diskObservation{disk: disk, source: diskSourceEmhttpd, err: collectionErr},
			wantState:    diskThermalWaking,
		},
		{
			name:            "unavailable",
			observation:     diskObservation{disk: disk, source: diskSourceDirect, err: collectionErr},
			wantState:       diskThermalUnavailable,
			wantUnavailable: true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			tracker := diskStateTracker{disk.id: test.initialState}
			readings := tracker.apply([]diskObservation{test.observation}, now, time.Minute)
			if len(readings) != 1 {
				t.Fatalf("readings = %#v; want one reading", readings)
			}
			if got := tracker[disk.id].thermalState; got != test.wantState {
				t.Fatalf("thermal state = %v; want %v", got, test.wantState)
			}
			if reading := readings[0]; reading.Temp != test.wantTemperature || reading.Unavailable != test.wantUnavailable {
				t.Fatalf("reading = %#v; want temperature %v, unavailable=%v",
					reading, test.wantTemperature, test.wantUnavailable)
			}
		})
	}
}

func TestDiskSnapshotUsesRuntimeOrderAndReturnsCopy(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	collector := newDiskCollector(diskDataPaths{})
	collector.now = func() time.Time { return now }
	collector.err = nil
	collector.updatedAt = now
	collector.lastSuccessfulSnapshot = []diskRuntimeDisk{
		{reading: sensors.Disk{ID: "second", Temp: 52}},
		{reading: sensors.Disk{ID: "first", Temp: 41}},
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

func TestAssignedDiskUsesUnraidTemperatureAndStableIdentity(t *testing.T) {
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

	collector := environment.collector()
	collector.refresh()
	disk := requireSingleDisk(t, collector)
	if disk.id != "WDC_stable_serial" || disk.name != "disk1" || disk.device != "sda" ||
		disk.temp != 35 || disk.unavailable {
		t.Fatalf("assigned disk = %#v", disk)
	}
}

func TestUnassignedDiskKeepsStableIdentityAcrossDeviceChange(t *testing.T) {
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

	collector := environment.collector()
	collector.refresh()
	first := requireSingleDisk(t, collector)
	if first.id != "TOSHIBA_stable_serial" || first.device != "sda" || first.temp != 42 || first.unavailable {
		t.Fatalf("unassigned disk = %#v", first)
	}

	writeDevice("sdb")
	collector.refresh()
	second := requireSingleDisk(t, collector)
	if second.id != first.id || second.device != "sdb" || second.temp != 42 || second.unavailable {
		t.Fatalf("unassigned disk after sdX change = %#v", second)
	}
}

func TestEmhttpdTemperatureIsAuthoritativeWhenHeartbeatHealthy(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	observations := makeDiskObservations([]unraidDisk{{
		id: "serial", name: "disk1", device: "sda", temperature: "35",
	}}, true)
	tracker := make(diskStateTracker)
	tracker.apply(observations, now, time.Minute)
	if state := tracker["serial"]; state.thermalState != diskThermalValid || !state.lastValidAt.Equal(now) {
		t.Fatalf("emhttpd reading = %#v; want valid with now", state)
	}
}

func TestActiveDiskRejectsInvalidTemperature(t *testing.T) {
	for name, temperature := range map[string]string{
		"missing temperature": "",
		"asterisk":            "*",
		"not numeric":         "warm",
	} {
		t.Run(name, func(t *testing.T) {
			now := time.Unix(1_800_000_000, 0)
			observations := makeDiskObservations([]unraidDisk{{
				id: "serial", name: "disk1", device: "sda", temperature: temperature,
			}}, true)
			readings := make(diskStateTracker).apply(observations, now, time.Minute)
			if len(readings) != 1 || !readings[0].Unavailable || readings[0].Temp != 0 {
				t.Fatalf("reading = %#v; want unavailable", readings)
			}
		})
	}
}

func TestEmhttpdTemperatureAllowsValuesOutsideTypicalSensorRange(t *testing.T) {
	for _, test := range []struct {
		raw  string
		want float64
	}{
		{raw: "0", want: 0},
		{raw: "-40.125", want: -40.125},
		{raw: "151.5", want: 151.5},
	} {
		got, err := parseEmhttpdTemperature(test.raw)
		if err != nil || got != test.want {
			t.Errorf("parseEmhttpdTemperature(%q) = %g, %v; want %g", test.raw, got, err, test.want)
		}
	}
}

func TestZeroEmhttpdTemperatureIsValidReading(t *testing.T) {
	environment := newDiskTestEnvironment(t, "30")
	environment.write(t, environment.paths.disksINI, "[disk1]\nid=serial\ndevice=sda\nrotational=1\nspundown=0\ntemp=0\n")
	collector := environment.collector()
	collector.refresh()
	disk := requireSingleDisk(t, collector)
	if disk.temp != 0 || disk.unavailable || collector.state["serial"].thermalState != diskThermalValid {
		t.Fatalf("zero-degree reading = %#v; state=%#v", disk, collector.state["serial"])
	}
}

func TestSleepingDiskReportsStandby(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	observations := makeDiskObservations([]unraidDisk{{
		id: "serial", name: "disk1", device: "sda",
		temperature: "*", spundown: true,
	}}, true)
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
	collector := environment.collector()
	collector.refresh()
	if disk := requireSingleDisk(t, collector); disk.temp != 35 || disk.unavailable {
		t.Fatalf("initial disk = %#v", disk)
	}

	writeAssigned("1", "*")
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
		t.Fatalf("invalid per-disk temperature changed healthy emhttpd source to %q", source)
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
	readings := tracker.apply([]diskObservation{{disk: disk, source: diskSourceEmhttpd, err: errors.New("invalid emhttpd temperature")}}, now.Add(time.Second), time.Minute)
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
			}
			collector.refresh()
			disk := requireSingleDisk(t, collector)
			if disk.temp != scenario.wantTemperature || disk.unavailable != scenario.wantUnavailable ||
				collector.state["serial"].thermalState != scenario.wantState {
				t.Fatalf("reading after inventory recovery = %#v; state=%#v", disk, collector.state["serial"])
			}

			if scenario.thenFresh {
				writeAssigned("0", "28")
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

	environment.now = environment.now.Add(time.Minute)
	collector.noteEmhttpPoll()
	environment.write(t, environment.paths.disksINI, inventory)
	collector.refresh()
	if disk := requireSingleDisk(t, collector); disk.temp != 35 || disk.unavailable {
		t.Fatalf("disk after inventory failure recovery = %#v", disk)
	}
}

func TestDiskSnapshotExpiryMatchesStaleStatus(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	collector := newDiskCollector(diskDataPaths{})
	collector.now = func() time.Time { return now }
	collector.err = nil
	collector.updatedAt = now
	collector.lastSuccessfulSnapshot = []diskRuntimeDisk{{reading: sensors.Disk{ID: "disk1", Temp: 35}}}

	// Fresh snapshot: both the VSOCK snapshot and the diagnostics status agree.
	if _, err := collector.snapshot(); err != nil {
		t.Fatalf("fresh snapshot = %v; want available", err)
	}
	if status := collector.status(); status.stale {
		t.Fatal("fresh snapshot reported as stale in status")
	}

	// Crossing the snapshot timeout expires the VSOCK snapshot, and the
	// diagnostics status must report the same expiry.
	now = now.Add(diskSnapshotTimeout + time.Millisecond)
	if _, err := collector.snapshot(); err == nil {
		t.Fatal("expired snapshot did not return an error")
	}
	if status := collector.status(); !status.stale {
		t.Fatal("expired snapshot not reported as stale in status")
	}
}
