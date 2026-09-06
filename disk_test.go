package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"unraid-vsock-sensors/internal/sensors"
)

func newDiskStateTracker() diskStateTracker {
	return make(diskStateTracker)
}

func TestDiskStateAllowsTransientSMARTFailure(t *testing.T) {
	disk := unraidDisk{id: "serial1", name: "disk1", device: "sdb", rotational: true}
	tracker := newDiskStateTracker()
	now := time.Unix(1000, 0)

	readings := tracker.apply([]unraidDisk{disk}, []diskProbe{{temperature: 35}}, now, 35*time.Second)
	if readings[0].Temp != 35 || readings[0].Unavailable {
		t.Fatalf("initial reading = %#v", readings)
	}
	readings = tracker.apply([]unraidDisk{disk}, []diskProbe{{err: errors.New("spin-up")}}, now, 35*time.Second)
	if readings[0].Temp != 35 || readings[0].Unavailable {
		t.Fatalf("failure grace reading = %#v", readings)
	}
	readings = tracker.apply([]unraidDisk{disk}, []diskProbe{{err: errors.New("still unavailable")}}, now.Add(35*time.Second), 35*time.Second)
	if !readings[0].Unavailable {
		t.Fatalf("expired failure reused stale temperature: %#v", readings)
	}
	readings = tracker.apply([]unraidDisk{disk}, []diskProbe{{temperature: 40}}, now.Add(36*time.Second), 35*time.Second)
	if readings[0].Temp != 40 || readings[0].Unavailable {
		t.Fatalf("recovered reading = %#v", readings)
	}
}

func TestDiskStateIsolatesTimedOutSMARTProbe(t *testing.T) {
	disks := []unraidDisk{
		{id: "serial1", name: "disk1", rotational: true},
		{id: "serial2", name: "disk2", rotational: true},
	}
	tracker := newDiskStateTracker()
	now := time.Unix(1000, 0)
	grace := 35 * time.Second

	tracker.apply(disks, []diskProbe{{temperature: 35}, {temperature: 40}}, now, grace)
	readings := tracker.apply(disks, []diskProbe{
		{err: context.DeadlineExceeded},
		{temperature: 42},
	}, now.Add(30*time.Second), grace)
	if readings[0].Temp != 35 || readings[0].Unavailable {
		t.Fatalf("timed out disk during grace = %#v", readings[0])
	}
	if readings[1].Temp != 42 || readings[1].Unavailable {
		t.Fatalf("successful disk affected by peer timeout = %#v", readings[1])
	}

	readings = tracker.apply(disks, []diskProbe{
		{err: context.DeadlineExceeded},
		{temperature: 43},
	}, now.Add(65*time.Second), grace)
	if !readings[0].Unavailable {
		t.Fatalf("timed out disk did not expire = %#v", readings[0])
	}
	if readings[1].Temp != 43 || readings[1].Unavailable {
		t.Fatalf("successful disk affected after peer grace = %#v", readings[1])
	}
}

func TestDiskStateGraceWithoutPreviousTemperatureAvoidsFailsafe(t *testing.T) {
	tracker := newDiskStateTracker()
	disk := unraidDisk{id: "serial1", name: "disk1", rotational: true}
	readings := tracker.apply(
		[]unraidDisk{disk}, []diskProbe{{err: errors.New("spin-up")}}, time.Now(), time.Minute,
	)
	if len(readings) != 1 || readings[0].Temp != 0 || readings[0].Unavailable {
		t.Fatalf("initial grace reading = %#v", readings)
	}
	if samples := makeDiskSamples(sensors.Response{Disks: readings}); len(samples) != 1 || samples[0].temperature == hwmonFailsafeTemp {
		t.Fatalf("initial grace triggered hwmon failsafe: %#v", samples)
	}
}

func TestDiskStateReportsStandbyAsZero(t *testing.T) {
	tracker := newDiskStateTracker()
	disk := unraidDisk{id: "serial1", name: "disk1", rotational: true, spundown: true}
	readings := tracker.apply([]unraidDisk{disk}, []diskProbe{{standby: true}}, time.Now(), time.Minute)
	if len(readings) != 1 || readings[0].Temp != 0 || readings[0].Unavailable {
		t.Fatalf("standby reading = %#v", readings)
	}
}

func TestCollectDiskProbesSkipsKnownStandbyDisk(t *testing.T) {
	probes := collectDiskProbes(context.Background(), []unraidDisk{{name: "disk1", spundown: true}})
	if len(probes) != 1 || !probes[0].standby || probes[0].err != nil {
		t.Fatalf("standby probe = %#v", probes)
	}
}

func TestDiskStatePurgesDisappearedDisks(t *testing.T) {
	tracker := newDiskStateTracker()
	disk := unraidDisk{id: "serial1", name: "disk1"}
	now := time.Unix(1000, 0)
	tracker.apply([]unraidDisk{disk}, []diskProbe{{temperature: 35}}, now, time.Minute)
	tracker.apply([]unraidDisk{disk}, []diskProbe{{err: errors.New("missing")}}, now, time.Minute)
	tracker.apply(nil, nil, now, time.Minute)
	if len(tracker) != 0 {
		t.Fatalf("state was not purged: %v", tracker)
	}

	readings := tracker.apply(
		[]unraidDisk{disk}, []diskProbe{{err: errors.New("missing")}}, now.Add(2*time.Minute), time.Minute,
	)
	if len(readings) != 1 || readings[0].Temp != 0 || readings[0].Unavailable {
		t.Fatalf("reappeared disk reused stale state: %#v", readings)
	}
}

func TestReadInventoryAndSelect(t *testing.T) {
	p := filepath.Join(t.TempDir(), "disks.ini")
	data := strings.ReplaceAll(strings.TrimSpace(`
		["disk1"]
		name="disk1"
		id="WDC_disk1_serial"
		device="sdb"
		temp="35"
		rotational="1"
		transport="ata"
		spundown="0"

		["fast"]
		id="NVME_fast_serial"
		device="nvme0n1"
		temp="48"
		rotational="0"
		transport="nvme"
		spundown="0"

		["disk2"]
		id="WDC_disk2_serial"
		device="sdc"
		temp="*"
		rotational="1"
		spundown="1"

		["disk13"]
		device=""
		status="DISK_NP"

		["flash"]
		device="sdi"
		transport="usb"
		rotational="1"
	`), "\n\t\t", "\n")
	if err := os.WriteFile(p, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	disks, err := readDisks(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(disks) != 3 || disks[0].id != "WDC_disk1_serial" || !disks[1].spundown {
		t.Fatalf("inventory = %#v", disks)
	}

	tracker := newDiskStateTracker()
	readings := tracker.apply(disks, []diskProbe{
		{temperature: 35}, {standby: true}, {temperature: 48},
	}, time.Now(), time.Minute)
	if got := selectDisks(readings, "hdd"); len(got) != 2 || got[0].Temp != 35 || got[1].Temp != 0 {
		t.Fatalf("hdd: %#v", got)
	}
	if got := selectDisks(readings, "nvme"); len(got) != 1 || got[0].Temp != 48 {
		t.Fatalf("nvme: %#v", got)
	}
	if got := selectDisks(readings, "fast"); len(got) != 1 || got[0].Device != "nvme0n1" {
		t.Fatalf("name: %#v", got)
	}
}

func TestParseSMARTTemperature(t *testing.T) {
	for name, test := range map[string]struct {
		json        string
		runErr      error
		wantTemp    float64
		wantStandby bool
		wantErr     bool
	}{
		"active": {
			json:     `{"smartctl":{"exit_status":0},"temperature":{"current":35}}`,
			wantTemp: 35,
		},
		"health warning still has valid temperature": {
			json:     `{"smartctl":{"exit_status":64},"temperature":{"current":41}}`,
			runErr:   &exec.ExitError{},
			wantTemp: 41,
		},
		"command failure rejects reported temperature": {
			json:    `{"smartctl":{"exit_status":4},"temperature":{"current":41}}`,
			runErr:  &exec.ExitError{},
			wantErr: true,
		},
		"standby": {
			json:        `{"smartctl":{"exit_status":3},"power_mode":{"name":"STANDBY"}}`,
			runErr:      &exec.ExitError{},
			wantStandby: true,
		},
		"ambiguous exit three": {
			json:    `{"smartctl":{"exit_status":3}}`,
			runErr:  &exec.ExitError{},
			wantErr: true,
		},
		"missing temperature": {
			json:    `{"smartctl":{"exit_status":0}}`,
			wantErr: true,
		},
		"missing exit status": {
			json:    `{"smartctl":{},"temperature":{"current":35}}`,
			wantErr: true,
		},
		"expired command with valid output": {
			json:    `{"smartctl":{"exit_status":0},"temperature":{"current":35}}`,
			runErr:  context.DeadlineExceeded,
			wantErr: true,
		},
		"invalid temperature": {
			json:    `{"smartctl":{"exit_status":0},"temperature":{"current":255}}`,
			wantErr: true,
		},
		"invalid JSON": {json: `{`, wantErr: true},
	} {
		t.Run(name, func(t *testing.T) {
			temperature, standby, err := parseSMARTTemperature("disk1", []byte(test.json), test.runErr)
			if temperature != test.wantTemp || standby != test.wantStandby || (err != nil) != test.wantErr {
				t.Fatalf("temperature/standby/error = %v/%v/%v; want %v/%v/error=%v", temperature, standby, err, test.wantTemp, test.wantStandby, test.wantErr)
			}
		})
	}
}

func TestDiskCollectorReportsExpiredValidity(t *testing.T) {
	collector := newDiskCollector("unused", time.Minute)
	collector.err = nil
	collector.readings = []sensors.Disk{{ID: "serial", Temp: 35}}
	collector.updatedAt = time.Now().Add(-collector.interval - diskCollectionTimeout)
	readings, err, _, validFor := collector.snapshot()
	if err != nil || len(readings) != 1 || validFor != 0 {
		t.Fatalf("expired snapshot = %#v, %v, valid for %s", readings, err, validFor)
	}
}

func TestDiskCollectorExpiresFailedReadingAtGraceDeadline(t *testing.T) {
	collector := newDiskCollector("unused", time.Minute)
	collector.err = nil
	collector.readings = []sensors.Disk{{ID: "serial", Temp: 35}}
	collector.updatedAt = time.Now()
	collector.state["serial"] = diskState{failedSince: time.Now().Add(-collector.grace - time.Second)}
	readings, err, _, _ := collector.snapshot()
	if err != nil || len(readings) != 1 || !readings[0].Unavailable || readings[0].Temp != 0 {
		t.Fatalf("expired failed reading = %#v, %v", readings, err)
	}
}

func TestSelectDisksExcludesExternalDisksFromKindSelectors(t *testing.T) {
	disks := []sensors.Disk{
		{Name: "internal", Device: "sdb", Transport: "ata", Rotational: true},
		{Name: "external", Device: "sdc", Transport: "usb", Rotational: true},
	}

	if got := selectDisks(disks, "hdd"); len(got) != 1 || got[0].Name != "internal" {
		t.Fatalf("hdd: %#v", got)
	}
	if got := selectDisks(disks, "external"); len(got) != 1 || got[0].Name != "external" {
		t.Fatalf("explicit name: %#v", got)
	}
	if got := selectDisks(disks, "all"); len(got) != 2 {
		t.Fatalf("all should include external disks: %#v", got)
	}
}
