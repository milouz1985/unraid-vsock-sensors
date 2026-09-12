// SPDX-License-Identifier: GPL-3.0-or-later

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
	interval  time.Duration
	mode      hbaMode
	mu        sync.RWMutex
	readings  []sensors.HBA
	err       error
	updatedAt time.Time
	reader    hbaSnapshotReader
}

type hbaMetadata struct {
	id         string
	model      string
	pciAddress string
}

const (
	maxSASAddressSize           = 16
	maxPCIAddressSize           = len("0000:00:00.0")
	maxMegaRAIDControllerSerial = 32 // Firmware controller information uses serial_no[32].
	maxSASHBAStableIDSize       = len("sas:") + maxSASAddressSize
	maxPCIHBAStableIDSize       = len("pci:") + maxPCIAddressSize
	maxSerialHBAStableIDSize    = len("serial:") + maxMegaRAIDControllerSerial
	maxHBAStableIDSize          = max(maxSASHBAStableIDSize, maxPCIHBAStableIDSize, maxSerialHBAStableIDSize)
)

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
	discoverMetadata func(context.Context) (map[int]hbaMetadata, error)
	readTemperatures func(context.Context) (map[int]float64, error)
}

func newHBAReaderForBackend(mode hbaBackendMode) hbaSnapshotReader {
	if mode == hbaBackendStorCLI {
		return &storCLIReader{
			discoverMetadata: discoverStorCLIHBAs,
			readTemperatures: readStorCLITemperatures,
		}
	}
	return newMPT3Reader()
}

func (r *storCLIReader) collect(ctx context.Context) ([]sensors.HBA, error) {
	freshDiscovery := false
	if r.metadata == nil {
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
	// Rediscover at most once per collection. If discovery already happened in
	// this call, leave the metadata invalidated so the next collection retries.
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
		identity := metadata[controller]
		readings = append(readings, sensors.HBA{
			ID: identity.id, Model: identity.model, PCIAddress: identity.pciAddress, Temp: temperature,
		})
	}
	sort.Slice(readings, func(i, j int) bool { return readings[i].ID < readings[j].ID })
	return readings
}

type hbaMode string

const (
	hbaModeEnabled  hbaMode = "enabled"
	hbaModeDisabled hbaMode = "disabled"
)

const hbaCollectionTimeout = 15 * time.Second

var (
	errNoHBA                 = errors.New("no HBA controllers found")
	errHBABackendUnavailable = errors.New("HBA backend unavailable")
)

func newConfiguredHBACollector(interval time.Duration, mode hbaMode, backend hbaBackendMode) *hbaCollector {
	c := &hbaCollector{
		interval: interval,
		mode:     mode,
		err:      errors.New("HBA temperatures have not been collected yet"),
		reader:   newHBAReaderForBackend(backend),
	}
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
	deadline, _ := ctx.Deadline()

	// Do not hold c.mu during backend I/O: a synchronous ioctl may outlive its
	// context, while snapshots must remain readable and expire independently.
	readings, err := c.reader.collect(ctx)
	if !time.Now().Before(deadline) {
		err = context.DeadlineExceeded
	} else if err == nil {
		// Do not accept a successful result if the parent was canceled while
		// the collector was returning, before this collection deadline.
		err = ctx.Err()
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.err = err
	if err != nil {
		c.readings = nil
		c.updatedAt = time.Time{}
		return
	}
	c.readings = readings
	c.updatedAt = time.Now()
}

func (c *hbaCollector) snapshot() ([]sensors.HBA, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.mode == hbaModeDisabled {
		// Disabled is an authoritative empty inventory, not a missing snapshot.
		return []sensors.HBA{}, nil
	}
	if c.err != nil {
		return nil, c.err
	}
	if !time.Now().Before(c.updatedAt.Add(c.interval + hbaCollectionTimeout)) {
		return nil, errors.New("HBA temperature snapshot expired")
	}
	return slices.Clone(c.readings), nil
}
