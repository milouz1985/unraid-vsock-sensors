package main

import (
	"context"
	"errors"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"gopkg.in/ini.v1"
	"unraid-vsock-sensors/internal/sensors"
)

const (
	diskFailureMargin              = 5 * time.Second
	defaultDiskInterval            = 30 * time.Second
	maximumRecommendedDiskInterval = 5 * time.Minute
	diskCollectionTimeout          = 15 * time.Second
	maxConcurrentSMARTReads        = 4
)

type diskCollector struct {
	path     string
	interval time.Duration
	grace    time.Duration

	mu        sync.RWMutex
	readings  []sensors.Disk
	err       error
	updatedAt time.Time
	state     diskStateTracker
}

type diskState struct {
	lastValid   float64
	failedSince time.Time
}

type diskStateTracker map[string]diskState

// unraidDisk is the inventory and power state read from Unraid's disks.ini.
// Temperature is collected separately through Unraid's smartctl_type helper.
type unraidDisk struct {
	id, name, device, transport string
	rotational                  bool
	spundown                    bool
}

type diskProbe struct {
	temperature float64
	standby     bool
	err         error
}

func (d unraidDisk) sensor(temp float64, unavailable bool) sensors.Disk {
	return sensors.Disk{
		ID: d.id, Name: d.name, Device: d.device, Transport: d.transport,
		Rotational: d.rotational, Temp: temp, Unavailable: unavailable,
	}
}

func newDiskCollector(path string, interval time.Duration) *diskCollector {
	return &diskCollector{
		path: path, interval: interval, grace: interval + diskFailureMargin,
		err:   errors.New("disk temperatures have not been collected yet"),
		state: make(diskStateTracker),
	}
}

func (c *diskCollector) run(ctx context.Context) {
	c.refresh(ctx)
	timer := time.NewTimer(c.interval)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			c.refresh(ctx)
			timer.Reset(c.interval)
		}
	}
}

func (c *diskCollector) refresh(parent context.Context) {
	ctx, cancel := context.WithTimeout(parent, diskCollectionTimeout)
	defer cancel()

	rawDisks, err := readDisks(c.path)
	var probes []diskProbe
	if err == nil {
		probes = collectDiskProbes(ctx, rawDisks)
	}

	now := time.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	c.err = err
	if err != nil {
		c.readings = nil
		c.updatedAt = time.Time{}
		return
	}
	// A collection timeout belongs to each probe which did not finish.
	// Successful probes remain usable and failed probes follow their own grace
	// period instead of invalidating the complete disk snapshot.
	c.readings = c.state.apply(rawDisks, probes, now, c.grace)
	c.updatedAt = now
}

func (c *diskCollector) snapshot() ([]sensors.Disk, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.err != nil {
		return nil, c.err
	}
	readings := slices.Clone(c.readings)
	now := time.Now()
	if !now.Before(c.updatedAt.Add(c.interval + diskCollectionTimeout)) {
		return nil, errors.New("disk temperature snapshot expired")
	}
	for i := range readings {
		state := c.state[readings[i].ID]
		if !state.failedSince.IsZero() && !now.Before(state.failedSince.Add(c.grace)) {
			readings[i].Temp = 0
			readings[i].Unavailable = true
		}
	}
	return readings, nil
}

func collectDiskProbes(ctx context.Context, disks []unraidDisk) []diskProbe {
	probes := make([]diskProbe, len(disks))
	var wg sync.WaitGroup
	limit := make(chan struct{}, maxConcurrentSMARTReads)
	for i := range disks {
		if disks[i].spundown {
			probes[i].standby = true
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case limit <- struct{}{}:
				defer func() { <-limit }()
			case <-ctx.Done():
				probes[i].err = ctx.Err()
				return
			}
			probes[i].temperature, probes[i].standby, probes[i].err = readSMARTTemperature(ctx, disks[i])
		}()
	}
	wg.Wait()
	return probes
}

func (s diskStateTracker) apply(disks []unraidDisk, probes []diskProbe, now time.Time, grace time.Duration) []sensors.Disk {
	present := make(map[string]struct{}, len(disks))
	readings := make([]sensors.Disk, 0, len(disks))
	for i, disk := range disks {
		present[disk.id] = struct{}{}
		probe := probes[i]
		state := s[disk.id]
		temperature, unavailable := probe.temperature, false
		switch {
		case probe.standby:
			temperature = 0
			state.failedSince = time.Time{}
		case probe.err == nil:
			state.lastValid = probe.temperature
			state.failedSince = time.Time{}
		default:
			if state.failedSince.IsZero() {
				state.failedSince = now
			}
			if now.Sub(state.failedSince) < grace {
				// Avoid the hwmon failsafe during a normal spin-up or one missed
				// SMART read. A persistent failure expires this grace explicitly.
				temperature = state.lastValid
			} else {
				temperature = 0
				unavailable = true
			}
		}
		s[disk.id] = state
		readings = append(readings, disk.sensor(temperature, unavailable))
	}
	for id := range s {
		if _, ok := present[id]; !ok {
			delete(s, id)
		}
	}
	return readings
}

// readDisks reads inventory and power state from Unraid. smartctl_type uses the
// same logical names to apply Unraid's controller-specific SMART settings.
func readDisks(disksINIPath string) ([]unraidDisk, error) {
	config, err := ini.Load(disksINIPath)
	if err != nil {
		return nil, err
	}

	var disks []unraidDisk
	for _, section := range config.Sections() {
		if section.Name() == ini.DefaultSection {
			continue
		}
		name := strings.Trim(section.Name(), "\"")
		id := strings.TrimSpace(section.Key("id").String())
		device := strings.TrimSpace(section.Key("device").String())
		status := strings.TrimSpace(section.Key("status").String())
		// disks.ini contains sections for every possible array slot, including
		// unassigned DISK_NP entries, plus the Unraid boot flash device.
		if id == "" || device == "" || strings.EqualFold(status, "DISK_NP") || strings.EqualFold(name, "flash") {
			continue
		}

		disks = append(disks, unraidDisk{
			id: id, name: name, device: device,
			transport:  strings.ToLower(section.Key("transport").String()),
			rotational: strings.TrimSpace(section.Key("rotational").String()) == "1",
			spundown:   strings.TrimSpace(section.Key("spundown").String()) == "1",
		})
	}

	sort.Slice(disks, func(i, j int) bool { return disks[i].name < disks[j].name })
	return disks, nil
}
