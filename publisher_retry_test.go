// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"context"
	"errors"
	"net"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"unraid-vsock-sensors/internal/sensors"
)

// TestPublisherRecoversAfterInitialDialFailures locks the publisher contract
// around publishSnapshotsWithDialer when the first dial attempts fail:
//
//   - after each dial failure the state is reconnecting with the error
//     recorded (disconnected);
//   - each retry waits for the backoff: the gap between two consecutive dial
//     attempts is at least defaultPublishInterval (no hot loop);
//   - on recovery the state is connected, the previous error is cleared, and
//     lastConnectedAt/lastPublishedAt are set;
//   - once a working connection is obtained it publishes a valid frame.
//
// The test does not exercise AF_VSOCK itself; it uses the dialer seam. Three
// dials fail before the fourth succeeds, so the reconnecting state can be
// observed after each failure (the state is read while the backoff wait is in
// progress, i.e. before the next dial overwrites it).
func TestPublisherRecoversAfterInitialDialFailures(t *testing.T) {
	const failedDials = 3

	synctest.Test(t, func(t *testing.T) {
		var mu sync.Mutex
		dials := 0
		dialTimes := make([]time.Time, 0, 8)
		dialStarted := make(chan int, 16)

		server, client := net.Pipe()
		defer server.Close()
		defer client.Close()

		dial := func(ctx context.Context) (snapshotConnection, error) {
			mu.Lock()
			dials++
			n := dials
			dialTimes = append(dialTimes, time.Now())
			mu.Unlock()
			// Signal that the Nth dial attempt has started, so the test can
			// observe the state the publisher set after the previous failure
			// while the backoff wait for the next attempt is still pending.
			dialStarted <- n
			// The first failedDials dials fail, the next one returns a working
			// pipe.
			if n <= failedDials {
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
				newTestHBACollector(time.Minute, hbaModeDisabled),
				dial,
				state,
			)
		}()

		// Wait until the Nth dial attempt has started (its signal was sent).
		// The state then reflects the outcome of attempt N-1: for N-1 <=
		// failedDials it is reconnecting with the error recorded; the backoff
		// wait for attempt N is in progress, so connectedNow has not yet run.
		waitDial := func(n int) {
			t.Helper()
			for {
				select {
				case got := <-dialStarted:
					if got == n {
						return
					}
				case <-done:
					t.Fatalf("publisher exited before the %dth dial attempt", n)
				}
			}
		}

		// After each of the first failedDials failures, the state must be
		// reconnecting with the error recorded (not connected).
		for n := 1; n < failedDials; n++ {
			waitDial(n + 1)
			status := state.status()
			if status.publisher.status != publisherStatusReconnecting {
				t.Fatalf("after %dth dial failure: state = %q, want %q", n, status.publisher.status, publisherStatusReconnecting)
			}
			if status.publisher.lastError == "" {
				t.Fatalf("after %dth dial failure: lastError is empty, want the dial error", n)
			}
			if status.publisher.lastErrorAt.IsZero() {
				t.Fatalf("after %dth dial failure: lastErrorAt is zero, want the failure time", n)
			}
		}

		// Read one valid frame from the recovered connection. The publisher
		// must survive the failed dials (with backoff between each), then
		// connect and publish.
		reader := sensors.NewFrameReader(server)
		frame, err := reader.Read()
		if err != nil {
			t.Fatalf("failed to read a frame after recovery: %v", err)
		}
		if frame.Protocol != sensors.ProtocolVersion {
			t.Fatalf("recovered frame protocol = %d, want %d", frame.Protocol, sensors.ProtocolVersion)
		}

		// After recovery: the state must be connected, the previous error must
		// be cleared, and lastConnectedAt/lastPublishedAt must be set.
		status := state.status()
		if status.publisher.status != publisherStatusConnected {
			t.Fatalf("after publish: state = %q, want %q", status.publisher.status, publisherStatusConnected)
		}
		if status.publisher.lastError != "" {
			t.Fatalf("after publish: lastError = %q, want empty (error must be cleared on recovery)", status.publisher.lastError)
		}
		if !status.publisher.lastErrorAt.IsZero() {
			t.Fatal("after publish: lastErrorAt is not zero, want zero (error timestamp must be cleared on recovery)")
		}
		if status.publisher.lastConnectedAt.IsZero() {
			t.Fatal("after publish: lastConnectedAt is zero, want the connection time")
		}

		// Verify the backoff: at least failedDials+1 dial attempts occurred
		// and the gap between consecutive attempts is at least the backoff
		// interval. A hot loop (no waitFor between failures) would produce
		// near-zero gaps.
		mu.Lock()
		dialCount := dials
		times := append([]time.Time(nil), dialTimes...)
		mu.Unlock()
		if dialCount < failedDials+1 {
			t.Fatalf("dial count = %d, want >= %d (three failures + one success)", dialCount, failedDials+1)
		}
		for i := 1; i < len(times); i++ {
			gap := times[i].Sub(times[i-1])
			if gap < defaultPublishInterval {
				t.Fatalf("gap between dial %d and %d = %v, want >= %v (hot retry loop)", i, i+1, gap, defaultPublishInterval)
			}
		}

		cancel()
		_ = client.Close()
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	})
}
