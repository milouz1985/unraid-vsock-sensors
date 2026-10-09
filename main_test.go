// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"context"
	"errors"
	"net"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"unraid-vsock-sensors/internal/sensors"
)

type closeCountingConnection struct {
	closeCount  atomic.Int32
	writes      atomic.Int32
	onWrite     func()
	writeErr    error
	deadlineErr error
}

func (c *closeCountingConnection) Close() error {
	c.closeCount.Add(1)
	return nil
}

func (c *closeCountingConnection) Write(p []byte) (int, error) {
	c.writes.Add(1)
	if c.onWrite != nil {
		c.onWrite()
	}
	if c.writeErr != nil {
		return 0, c.writeErr
	}
	return len(p), nil
}

func (c *closeCountingConnection) SetWriteDeadline(time.Time) error { return c.deadlineErr }

func TestPublisherClosesConnectionOnIOError(t *testing.T) {
	for _, test := range []struct {
		name        string
		writeErr    error
		deadlineErr error
	}{
		{name: "write", writeErr: errors.New("write failed")},
		{name: "deadline", deadlineErr: errors.New("deadline failed")},
	} {
		t.Run(test.name, func(t *testing.T) {
			conn := &closeCountingConnection{writeErr: test.writeErr, deadlineErr: test.deadlineErr}
			log := stickyErrorLog{context: "test"}
			err := publishConnection(context.Background(), conn,
				newDiskCollector(diskDataPaths{}), newTestHBACollector(time.Minute, hbaModeDisabled), newServiceState(0), &log)
			if err == nil || conn.closeCount.Load() != 1 {
				t.Fatalf("publish = %v, closes = %d", err, conn.closeCount.Load())
			}
		})
	}
}

func TestPublisherClosesConnectionAfterDialCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	conn := &closeCountingConnection{}
	err := publishSnapshotsWithDialer(ctx, newDiskCollector(diskDataPaths{}),
		newTestHBACollector(time.Minute, hbaModeDisabled),
		func(context.Context) (snapshotConnection, error) {
			cancel()
			return conn, nil
		}, newServiceState(0))
	if err != nil || conn.closeCount.Load() != 1 || conn.writes.Load() != 0 {
		t.Fatalf("publish = %v, closes = %d, writes = %d", err, conn.closeCount.Load(), conn.writes.Load())
	}
}

func TestPublisherClosesConnectionDuringSnapshotWait(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	conn := &closeCountingConnection{onWrite: cancel}
	err := publishSnapshotsWithDialer(ctx, newDiskCollector(diskDataPaths{}),
		newTestHBACollector(time.Minute, hbaModeDisabled),
		func(context.Context) (snapshotConnection, error) { return conn, nil }, newServiceState(0))
	if err != nil || conn.closeCount.Load() != 1 || conn.writes.Load() != 1 {
		t.Fatalf("publish = %v, closes = %d, writes = %d", err, conn.closeCount.Load(), conn.writes.Load())
	}
}

func TestConfiguredHBACollectorUsesBackendInterval(t *testing.T) {
	for _, test := range []struct {
		name    string
		backend hbaBackendMode
		want    time.Duration
		wantErr bool
	}{
		{name: "native", backend: hbaBackendMPT3CTL, want: 15 * time.Second},
		{name: "StorCLI", backend: hbaBackendStorCLI, want: 30 * time.Second},
		{name: "invalid", backend: "unknown", wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			collector, err := newConfiguredHBACollector(hbaModeEnabled, test.backend)
			if (err != nil) != test.wantErr {
				t.Fatalf("newConfiguredHBACollector() error = %v; want error=%v", err, test.wantErr)
			}
			if err == nil && collector.interval != test.want {
				t.Fatalf("collector interval = %s; want %s", collector.interval, test.want)
			}
		})
	}
}

func TestPublishedSnapshotMetadata(t *testing.T) {
	response := collectorSnapshot(
		newDiskCollector(diskDataPaths{}),
		newTestHBACollector(time.Minute, hbaModeDisabled),
	)

	if response.Protocol != sensors.ProtocolVersion {
		t.Fatalf("got protocol %d, want %d", response.Protocol, sensors.ProtocolVersion)
	}
	if response.HBAs == nil || len(response.HBAs) != 0 || response.HBAError != "" {
		t.Fatalf("disabled HBA snapshot = %#v, error %q", response.HBAs, response.HBAError)
	}
	if response.Error == "" {
		t.Fatal("uncollected disks were reported as available")
	}
}

func capturePublishedSnapshots(
	t *testing.T,
	disks *diskCollector,
	hbas *hbaCollector,
	count int,
) []sensors.Response {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	server, client := net.Pipe()
	defer server.Close()
	dials := 0
	done := make(chan error, 1)
	go func() {
		done <- publishSnapshotsWithDialer(ctx, disks, hbas, func(context.Context) (snapshotConnection, error) {
			dials++
			return client, nil
		}, newServiceState(0))
	}()

	reader := sensors.NewFrameReader(server)
	snapshots := make([]sensors.Response, 0, count)
	for range count {
		snapshot, err := reader.Read()
		if err != nil {
			t.Fatal(err)
		}
		snapshots = append(snapshots, snapshot)
	}
	cancel()
	_ = server.Close()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if dials != 1 {
		t.Fatalf("dial count = %d, want 1", dials)
	}
	return snapshots
}

func TestPublisherStreamsSuccessiveSnapshotsAndReconnects(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		env := newDiskTestEnvironment(t, "30")
		env.write(t, env.paths.disksINI, "[disk1]\nid=serial-hdd\ndevice=sda\ntransport=ata\nrotational=1\nspundown=0\ntemp=35\n"+
			"[pool1]\nid=serial-nvme\ndevice=nvme0n1\ntransport=nvme\nrotational=0\nspundown=0\ntemp=*\n")
		disks := env.collector()
		disks.refresh()
		initial := map[string]sensors.Disk{
			"serial-hdd":  {ID: "serial-hdd", Name: "disk1", Device: "sda", Transport: "ata", Rotational: true, Temp: 35},
			"serial-nvme": {ID: "serial-nvme", Name: "pool1", Device: "nvme0n1", Transport: "nvme", Temp: 0, Unavailable: true},
		}
		updated := map[string]sensors.Disk{
			"serial-hdd":  {ID: "serial-hdd", Name: "disk1", Device: "sda", Transport: "ata", Rotational: true, Temp: 42},
			"serial-nvme": {ID: "serial-nvme", Name: "pool1", Device: "nvme0n1", Transport: "nvme", Temp: 48},
		}

		firstServer, firstClient := net.Pipe()
		secondServer, secondClient := net.Pipe()
		defer firstServer.Close()
		defer firstClient.Close()
		defer secondServer.Close()
		defer secondClient.Close()
		connections := []snapshotConnection{firstClient, secondClient}
		dials := 0
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		done := make(chan error, 1)
		go func() {
			done <- publishSnapshotsWithDialer(
				ctx,
				disks,
				newTestHBACollector(time.Minute, hbaModeDisabled),
				func(context.Context) (snapshotConnection, error) {
					if dials >= len(connections) {
						return nil, errors.New("unexpected extra dial")
					}
					conn := connections[dials]
					dials++
					return conn, nil
				},
				newServiceState(0),
			)
		}()

		readSnapshot := func(reader *sensors.FrameReader, want map[string]sensors.Disk) {
			t.Helper()
			got, err := reader.Read()
			if err != nil {
				t.Fatal(err)
			}
			if got.Protocol != 1 || got.Error != "" || got.HBAError != "" || got.HBAs == nil || len(got.HBAs) != 0 {
				t.Fatalf("snapshot metadata = %#v; want protocol 1, no errors and an empty disabled HBA inventory", got)
			}
			if len(got.Disks) != len(want) {
				t.Fatalf("snapshot disks = %#v; want %#v", got.Disks, want)
			}
			seen := make(map[string]bool)
			for _, disk := range got.Disks {
				if expected, ok := want[disk.ID]; !ok || seen[disk.ID] || disk != expected {
					t.Fatalf("published disk = %#v; want unique entry %#v", disk, expected)
				}
				seen[disk.ID] = true
			}
		}

		firstReader := sensors.NewFrameReader(firstServer)
		readSnapshot(firstReader, initial)
		// Settle the completed publication before changing the source. The
		// bubble's clock stays fixed while this goroutine refreshes the fixture.
		synctest.Wait()
		env.write(t, env.paths.disksINI, "[disk1]\nid=serial-hdd\ndevice=sda\ntransport=ata\nrotational=1\nspundown=0\ntemp=42\n"+
			"[pool1]\nid=serial-nvme\ndevice=nvme0n1\ntransport=nvme\nrotational=0\nspundown=0\ntemp=48\n")
		disks.refresh()
		readSnapshot(firstReader, updated)
		_ = firstServer.Close()
		secondReader := sensors.NewFrameReader(secondServer)
		readSnapshot(secondReader, updated)
		readSnapshot(secondReader, updated)
		cancel()
		_ = secondServer.Close()
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		if dials != 2 {
			t.Fatalf("dial count = %d, want 2", dials)
		}
	})
}
