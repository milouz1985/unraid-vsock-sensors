package main

import (
	"context"
	"errors"
	"fmt"
	"maps"
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

type hbaBackendMode string

const (
	hbaBackendMPT3CTL hbaBackendMode = "mpt3ctl"
	hbaBackendStorCLI hbaBackendMode = "storcli"
)

type hbaSnapshotReader interface {
	collect(context.Context) ([]sensors.HBA, error)
}

type storCLIReader struct {
	metadata         map[int]hbaMetadata
	topology         string
	topologyKnown    bool
	discoverMetadata func(context.Context) (map[int]hbaMetadata, error)
	readTemperatures func(context.Context) (map[int]float64, error)
	readTopology     func() (string, error)
}

func newHBAReaderForBackend(mode hbaBackendMode) hbaSnapshotReader {
	if mode == hbaBackendStorCLI {
		return &storCLIReader{
			discoverMetadata: discoverStorCLIHBAs,
			readTemperatures: readStorCLITemperatures,
			readTopology:     readHBATopology,
		}
	}
	return newMPT3Reader()
}

func (r *storCLIReader) collect(ctx context.Context) ([]sensors.HBA, error) {
	freshDiscovery := false
	topologyChanged := false
	if r.metadata != nil {
		if topology, err := r.readTopology(); err == nil {
			// If discovery could not establish a baseline, rediscover as soon as
			// sysfs becomes readable: the hardware may have changed meanwhile.
			topologyChanged = !r.topologyKnown || topology != r.topology
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

func (r *storCLIReader) discover(ctx context.Context) error {
	metadata, err := r.discoverMetadata(ctx)
	if err != nil {
		return fmt.Errorf("storcli discovery: %w", err)
	}
	r.metadata = metadata
	if topology, topologyErr := r.readTopology(); topologyErr == nil {
		r.topology, r.topologyKnown = topology, true
	}
	return nil
}

func (r *storCLIReader) read(ctx context.Context) ([]sensors.HBA, error) {
	controllers := sortedIntKeys(r.metadata)
	temperatures, err := r.readTemperatures(ctx)
	if err != nil {
		return nil, err
	}
	if err := validateHBAControllerSet(controllers, temperatures); err != nil {
		return nil, err
	}
	return buildHBAReadings(temperatures, r.metadata), nil
}

func validateHBAControllerSet(controllers []int, temperatures map[int]float64) error {
	actual := sortedIntKeys(temperatures)
	if !slices.Equal(controllers, actual) {
		return fmt.Errorf("HBA controller set changed: expected %v, got %v", controllers, actual)
	}
	return nil
}

func sortedIntKeys[V any](values map[int]V) []int {
	return slices.Sorted(maps.Keys(values))
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
