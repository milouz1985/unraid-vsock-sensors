// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"
	"unraid-vsock-sensors/internal/sensors"
)

func TestHBACollectorSnapshotDoesNotWaitForRefresh(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	collector := newTestHBACollector(time.Minute, hbaModeEnabled)
	collector.reader = hbaSnapshotReaderFunc(func(context.Context) ([]sensors.HBA, error) {
		close(started)
		<-release
		return []sensors.HBA{{ID: "sas:1234", Temp: 42}}, nil
	})
	done := make(chan struct{})
	go func() { collector.refresh(context.Background()); close(done) }()
	<-started
	readDone := make(chan struct{})
	go func() { _, _ = collector.snapshot(); close(readDone) }()
	select {
	case <-readDone:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("cached read blocked during HBA refresh")
	}
	close(release)
	<-done
}

func TestHBACollectorFailureInvalidatesSnapshot(t *testing.T) {
	collector := newTestHBACollector(time.Minute, hbaModeEnabled)
	collector.reader = hbaSnapshotReaderFunc(func(context.Context) ([]sensors.HBA, error) {
		return []sensors.HBA{{ID: "sas:1234", Temp: 42}}, nil
	})
	collector.refresh(context.Background())
	collector.reader = hbaSnapshotReaderFunc(func(context.Context) ([]sensors.HBA, error) {
		return nil, errors.New("failed")
	})
	collector.refresh(context.Background())
	readings, err := collector.snapshot()
	if len(readings) != 0 || err == nil {
		t.Fatalf("failed refresh returned %#v, %v", readings, err)
	}
	status := collector.status()
	if len(status.lastSuccessfulSnapshot) != 1 || status.lastSuccessfulSnapshot[0].Temp != 42 {
		t.Fatalf("last successful diagnostic snapshot = %#v", status.lastSuccessfulSnapshot)
	}
	response := collectorSnapshot(newDiskCollector(diskDataPaths{}), collector)
	if len(response.HBAs) != 0 || response.HBAError == "" {
		t.Fatalf("publisher response after HBA failure = %#v", response)
	}
}

func TestHBACollectorSnapshotReturnsDefensiveCopy(t *testing.T) {
	collector := newTestHBACollector(time.Minute, hbaModeEnabled)
	collector.reader = hbaSnapshotReaderFunc(func(context.Context) ([]sensors.HBA, error) {
		return []sensors.HBA{{ID: "sas:1234", Temp: 42}}, nil
	})
	collector.refresh(context.Background())
	readings, err := collector.snapshot()
	if err != nil || len(readings) != 1 {
		t.Fatalf("snapshot = %#v, %v", readings, err)
	}
	readings[0].Temp = 99
	if fresh, err := collector.snapshot(); err != nil || fresh[0].Temp != 42 {
		t.Fatalf("snapshot mutation reached collector: %#v, %v", fresh, err)
	}
}

func TestHBACollectorSuccessfulEmptyInventory(t *testing.T) {
	collector := newTestHBACollector(time.Minute, hbaModeEnabled)
	collector.reader = hbaSnapshotReaderFunc(func(context.Context) ([]sensors.HBA, error) {
		return []sensors.HBA{}, nil
	})
	collector.refresh(context.Background())
	readings, err := collector.snapshot()
	if err != nil || readings == nil || len(readings) != 0 {
		t.Fatalf("empty snapshot = %#v, %v", readings, err)
	}
}

func TestHBACollectorExpiresBlockedRefreshAndRecovers(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		collector := newTestHBACollector(time.Minute, hbaModeEnabled)
		collector.reader = hbaSnapshotReaderFunc(func(context.Context) ([]sensors.HBA, error) {
			return []sensors.HBA{{ID: "sas:1234", Temp: 42}}, nil
		})
		collector.refresh(context.Background())
		// The normal interval between collections must not expire the cache.
		time.Sleep(collector.interval)
		if readings, err := collector.snapshot(); err != nil || len(readings) != 1 || readings[0].Temp != 42 {
			t.Fatalf("between collections: readings=%v err=%v", readings, err)
		}

		release := make(chan struct{})
		defer close(release)
		collector.reader = hbaSnapshotReaderFunc(func(context.Context) ([]sensors.HBA, error) {
			<-release // A synchronous ioctl can keep waiting after its context expires.
			return []sensors.HBA{{ID: "sas:1234", Temp: 43}}, nil
		})
		go collector.refresh(context.Background())
		synctest.Wait()
		if readings, err := collector.snapshot(); err != nil || len(readings) != 1 || readings[0].Temp != 42 {
			t.Fatalf("during collection: readings=%v err=%v", readings, err)
		}

		time.Sleep(hbaCollectionTimeout)
		synctest.Wait()
		if readings, err := collector.snapshot(); len(readings) != 0 || err == nil {
			t.Fatalf("blocked past deadline: readings=%v err=%v", readings, err)
		}

		release <- struct{}{}
		synctest.Wait()
		if readings, err := collector.snapshot(); len(readings) != 0 || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("late successful collection: readings=%v err=%v", readings, err)
		}

		collector.reader = hbaSnapshotReaderFunc(func(context.Context) ([]sensors.HBA, error) {
			return []sensors.HBA{{ID: "sas:1234", Temp: 44}}, nil
		})
		collector.refresh(context.Background())
		if readings, err := collector.snapshot(); err != nil || len(readings) != 1 || readings[0].Temp != 44 {
			t.Fatalf("after recovery: readings=%v err=%v", readings, err)
		}
	})
}

func TestBlockedHBACollectionDoesNotStopSnapshotPublication(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		collector := newTestHBACollector(time.Second, hbaModeEnabled)
		collector.reader = hbaSnapshotReaderFunc(func(context.Context) ([]sensors.HBA, error) {
			return []sensors.HBA{{ID: "sas:1234", Temp: 42}}, nil
		})
		collector.refresh(context.Background())

		release := make(chan struct{})
		collector.reader = hbaSnapshotReaderFunc(func(context.Context) ([]sensors.HBA, error) {
			<-release // Simulate a synchronous ioctl ignoring its expired context.
			return nil, context.DeadlineExceeded
		})
		go collector.refresh(context.Background())
		synctest.Wait()
		time.Sleep(collector.interval + hbaCollectionTimeout)
		synctest.Wait()

		frames := capturePublishedSnapshots(
			t,
			newDiskCollector(diskDataPaths{}),
			collector,
			2,
		)
		for index, frame := range frames {
			if frame.HBAError == "" || len(frame.HBAs) != 0 {
				t.Fatalf("frame %d republished HBA data instead of the collection error: %#v", index, frame)
			}
		}

		close(release)
		synctest.Wait()
	})
}

func TestHBACollectorDisabledDoesNotCollect(t *testing.T) {
	collector := newTestHBACollector(time.Millisecond, hbaModeDisabled)
	collector.reader = hbaSnapshotReaderFunc(func(context.Context) ([]sensors.HBA, error) {
		t.Fatal("disabled collector performed collection")
		return nil, nil
	})
	collector.run(context.Background())
	readings, err := collector.snapshot()
	if readings == nil || len(readings) != 0 || err != nil {
		t.Fatalf("got %#v, %v", readings, err)
	}
}

func TestHBAStableIDPrefersSASAcrossBackends(t *testing.T) {
	if got, want := hbaStableID("0x56:C9:2B:F0:00:2E:67:05", "0000:06:10.0", "SERIAL"), "sas:56c92bf0002e6705"; got != want {
		t.Fatalf("ID = %q, want %q", got, want)
	}
	if got, want := hbaStableID("", "0000:06:10.0", "SERIAL"), "pci:0000:06:10.0"; got != want {
		t.Fatalf("PCI fallback ID = %q, want %q", got, want)
	}
	if got, want := hbaStableID("", "", "SERIAL"), "serial:serial"; got != want {
		t.Fatalf("serial fallback ID = %q, want %q", got, want)
	}
}
