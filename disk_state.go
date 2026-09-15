// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"unraid-vsock-sensors/internal/sensors"
)

type diskState struct {
	lastValid   float64
	hasValid    bool
	failedSince time.Time
	hardFailed  bool
	lastValidAt time.Time
	lastSource  diskTemperatureSource
	cacheAt     time.Time
}

type diskStateTracker map[string]diskState

type diskObservation struct {
	disk        unraidDisk
	temperature float64
	standby     bool
	err         error
	source      diskTemperatureSource
	failure     diskFailurePolicy
	measuredAt  time.Time
	cacheAt     time.Time
}

type diskFailurePolicy uint8

const (
	diskFailureRetainPrevious diskFailurePolicy = iota
	diskFailureDiscardPrevious
)

func makeDiskObservations(disks []unraidDisk, smartDir string, now time.Time, freshness time.Duration) []diskObservation {
	observations := make([]diskObservation, 0, len(disks))
	for _, disk := range disks {
		observation := diskObservation{
			disk: disk, standby: disk.spundown, source: diskSourceEmhttpd,
			failure: diskFailureRetainPrevious,
		}
		if disk.spundown {
			observations = append(observations, observation)
			continue
		}

		observation.temperature, observation.err = parseCachedTemperature(disk.temperature)
		if observation.err == nil {
			info, err := os.Stat(filepath.Join(smartDir, disk.smartName))
			if err != nil {
				observation.err = fmt.Errorf("SMART cache for %s: %w", disk.name, err)
			} else {
				observation.cacheAt = info.ModTime()
				if now.Sub(info.ModTime()) > freshness {
					observation.err = fmt.Errorf("SMART cache for %s is stale by %s", disk.name, now.Sub(info.ModTime())-freshness)
				} else {
					observation.measuredAt = info.ModTime()
				}
			}
		}
		observations = append(observations, observation)
	}
	return observations
}

func parseCachedTemperature(raw string) (float64, error) {
	if raw == "" || raw == "*" {
		return 0, errors.New("cached temperature is unavailable")
	}
	temperature, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return 0, fmt.Errorf("cached temperature %q is not numeric", raw)
	}
	if math.IsNaN(temperature) || math.IsInf(temperature, 0) {
		return 0, fmt.Errorf("cached temperature %q is invalid", raw)
	}
	return temperature, nil
}

func (s diskStateTracker) markFailure(now time.Time) {
	for id, state := range s {
		if state.failedSince.IsZero() {
			state.failedSince = now
			s[id] = state
		}
	}
}

func (s diskStateTracker) apply(observations []diskObservation, now time.Time, grace time.Duration) []sensors.Disk {
	present := make(map[string]struct{}, len(observations))
	readings := make([]sensors.Disk, 0, len(observations))
	for _, observation := range observations {
		disk := observation.disk
		present[disk.id] = struct{}{}
		state := s[disk.id]
		temperature, unavailable := observation.temperature, false
		switch {
		case observation.standby:
			temperature = 0
			state.failedSince = time.Time{}
			state.hardFailed = false
		case observation.err == nil:
			state.lastValid = observation.temperature
			state.hasValid = true
			state.lastValidAt = observation.measuredAt
			if state.lastValidAt.IsZero() {
				state.lastValidAt = now
			}
			state.lastSource = observation.source
			if !observation.cacheAt.IsZero() {
				state.cacheAt = observation.cacheAt
			}
			state.failedSince = time.Time{}
			state.hardFailed = false
		default:
			if state.failedSince.IsZero() {
				state.failedSince = now
			}
			state.hardFailed = state.hardFailed || observation.failure == diskFailureDiscardPrevious
			if !state.hardFailed && state.hasValid && now.Sub(state.failedSince) < grace {
				temperature = state.lastValid
			} else {
				temperature = 0
				unavailable = true
			}
		}
		s[disk.id] = state
		readings = append(readings, sensors.Disk{
			ID: disk.id, Name: disk.name, Device: disk.device,
			Transport: disk.transport, Rotational: disk.rotational,
			Temp: temperature, Unavailable: unavailable,
		})
	}
	for id := range s {
		if _, ok := present[id]; !ok {
			delete(s, id)
		}
	}
	return readings
}
