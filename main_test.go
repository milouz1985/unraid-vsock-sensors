// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"context"
	"errors"
	"net"
	"testing"
	"testing/synctest"
	"time"

	"unraid-vsock-sensors/internal/sensors"
)

func TestPublishedSnapshotMetadata(t *testing.T) {
	response := collectorSnapshot(
		newDiskCollector("unused", time.Minute),
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
		})
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
		firstServer, firstClient := net.Pipe()
		secondServer, secondClient := net.Pipe()
		connections := []snapshotConnection{firstClient, secondClient}
		dials := 0
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		done := make(chan error, 1)
		go func() {
			done <- publishSnapshotsWithDialer(
				ctx,
				newDiskCollector("unused", time.Minute),
				newTestHBACollector(time.Minute, hbaModeDisabled),
				func(context.Context) (snapshotConnection, error) {
					if dials >= len(connections) {
						return nil, errors.New("unexpected extra dial")
					}
					conn := connections[dials]
					dials++
					return conn, nil
				},
			)
		}()

		readFrames := func(conn net.Conn, count int) {
			t.Helper()
			reader := sensors.NewFrameReader(conn)
			for range count {
				if _, err := reader.Read(); err != nil {
					t.Fatal(err)
				}
			}
		}

		readFrames(firstServer, 2)
		_ = firstServer.Close()
		readFrames(secondServer, 2)
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
