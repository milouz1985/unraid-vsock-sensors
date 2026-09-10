// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"bytes"
	"context"
	"errors"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

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
			smartDir: filepath.Join(root, "smart"), diskConfig: filepath.Join(root, "disk.cfg"),
		},
		now: time.Unix(1_800_000_000, 0),
	}
	if err := os.Mkdir(environment.paths.smartDir, 0700); err != nil {
		t.Fatal(err)
	}
	environment.write(t, environment.paths.disksINI, "[flash]\ndevice=sda\n")
	environment.write(t, environment.paths.devsINI, "")
	environment.write(t, environment.paths.diskConfig, "poll_attributes=\""+pollAttributes+"\"\n")
	return environment
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

func TestInvalidPollAttributesUsesDocumentedFallback(t *testing.T) {
	path := filepath.Join(t.TempDir(), "disk.cfg")
	if err := os.WriteFile(path, []byte("poll_attributes=\"legacy\"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cache := diskConfigCache{path: path}
	interval, changed, err := cache.load()
	if interval != defaultPollAttributes || !changed || err == nil {
		t.Fatalf("load = %s, changed=%v, err=%v", interval, changed, err)
	}
	if _, changed, _ := cache.load(); changed {
		t.Fatal("unchanged disk.cfg was reparsed")
	}
}

func TestDiskConfigCacheReloadsOnlyAfterFileChange(t *testing.T) {
	path := filepath.Join(t.TempDir(), "disk.cfg")
	mtime := time.Unix(1_800_000_000, 0)
	write := func(value string, timestamp time.Time) {
		t.Helper()
		if err := os.WriteFile(path, []byte("poll_attributes=\""+value+"\"\n"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, timestamp, timestamp); err != nil {
			t.Fatal(err)
		}
	}
	write("30", mtime)
	cache := diskConfigCache{path: path}
	if interval, changed, err := cache.load(); interval != 30*time.Second || !changed || err != nil {
		t.Fatalf("initial load = %s, changed=%v, err=%v", interval, changed, err)
	}
	if _, changed, _ := cache.load(); changed {
		t.Fatal("unchanged disk.cfg was reparsed")
	}

	write("60", mtime.Add(time.Second))
	if interval, changed, err := cache.load(); interval != 60*time.Second || !changed || err != nil {
		t.Fatalf("reloaded config = %s, changed=%v, err=%v", interval, changed, err)
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
	disks, err := readDiskInventory(environment.paths.disksINI, environment.paths.devsINI)
	if err != nil || len(disks) != 1 || disks[0].name != "disk1" || disks[0].smartName != "disk1" {
		t.Fatalf("deduplicated inventory = %#v, %v", disks, err)
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
	disks, err := readDisks(environment.paths.disksINI)
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
