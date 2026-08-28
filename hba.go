package main

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"unraid-vsock-sensors/internal/sensors"
)

type hbaCollector struct {
	interval        time.Duration
	mode            hbaMode
	mu              sync.RWMutex
	readings        []sensors.HBA
	err             error
	collectSnapshot func(context.Context) ([]sensors.HBA, error)
}

type hbaMetadata struct{ id, model, pciAddress string }

type hbaBackend struct {
	name             string
	discover         func(context.Context) (map[int]hbaMetadata, error)
	readTemperatures func(context.Context, []int) ([]sensors.HBA, error)
}

type hbaBackendMode string

const (
	hbaBackendAuto    hbaBackendMode = "auto"
	hbaBackendMPT3CTL hbaBackendMode = "mpt3ctl"
	hbaBackendStorCLI hbaBackendMode = "storcli"
)

type hbaReader struct {
	metadata map[int]hbaMetadata
	backend  *hbaBackend
	backends []hbaBackend
}

func newHBAReader() *hbaReader {
	return newHBAReaderForBackend(hbaBackendAuto)
}

func newHBAReaderForBackend(mode hbaBackendMode) *hbaReader {
	backends := []hbaBackend{
		{name: "mpt3ctl", discover: discoverMPT3HBAs, readTemperatures: readMPT3Temperatures},
		{name: "storcli", discover: discoverStorCLIHBAs, readTemperatures: readStorCLITemperatures},
	}
	switch mode {
	case hbaBackendMPT3CTL:
		backends = backends[:1]
	case hbaBackendStorCLI:
		backends = backends[1:]
	}
	return &hbaReader{backends: backends}
}

func (r *hbaReader) collect(ctx context.Context) ([]sensors.HBA, error) {
	if r.backend == nil {
		var unavailable []error
		for i := range r.backends {
			metadata, err := r.backends[i].discover(ctx)
			if err == nil {
				r.backend, r.metadata = &r.backends[i], metadata
				break
			}
			if !errors.Is(err, errHBABackendUnavailable) && !errors.Is(err, errNoHBA) {
				return nil, err
			}
			unavailable = append(unavailable, fmt.Errorf("%s: %w", r.backends[i].name, err))
		}
		if r.backend == nil {
			return nil, fmt.Errorf("no HBA backend found: %w", errors.Join(unavailable...))
		}
	}
	controllers := make([]int, 0, len(r.metadata))
	for controller := range r.metadata {
		controllers = append(controllers, controller)
	}
	sort.Ints(controllers)
	readings, err := r.backend.readTemperatures(ctx, controllers)
	if err != nil {
		return nil, err
	}
	return applyHBAMetadata(readings, r.metadata), nil
}

func applyHBAMetadata(readings []sensors.HBA, metadata map[int]hbaMetadata) []sensors.HBA {
	matched := make([]sensors.HBA, 0, len(readings))
	for _, reading := range readings {
		controller, err := hbaControllerNumber(reading.Name)
		if err != nil {
			continue
		}
		identity, ok := metadata[controller]
		if !ok {
			continue
		}
		reading.ID, reading.Model, reading.PCIAddress = identity.id, identity.model, identity.pciAddress
		matched = append(matched, reading)
	}
	return matched
}

func hbaControllerNumber(name string) (int, error) {
	if !strings.HasPrefix(name, "hba") {
		return 0, fmt.Errorf("invalid HBA name %q", name)
	}
	return strconv.Atoi(strings.TrimPrefix(name, "hba"))
}

type hbaMode string

const (
	hbaModeEnabled       hbaMode = "enabled"
	hbaModeDisabled      hbaMode = "disabled"
	hbaCollectionTimeout         = 15 * time.Second
)

var (
	errNoHBA                 = errors.New("no HBA controllers found")
	errHBABackendUnavailable = errors.New("HBA backend unavailable")
)

func newHBACollector(interval time.Duration, mode hbaMode) *hbaCollector {
	return newConfiguredHBACollector(interval, mode, hbaBackendAuto)
}

func newConfiguredHBACollector(interval time.Duration, mode hbaMode, backend hbaBackendMode) *hbaCollector {
	reader := newHBAReaderForBackend(backend)
	c := &hbaCollector{interval: interval, mode: mode, err: errors.New("HBA temperatures have not been collected yet"), collectSnapshot: reader.collect}
	if mode == hbaModeDisabled {
		c.err = nil
	}
	return c
}

func (c *hbaCollector) run(ctx context.Context) {
	if c.mode == hbaModeDisabled {
		return
	}
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

func (c *hbaCollector) refresh(parent context.Context) {
	ctx, cancel := context.WithTimeout(parent, hbaCollectionTimeout)
	defer cancel()
	readings, err := c.collectSnapshot(ctx)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.err = err
	if err != nil {
		c.readings = nil
		return
	}
	c.readings = readings
}

func (c *hbaCollector) read() ([]sensors.HBA, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return slices.Clone(c.readings), c.err
}

func selectHBAs(hbas []sensors.HBA, selector string) []sensors.HBA {
	selector = strings.ToLower(selector)
	var matches []sensors.HBA
	for _, sensor := range hbas {
		if selector == "all" || sensor.Name == selector {
			matches = append(matches, sensor)
		}
	}
	return matches
}
