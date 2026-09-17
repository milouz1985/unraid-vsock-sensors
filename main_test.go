// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
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
				newDiskCollector(diskDataPaths{}), newTestHBACollector(time.Minute, hbaModeDisabled), nil, &log)
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
		}, nil)
	if err != nil || conn.closeCount.Load() != 1 || conn.writes.Load() != 0 {
		t.Fatalf("publish = %v, closes = %d, writes = %d", err, conn.closeCount.Load(), conn.writes.Load())
	}
}

func TestPublisherClosesConnectionDuringSnapshotWait(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	conn := &closeCountingConnection{onWrite: cancel}
	err := publishSnapshotsWithDialer(ctx, newDiskCollector(diskDataPaths{}),
		newTestHBACollector(time.Minute, hbaModeDisabled),
		func(context.Context) (snapshotConnection, error) { return conn, nil }, nil)
	if err != nil || conn.closeCount.Load() != 1 || conn.writes.Load() != 1 {
		t.Fatalf("publish = %v, closes = %d, writes = %d", err, conn.closeCount.Load(), conn.writes.Load())
	}
}

func TestResolveHBAInterval(t *testing.T) {
	for _, test := range []struct {
		name     string
		backend  hbaBackendMode
		interval time.Duration
		explicit bool
		want     time.Duration
		wantErr  bool
	}{
		{name: "native default", backend: hbaBackendMPT3CTL, want: 15 * time.Second},
		{name: "StorCLI default", backend: hbaBackendStorCLI, want: 30 * time.Second},
		{name: "native explicit", backend: hbaBackendMPT3CTL, interval: 45 * time.Second, explicit: true, want: 45 * time.Second},
		{name: "StorCLI explicit", backend: hbaBackendStorCLI, interval: 45 * time.Second, explicit: true, want: 45 * time.Second},
		{name: "explicit zero", backend: hbaBackendMPT3CTL, explicit: true, wantErr: true},
		{name: "negative interval", backend: hbaBackendStorCLI, interval: -time.Second, explicit: true, wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := resolveHBAInterval(test.backend, test.interval, test.explicit)
			if got != test.want || (err != nil) != test.wantErr {
				t.Fatalf("resolveHBAInterval() = %s, %v; want %s, error=%v", got, err, test.want, test.wantErr)
			}
			if test.wantErr && !strings.Contains(err.Error(), "hba-interval") {
				t.Fatalf("interval error = %v", err)
			}
		})
	}
}

func TestResolveHBAIntervalRejectsInvalidBackendWithExplicitInterval(t *testing.T) {
	interval, err := resolveHBAInterval("unknown", 45*time.Second, true)
	if interval != 0 || err == nil || !strings.Contains(err.Error(), "invalid HBA backend") {
		t.Fatalf("resolveHBAInterval() = %s, %v; want invalid backend error", interval, err)
	}
}

func TestServeUsesSharedControlSocketOverride(t *testing.T) {
	environmentPath := filepath.Join(t.TempDir(), "environment.sock")
	if err := os.WriteFile(environmentPath, []byte("not a socket"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(controlSocketEnvironmentVariable, environmentPath)
	if err := serve([]string{"--hba-mode", "disabled"}); err == nil || !strings.Contains(err.Error(), environmentPath) {
		t.Fatalf("serve with %s = %v; want error for %q", controlSocketEnvironmentVariable, err, environmentPath)
	}

	flagPath := filepath.Join(t.TempDir(), "flag.sock")
	if err := os.WriteFile(flagPath, []byte("not a socket"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := serve([]string{"--hba-mode", "disabled", "--control-socket", flagPath}); err == nil || !strings.Contains(err.Error(), flagPath) {
		t.Fatalf("serve with explicit --control-socket = %v; want error for %q", err, flagPath)
	}
}

func TestServeRejectsInvalidExplicitHBAInterval(t *testing.T) {
	for _, value := range []string{"0", "-1s", "invalid"} {
		t.Run(value, func(t *testing.T) {
			if err := serve([]string{"--hba-backend", "storcli", "--hba-interval", value}); err == nil {
				t.Fatalf("serve accepted interval %q", value)
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
		}, nil)
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
				newDiskCollector(diskDataPaths{}),
				newTestHBACollector(time.Minute, hbaModeDisabled),
				func(context.Context) (snapshotConnection, error) {
					if dials >= len(connections) {
						return nil, errors.New("unexpected extra dial")
					}
					conn := connections[dials]
					dials++
					return conn, nil
				},
				nil,
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
