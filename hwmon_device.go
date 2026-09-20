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
	if inventory.sensors == nil || !sameHWMonConfiguration(inventory.sensors, current) {
		inventory.sensors = nil
		if err := configureHWMonFamily(configRoot, deviceRoot, namespace, current, milliCelsius); err != nil {
			return false, err
		}
		inventory.sensors = sensorsFromSamples(current)
		return true, nil
	}

	if err := updateHWMonFamily(deviceRoot, namespace, current, milliCelsius, true); err != nil {
		if !errors.Is(err, os.ErrNotExist) && !errors.Is(err, unix.ENODEV) {
			return false, err
		}
		inventory.sensors = nil
		if configureErr := configureHWMonFamily(configRoot, deviceRoot, namespace, current, milliCelsius); configureErr != nil {
			return false, fmt.Errorf("reconfigure stale %s inventory: %w", namespace, configureErr)
		}
		inventory.sensors = sensorsFromSamples(current)
		return true, nil
	}
	return false, nil
}

func validateHWMonSamples(namespace string, readings []hwmonSample) ([]int64, error) {
	prefix := namespace + ":"
	ids := make(map[string]struct{}, len(readings))
	milliCelsius := make([]int64, len(readings))
	if namespace != "disk" && namespace != "hba" {
		return nil, fmt.Errorf("invalid hwmon namespace %q", namespace)
	}
	for index, reading := range readings {
		sensor := reading.sensor
		if !strings.HasPrefix(sensor.id, prefix) || len(sensor.id) > maxHWMonIDSize ||
			strings.ContainsAny(sensor.id, "\x00\t\r\n") {
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

func configureHWMonFamily(
	configRoot, deviceRoot, namespace string,
	readings []hwmonSample,
	milliCelsius []int64,
) error {
	familyPath := filepath.Join(configRoot, namespace)
	desired := make(map[string]struct{}, len(readings))
	for _, reading := range readings {
		key, err := hwmonSensorKey(namespace, reading.sensor.id)
		if err != nil {
			return err
		}
		desired[key] = struct{}{}
		sensorPath := filepath.Join(familyPath, key)
		if err := os.Mkdir(sensorPath, 0700); err != nil && !errors.Is(err, os.ErrExist) {
			return fmt.Errorf("create hwmon sensor %q: %w", reading.sensor.id, err)
		}
		if err := writeKernelAttribute(filepath.Join(sensorPath, "label"), reading.sensor.label); err != nil {
			return fmt.Errorf("label hwmon sensor %q: %w", reading.sensor.id, err)
		}
	}
	if err := updateHWMonFamily(deviceRoot, namespace, readings, milliCelsius, false); err != nil {
		return err
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
	deviceRoot, namespace string,
	readings []hwmonSample,
	milliCelsius []int64,
	skipUnavailable bool,
) error {
	for index, reading := range readings {
		path, err := hwmonTemperatureDevicePath(deviceRoot, namespace, reading.sensor.id)
		if err != nil {
			return err
		}
		if skipUnavailable && reading.omitOnCommit {
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

func hwmonSensorKey(namespace, id string) (string, error) {
	prefix := namespace + ":"
	if !strings.HasPrefix(id, prefix) || len(id) == len(prefix) {
		return "", fmt.Errorf("hwmon sensor %q is outside %s namespace", id, namespace)
	}
	return hex.EncodeToString([]byte(strings.TrimPrefix(id, prefix))), nil
}

func hwmonTemperatureDevicePath(deviceRoot, namespace, id string) (string, error) {
	if _, err := hwmonSensorKey(namespace, id); err != nil {
		return "", err
	}
	return filepath.Join(deviceRoot, "virt-temp", hex.EncodeToString([]byte(id))), nil
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
