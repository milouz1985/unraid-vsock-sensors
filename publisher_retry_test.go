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

// Each of two consecutive refused connections must back off before recovery,
// publication and clearing the failure from diagnostics.
func TestPublisherRecoversAfterInitialDialFailures(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		server, client := net.Pipe()
		defer server.Close()
		defer client.Close()

		dialStarted := make(chan time.Time, 3)
		dialAttempts := 0
		dial := func(context.Context) (snapshotConnection, error) {
			dialAttempts++
			dialStarted <- time.Now()
			if dialAttempts <= 2 {
				return nil, errors.New("vsock dial refused")
			}
			return client, nil
		}

		state := newServiceState(990)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		done := make(chan error, 1)
		go func() {
			done <- publishSnapshotsWithDialer(ctx,
				newDiskCollector(diskDataPaths{}),
				newTestHBACollector(time.Minute, hbaModeDisabled), dial, state)
		}()

		var dialTimes [3]time.Time
		for attempt := 1; attempt <= 2; attempt++ {
			dialTimes[attempt-1] = <-dialStarted
			synctest.Wait()
			status := state.status().publisher
			if status.status != publisherStatusReconnecting || status.lastError != "vsock dial refused" || status.lastErrorAt.IsZero() {
				t.Fatalf("after dial failure %d: status = %#v", attempt, status)
			}
		}

		frame, err := sensors.NewFrameReader(server).Read()
		if err != nil {
			t.Fatalf("read frame after reconnect: %v", err)
		}
		if frame.Protocol != 1 {
			t.Fatalf("frame protocol = %d, want 1", frame.Protocol)
		}
		dialTimes[2] = <-dialStarted
		synctest.Wait()
		status := state.status().publisher
		if status.status != publisherStatusConnected || status.lastError != "" || !status.lastErrorAt.IsZero() {
			t.Fatalf("after recovery: status = %#v", status)
		}
		if status.lastConnectedAt.IsZero() || status.lastPublishedAt.IsZero() {
			t.Fatal("recovery did not record connection and publication")
		}
		if len(dialStarted) != 0 {
			t.Fatal("unexpected additional dial after recovery")
		}
		for attempt := 1; attempt < len(dialTimes); attempt++ {
			if gap := dialTimes[attempt].Sub(dialTimes[attempt-1]); gap < time.Second {
				t.Fatalf("backoff before dial %d = %v, want at least 1s", attempt+1, gap)
			}
		}

		cancel()
		_ = client.Close()
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	})
}
