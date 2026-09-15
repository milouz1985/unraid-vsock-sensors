// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"unraid-vsock-sensors/internal/sensors"
)

func TestDiagnosticsSnapshotStatesAndNoCollectorMutation(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	disks := newDiskCollector(diskDataPaths{})
	disks.mu.Lock()
	disks.err = nil
	disks.updatedAt = now
	disks.diagnosticReady = true
	disks.diagnosticPoll = 30 * time.Second
	disks.diagnosticDisks = []diagnosticDisk{{ID: "serial", Name: "disk1", Status: "valid", Source: "emhttpd cache", LastValidAt: timePointer(now.Add(-12 * time.Second))}}
	disks.mu.Unlock()
	disks.now = func() time.Time { return now.Add(-2 * time.Second) }
	disks.noteEmhttpPoll()
	hbas := newConfiguredHBACollector(15*time.Second, hbaModeDisabled, hbaBackendMPT3CTL)
	state := newDiagnosticsState(990, hbaBackendMPT3CTL)
	before := slicesOfDiagnosticDisks(disks)
	snapshot := state.snapshot(disks, hbas, now)
	if snapshot.Emhttpd.Status != "healthy" || snapshot.Emhttpd.TemperatureSource != "emhttpd cache" || snapshot.Emhttpd.LastPollAt == nil {
		t.Fatalf("unexpected emhttpd state: %+v", snapshot.Emhttpd)
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
	if snapshot.HBA.Status != "disabled" || snapshot.HBA.LastError != "" {
		t.Fatalf("disabled HBA treated as error: %+v", snapshot.HBA)
	}
	if !reflect.DeepEqual(before, slicesOfDiagnosticDisks(disks)) {
		t.Fatal("diagnostic read mutated disk collector")
	}
	data, err := json.Marshal(snapshot)
	if err != nil || !json.Valid(data) {
		t.Fatalf("invalid JSON: %v", err)
	}

	disks.mu.Lock()
	disks.diagnosticFallback = true
	disks.diagnosticFallbackSince = now.Add(-time.Minute)
	disks.diagnosticAttempt = now.Add(-5 * time.Second)
	disks.diagnosticFallbackError = "SMART failed"
	disks.err = errors.New("inventory failed")
	disks.diagnosticDisks[0].Status = "unavailable"
	disks.mu.Unlock()
	snapshot = state.snapshot(disks, hbas, now)
	if snapshot.Emhttpd.Status != "stale" || snapshot.Emhttpd.TemperatureSource != "direct SMART fallback" || snapshot.Emhttpd.LastFallbackAttemptAgeSeconds == nil || *snapshot.Emhttpd.LastFallbackAttemptAgeSeconds != 5 {
		t.Fatalf("unexpected fallback state: %+v", snapshot.Emhttpd)
	}
	if snapshot.Disks.Status != "error" || snapshot.Disks.Error != "inventory failed" || snapshot.Emhttpd.FallbackError != "SMART failed" {
		t.Fatalf("errors not separated: %+v %+v", snapshot.Disks, snapshot.Emhttpd)
	}
	disks.mu.Lock()
	disks.err = nil
	disks.diagnosticFallback = false
	disks.diagnosticPoll = 0
	disks.mu.Unlock()
	snapshot = state.snapshot(disks, hbas, now)
	if snapshot.Emhttpd.Status != "polling disabled" || snapshot.Emhttpd.StaleAfter != "disabled" {
		t.Fatalf("disabled polling has a stale threshold: %+v", snapshot.Emhttpd)
	}
}

func slicesOfDiagnosticDisks(c *diskCollector) []diagnosticDisk {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return append([]diagnosticDisk(nil), c.diagnosticDisks...)
}

func TestDiagnosticsVSOCKAndHBAError(t *testing.T) {
	disks := newDiskCollector(diskDataPaths{})
	hbas := newConfiguredHBACollector(30*time.Second, hbaModeEnabled, hbaBackendStorCLI)
	hbas.mu.Lock()
	hbas.err = errors.New("backend unavailable")
	hbas.lastErrorAt = time.Now()
	hbas.lastSuccessfulAt = time.Now().Add(-time.Minute)
	hbas.diagnosticReadings = []sensors.HBA{{ID: "sas:1", Temp: 45}}
	hbas.mu.Unlock()
	state := newDiagnosticsState(991, hbaBackendStorCLI)
	state.connectedNow()
	state.publishedNow()
	snapshot := state.snapshot(disks, hbas, time.Now())
	if snapshot.VSOCK.Status != "connected" || snapshot.VSOCK.LastConnectedAt == nil || snapshot.VSOCK.LastPublishedAt == nil || snapshot.VSOCK.LastPublishedAgeSeconds == nil {
		t.Fatalf("missing successful VSOCK events: %+v", snapshot.VSOCK)
	}
	if snapshot.HBA.Status != "error" || snapshot.HBA.LastError != "backend unavailable" || snapshot.HBA.LastErrorAt == nil || snapshot.HBA.Count != 1 {
		t.Fatalf("missing HBA error: %+v", snapshot.HBA)
	}
	state.disconnected(errors.New("write failed"))
	snapshot = state.snapshot(disks, hbas, time.Now())
	if snapshot.VSOCK.Status != "reconnecting" || snapshot.VSOCK.LastError != "write failed" || snapshot.VSOCK.LastPublishedAt == nil {
		t.Fatalf("missing VSOCK failure or prior publication: %+v", snapshot.VSOCK)
	}
	state.connectedNow()
	if got := state.snapshot(disks, hbas, time.Now()).VSOCK.LastError; got != "" {
		t.Fatalf("error not cleared: %q", got)
	}
}

func TestReadDiagnosticsRuntimeFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runtime", "diagnostics.json")
	now := time.Now()
	state := newDiagnosticsState(990, hbaBackendMPT3CTL)
	snapshot := state.snapshot(newDiskCollector(diskDataPaths{}), newConfiguredHBACollector(15*time.Second, hbaModeDisabled, hbaBackendMPT3CTL), now)
	if err := writeDiagnosticsAtomic(path, snapshot); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0644 {
		t.Fatalf("unexpected file mode: %v, %v", info, err)
	}
	read, err := readDiagnostics(path, now)
	if err != nil || read.PID != os.Getpid() {
		t.Fatalf("read valid snapshot: %+v, %v", read, err)
	}
	if _, err := readDiagnostics(path, now.Add(diagnosticsMaxAge+time.Second)); err == nil || !strings.Contains(err.Error(), "stale") {
		t.Fatalf("expected stale, got %v", err)
	}
	snapshot.PID = 99999999
	if err := writeDiagnosticsAtomic(path, snapshot); err != nil {
		t.Fatal(err)
	}
	if _, err := readDiagnostics(path, now); err == nil || !strings.Contains(err.Error(), "stopped") {
		t.Fatalf("expected stopped, got %v", err)
	}
	if err := os.WriteFile(path, []byte("{"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := readDiagnostics(path, now); err == nil || !strings.Contains(err.Error(), "invalid") {
		t.Fatalf("expected invalid, got %v", err)
	}
	if _, err := readDiagnostics(path+".absent", now); err == nil || !strings.Contains(err.Error(), "absent") {
		t.Fatalf("expected absent, got %v", err)
	}
}

func TestBuildDiagnosticDisksRetainedAndStandby(t *testing.T) {
	now := time.Now()
	state := diskStateTracker{"one": {hasValid: true, lastValidAt: now.Add(-30 * time.Second), lastSource: "direct SMART fallback"}}
	disks := []unraidDisk{{id: "one", name: "disk1", device: "sda", smartName: "disk1"}, {id: "two", name: "disk2", device: "sdb", spundown: true}}
	readings := []sensors.Disk{{ID: "one", Temp: 30}, {ID: "two", Temp: 0}}
	items := buildDiagnosticDisks(disks, readings, nil, state, true, true)
	if items[0].Status != "retained" || items[0].Source != "direct SMART fallback" || items[0].LastValidAt == nil {
		t.Fatalf("retained sample: %+v", items[0])
	}
	observations := []diskObservation{{disk: disks[0]}, {disk: disks[1], standby: true}}
	items = buildDiagnosticDisks(disks, readings, observations, state, true, false)
	if items[1].Status != "standby" || items[1].Temperature != nil {
		t.Fatalf("standby sample: %+v", items[1])
	}
}
