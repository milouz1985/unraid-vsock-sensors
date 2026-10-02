// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"unraid-vsock-sensors/internal/sensors"
)

// TestHBACollectorRunSingleInFlight locks the "one collection in flight at a
// time" contract of hbaCollector.run in active mode. The initial collection is
// allowed to complete so that the timer is created and periodic ticks fire. The
// second (periodic) collection is then blocked; while it is blocked, several
// collection deadlines fire. With the serialized loop, no third collection may
// start: calls stays at 2 and maxConcurrent stays at 1. If the implementation
// ever spawned a concurrent goroutine per tick (go c.refresh(...)) instead of
// serializing collections through the loop, a third collection would start
// while the second is still blocked and the test would fail.
func TestHBACollectorRunSingleInFlight(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const interval = 10 * time.Millisecond
		collector := newTestHBACollector(interval, hbaModeEnabled)

		var active, maxConcurrent, calls atomic.Int32
		blockSecond := make(chan struct{})
		secondStarted := make(chan struct{})
		collector.reader = hbaSnapshotReaderFunc(func(context.Context) ([]sensors.HBA, error) {
			current := calls.Add(1)
			n := active.Add(1)
			if n > maxConcurrent.Load() {
				maxConcurrent.Store(n)
			}
			defer active.Add(-1)
			// Signal that the second (periodic) collection has started.
			if current == 2 {
				close(secondStarted)
				// Block the second collection so that subsequent ticks fire
				// while it is still in flight. The initial collection is not
				// blocked so that the timer is created and ticks can occur.
				<-blockSecond
			}
			return []sensors.HBA{{ID: "sas:1234", Temp: 42}}, nil
		})

		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() { collector.run(ctx); close(done) }()

		// Wait for the initial collection to complete and the first periodic
		// tick to start the second collection (which then blocks). The
		// secondStarted channel is closed by the reader when calls reaches 2.
		<-secondStarted
		synctest.Wait()
		if got := active.Load(); got != 1 {
			t.Fatalf("active = %d, want 1 (second collection must be in flight)", got)
		}

		// Several collection deadlines fire while the second collection is
		// still blocked. With the serialized loop, no third collection may
		// start: calls stays at 2 and maxConcurrent stays at 1.
		for range 3 {
			time.Sleep(interval)
			synctest.Wait()
			if got := calls.Load(); got != 2 {
				t.Fatalf("a third collection started while the second was in flight: calls = %d, want 2", got)
			}
			if got := maxConcurrent.Load(); got > 1 {
				t.Fatalf("maxConcurrent = %d, want 1", got)
			}
		}

		// Release the second collection: the loop must resume normally and the
		// next tick must start a new collection.
		close(blockSecond)
		synctest.Wait()
		if got := active.Load(); got != 0 {
			t.Fatalf("active = %d after release, want 0", got)
		}
		time.Sleep(interval)
		synctest.Wait()
		if got := calls.Load(); got < 3 {
			t.Fatalf("calls = %d after release, want >= 3 (loop must continue)", got)
		}
		if got := maxConcurrent.Load(); got > 1 {
			t.Fatalf("maxConcurrent = %d, want 1", got)
		}

		cancel()
		<-done
		if got := maxConcurrent.Load(); got > 1 {
			t.Fatalf("maxConcurrent = %d over the whole run, want 1", got)
		}
	})
}

// TestHBACollectorRunCancelledDuringCollection ensures that a late successful
// collection that completes after the parent context is canceled does not
// become the newly published state.
func TestHBACollectorRunCancelledDuringCollection(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		collector := newTestHBACollector(time.Minute, hbaModeEnabled)

		started, release := make(chan struct{}), make(chan struct{})
		var calls atomic.Int32
		collector.reader = hbaSnapshotReaderFunc(func(context.Context) ([]sensors.HBA, error) {
			calls.Add(1)
			if calls.Load() == 1 {
				close(started)
				<-release
			}
			return []sensors.HBA{{ID: "sas:1234", Temp: 99}}, nil
		})

		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() { collector.run(ctx); close(done) }()

		// Wait for the initial collection to start and be blocked.
		<-started
		synctest.Wait()

		// Cancel the parent context while the collection is in flight.
		cancel()
		// The reader then returns a success late.
		close(release)
		<-done
		synctest.Wait()

		// The late success must not have been published as the new state.
		readings, err := collector.snapshot()
		if err == nil {
			t.Fatalf("snapshot = %#v after canceled collection, want an error (late success must not be published)", readings)
		}
	})
}

// TestHBACollectorRunDisabledExitsImmediately ensures the disabled collector
// returns without performing any collection.
func TestHBACollectorRunDisabledExitsImmediately(t *testing.T) {
	collector := newTestHBACollector(time.Millisecond, hbaModeDisabled)
	var calls atomic.Int32
	collector.reader = hbaSnapshotReaderFunc(func(context.Context) ([]sensors.HBA, error) {
		calls.Add(1)
		return nil, errors.New("must not collect")
	})
	collector.run(context.Background())
	if got := calls.Load(); got != 0 {
		t.Fatalf("calls = %d for a disabled collector, want 0", got)
	}
}
