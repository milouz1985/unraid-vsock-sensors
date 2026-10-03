// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"unraid-vsock-sensors/internal/sensors"
)

type orchHarness struct {
	configRoot  string
	deviceRoot  string
	topologyDir string
	publisher   *hwmonPublisher
	snapshots   chan receivedSnapshot
	bgErrors    chan error
	result      chan error
}

func newOrchHarness(t *testing.T, diskSamples []hwmonSample) *orchHarness {
	t.Helper()
	root := t.TempDir()
	configRoot := filepath.Join(root, "config")
	deviceRoot := filepath.Join(root, "dev")
	topologyDir := filepath.Join(root, "run")
	if err := os.MkdirAll(configRoot, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(deviceRoot, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(topologyDir, 0700); err != nil {
		t.Fatal(err)
	}
	prepareFakeHWMonKernel(t, configRoot, deviceRoot, "disk", diskSamples)
	prepareFakeHWMonKernel(t, configRoot, deviceRoot, "hba", nil)

	publisher := &hwmonPublisher{cachePath: filepath.Join(root, "inventory.json")}
	return &orchHarness{
		configRoot:  configRoot,
		deviceRoot:  deviceRoot,
		topologyDir: topologyDir,
		publisher:   publisher,
		snapshots:   make(chan receivedSnapshot, 10),
		bgErrors:    make(chan error, 10),
		result:      make(chan error, 1),
	}
}

func (h *orchHarness) start(ctx context.Context) {
	publisher := h.publisher
	go func() {
		h.result <- hwmonOrchestrationLoop(
			ctx, h.snapshots, h.bgErrors, publisher,
			h.configRoot, h.deviceRoot,
			filepath.Join(h.topologyDir, "topology-changed"),
		)
	}()
}

func (h *orchHarness) stop(t *testing.T, wantErr error) {
	t.Helper()
	select {
	case err := <-h.result:
		if wantErr == nil {
			if err != nil {
				t.Fatalf("loop returned %v, want nil", err)
			}
		} else if !errors.Is(err, wantErr) {
			t.Fatalf("loop error = %v, want %v", err, wantErr)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("loop did not stop in time")
	}
}

func (h *orchHarness) readDevice(t *testing.T, sensorID string) string {
	t.Helper()
	path := hwmonTemperatureDevicePath(h.deviceRoot, sensorID)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read device %s: %v", sensorID, err)
	}
	return strings.TrimSpace(string(data))
}

func (h *orchHarness) topologyPath() string {
	return filepath.Join(h.topologyDir, "topology-changed")
}

func (h *orchHarness) topologyExists(t *testing.T) bool {
	t.Helper()
	_, err := os.Stat(h.topologyPath())
	if err == nil {
		return true
	}
	if os.IsNotExist(err) {
		return false
	}
	t.Fatalf("stat topology file: %v", err)
	return false
}

func (h *orchHarness) removeTopology(t *testing.T) {
	t.Helper()
	if err := os.Remove(h.topologyPath()); err != nil {
		t.Fatalf("remove topology file: %v", err)
	}
}

func diskSnap1(temp float64, receivedAt time.Time) receivedSnapshot {
	return receivedSnapshot{
		response: sensors.Response{
			Protocol: sensors.ProtocolVersion,
			Disks:    []sensors.Disk{{ID: "1", Name: "disk1", Device: "sda", Rotational: true, Temp: temp}},
			HBAs:     []sensors.HBA{},
		},
		receivedAt: receivedAt,
	}
}

// TestHWMonOrchRejectsStaleAndPublishesFresh verifies that a snapshot whose
// receivedAt is older than snapshotStreamTimeout is discarded by the loop
// (never applied to the device), while a subsequent fresh snapshot is
// published. It detects mutation A (expired() check removed).
//
// The stale snapshot uses a different disk ID than the fresh one, so the
// stale value cannot be hidden by the fresh publish overwriting the same
// device file.
func TestHWMonOrchRejectsStaleAndPublishesFresh(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		diskSamples := []hwmonSample{
			hwmonTestSample("disk:stale", "staleDisk", 38),
			hwmonTestSample("disk:fresh", "freshDisk", 47),
		}
		h := newOrchHarness(t, diskSamples)
		h.start(ctx)

		now := time.Now()
		h.snapshots <- receivedSnapshot{
			response: sensors.Response{
				Protocol: sensors.ProtocolVersion,
				Disks:    []sensors.Disk{{ID: "stale", Name: "staleDisk", Device: "sda", Rotational: true, Temp: 38}},
				HBAs:     []sensors.HBA{},
			},
			receivedAt: now.Add(-snapshotStreamTimeout - time.Second),
		}
		synctest.Wait()

		gotStale := h.readDevice(t, "disk:stale")
		if gotStale == "38000" {
			t.Fatal("stale snapshot (38) was applied to the device; expired() check is missing or broken")
		}

		h.snapshots <- receivedSnapshot{
			response: sensors.Response{
				Protocol: sensors.ProtocolVersion,
				Disks:    []sensors.Disk{{ID: "fresh", Name: "freshDisk", Device: "sdb", Rotational: true, Temp: 47}},
				HBAs:     []sensors.HBA{},
			},
			receivedAt: time.Now(),
		}
		synctest.Wait()

		gotFresh := h.readDevice(t, "disk:fresh")
		if gotFresh != "47000" {
			t.Fatalf("fresh device value = %s, want 47000", gotFresh)
		}

		cancel()
		h.stop(t, nil)
	})
}

// TestHWMonOrchPropagatesBackgroundError verifies that an error sent on the
// backgroundErrors channel is returned by the loop. It detects mutation C
// (error ignored / replaced by continue or nil).
func TestHWMonOrchPropagatesBackgroundError(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		h := newOrchHarness(t, nil)
		h.start(ctx)

		sentinel := errors.New("receiver failure")
		h.bgErrors <- sentinel

		h.stop(t, sentinel)
	})
}

// TestHWMonOrchCleanShutdown verifies that cancelling the context causes the
// loop to return nil.
func TestHWMonOrchCleanShutdown(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())

		h := newOrchHarness(t, nil)
		h.start(ctx)

		cancel()
		h.stop(t, nil)
	})
}

// TestHWMonOrchTopologyFirstSnapshot verifies that the first guest snapshot
// triggers a topology notification even when no reconfiguration is needed.
// The topology is pre-initialized from cache so that publish() returns
// reconfigured=false; the notification must come from firstGuestSnapshot.
// It detects mutation C (firstGuestSnapshot forced to false).
func TestHWMonOrchTopologyFirstSnapshot(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		diskSamples := []hwmonSample{hwmonTestSample("disk:1", "disk1", 38)}
		h := newOrchHarness(t, diskSamples)
		h.publisher.disks = hwmonInventory{sensors: sensorsFromSamples(diskSamples)}
		h.publisher.hbas = hwmonInventory{sensors: []hwmonSensor{}}
		h.start(ctx)

		h.snapshots <- diskSnap1(38, time.Now())
		synctest.Wait()

		if !h.topologyExists(t) {
			t.Fatal("topology notification file not created after first snapshot with pre-initialized topology (reconfigured=false)")
		}

		cancel()
		h.stop(t, nil)
	})
}

// TestHWMonOrchTopologyNoNotificationOnTempOnly verifies that a temperature-
// only change (same inventory) does NOT trigger a new topology notification.
// It detects mutation D (notification on every publish).
func TestHWMonOrchTopologyNoNotificationOnTempOnly(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		diskSamples := []hwmonSample{hwmonTestSample("disk:1", "disk1", 38)}
		h := newOrchHarness(t, diskSamples)
		h.publisher.disks = hwmonInventory{sensors: sensorsFromSamples(diskSamples)}
		h.publisher.hbas = hwmonInventory{sensors: []hwmonSensor{}}
		h.start(ctx)

		h.snapshots <- diskSnap1(38, time.Now())
		synctest.Wait()
		if !h.topologyExists(t) {
			t.Fatal("topology notification not created after first snapshot")
		}
		h.removeTopology(t)

		h.snapshots <- diskSnap1(47, time.Now())
		synctest.Wait()

		got := h.readDevice(t, "disk:1")
		if got != "47000" {
			t.Fatalf("device value after temp change = %s, want 47000", got)
		}
		if h.topologyExists(t) {
			t.Fatal("topology notification fired again for a temperature-only change")
		}

		cancel()
		h.stop(t, nil)
	})
}

// TestHWMonOrchTopologyNotificationOnLabelChange verifies that a label change
// (real topology change) triggers a new topology notification.
// It detects mutation E (notification suppressed after reconfiguration).
func TestHWMonOrchTopologyNotificationOnLabelChange(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		diskSamples := []hwmonSample{hwmonTestSample("disk:1", "disk1", 38)}
		h := newOrchHarness(t, diskSamples)
		h.start(ctx)

		now := time.Now()
		h.snapshots <- receivedSnapshot{
			response: sensors.Response{
				Protocol: sensors.ProtocolVersion,
				Disks:    []sensors.Disk{{ID: "1", Name: "disk1", Device: "sda", Rotational: true, Temp: 38}},
				HBAs:     []sensors.HBA{},
			},
			receivedAt: now,
		}
		synctest.Wait()
		if !h.topologyExists(t) {
			t.Fatal("topology notification not created after first snapshot")
		}
		h.removeTopology(t)

		h.snapshots <- receivedSnapshot{
			response: sensors.Response{
				Protocol: sensors.ProtocolVersion,
				Disks:    []sensors.Disk{{ID: "1", Name: "disk1-renamed", Device: "sda", Rotational: true, Temp: 38}},
				HBAs:     []sensors.HBA{},
			},
			receivedAt: time.Now(),
		}
		synctest.Wait()

		if !h.topologyExists(t) {
			t.Fatal("topology notification did not fire on label (topology) change")
		}

		cancel()
		h.stop(t, nil)
	})
}
