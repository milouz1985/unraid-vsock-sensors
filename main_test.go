package main

import (
	"bytes"
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"unraid-vsock-sensors/internal/sensors"
)

func TestPublishedSnapshotMetadata(t *testing.T) {
	response := collectorSnapshot(
		newDiskCollector("unused", time.Minute),
		newHBACollector(time.Minute, hbaModeDisabled),
	)

	if response.Version != version {
		t.Fatalf("got version %q, want %q", response.Version, version)
	}
	if !response.HBADisabled {
		t.Fatal("disabled HBA collection was not reported")
	}
	if response.Error == "" {
		t.Fatal("uncollected disks were reported as available")
	}
}

type recordingSnapshotConnection struct {
	bytes.Buffer
	cancel    context.CancelFunc
	remaining int
}

func (c *recordingSnapshotConnection) Close() error                     { return nil }
func (c *recordingSnapshotConnection) SetWriteDeadline(time.Time) error { return nil }
func (c *recordingSnapshotConnection) Write(payload []byte) (int, error) {
	written, err := c.Buffer.Write(payload)
	c.remaining -= bytes.Count(payload, []byte{'\n'})
	if c.remaining <= 0 {
		c.cancel()
	}
	return written, err
}

func capturePublishedSnapshots(
	t *testing.T,
	disks *diskCollector,
	hbas *hbaCollector,
	count int,
) []sensors.Response {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	conn := &recordingSnapshotConnection{cancel: cancel, remaining: count}
	dials := 0
	err := publishSnapshotsWithDialer(ctx, time.Millisecond, disks, hbas, func(context.Context) (snapshotConnection, error) {
		dials++
		return conn, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if dials != 1 {
		t.Fatalf("dial count = %d, want 1", dials)
	}

	reader := sensors.NewFrameReader(&conn.Buffer)
	snapshots := make([]sensors.Response, 0, count)
	for range count {
		snapshot, err := reader.Read()
		if err != nil {
			t.Fatal(err)
		}
		snapshots = append(snapshots, snapshot)
	}
	return snapshots
}

func TestPublisherStreamsSuccessiveSnapshotsAndReconnects(t *testing.T) {
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
			time.Millisecond,
			newDiskCollector("unused", time.Minute),
			newHBACollector(time.Minute, hbaModeDisabled),
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

	readFrames := func(conn net.Conn, count int) []sensors.Response {
		t.Helper()
		if err := conn.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
			t.Fatal(err)
		}
		reader := sensors.NewFrameReader(conn)
		frames := make([]sensors.Response, 0, count)
		for range count {
			frame, err := reader.Read()
			if err != nil {
				t.Fatal(err)
			}
			frames = append(frames, frame)
		}
		return frames
	}

	first := readFrames(firstServer, 2)
	_ = firstServer.Close()
	second := readFrames(secondServer, 2)
	cancel()
	_ = secondServer.Close()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if dials != 2 {
		t.Fatalf("dial count = %d, want 2", dials)
	}
	if first[0].Timestamp.IsZero() || first[1].Timestamp.Before(first[0].Timestamp) ||
		second[0].Timestamp.Before(first[1].Timestamp) {
		t.Fatalf("publication timestamps are not monotonic: first=%v second=%v", first, second)
	}
}
