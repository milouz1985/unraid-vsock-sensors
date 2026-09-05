package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"gopkg.in/ini.v1"
	"unraid-vsock-sensors/internal/sensors"
)

const (
	diskPollMargin             = 5 * time.Second
	defaultDiskPollInterval    = 2 * time.Minute
	maximumRecommendedDiskPoll = 5 * time.Minute
	diskCollectionTimeout      = 15 * time.Second
	maxConcurrentSMARTReads    = 4
)

type diskCollector struct {
	path     string
	interval time.Duration
	grace    time.Duration

	mu         sync.RWMutex
	readings   []sensors.Disk
	err        error
	validUntil time.Time
	failAfter  map[string]time.Time
	state      diskStateTracker
}

type diskStateTracker struct {
	lastValid   map[string]float64
	failedSince map[string]time.Time
}

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

func newDiskCollector(path string, interval, grace time.Duration) *diskCollector {
	return &diskCollector{
		path: path, interval: interval, grace: grace,
		err: errors.New("disk temperatures have not been collected yet"),
		state: diskStateTracker{
			lastValid: make(map[string]float64), failedSince: make(map[string]time.Time),
		},
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
	var readings []sensors.Disk
	if err == nil {
		probes := collectDiskProbes(ctx, rawDisks)
		// A collection timeout belongs to each probe which did not finish.
		// Successful probes remain usable and failed probes follow their own
		// grace period instead of invalidating the complete disk snapshot.
		readings = c.state.apply(rawDisks, probes, time.Now(), c.grace)
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	c.err = err
	if err != nil {
		c.readings = nil
		c.validUntil = time.Time{}
		c.failAfter = nil
		return
	}
	c.readings = readings
	c.validUntil = time.Now().Add(c.interval + diskCollectionTimeout)
	c.failAfter = c.state.failureDeadlines(c.grace)
}

func (c *diskCollector) read() ([]sensors.Disk, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.err != nil {
		return nil, c.err
	}
	if time.Now().After(c.validUntil) {
		return nil, errors.New("disk temperature snapshot expired")
	}
	readings := slices.Clone(c.readings)
	now := time.Now()
	for i := range readings {
		if deadline, ok := c.failAfter[readings[i].ID]; ok && !now.Before(deadline) {
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

func (s *diskStateTracker) apply(disks []unraidDisk, probes []diskProbe, now time.Time, grace time.Duration) []sensors.Disk {
	present := make(map[string]struct{}, len(disks))
	readings := make([]sensors.Disk, 0, len(disks))
	for i, disk := range disks {
		present[disk.id] = struct{}{}
		probe := probes[i]
		temperature, unavailable := probe.temperature, false
		switch {
		case probe.standby:
			temperature = 0
			delete(s.failedSince, disk.id)
		case probe.err == nil:
			s.lastValid[disk.id] = probe.temperature
			delete(s.failedSince, disk.id)
		default:
			started, ok := s.failedSince[disk.id]
			if !ok {
				started = now
				s.failedSince[disk.id] = started
			}
			if now.Sub(started) < grace {
				// Avoid the hwmon failsafe during a normal spin-up or one missed
				// SMART read. A persistent failure expires this grace explicitly.
				temperature = s.lastValid[disk.id]
			} else {
				temperature = 0
				unavailable = true
			}
		}
		readings = append(readings, disk.sensor(temperature, unavailable))
	}
	for id := range s.lastValid {
		if _, ok := present[id]; !ok {
			delete(s.lastValid, id)
		}
	}
	for id := range s.failedSince {
		if _, ok := present[id]; !ok {
			delete(s.failedSince, id)
		}
	}
	return readings
}

func (s *diskStateTracker) failureDeadlines(grace time.Duration) map[string]time.Time {
	deadlines := make(map[string]time.Time, len(s.failedSince))
	for id, started := range s.failedSince {
		deadlines[id] = started.Add(grace)
	}
	return deadlines
}

func diskPollingIntervals(configPath string) (configured, effective, grace time.Duration, err error) {
	file, err := os.Open(configPath)
	if err != nil {
		return 0, 0, 0, err
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "poll_attributes=") {
			continue
		}
		value := strings.Trim(strings.TrimPrefix(line, "poll_attributes="), `"`)
		seconds, parseErr := strconv.ParseUint(value, 10, 32)
		if parseErr != nil {
			return 0, 0, 0, fmt.Errorf("invalid poll_attributes %q: %w", value, parseErr)
		}
		configured = time.Duration(seconds) * time.Second
		break
	}
	if err := scanner.Err(); err != nil {
		return 0, 0, 0, err
	}
	if configured == 0 {
		return 0, defaultDiskPollInterval, defaultDiskPollInterval, nil
	}
	return configured, configured, configured + diskPollMargin, nil
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

func selectDisks(disks []sensors.Disk, selector string) []sensors.Disk {
	selector = strings.ToLower(selector)
	var matches []sensors.Disk

	for _, disk := range disks {
		var match bool
		switch selector {
		case "all":
			match = true
		case "nvme":
			match = !disk.IsExternal() && disk.Kind() == sensors.DiskKindNVMe
		case "hdd":
			match = !disk.IsExternal() && disk.Kind() == sensors.DiskKindHDD
		case "ssd":
			match = !disk.IsExternal() && disk.Kind() == sensors.DiskKindSATASSD
		default:
			match = strings.EqualFold(disk.Name, selector) || strings.EqualFold(disk.Device, selector)
		}
		if match {
			matches = append(matches, disk)
		}
	}

	return matches
}
