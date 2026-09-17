// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"unraid-vsock-sensors/internal/sensors"
)

type hbaCollector struct {
	interval               time.Duration
	mode                   hbaMode
	backend                hbaBackendMode
	mu                     sync.RWMutex
	err                    error
	lastSuccessfulAt       time.Time
	lastErrorAt            time.Time
	lastSuccessfulSnapshot []sensors.HBA
	reader                 hbaSnapshotReader
}

type hbaCollectorStatus struct {
	interval               time.Duration
	mode                   hbaMode
	backend                hbaBackendMode
	lastSuccessfulAt       time.Time
	lastErrorAt            time.Time
	err                    error
	lastSuccessfulSnapshot []sensors.HBA
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

// hbaStableID gives every backend the same stable sensor key. Prefer the SAS
// address shared by mpt3ctl and StorCLI, then progressively weaker fallbacks.
// A pci: fallback identifies the current PCI location, not necessarily the same
// physical controller after hardware replacement.
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

func newHBAReaderForBackend(mode hbaBackendMode) hbaSnapshotReader {
	if mode == hbaBackendStorCLI {
		return &storCLIReader{
			discoverMetadata: discoverStorCLIHBAs,
			readTemperatures: readStorCLITemperatures,
		}
	}
	return newMPT3Reader()
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

func resolveHBAInterval(backend hbaBackendMode, interval time.Duration, explicit bool) (time.Duration, error) {
	switch backend {
	case hbaBackendMPT3CTL:
		if !explicit {
			interval = 15 * time.Second
		}
	case hbaBackendStorCLI:
		if !explicit {
			interval = 30 * time.Second
		}
	default:
		return 0, fmt.Errorf("invalid HBA backend %q", backend)
	}
	if interval <= 0 {
		return 0, errors.New("hba-interval must be greater than zero")
	}
	return interval, nil
}

func newConfiguredHBACollector(interval time.Duration, mode hbaMode, backend hbaBackendMode) *hbaCollector {
	c := &hbaCollector{
		interval: interval,
		mode:     mode,
		backend:  backend,
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
	finishedAt := time.Now()
	if !finishedAt.Before(deadline) {
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
		c.lastErrorAt = finishedAt
		return
	}
	c.lastSuccessfulAt = finishedAt
	c.lastErrorAt = time.Time{}
	c.lastSuccessfulSnapshot = slices.Clone(readings)
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
	if !time.Now().Before(c.lastSuccessfulAt.Add(c.interval + hbaCollectionTimeout)) {
		return nil, errors.New("HBA temperature snapshot expired")
	}
	return slices.Clone(c.lastSuccessfulSnapshot), nil
}

func (c *hbaCollector) status() hbaCollectorStatus {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return hbaCollectorStatus{
		interval: c.interval, mode: c.mode, backend: c.backend,
		lastSuccessfulAt: c.lastSuccessfulAt, lastErrorAt: c.lastErrorAt, err: c.err,
		lastSuccessfulSnapshot: slices.Clone(c.lastSuccessfulSnapshot),
	}
}
