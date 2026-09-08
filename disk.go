package main

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"gopkg.in/ini.v1"
	"unraid-vsock-sensors/internal/sensors"
)

const (
	defaultDiskInterval            = 30 * time.Second
	diskRetryDelay                 = 2 * time.Second
	maxDiskRetries                 = 2
	maximumRecommendedDiskInterval = 5 * time.Minute
	diskCollectionTimeout          = 15 * time.Second
	maxConcurrentSMARTReads        = 4
)

type diskCollector struct {
	path       string
	interval   time.Duration
	retryDelay time.Duration

	mu        sync.RWMutex
	readings  []sensors.Disk
	err       error
	updatedAt time.Time
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

func newDiskCollector(path string, interval time.Duration) *diskCollector {
	return &diskCollector{
		path: path, interval: interval, retryDelay: min(interval, diskRetryDelay),
		err: errors.New("disk temperatures have not been collected yet"),
	}
}

func (c *diskCollector) run(ctx context.Context) {
	retries := 0
	for {
		failed := c.refresh(ctx)
		// Give a transient failure two chances to recover before virt_temp's
		// stale timeout, but never turn a persistent failure into a tight loop.
		var delay time.Duration
		delay, retries = nextDiskCollection(failed, retries, c.interval, c.retryDelay)

		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

func nextDiskCollection(failed bool, retries int, interval, retryDelay time.Duration) (time.Duration, int) {
	if failed && retries < maxDiskRetries {
		return retryDelay, retries + 1
	}
	return interval, 0
}

func (c *diskCollector) refresh(parent context.Context) bool {
	ctx, cancel := context.WithTimeout(parent, diskCollectionTimeout)
	defer cancel()

	rawDisks, err := readDisks(c.path)
	var probes []diskProbe
	failed := err != nil
	if err == nil {
		probes = collectDiskProbes(ctx, rawDisks)
		for _, probe := range probes {
			failed = failed || probe.err != nil
		}
	}

	now := time.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	c.err = err
	if err != nil {
		c.readings = nil
		c.updatedAt = time.Time{}
		return failed
	}
	// A collection timeout belongs to each probe which did not finish.
	// Successful probes remain usable while failed probes are omitted from hwmon
	// commits and retried twice before the normal collection interval resumes.
	c.readings = makeDiskReadings(rawDisks, probes)
	c.updatedAt = now
	return failed
}

func (c *diskCollector) snapshot() ([]sensors.Disk, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.err != nil {
		return nil, c.err
	}
	if !time.Now().Before(c.updatedAt.Add(c.interval + diskCollectionTimeout)) {
		return nil, errors.New("disk temperature snapshot expired")
	}
	return slices.Clone(c.readings), nil
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

func makeDiskReadings(disks []unraidDisk, probes []diskProbe) []sensors.Disk {
	readings := make([]sensors.Disk, 0, len(disks))
	for i, disk := range disks {
		probe := probes[i]
		temperature := probe.temperature
		if probe.standby || probe.err != nil {
			temperature = 0
		}
		readings = append(readings, sensors.Disk{
			ID: disk.id, Name: disk.name, Device: disk.device,
			Transport: disk.transport, Rotational: disk.rotational,
			Temp: temperature, Unavailable: probe.err != nil,
		})
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
	sections := 0
	for _, section := range config.Sections() {
		if section.Name() == ini.DefaultSection {
			continue
		}
		sections++
		name := strings.Trim(section.Name(), "\"")
		transport := strings.ToLower(strings.TrimSpace(section.Key("transport").String()))
		id := strings.TrimSpace(section.Key("id").String())
		device := strings.TrimSpace(section.Key("device").String())
		status := strings.TrimSpace(section.Key("status").String())
		// disks.ini contains sections for every possible array slot, including
		// unassigned DISK_NP entries, the Unraid boot flash device and external
		// USB disks. USB temperatures have no consumer in the push protocol.
		if strings.EqualFold(status, "DISK_NP") || strings.EqualFold(name, "flash") ||
			transport == "usb" {
			continue
		}
		if id == "" {
			return nil, fmt.Errorf("active disk %q has no stable ID", name)
		}
		if device == "" {
			return nil, fmt.Errorf("active disk %q has no device", name)
		}

		disks = append(disks, unraidDisk{
			id: id, name: name, device: device,
			transport:  transport,
			rotational: strings.TrimSpace(section.Key("rotational").String()) == "1",
			spundown:   strings.TrimSpace(section.Key("spundown").String()) == "1",
		})
	}
	if sections == 0 {
		return nil, errors.New("disk inventory contains no sections")
	}

	sort.Slice(disks, func(i, j int) bool { return disks[i].name < disks[j].name })
	return disks, nil
}
