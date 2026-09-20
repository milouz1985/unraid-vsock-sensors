// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

const (
	maxDiskHWMonIDSize = len("disk:") + maxUnraidDiskIDSize
	maxHBAHWMonIDSize  = len("hba:") + maxHBAStableIDSize
	maxHWMonIDSize     = max(maxDiskHWMonIDSize, maxHBAHWMonIDSize)
	maxHWMonLabelSize  = 95
	maxHWMonSensors    = 1024
)

func publishHWMonFamily(
	configRoot, deviceRoot, namespace string,
	inventory *hwmonInventory,
	current []hwmonSample,
) (bool, error) {
	milliCelsius, err := validateHWMonSamples(namespace, current)
	if err != nil {
		return false, err
	}
	reconcileImmediately := inventory.needsReconcile || inventory.sensors == nil ||
		!sameHWMonConfiguration(inventory.sensors, current)
	if !reconcileImmediately {
		err = updateHWMonFamily(deviceRoot, current, milliCelsius)
		if err == nil {
			return false, nil
		}
		if !errors.Is(err, os.ErrNotExist) && !errors.Is(err, unix.ENODEV) {
			return false, err
		}
	}

	if err := reconcileHWMonFamily(configRoot, deviceRoot, namespace, current, milliCelsius); err != nil {
		inventory.needsReconcile = true
		if !reconcileImmediately {
			return false, fmt.Errorf("reconcile stale %s inventory: %w", namespace, err)
		}
		return false, err
	}
	inventory.sensors = sensorsFromSamples(current)
	inventory.needsReconcile = false
	return true, nil
}

func validateHWMonSamples(namespace string, readings []hwmonSample) ([]int64, error) {
	if namespace != "disk" && namespace != "hba" {
		return nil, fmt.Errorf("invalid hwmon namespace %q", namespace)
	}
	if len(readings) > maxHWMonSensors {
		return nil, fmt.Errorf("too many hwmon sensors: %d exceeds maximum %d", len(readings), maxHWMonSensors)
	}
	prefix := namespace + ":"
	ids := make(map[string]struct{}, len(readings))
	milliCelsius := make([]int64, len(readings))
	for index, reading := range readings {
		sensor := reading.sensor
		if !strings.HasPrefix(sensor.id, prefix) || len(sensor.id) == len(prefix) ||
			len(sensor.id) > maxHWMonIDSize ||
			strings.ContainsRune(sensor.id, '\x00') {
			return nil, fmt.Errorf("invalid hwmon sensor ID %q", sensor.id)
		}
		if _, duplicate := ids[sensor.id]; duplicate {
			return nil, fmt.Errorf("duplicate hwmon sensor ID %q", sensor.id)
		}
		ids[sensor.id] = struct{}{}
		if sensor.label == "" || len(sensor.label) > maxHWMonLabelSize ||
			strings.ContainsAny(sensor.label, "\x00\t\r\n") {
			return nil, fmt.Errorf("invalid hwmon sensor label %q", sensor.label)
		}
		if math.IsNaN(reading.temperature) || math.IsInf(reading.temperature, 0) {
			return nil, fmt.Errorf("invalid temperature for %q", sensor.id)
		}
		rounded := math.Round(reading.temperature * 1000)
		const maxInt64Exclusive = float64(1 << 63)
		if rounded < -maxInt64Exclusive || rounded >= maxInt64Exclusive {
			return nil, fmt.Errorf("temperature cannot be represented in milli-Celsius for %q", sensor.id)
		}
		milliCelsius[index] = int64(rounded)
	}
	return milliCelsius, nil
}

func reconcileHWMonFamily(
	configRoot, deviceRoot, namespace string,
	readings []hwmonSample,
	milliCelsius []int64,
) error {
	familyPath := filepath.Join(configRoot, namespace)
	desired := make(map[string]struct{}, len(readings))
	for index, reading := range readings {
		key := hwmonSensorKey(namespace, reading.sensor.id)
		desired[key] = struct{}{}
		sensorPath := filepath.Join(familyPath, key)
		if err := os.Mkdir(sensorPath, 0700); err != nil && !errors.Is(err, os.ErrExist) {
			return fmt.Errorf("create hwmon sensor %q: %w", reading.sensor.id, err)
		}
		devicePath := hwmonTemperatureDevicePath(deviceRoot, reading.sensor.id)
		// The first label write registers hwmon, so initialize its value first.
		if err := writeKernelAttribute(devicePath, strconv.FormatInt(milliCelsius[index], 10)); err != nil {
			return fmt.Errorf("initialize hwmon sensor %q: %w", reading.sensor.id, err)
		}
		if err := writeKernelAttribute(filepath.Join(sensorPath, "label"), reading.sensor.label); err != nil {
			return fmt.Errorf("label hwmon sensor %q: %w", reading.sensor.id, err)
		}
	}

	entries, err := os.ReadDir(familyPath)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		if _, keep := desired[entry.Name()]; keep {
			continue
		}
		if err := os.Remove(filepath.Join(familyPath, entry.Name())); err != nil {
			return fmt.Errorf("remove stale hwmon sensor %q: %w", entry.Name(), err)
		}
	}
	return nil
}

func updateHWMonFamily(
	deviceRoot string,
	readings []hwmonSample,
	milliCelsius []int64,
) error {
	for index, reading := range readings {
		path := hwmonTemperatureDevicePath(deviceRoot, reading.sensor.id)
		if reading.skipRefresh {
			if _, err := os.Stat(path); err != nil {
				return fmt.Errorf("check hwmon sensor %q: %w", reading.sensor.id, err)
			}
			continue
		}
		if err := writeKernelAttribute(path, strconv.FormatInt(milliCelsius[index], 10)); err != nil {
			return fmt.Errorf("update hwmon sensor %q: %w", reading.sensor.id, err)
		}
	}
	return nil
}

func hwmonSensorKey(namespace, id string) string {
	return hex.EncodeToString([]byte(id[len(namespace)+1:]))
}

func hwmonTemperatureDevicePath(deviceRoot, id string) string {
	return filepath.Join(deviceRoot, "virt-temp", hex.EncodeToString([]byte(id)))
}

func writeKernelAttribute(path, value string) (err error) {
	file, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	defer func() {
		err = errors.Join(err, file.Close())
	}()
	_, err = fmt.Fprintln(file, value)
	return err
}
