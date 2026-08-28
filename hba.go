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
	readTemperatures func(context.Context, []int) (map[int]float64, error)
}

type hbaBackendMode string

const (
	hbaBackendMPT3CTL hbaBackendMode = "mpt3ctl"
	hbaBackendStorCLI hbaBackendMode = "storcli"
)

type hbaReader struct {
	metadata map[int]hbaMetadata
	backend  hbaBackend
}

func newHBAReader() *hbaReader {
	return newHBAReaderForBackend(hbaBackendMPT3CTL)
}

func newHBAReaderForBackend(mode hbaBackendMode) *hbaReader {
	backend := hbaBackend{name: "mpt3ctl", discover: discoverMPT3HBAs, readTemperatures: readMPT3Temperatures}
	switch mode {
	case hbaBackendStorCLI:
		backend = hbaBackend{name: "storcli", discover: discoverStorCLIHBAs, readTemperatures: readStorCLITemperatures}
	}
	return &hbaReader{backend: backend}
}

func (r *hbaReader) collect(ctx context.Context) ([]sensors.HBA, error) {
	if r.metadata == nil {
		metadata, err := r.backend.discover(ctx)
		if err != nil {
			return nil, fmt.Errorf("%s discovery: %w", r.backend.name, err)
		}
		r.metadata = metadata
	}
	controllers := make([]int, 0, len(r.metadata))
	for controller := range r.metadata {
		controllers = append(controllers, controller)
	}
	sort.Ints(controllers)
	temperatures, err := r.backend.readTemperatures(ctx, controllers)
	if err != nil {
		return nil, err
	}
	return buildHBAReadings(temperatures, r.metadata), nil
}

func buildHBAReadings(temperatures map[int]float64, metadata map[int]hbaMetadata) []sensors.HBA {
	readings := make([]sensors.HBA, 0, len(temperatures))
	for controller, temperature := range temperatures {
		identity, ok := metadata[controller]
		if !ok {
			continue
		}
		readings = append(readings, sensors.HBA{
			ID: identity.id, Model: identity.model, PCIAddress: identity.pciAddress, Temp: temperature,
		})
	}
	sort.Slice(readings, func(i, j int) bool { return readings[i].ID < readings[j].ID })
	return readings
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
	return newConfiguredHBACollector(interval, mode, hbaBackendMPT3CTL)
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
		if selector == "all" || strings.EqualFold(sensor.ID, selector) {
			matches = append(matches, sensor)
		}
	}
	return matches
}
