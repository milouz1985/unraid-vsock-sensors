// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func TestRefreshRequestsAreCoalesced(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	refresh := make(chan struct{}, 1)
	started := make(chan int32, 3)
	release := make(chan struct{})
	var count atomic.Int32
	done := make(chan struct{})
	go func() {
		runDiskRefreshLoop(ctx, refresh, time.Hour, func() {
			current := count.Add(1)
			started <- current
			if current == 1 {
				<-release
			}
		})
		close(done)
	}()
	if current := <-started; current != 1 {
		t.Fatalf("first collection = %d", current)
	}
	for range 100 {
		requestDiskRefresh(refresh)
	}
	close(release)
	if current := <-started; current != 2 {
		t.Fatalf("coalesced collection = %d", current)
	}
	if got := count.Load(); got != 2 {
		t.Fatalf("collection count = %d, want 2", got)
	}
	cancel()
	<-done
}

func TestWatchdogMarksExpiredCacheUnavailableWithoutEvent(t *testing.T) {
	environment := newDiskTestEnvironment(t, "0")
	environment.write(t, environment.paths.disksINI, "[disk1]\nid=serial\ndevice=sda\nrotational=1\nspundown=0\ntemp=35\n")
	environment.report(t, "disk1", environment.now)

	var nowUnixNano atomic.Int64
	nowUnixNano.Store(environment.now.UnixNano())
	collector := newDiskCollector(environment.paths)
	collector.watchdog = 5 * time.Millisecond
	collector.now = func() time.Time { return time.Unix(0, nowUnixNano.Load()) }
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		collector.run(ctx, nil)
		close(done)
	}()
	eventuallyDisk(t, collector, func(disk sensorsDisk) bool { return disk.temp == 35 && !disk.unavailable })

	nowUnixNano.Add(int64(11 * time.Second))
	eventuallyDisk(t, collector, func(disk sensorsDisk) bool { return disk.unavailable })
	cancel()
	<-done
}

func eventuallyDisk(t *testing.T, collector *diskCollector, accept func(sensorsDisk) bool) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		readings, err := collector.snapshot()
		if err == nil && len(readings) == 1 {
			disk := sensorsDisk{
				readings[0].ID, readings[0].Name, readings[0].Device,
				readings[0].Temp, readings[0].Unavailable,
			}
			if accept(disk) {
				return
			}
		}
		time.Sleep(time.Millisecond)
	}
	readings, err := collector.snapshot()
	t.Fatalf("condition not reached; snapshot = %#v, %v", readings, err)
}

func TestDiskCollectorConcurrentRefreshAndSnapshot(t *testing.T) {
	environment := newDiskTestEnvironment(t, "30")
	environment.write(t, environment.paths.disksINI, "[disk1]\nid=serial\ndevice=sda\nrotational=1\nspundown=0\ntemp=35\n")
	environment.report(t, "disk1", environment.now)
	collector := environment.collector()
	collector.refresh()

	start := make(chan struct{})
	done := make(chan error, 2)
	go func() {
		<-start
		for range 100 {
			collector.refresh()
		}
		done <- nil
	}()
	go func() {
		<-start
		for range 100 {
			readings, err := collector.snapshot()
			if err != nil {
				done <- err
				return
			}
			if len(readings) != 1 || readings[0].ID != "serial" {
				done <- errors.New("concurrent snapshot lost the disk reading")
				return
			}
		}
		done <- nil
	}()
	close(start)
	for range 2 {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
}
