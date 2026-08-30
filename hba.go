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

// hbaStableID gives every backend the same controller identity. Prefer the SAS
// address shared by mpt3ctl and StorCLI, then progressively weaker fallbacks.
func hbaStableID(sasAddress, pciAddress, serial string) string {
	if sasAddress = hbaIdentityValue(sasAddress); sasAddress != "" {
		if sasAddress = normalizeSASAddress(sasAddress); sasAddress != "" {
			return "sas:" + sasAddress
		}
	}
	if pciAddress != "" {
		return "pci:" + pciAddress
	}
	if serial = hbaIdentityValue(serial); serial != "" {
		return "serial:" + strings.ToLower(serial)
	}
	return ""
}

func normalizeSASAddress(address string) string {
	address = strings.TrimPrefix(strings.ToLower(strings.TrimSpace(address)), "0x")
	address = strings.NewReplacer(":", "", "-", "", " ", "").Replace(address)
	value, err := strconv.ParseUint(address, 16, 64)
	if err != nil || value == 0 {
		return ""
	}
	return fmt.Sprintf("%016x", value)
}

func hbaIdentityValue(value string) string {
	value = strings.TrimSpace(value)
	switch strings.ToLower(value) {
	case "", "n/a", "na", "none", "unknown":
		return ""
	default:
		return value
	}
}

type hbaBackend struct {
	name             string
	collect          func(context.Context) ([]sensors.HBA, error)
	discover         func(context.Context) (map[int]hbaMetadata, error)
	readTemperatures func(context.Context, []int) (map[int]float64, error)
	topology         func() (string, error)
}

type hbaBackendMode string

const (
	hbaBackendMPT3CTL hbaBackendMode = "mpt3ctl"
	hbaBackendStorCLI hbaBackendMode = "storcli"
)

type hbaReader struct {
	metadata      map[int]hbaMetadata
	topology      string
	topologyKnown bool
	backend       hbaBackend
}

func newHBAReaderForBackend(mode hbaBackendMode) *hbaReader {
	backend := hbaBackend{name: "mpt3ctl", collect: readMPT3Snapshot}
	switch mode {
	case hbaBackendStorCLI:
		backend = hbaBackend{
			name: "storcli", discover: discoverStorCLIHBAs, readTemperatures: readStorCLITemperatures,
			topology: readHBATopology,
		}
	}
	return &hbaReader{backend: backend}
}

func (r *hbaReader) collect(ctx context.Context) ([]sensors.HBA, error) {
	if r.backend.collect != nil {
		return r.backend.collect(ctx)
	}
	freshDiscovery := false
	topologyChanged := false
	if r.metadata != nil && r.backend.topology != nil {
		if topology, err := r.backend.topology(); err == nil {
			topologyChanged = r.topologyKnown && topology != r.topology
		}
	}
	if r.metadata == nil || topologyChanged {
		if err := r.discover(ctx); err != nil {
			return nil, err
		}
		freshDiscovery = true
	}
	readings, err := r.read(ctx)
	if err == nil {
		return readings, nil
	}
	r.metadata = nil
	if freshDiscovery {
		return nil, err
	}
	if discoveryErr := r.discover(ctx); discoveryErr != nil {
		return nil, errors.Join(err, discoveryErr)
	}
	return r.read(ctx)
}

func (r *hbaReader) discover(ctx context.Context) error {
	metadata, err := r.backend.discover(ctx)
	if err != nil {
		return fmt.Errorf("%s discovery: %w", r.backend.name, err)
	}
	r.metadata = metadata
	if r.backend.topology != nil {
		if topology, topologyErr := r.backend.topology(); topologyErr == nil {
			r.topology, r.topologyKnown = topology, true
		}
	}
	return nil
}

func (r *hbaReader) read(ctx context.Context) ([]sensors.HBA, error) {
	controllers := make([]int, 0, len(r.metadata))
	for controller := range r.metadata {
		controllers = append(controllers, controller)
	}
	sort.Ints(controllers)
	temperatures, err := r.backend.readTemperatures(ctx, controllers)
	if err != nil {
		return nil, err
	}
	if err := validateHBAControllerSet(controllers, temperatures); err != nil {
		return nil, err
	}
	return buildHBAReadings(temperatures, r.metadata), nil
}

func validateHBAControllerSet(controllers []int, temperatures map[int]float64) error {
	if len(controllers) != len(temperatures) {
		return fmt.Errorf("HBA controller set changed: expected %v, got %v", controllers, sortedHBAControllers(temperatures))
	}
	for _, controller := range controllers {
		if _, ok := temperatures[controller]; !ok {
			return fmt.Errorf("HBA controller set changed: expected %v, got %v", controllers, sortedHBAControllers(temperatures))
		}
	}
	return nil
}

func sortedHBAControllers(temperatures map[int]float64) []int {
	controllers := make([]int, 0, len(temperatures))
	for controller := range temperatures {
		controllers = append(controllers, controller)
	}
	sort.Ints(controllers)
	return controllers
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
