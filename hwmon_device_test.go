// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"bytes"
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

func TestPublisherOmitsUnavailableDiskAndItsGroupFromCommit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "virt-temp")
	if err := os.WriteFile(path, nil, 0600); err != nil {
		t.Fatal(err)
	}
	inventory := hwmonInventory{}
	initial := makeDiskSamples(sensors.Response{Disks: []sensors.Disk{
		{ID: "1", Name: "disk1", Device: "sda", Rotational: true, Temp: 34},
		{ID: "2", Name: "disk2", Device: "sdb", Rotational: true, Temp: 38},
		{ID: "3", Name: "cache", Device: "nvme0n1", Transport: "nvme", Temp: 45},
	}})
	if _, err := publishHWMonFamily(path, "disk", &inventory, initial); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(path, 0); err != nil {
		t.Fatal(err)
	}
	current := makeDiskSamples(sensors.Response{Disks: []sensors.Disk{
		{ID: "1", Name: "disk1", Device: "sda", Rotational: true, Temp: 35},
		{ID: "2", Name: "disk2", Device: "sdb", Rotational: true, Unavailable: true},
		{ID: "3", Name: "cache", Device: "nvme0n1", Transport: "nvme", Temp: 46},
	}})
	if _, err := publishHWMonFamily(path, "disk", &inventory, current); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want := "sample\tdisk:1\t35000\tdisk1\n" +
		"sample\tdisk:3\t46000\tcache\n" +
		"commit\tdisk\n"
	if got := string(data); got != want {
		t.Fatalf("update = %q, want failed disk and group omitted %q", got, want)
	}
}

func TestPublisherConfiguresUnavailableDiskAndItsGroupAtFailsafe(t *testing.T) {
	path := filepath.Join(t.TempDir(), "virt-temp")
	if err := os.WriteFile(path, nil, 0600); err != nil {
		t.Fatal(err)
	}
	readings := makeDiskSamples(sensors.Response{Disks: []sensors.Disk{
		{ID: "1", Name: "disk1", Device: "sda", Rotational: true, Temp: 35},
		{ID: "2", Name: "disk2", Device: "sdb", Rotational: true, Unavailable: true},
	}})
	if _, err := publishHWMonFamily(path, "disk", &hwmonInventory{}, readings); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want := "sample\tdisk:group:hdd\t100000\tHDD maximum\n" +
		"sample\tdisk:1\t35000\tdisk1\n" +
		"sample\tdisk:2\t100000\tdisk2\n" +
		"configure\tdisk\n"
	if got := string(data); got != want {
		t.Fatalf("configuration = %q, want failed disk and group at failsafe %q", got, want)
	}
}

func TestEncodeHWMonSamples(t *testing.T) {
	readings := []hwmonSample{
		hwmonTestSample("disk:1", "disk1 (sda)", 34.125),
		hwmonTestSample("disk:group:hdd", "HDD maximum", 38),
	}
	var output bytes.Buffer
	if err := encodeHWMonSamples(&output, "disk", "commit", readings); err != nil {
		t.Fatal(err)
	}
	want := "sample\tdisk:1\t34125\tdisk1 (sda)\n" +
		"sample\tdisk:group:hdd\t38000\tHDD maximum\n" +
		"commit\tdisk\n"
	if got := output.String(); got != want {
		t.Fatalf("encoded snapshot = %q, want %q", got, want)
	}
}

func TestEncodeHWMonSamplesIDSizeBoundary(t *testing.T) {
	maximumID := "disk:" + strings.Repeat("a", maxHWMonIDSize-len("disk:"))
	if got := len(maximumID); got != maxHWMonIDSize {
		t.Fatalf("maximum disk hwmon ID is %d bytes, want %d", got, maxHWMonIDSize)
	}
	if err := encodeHWMonSamples(&bytes.Buffer{}, "disk", "commit", []hwmonSample{
		hwmonTestSample(maximumID, "Maximum ID", 30),
	}); err != nil {
		t.Fatalf("maximum-length ID rejected: %v", err)
	}
	if err := encodeHWMonSamples(&bytes.Buffer{}, "disk", "commit", []hwmonSample{
		hwmonTestSample(maximumID+"X", "Oversized ID", 30),
	}); err == nil {
		t.Fatal("ID larger than maxHWMonIDSize accepted")
	}
}

func TestEncodeHWMonSamplesAllowsSignedTemperaturesOutsideHardwareRanges(t *testing.T) {
	readings := []hwmonSample{
		hwmonTestSample("hba:sas:negative", "Negative", -40.125),
		hwmonTestSample("hba:sas:high", "High", 200),
	}
	var output bytes.Buffer
	if err := encodeHWMonSamples(&output, "hba", "commit", readings); err != nil {
		t.Fatal(err)
	}
	want := "sample\thba:sas:negative\t-40125\tNegative\n" +
		"sample\thba:sas:high\t200000\tHigh\n" +
		"commit\thba\n"
	if got := output.String(); got != want {
		t.Fatalf("encoded snapshot = %q, want %q", got, want)
	}
}

func TestEncodeHWMonSamplesAllowsEmptyCommitSubset(t *testing.T) {
	readings := []hwmonSample{{
		sensor:       hwmonSensor{id: "disk:1", label: "disk1 (sda)"},
		temperature:  hwmonFailsafeTemp,
		omitOnCommit: true,
	}}
	var output bytes.Buffer
	if err := encodeHWMonSamples(&output, "disk", "commit", readings); err != nil {
		t.Fatal(err)
	}
	if got, want := output.String(), "commit\tdisk\n"; got != want {
		t.Fatalf("encoded snapshot = %q, want empty subset %q", got, want)
	}
}

func TestEncodeHWMonSamplesRejectsInvalidFields(t *testing.T) {
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
			if err := encodeHWMonSamples(&bytes.Buffer{}, test.namespace, "commit", []hwmonSample{test.reading}); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
}

func TestEncodeHWMonSamplesValidatesBeforeWriting(t *testing.T) {
	readings := []hwmonSample{
		hwmonTestSample("disk:1", "disk1", 30),
		hwmonTestSample("disk:2", "invalid\nlabel", 31),
	}
	var output bytes.Buffer
	if err := encodeHWMonSamples(&output, "disk", "commit", readings); err == nil {
		t.Fatal("expected validation error")
	}
	if output.Len() != 0 {
		t.Fatalf("validation wrote %q before returning an error", output.String())
	}
}

func TestEncodeHWMonSamplesRejectsDuplicateIDsBeforeWriting(t *testing.T) {
	readings := []hwmonSample{
		hwmonTestSample("hba:serial:1234", "hba0", 50),
		hwmonTestSample("hba:serial:1234", "hba1", 51),
	}
	var output bytes.Buffer
	if err := encodeHWMonSamples(&output, "hba", "commit", readings); err == nil {
		t.Fatal("expected duplicate ID error")
	}
	if output.Len() != 0 {
		t.Fatalf("validation wrote %q before returning an error", output.String())
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
