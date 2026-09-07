package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
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

func TestReadInventory(t *testing.T) {
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
	if len(readings) != 3 || readings[0].Temp != 35 || readings[1].Temp != 0 || readings[2].Temp != 48 {
		t.Fatalf("readings: %#v", readings)
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

func TestDiskCollectorExpiresStaleSnapshot(t *testing.T) {
	collector := newDiskCollector("unused", time.Minute)
	collector.err = nil
	collector.readings = []sensors.Disk{{ID: "serial", Temp: 35}}
	collector.updatedAt = time.Now().Add(-collector.interval - diskCollectionTimeout)
	readings, err := collector.snapshot()
	if err == nil || len(readings) != 0 {
		t.Fatalf("expired snapshot = %#v, %v", readings, err)
	}
}

func TestBlockedDiskCollectionDoesNotStopSnapshotPublication(t *testing.T) {
	fifo := filepath.Join(t.TempDir(), "disks.ini")
	if err := syscall.Mkfifo(fifo, 0600); err != nil {
		t.Fatal(err)
	}
	collector := newDiskCollector(fifo, time.Minute)
	collector.err = nil
	collector.readings = []sensors.Disk{{ID: "serial", Name: "disk1", Temp: 35}}
	collector.updatedAt = time.Now()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	refreshDone := make(chan struct{})
	go func() {
		collector.refresh(ctx)
		close(refreshDone)
	}()

	type openResult struct {
		file *os.File
		err  error
	}
	opened := make(chan openResult, 1)
	go func() {
		file, err := os.OpenFile(fifo, os.O_WRONLY, 0)
		opened <- openResult{file: file, err: err}
	}()
	writer := <-opened
	if writer.err != nil {
		t.Fatal(writer.err)
	}

	frames := capturePublishedSnapshots(
		t,
		collector,
		newHBACollector(time.Minute, hbaModeDisabled),
		2,
	)
	for index, frame := range frames {
		if frame.Error != "" || len(frame.Disks) != 1 || frame.Disks[0].Temp != 35 {
			t.Fatalf("frame %d was blocked or lost cached disk data: %#v", index, frame)
		}
	}

	_, _ = writer.file.Write([]byte("[disk1]\nid=serial\ndevice=sda\n"))
	_ = writer.file.Close()
	select {
	case <-refreshDone:
	case <-time.After(time.Second):
		t.Fatal("blocked disk collection did not finish after releasing the FIFO")
	}
}

func TestDiskCollectorExpiresFailedReadingAtGraceDeadline(t *testing.T) {
	collector := newDiskCollector("unused", time.Minute)
	collector.err = nil
	collector.readings = []sensors.Disk{{ID: "serial", Temp: 35}}
	collector.updatedAt = time.Now()
	collector.state["serial"] = diskState{failedSince: time.Now().Add(-collector.grace - time.Second)}
	readings, err := collector.snapshot()
	if err != nil || len(readings) != 1 || !readings[0].Unavailable || readings[0].Temp != 0 {
		t.Fatalf("expired failed reading = %#v, %v", readings, err)
	}
}

func TestDiskCollectorConcurrentRefreshAndSnapshot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "disks.ini")
	data := "[disk1]\nid=serial\ndevice=sda\nrotational=1\ntransport=ata\nspundown=1\n"
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	collector := newDiskCollector(path, time.Minute)
	collector.refresh(context.Background())

	start := make(chan struct{})
	done := make(chan error, 2)
	go func() {
		<-start
		for range 100 {
			collector.refresh(context.Background())
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
