// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"strings"
	"syscall"
)

const (
	maxHWMonIDSize    = 63
	maxHWMonLabelSize = 95
)

func publishHWMonFamily(
	path, namespace string,
	inventory *hwmonInventory,
	current []hwmonSample,
) (bool, error) {
	return publishHWMonFamilyWithWriter(path, namespace, inventory, current, writeHWMonSamples)
}

func publishHWMonFamilyWithWriter(
	path, namespace string,
	inventory *hwmonInventory,
	current []hwmonSample,
	write func(string, string, string, []hwmonSample) error,
) (bool, error) {
	if !inventory.initialized || !sameHWMonConfiguration(inventory.sensors, current) {
		if err := write(path, namespace, "configure", current); err != nil {
			return false, err
		}
		inventory.initialized = true
		inventory.sensors = sensorsFromSamples(current)
		return true, nil
	}

	if err := write(path, namespace, "commit", current); err != nil {
		if !errors.Is(err, syscall.ESTALE) {
			return false, err
		}
		if configureErr := write(path, namespace, "configure", current); configureErr != nil {
			return false, fmt.Errorf("reconfigure stale %s inventory: %w", namespace, configureErr)
		}
		inventory.sensors = sensorsFromSamples(current)
		return true, nil
	}
	return false, nil
}

// Each operation needs a fresh file: virt_temp stages records per open session
// and accepts only one configure or commit on that session.
func writeHWMonSamples(path, namespace, operation string, readings []hwmonSample) (err error) {
	device, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	defer func() {
		err = errors.Join(err, device.Close())
	}()
	return encodeHWMonSamples(device, namespace, operation, readings)
}

// encodeHWMonSamples writes the text protocol consumed by virt-temp's
// device_write(). Each sample is one tab-separated write. The final configure
// or commit write makes the kernel validate every staged sample before applying
// the operation.
func encodeHWMonSamples(out io.Writer, namespace, operation string, readings []hwmonSample) error {
	prefix := namespace + ":"
	ids := make(map[string]struct{}, len(readings))
	if namespace != "disk" && namespace != "hba" {
		return fmt.Errorf("invalid hwmon namespace %q", namespace)
	}
	if operation != "configure" && operation != "commit" {
		return fmt.Errorf("invalid hwmon operation %q", operation)
	}
	for _, reading := range readings {
		sensor := reading.sensor
		if !strings.HasPrefix(sensor.id, prefix) || len(sensor.id) > maxHWMonIDSize ||
			strings.ContainsAny(sensor.id, "\t\r\n") {
			return fmt.Errorf("invalid hwmon sensor ID %q", sensor.id)
		}
		if _, duplicate := ids[sensor.id]; duplicate {
			return fmt.Errorf("duplicate hwmon sensor ID %q", sensor.id)
		}
		ids[sensor.id] = struct{}{}
		if sensor.label == "" || len(sensor.label) > maxHWMonLabelSize ||
			strings.ContainsAny(sensor.label, "\t\r\n") {
			return fmt.Errorf("invalid hwmon sensor label %q", sensor.label)
		}
		if math.IsNaN(reading.temperature) || math.IsInf(reading.temperature, 0) {
			return fmt.Errorf("invalid temperature for %q", sensor.id)
		}
		if reading.temperature < 0 || reading.temperature > 150 {
			return fmt.Errorf("temperature out of range for %q", sensor.id)
		}
	}
	for _, reading := range readings {
		if operation == "commit" && reading.omitOnCommit {
			continue
		}
		milliCelsius := int64(math.Round(reading.temperature * 1000))
		if _, err := fmt.Fprintf(out, "sample\t%s\t%d\t%s\n", reading.sensor.id, milliCelsius, reading.sensor.label); err != nil {
			return err
		}
	}
	_, err := fmt.Fprintf(out, "%s\t%s\n", operation, namespace)
	return err
}
