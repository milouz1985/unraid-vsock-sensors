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
	thermalState  diskThermalState
	wakeStartedAt time.Time
	lastValidAt   time.Time
	lastSource    diskTemperatureSource
	cacheAt       time.Time
}

type diskStateTracker map[string]diskState

type diskThermalState uint8

const (
	diskThermalUnavailable diskThermalState = iota
	diskThermalValid
	diskThermalStandby
	diskThermalWaking
)

type diskObservation struct {
	disk        unraidDisk
	temperature float64
	standby     bool
	err         error
	source      diskTemperatureSource
	measuredAt  time.Time
	cacheAt     time.Time
}

func makeDiskObservations(disks []unraidDisk, smartDir string, now time.Time, freshness time.Duration) []diskObservation {
	observations := make([]diskObservation, 0, len(disks))
	for _, disk := range disks {
		observation := diskObservation{
			disk: disk, standby: disk.spundown, source: diskSourceEmhttpd,
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

// invalidateContinuity prevents a previously observed standby state from
// authorizing wake grace after an interval where the inventory was unknown.
// Historical measurement metadata remains available to diagnostics.
func (s diskStateTracker) invalidateContinuity() {
	for id, state := range s {
		state.thermalState = diskThermalUnavailable
		state.wakeStartedAt = time.Time{}
		s[id] = state
	}
}

// wakeGrace is the maximum time Unraid has to publish the first fresh SMART
// temperature after an uninterrupted observed transition from standby to
// active. In standby or waking state, 0 is a synthetic control value; the
// thermal state distinguishes it from a valid physical 0 °C measurement.
func (s diskStateTracker) apply(observations []diskObservation, now time.Time, wakeGrace time.Duration) []sensors.Disk {
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
			state.thermalState = diskThermalStandby
			state.wakeStartedAt = time.Time{}
		case observation.err == nil:
			state.thermalState = diskThermalValid
			state.wakeStartedAt = time.Time{}
			state.lastValidAt = observation.measuredAt
			if state.lastValidAt.IsZero() {
				state.lastValidAt = now
			}
			state.lastSource = observation.source
			if !observation.cacheAt.IsZero() {
				state.cacheAt = observation.cacheAt
			}
		default:
			if observation.source == diskSourceEmhttpd && state.thermalState == diskThermalStandby {
				state.thermalState = diskThermalWaking
				state.wakeStartedAt = now
			}
			if observation.source == diskSourceEmhttpd && state.thermalState == diskThermalWaking &&
				now.Sub(state.wakeStartedAt) < wakeGrace {
				// Preserve only the synthetic standby value while Unraid obtains
				// the first post-wake sample; never reuse the pre-standby reading.
				temperature = 0
			} else {
				temperature = 0
				unavailable = true
				state.thermalState = diskThermalUnavailable
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
