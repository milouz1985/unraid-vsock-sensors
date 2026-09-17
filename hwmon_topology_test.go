// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unraid-vsock-sensors/internal/sensors"
)

func TestPublishHWMonStateKeepsFamiliesIndependent(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "virt-temp")
	if err := os.WriteFile(path, nil, 0600); err != nil {
		t.Fatal(err)
	}
	state := sensors.Response{
		Error: "disks.ini failed",
		HBAs:  []sensors.HBA{{ID: "sas:1234", Temp: 51}},
	}
	publisher := &hwmonPublisher{cachePath: filepath.Join(directory, "inventory.json")}
	_, err := publisher.publish(path, state)
	if err == nil || err.Error() != "disks: disks.ini failed" {
		t.Fatalf("error = %v, want disks.ini failure only", err)
	}
	data, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if got, want := string(data), "sample\thba:sas:1234\t51000\tsas:1234\nconfigure\thba\n"; got != want {
		t.Fatalf("HBA snapshot = %q, want %q", got, want)
	}
}

func TestPublisherDistinguishesMissingAndEmptyDiskInventory(t *testing.T) {
	directory := t.TempDir()
	device := filepath.Join(directory, "virt-temp")
	if err := os.WriteFile(device, nil, 0600); err != nil {
		t.Fatal(err)
	}
	publisher := &hwmonPublisher{
		cachePath: filepath.Join(directory, "inventory.json"),
		disks: hwmonInventory{initialized: true, sensors: []hwmonSensor{
			{id: "disk:serial", label: "disk1 (sda)"},
		}},
		hbas: hwmonInventory{initialized: true},
	}

	_, err := publisher.publish(device, sensors.Response{})
	if err == nil || !strings.Contains(err.Error(), "disks: inventory is missing") {
		t.Fatalf("missing inventory error = %v", err)
	}
	if len(publisher.disks.sensors) != 1 {
		t.Fatal("missing inventory changed the configured disk")
	}

	changed, err := publisher.publish(device, sensors.Response{
		Disks: []sensors.Disk{}, HBAs: []sensors.HBA{},
	})
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	if len(publisher.disks.sensors) != 0 {
		t.Fatalf("disk inventory = %#v, want empty", publisher.disks.sensors)
	}

	if err := os.Truncate(device, 0); err != nil {
		t.Fatal(err)
	}
	restored := &hwmonPublisher{cachePath: publisher.cachePath}
	if err := restored.restore(device); err != nil {
		t.Fatal(err)
	}
	if !restored.disks.initialized || len(restored.disks.sensors) != 0 {
		t.Fatalf("restored disk inventory = %#v, want initialized and empty", restored.disks)
	}
}

func TestPublisherDistinguishesMissingAndEmptyHBAInventory(t *testing.T) {
	directory := t.TempDir()
	device := filepath.Join(directory, "virt-temp")
	if err := os.WriteFile(device, nil, 0600); err != nil {
		t.Fatal(err)
	}
	publisher := &hwmonPublisher{
		cachePath: filepath.Join(directory, "inventory.json"),
		disks:     hwmonInventory{initialized: true},
		hbas: hwmonInventory{initialized: true, sensors: []hwmonSensor{
			{id: "hba:serial", label: "HBA 1"},
		}},
	}

	_, err := publisher.publish(device, sensors.Response{Disks: []sensors.Disk{}})
	if err == nil || !strings.Contains(err.Error(), "HBA: inventory is missing") {
		t.Fatalf("missing inventory error = %v", err)
	}
	if len(publisher.hbas.sensors) != 1 {
		t.Fatal("missing inventory changed the configured HBA")
	}

	changed, err := publisher.publish(device, sensors.Response{
		Disks: []sensors.Disk{}, HBAs: []sensors.HBA{},
	})
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	if len(publisher.hbas.sensors) != 0 {
		t.Fatalf("HBA inventory = %#v, want empty", publisher.hbas.sensors)
	}

	if err := os.Truncate(device, 0); err != nil {
		t.Fatal(err)
	}
	restored := &hwmonPublisher{cachePath: publisher.cachePath}
	if err := restored.restore(device); err != nil {
		t.Fatal(err)
	}
	if !restored.hbas.initialized || len(restored.hbas.sensors) != 0 {
		t.Fatalf("restored HBA inventory = %#v, want initialized and empty", restored.hbas)
	}
}

func TestPublisherReconfiguresWhenLabelChanges(t *testing.T) {
	path := filepath.Join(t.TempDir(), "virt-temp")
	if err := os.WriteFile(path, nil, 0600); err != nil {
		t.Fatal(err)
	}
	inventory := hwmonInventory{}
	initial := []hwmonSample{hwmonTestSample("disk:serial", "disk1 (sda)", 34)}
	if _, err := publishHWMonFamily(path, "disk", &inventory, initial); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(path, 0); err != nil {
		t.Fatal(err)
	}
	changed := []hwmonSample{hwmonTestSample("disk:serial", "disk1 (sdb)", 35)}
	reconfigured, err := publishHWMonFamily(path, "disk", &inventory, changed)
	if err != nil {
		t.Fatal(err)
	}
	if !reconfigured {
		t.Fatal("a label change must reconfigure the hwmon family")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(data), "sample\tdisk:serial\t35000\tdisk1 (sdb)\nconfigure\tdisk\n"; got != want {
		t.Fatalf("update = %q, want replacement configuration %q", got, want)
	}
	if got, want := inventory.sensors[0].label, "disk1 (sdb)"; got != want {
		t.Fatalf("configured label = %q, want %q", got, want)
	}
}

func TestPublisherPublishesEmptyHBAInventory(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "virt-temp")
	if err := os.WriteFile(path, nil, 0600); err != nil {
		t.Fatal(err)
	}
	publisher := &hwmonPublisher{
		cachePath: filepath.Join(directory, "inventory.json"),
		disks:     hwmonInventory{initialized: true},
	}
	changed, err := publisher.publish(path, sensors.Response{Disks: []sensors.Disk{}, HBAs: []sensors.HBA{}})
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(data), "configure\thba\n"; got != want {
		t.Fatalf("configuration = %q, want %q", got, want)
	}

	publisher.hbas = hwmonInventory{
		initialized: true,
		sensors:     []hwmonSensor{{id: "hba:sas:1234", label: "HBA"}},
	}
	if err := os.Truncate(path, 0); err != nil {
		t.Fatal(err)
	}
	changed, err = publisher.publish(path, sensors.Response{Disks: []sensors.Disk{}, HBAs: []sensors.HBA{}})
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	if len(publisher.hbas.sensors) != 0 {
		t.Fatalf("HBA inventory = %#v, want empty", publisher.hbas.sensors)
	}
	data, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(data), "configure\thba\n"; got != want {
		t.Fatalf("cleared configuration = %q, want %q", got, want)
	}
}

func TestSanitizeHWMonLabel(t *testing.T) {
	tests := []struct {
		name     string
		label    string
		fallback string
		want     string
	}{
		{
			name:     "control characters",
			label:    " Mega\tRAID\n\x00 ",
			fallback: "hba:sas:1234",
			want:     "Mega RAID",
		},
		{
			name:     "empty after sanitizing",
			label:    "\t\r\n\x00",
			fallback: "hba:sas:1234",
			want:     "hba:sas:1234",
		},
		{
			name:     "ASCII truncation",
			label:    strings.Repeat("a", maxHWMonLabelSize+1),
			fallback: "fallback",
			want:     strings.Repeat("a", maxHWMonLabelSize),
		},
		{
			name:     "UTF-8 truncation keeps rune boundary",
			label:    strings.Repeat("é", 48),
			fallback: "fallback",
			want:     strings.Repeat("é", 47),
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := sanitizeHWMonLabel(test.label, test.fallback); got != test.want {
				t.Fatalf("sanitizeHWMonLabel(%q) = %q, want %q", test.label, got, test.want)
			}
		})
	}
}

func TestMakeHWMonSamplesSanitizesDynamicLabels(t *testing.T) {
	state := sensors.Response{
		Disks: []sensors.Disk{{
			ID:     "disk-id",
			Name:   "disk\none",
			Device: "sda",
			Temp:   32,
		}},
		HBAs: []sensors.HBA{{
			ID:         "sas:1234",
			Model:      strings.Repeat("M", maxHWMonLabelSize+20) + "\tmodel",
			PCIAddress: "0000:03:00.0",
			Temp:       51,
		}},
	}

	disks, hbas := makeHWMonSamples(state)
	if got, want := disks[0].sensor.label, "disk one (sda)"; got != want {
		t.Fatalf("disk label = %q, want %q", got, want)
	}
	if got := hbas[0].sensor.label; len(got) != maxHWMonLabelSize || strings.ContainsAny(got, "\t\r\n\x00") {
		t.Fatalf("sanitized HBA label = %q (%d bytes)", got, len(got))
	}

	var encoded strings.Builder
	if err := encodeHWMonSamples(&encoded, "disk", "configure", disks); err != nil {
		t.Fatalf("encode sanitized disk sample: %v", err)
	}
	encoded.Reset()
	if err := encodeHWMonSamples(&encoded, "hba", "configure", hbas); err != nil {
		t.Fatalf("encode sanitized HBA sample: %v", err)
	}
}
