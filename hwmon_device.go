// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"strings"

	"golang.org/x/sys/unix"
)

const (
	maxDiskHWMonIDSize = len("disk:") + maxUnraidDiskIDSize
	maxHBAHWMonIDSize  = len("hba:") + maxHBAStableIDSize
	maxHWMonIDSize     = max(maxDiskHWMonIDSize, maxHBAHWMonIDSize)
	maxHWMonLabelSize  = 95
)

func publishHWMonFamily(
	path, namespace string,
	inventory *hwmonInventory,
	current []hwmonSample,
) (bool, error) {
	if inventory.sensors == nil || !sameHWMonConfiguration(inventory.sensors, current) {
		if err := writeHWMonSamples(path, namespace, "configure", current); err != nil {
			return false, err
		}
		inventory.sensors = sensorsFromSamples(current)
		return true, nil
	}

	if err := writeHWMonSamples(path, namespace, "commit", current); err != nil {
		if !errors.Is(err, unix.ESTALE) {
			return false, err
		}
		if configureErr := writeHWMonSamples(path, namespace, "configure", current); configureErr != nil {
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
	milliCelsius := make([]int64, len(readings))
	if namespace != "disk" && namespace != "hba" {
		return fmt.Errorf("invalid hwmon namespace %q", namespace)
	}
	if operation != "configure" && operation != "commit" {
		return fmt.Errorf("invalid hwmon operation %q", operation)
	}
	for index, reading := range readings {
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
		rounded := math.Round(reading.temperature * 1000)
		const maxInt64Exclusive = float64(1 << 63)
		if rounded < -maxInt64Exclusive || rounded >= maxInt64Exclusive {
			return fmt.Errorf("temperature cannot be represented in milli-Celsius for %q", sensor.id)
		}
		milliCelsius[index] = int64(rounded)
	}
	for index, reading := range readings {
		if operation == "commit" && reading.omitOnCommit {
			continue
		}
		if _, err := fmt.Fprintf(out, "sample\t%s\t%d\t%s\n", reading.sensor.id, milliCelsius[index], reading.sensor.label); err != nil {
			return err
		}
	}
	_, err := fmt.Fprintf(out, "%s\t%s\n", operation, namespace)
	return err
}
