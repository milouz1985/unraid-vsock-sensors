// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"unraid-vsock-sensors/internal/sensors"
)

func TestDiagnosticsSnapshotStatesAndNoRuntimeMutation(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	service := newServiceState(990).status()
	disks := diskCollectorStatus{
		updatedAt:   now,
		policyError: "invalid disk policy for ID \"serial\"",
		source: smartSourceStatus{
			initialized: true, pollInterval: 30 * time.Second, heartbeatSeen: true,
			lastHeartbeat: now.Add(-2 * time.Second), source: diskSourceEmhttpd,
		},
		disks: []diskRuntimeDisk{{
			disk:    unraidDisk{id: "serial", name: "disk1"},
			reading: sensors.Disk{ID: "serial", Name: "disk1", Temp: 35}, hasReading: true,
			state: diskState{thermalState: diskThermalValid, lastValidAt: now.Add(-12 * time.Second), lastSource: diskSourceEmhttpd},
		}},
	}
	hbas := hbaCollectorStatus{interval: 15 * time.Second, mode: hbaModeDisabled, backend: hbaBackendMPT3CTL}
	before := append([]diskRuntimeDisk(nil), disks.disks...)
	snapshot := buildDiagnosticsSnapshot(service, disks, hbas, now)
	if snapshot.Emhttpd.Status != "healthy" || snapshot.Emhttpd.TemperatureSource != "emhttpd" || snapshot.Emhttpd.LastPollAt == nil {
		t.Fatalf("unexpected emhttpd state: %+v", snapshot.Emhttpd)
	}
	if snapshot.Disks.Items[0].Source != "emhttpd" {
		t.Fatalf("unexpected disk source: %+v", snapshot.Disks.Items[0])
	}
	if snapshot.Emhttpd.LastPollAgeSeconds == nil || *snapshot.Emhttpd.LastPollAgeSeconds != 2 {
		t.Fatalf("unexpected heartbeat age: %+v", snapshot.Emhttpd)
	}
	if snapshot.Config.PollAttributes != "30s" || snapshot.Config.EmhttpdStaleAfter != "45s" {
		t.Fatalf("unexpected effective intervals: %+v", snapshot.Config)
	}
	if snapshot.Disks.Items[0].LastValidAgeSeconds == nil || *snapshot.Disks.Items[0].LastValidAgeSeconds != 12 {
		t.Fatalf("unexpected sample age: %+v", snapshot.Disks.Items[0])
	}
	if snapshot.Disks.PolicyError != disks.policyError {
		t.Fatalf("missing disk policy error: %+v", snapshot.Disks)
	}
	if snapshot.HBA.Status != "disabled" || snapshot.HBA.LastError != "" {
		t.Fatalf("disabled HBA treated as error: %+v", snapshot.HBA)
	}
	if !reflect.DeepEqual(before, disks.disks) {
		t.Fatal("diagnostic conversion mutated disk runtime state")
	}
	data, err := json.Marshal(snapshot)
	if err != nil || !json.Valid(data) {
		t.Fatalf("invalid JSON: %v", err)
	}

	disks.source.source = diskSourceDirect
	disks.source.fallbackSince = now.Add(-time.Minute)
	disks.source.lastFallbackAttempt = now.Add(-5 * time.Second)
	disks.source.lastFallbackError = "SMART failed"
	disks.err = errors.New("inventory failed")
	disks.disks[0].reading.Unavailable = true
	disks.disks[0].state.thermalState = diskThermalUnavailable
	snapshot = buildDiagnosticsSnapshot(service, disks, hbas, now)
	if snapshot.Emhttpd.Status != "stale" || snapshot.Emhttpd.TemperatureSource != "direct SMART fallback" || snapshot.Emhttpd.LastFallbackAttemptAgeSeconds == nil || *snapshot.Emhttpd.LastFallbackAttemptAgeSeconds != 5 {
		t.Fatalf("unexpected fallback state: %+v", snapshot.Emhttpd)
	}
	if snapshot.Disks.Status != "error" || snapshot.Disks.Error != "inventory failed" || snapshot.Emhttpd.FallbackError != "SMART failed" {
		t.Fatalf("errors not separated: %+v %+v", snapshot.Disks, snapshot.Emhttpd)
	}
	disks.err = nil
	disks.source.source = diskSourceEmhttpd
	disks.source.pollInterval = 0
	snapshot = buildDiagnosticsSnapshot(service, disks, hbas, now)
	if snapshot.Emhttpd.Status != "polling disabled" || snapshot.Emhttpd.StaleAfter != "disabled" {
		t.Fatalf("disabled polling has a stale threshold: %+v", snapshot.Emhttpd)
	}
}

func TestCollectorStatusReturnsIndependentCopies(t *testing.T) {
	disks := newDiskCollector(diskDataPaths{})
	disks.lastSuccessfulSnapshot = []diskRuntimeDisk{{disk: unraidDisk{id: "disk-id"}}}
	diskStatus := disks.status()
	diskStatus.disks[0].disk.id = "changed"
	if got := disks.status().disks[0].disk.id; got != "disk-id" {
		t.Fatalf("disk status mutated collector: %q", got)
	}

	hbas := newConfiguredHBACollector(time.Minute, hbaModeEnabled, hbaBackendMPT3CTL)
	hbas.lastSuccessfulSnapshot = []sensors.HBA{{ID: "sas:1", Temp: 45}}
	hbaStatus := hbas.status()
	hbaStatus.lastSuccessfulSnapshot[0].Temp = 99
	if got := hbas.status().lastSuccessfulSnapshot[0].Temp; got != 45 {
		t.Fatalf("HBA status mutated collector: %v", got)
	}
}

func TestDiagnosticsVSOCKAndHBAError(t *testing.T) {
	service := newServiceState(991)
	service.connectedNow()
	service.publishedNow()
	hbas := hbaCollectorStatus{
		interval: 30 * time.Second, mode: hbaModeEnabled, backend: hbaBackendStorCLI,
		err: errors.New("backend unavailable"), lastErrorAt: time.Now(),
		lastSuccessfulAt:       time.Now().Add(-time.Minute),
		lastSuccessfulSnapshot: []sensors.HBA{{ID: "sas:1", Temp: 45}},
	}
	snapshot := buildDiagnosticsSnapshot(service.status(), newDiskCollector(diskDataPaths{}).status(), hbas, time.Now())
	if snapshot.VSOCK.Status != "connected" || snapshot.VSOCK.LastConnectedAt == nil || snapshot.VSOCK.LastPublishedAt == nil || snapshot.VSOCK.LastPublishedAgeSeconds == nil {
		t.Fatalf("missing successful VSOCK events: %+v", snapshot.VSOCK)
	}
	if snapshot.HBA.Status != "error" || snapshot.HBA.LastError != "backend unavailable" || snapshot.HBA.LastErrorAt == nil || snapshot.HBA.Count != 1 {
		t.Fatalf("missing HBA error: %+v", snapshot.HBA)
	}
	service.disconnected(errors.New("write failed"))
	snapshot = buildDiagnosticsSnapshot(service.status(), newDiskCollector(diskDataPaths{}).status(), hbas, time.Now())
	if snapshot.VSOCK.Status != "reconnecting" || snapshot.VSOCK.LastError != "write failed" || snapshot.VSOCK.LastPublishedAt == nil {
		t.Fatalf("missing VSOCK failure or prior publication: %+v", snapshot.VSOCK)
	}
	service.connectedNow()
	if got := buildDiagnosticsSnapshot(service.status(), newDiskCollector(diskDataPaths{}).status(), hbas, time.Now()).VSOCK.LastError; got != "" {
		t.Fatalf("error not cleared: %q", got)
	}
}

func TestWriteDiagnosticsRuntimeFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runtime", "diagnostics.json")
	now := time.Now()
	snapshot := buildDiagnosticsSnapshot(
		newServiceState(990).status(), newDiskCollector(diskDataPaths{}).status(),
		newConfiguredHBACollector(15*time.Second, hbaModeDisabled, hbaBackendMPT3CTL).status(), now,
	)
	if err := writeDiagnosticsAtomic(path, snapshot); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0644 {
		t.Fatalf("unexpected file mode: %v, %v", info, err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var decoded diagnosticsSnapshot
	if err := json.Unmarshal(data, &decoded); err != nil || decoded.PID != os.Getpid() || decoded.SchemaVersion != 1 {
		t.Fatalf("written snapshot = %+v, %v", decoded, err)
	}
}

func TestBuildDiagnosticDisksUsesStableIDs(t *testing.T) {
	now := time.Now()
	disks := []unraidDisk{
		{id: "one", name: "disk1", device: "sda"},
		{id: "two", name: "disk2", device: "sdb", spundown: true},
	}
	readings := []sensors.Disk{
		{ID: "two", Temp: 99}, // Deliberately reversed: must not leak to disk one.
		{ID: "one", Temp: 30},
	}
	observations := []diskObservation{
		{disk: disks[1], standby: true, source: diskSourceDirect},
		{disk: disks[0], source: diskSourceDirect},
	}
	collector := &diskCollector{
		state: diskStateTracker{
			"one": {thermalState: diskThermalValid, lastValidAt: now.Add(-30 * time.Second), lastSource: diskSourceDirect},
			"two": {thermalState: diskThermalStandby},
		},
	}
	runtime := collector.buildDiskRuntimeSnapshot(disks, readings, observations, false)
	items := buildDiagnosticDisks(runtime)
	if items[0].Temperature == nil || *items[0].Temperature != 30 || items[0].Source != "direct SMART fallback" {
		t.Fatalf("disk one received the wrong reading: %+v", items[0])
	}
	if items[1].Status != "standby" || items[1].Temperature != nil {
		t.Fatalf("standby sample: %+v", items[1])
	}
}

func TestBuildDiagnosticDisksHidesSyntheticTemperatures(t *testing.T) {
	disks := []diskRuntimeDisk{
		{
			disk:       unraidDisk{id: "standby", name: "disk1"},
			reading:    sensors.Disk{ID: "standby", Temp: 0},
			hasReading: true,
			state:      diskState{thermalState: diskThermalStandby},
		},
		{
			disk:            unraidDisk{id: "waking", name: "disk2"},
			reading:         sensors.Disk{ID: "waking", Temp: 0},
			hasReading:      true,
			collectionError: errors.New("temperature pending"),
			state:           diskState{thermalState: diskThermalWaking},
		},
		{
			disk:       unraidDisk{id: "zero", name: "disk3"},
			reading:    sensors.Disk{ID: "zero", Temp: 0},
			hasReading: true,
			state:      diskState{thermalState: diskThermalValid},
		},
	}
	items := buildDiagnosticDisks(disks)
	if items[0].Status != diagnosticDiskStandby || items[0].Temperature != nil {
		t.Fatalf("standby diagnostic = %+v", items[0])
	}
	if items[1].Status != diagnosticDiskWaking || items[1].Temperature != nil || items[1].Error == "" {
		t.Fatalf("waking diagnostic = %+v", items[1])
	}
	if items[2].Status != diagnosticDiskValid || items[2].Temperature == nil || *items[2].Temperature != 0 {
		t.Fatalf("valid zero-degree diagnostic = %+v", items[2])
	}
}

func TestBuildDiagnosticDisksPreservesUncollectedNil(t *testing.T) {
	if items := buildDiagnosticDisks(nil); items != nil {
		t.Fatalf("uncollected diagnostics items = %#v; want nil", items)
	}
	if items := buildDiagnosticDisks([]diskRuntimeDisk{}); items == nil {
		t.Fatal("successful empty inventory was reported as uncollected")
	}
}
