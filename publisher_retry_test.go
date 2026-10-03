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

// A refused initial connection must back off, reconnect, publish a frame and
// clear the failure from diagnostics. A single failure exercises this transition.
func TestPublisherRecoversAfterInitialDialFailures(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		server, client := net.Pipe()
		defer server.Close()
		defer client.Close()

		firstDial := make(chan struct{})
		var dialTimes []time.Time
		dial := func(context.Context) (snapshotConnection, error) {
			dialTimes = append(dialTimes, time.Now())
			if len(dialTimes) == 1 {
				close(firstDial)
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

		<-firstDial
		synctest.Wait()
		status := state.status().publisher
		if status.status != publisherStatusReconnecting || status.lastError != "vsock dial refused" || status.lastErrorAt.IsZero() {
			t.Fatalf("after dial failure: status = %#v", status)
		}

		frame, err := sensors.NewFrameReader(server).Read()
		if err != nil {
			t.Fatalf("read frame after reconnect: %v", err)
		}
		if frame.Protocol != 1 {
			t.Fatalf("frame protocol = %d, want 1", frame.Protocol)
		}
		synctest.Wait()
		status = state.status().publisher
		if status.status != publisherStatusConnected || status.lastError != "" || !status.lastErrorAt.IsZero() {
			t.Fatalf("after recovery: status = %#v", status)
		}
		if status.lastConnectedAt.IsZero() || status.lastPublishedAt.IsZero() {
			t.Fatal("recovery did not record connection and publication")
		}
		if len(dialTimes) != 2 || dialTimes[1].Sub(dialTimes[0]) < defaultPublishInterval {
			t.Fatalf("dial times = %v, want failure then success separated by backoff", dialTimes)
		}

		cancel()
		_ = client.Close()
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	})
}
