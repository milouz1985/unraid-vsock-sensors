// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"errors"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"unraid-vsock-sensors/internal/sensors"
)

func TestMakeHWMonSamples(t *testing.T) {
	state := sensors.Response{
		Disks: []sensors.Disk{
			{ID: "1", Name: "disk1", Device: "sda", Rotational: true, Temp: 34},
			{ID: "2", Name: "disk2", Device: "sdb", Rotational: true, Temp: 38},
			{ID: "3", Name: "cache", Device: "nvme0n1", Transport: "nvme", Temp: 45},
		},
		HBAs: []sensors.HBA{{ID: "sas:1234", Model: "SAS3008", PCIAddress: "0000:06:10.0", Temp: 51}},
	}

	disks, hbas := makeHWMonSamples(state)
	wantDisks := []hwmonSample{
		hwmonTestSample("disk:group:hdd", "HDD maximum", 38),
		hwmonTestSample("disk:1", "disk1", 34),
		hwmonTestSample("disk:2", "disk2", 38),
		hwmonTestSample("disk:3", "cache", 45),
	}
	wantHBAs := []hwmonSample{hwmonTestSample("hba:sas:1234", "SAS3008 (0000:06:10.0)", 51)}
	if !reflect.DeepEqual(disks, wantDisks) {
		t.Fatalf("disk readings = %#v, want %#v", disks, wantDisks)
	}
	if !reflect.DeepEqual(hbas, wantHBAs) {
		t.Fatalf("HBA readings = %#v, want %#v", hbas, wantHBAs)
	}
}

func TestMakeHWMonSamplesFailsSafeUnavailableDiskAndItsGroup(t *testing.T) {
	state := sensors.Response{Disks: []sensors.Disk{
		{ID: "1", Name: "disk1", Device: "sda", Rotational: true, Temp: 35},
		{ID: "2", Name: "disk2", Device: "sdb", Rotational: true, Unavailable: true},
		{ID: "3", Name: "cache", Device: "nvme0n1", Transport: "nvme", Temp: 46},
	}}
	want := []hwmonSample{
		{
			sensor:       hwmonSensor{id: "disk:group:hdd", label: "HDD maximum"},
			temperature:  hwmonFailsafeTemp,
			omitOnCommit: true,
		},
		hwmonTestSample("disk:1", "disk1", 35),
		{
			sensor:       hwmonSensor{id: "disk:2", label: "disk2"},
			temperature:  hwmonFailsafeTemp,
			omitOnCommit: true,
		},
		hwmonTestSample("disk:3", "cache", 46),
	}
	if got := makeDiskSamples(state); !reflect.DeepEqual(got, want) {
		t.Fatalf("disk readings = %#v, want explicit disk and group failsafe %#v", got, want)
	}
}

func TestUpdateHWMonFamilySkipsUnavailableSensors(t *testing.T) {
	deviceRoot := t.TempDir()
	readings := makeDiskSamples(sensors.Response{Disks: []sensors.Disk{
		{ID: "1", Name: "disk1", Device: "sda", Rotational: true, Temp: 35},
		{ID: "2", Name: "disk2", Device: "sdb", Rotational: true, Unavailable: true},
		{ID: "3", Name: "cache", Device: "nvme0n1", Transport: "nvme", Temp: 46},
	}})
	prepareFakeHWMonKernel(t, t.TempDir(), deviceRoot, "disk", readings)
	for _, reading := range readings {
		path, err := hwmonTemperatureDevicePath(deviceRoot, "disk", reading.sensor.id)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("unchanged\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	values, err := validateHWMonSamples("disk", readings)
	if err != nil {
		t.Fatal(err)
	}
	if err := updateHWMonFamily(deviceRoot, "disk", readings, values, true); err != nil {
		t.Fatal(err)
	}
	for _, reading := range readings {
		path, _ := hwmonTemperatureDevicePath(deviceRoot, "disk", reading.sensor.id)
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if reading.omitOnCommit {
			if !strings.HasPrefix(string(data), "unchanged") {
				t.Fatalf("%s was refreshed despite omitOnCommit: %q", reading.sensor.id, data)
			}
		} else if strings.HasPrefix(string(data), "unchanged") {
			t.Fatalf("%s was not refreshed", reading.sensor.id)
		}
	}
}

func TestUpdateHWMonFamilyDetectsMissingUnavailableSensor(t *testing.T) {
	deviceRoot := t.TempDir()
	readings := makeDiskSamples(sensors.Response{Disks: []sensors.Disk{
		{ID: "1", Name: "disk1", Device: "sda", Rotational: true, Unavailable: true},
	}})
	values, err := validateHWMonSamples("disk", readings)
	if err != nil {
		t.Fatal(err)
	}

	err = updateHWMonFamily(deviceRoot, "disk", readings, values, true)
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing unavailable sensor error = %v, want os.ErrNotExist", err)
	}
}

func TestValidateHWMonSamplesIDSizeBoundary(t *testing.T) {
	maximumID := "disk:" + strings.Repeat("a", maxHWMonIDSize-len("disk:"))
	if got := len(maximumID); got != maxHWMonIDSize {
		t.Fatalf("maximum disk hwmon ID is %d bytes, want %d", got, maxHWMonIDSize)
	}
	if _, err := validateHWMonSamples("disk", []hwmonSample{
		hwmonTestSample(maximumID, "Maximum ID", 30),
	}); err != nil {
		t.Fatalf("maximum-length ID rejected: %v", err)
	}
	if _, err := validateHWMonSamples("disk", []hwmonSample{
		hwmonTestSample(maximumID+"X", "Oversized ID", 30),
	}); err == nil {
		t.Fatal("ID larger than maxHWMonIDSize accepted")
	}
}

func TestValidateHWMonSamplesAllowsSignedTemperaturesOutsideHardwareRanges(t *testing.T) {
	readings := []hwmonSample{
		hwmonTestSample("hba:sas:negative", "Negative", -40.125),
		hwmonTestSample("hba:sas:high", "High", 200),
	}
	got, err := validateHWMonSamples("hba", readings)
	if err != nil {
		t.Fatal(err)
	}
	want := []int64{-40125, 200000}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("milli-Celsius = %v, want %v", got, want)
	}
}

func TestValidateHWMonSamplesRejectsInvalidFields(t *testing.T) {
	tests := []struct {
		name      string
		namespace string
		reading   hwmonSample
	}{
		{name: "namespace", namespace: "other", reading: hwmonTestSample("other:1", "disk1", 30)},
		{name: "wrong prefix", namespace: "disk", reading: hwmonTestSample("hba:0", "hba0", 30)},
		{name: "newline", namespace: "disk", reading: hwmonTestSample("disk:1", "disk1\nbad", 30)},
		{name: "NaN", namespace: "disk", reading: hwmonTestSample("disk:1", "disk1", math.NaN())},
		{name: "huge", namespace: "disk", reading: hwmonTestSample("disk:1", "disk1", 1e300)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := validateHWMonSamples(test.namespace, []hwmonSample{test.reading}); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
}

func TestValidateHWMonSamplesRejectsDuplicateIDs(t *testing.T) {
	readings := []hwmonSample{
		hwmonTestSample("hba:serial:1234", "hba0", 50),
		hwmonTestSample("hba:serial:1234", "hba1", 51),
	}
	if _, err := validateHWMonSamples("hba", readings); err == nil {
		t.Fatal("expected duplicate ID error")
	}
}

func TestHWMonSensorPathsAreDeterministicAndFilesystemSafe(t *testing.T) {
	key, err := hwmonSensorKey("disk", "disk:group:hdd")
	if err != nil {
		t.Fatal(err)
	}
	if key != "67726f75703a686464" {
		t.Fatalf("key = %q", key)
	}
	path, err := hwmonTemperatureDevicePath("/dev", "disk", "disk:group:hdd")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := path, filepath.Join("/dev", "virt-temp", "6469736b3a67726f75703a686464"); got != want {
		t.Fatalf("device path = %q, want %q", got, want)
	}
}

func makeDiskSamples(state sensors.Response) []hwmonSample {
	disks, _ := makeHWMonSamples(state)
	return disks
}

func hwmonTestSample(id, label string, temperature float64) hwmonSample {
	return hwmonSample{
		sensor: hwmonSensor{id: id, label: label}, temperature: temperature,
	}
}

func prepareFakeHWMonKernel(t *testing.T, configRoot, deviceRoot, namespace string, readings []hwmonSample) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(configRoot, namespace), 0700); err != nil {
		t.Fatal(err)
	}
	for _, reading := range readings {
		key, err := hwmonSensorKey(namespace, reading.sensor.id)
		if err != nil {
			t.Fatal(err)
		}
		directory := filepath.Join(configRoot, namespace, key)
		if err := os.MkdirAll(directory, 0700); err != nil {
			t.Fatal(err)
		}
		label := filepath.Join(directory, "label")
		if _, err := os.Stat(label); os.IsNotExist(err) {
			if err := os.WriteFile(label, nil, 0600); err != nil {
				t.Fatal(err)
			}
		}
		device, err := hwmonTemperatureDevicePath(deviceRoot, namespace, reading.sensor.id)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Dir(device), 0700); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(device); os.IsNotExist(err) {
			if err := os.WriteFile(device, nil, 0600); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func prepareFakeStaleSensor(t *testing.T, configRoot, namespace, id string) {
	t.Helper()
	key, err := hwmonSensorKey(namespace, id)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(configRoot, namespace, key), 0700); err != nil {
		t.Fatal(err)
	}
}
