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
	root := t.TempDir()
	configRoot := filepath.Join(root, "config")
	deviceRoot := filepath.Join(root, "dev")
	if err := os.MkdirAll(deviceRoot, 0700); err != nil {
		t.Fatal(err)
	}
	state := sensors.Response{
		Error: "disks.ini failed",
		HBAs:  []sensors.HBA{{ID: "sas:1234", Temp: 51}},
	}
	_, hbas := makeHWMonSamples(state)
	prepareFakeHWMonKernel(t, configRoot, deviceRoot, "disk", nil)
	prepareFakeHWMonKernel(t, configRoot, deviceRoot, "hba", hbas)
	publisher := &hwmonPublisher{cachePath: filepath.Join(root, "inventory.json")}
	_, err := publisher.publish(configRoot, deviceRoot, state)
	if err == nil || err.Error() != "disks: disks.ini failed" {
		t.Fatalf("error = %v, want disks.ini failure only", err)
	}
	if publisher.hbas.sensors == nil || len(publisher.hbas.sensors) != 1 {
		t.Fatalf("HBA inventory = %#v, want one configured sensor", publisher.hbas)
	}
}

func TestPublisherClearsEmptyFamily(t *testing.T) {
	root := t.TempDir()
	configRoot := filepath.Join(root, "config")
	deviceRoot := filepath.Join(root, "dev")
	if err := os.MkdirAll(deviceRoot, 0700); err != nil {
		t.Fatal(err)
	}
	prepareFakeHWMonKernel(t, configRoot, deviceRoot, "disk", nil)
	prepareFakeHWMonKernel(t, configRoot, deviceRoot, "hba", nil)
	prepareFakeStaleSensor(t, configRoot, "hba", "hba:sas:1234")
	publisher := &hwmonPublisher{
		cachePath: filepath.Join(root, "inventory.json"),
		disks:     hwmonInventory{sensors: []hwmonSensor{}},
		hbas: hwmonInventory{sensors: []hwmonSensor{
			{id: "hba:sas:1234", label: "HBA"},
		}},
	}
	changed, err := publisher.publish(configRoot, deviceRoot, sensors.Response{
		Disks: []sensors.Disk{}, HBAs: []sensors.HBA{},
	})
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	if len(publisher.hbas.sensors) != 0 {
		t.Fatalf("HBA inventory = %#v, want empty", publisher.hbas.sensors)
	}
	entries, err := os.ReadDir(filepath.Join(configRoot, "hba"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("stale HBA configfs items remain: %v", entries)
	}
}

func TestSanitizeHWMonLabel(t *testing.T) {
	tests := []struct {
		name     string
		label    string
		fallback string
		want     string
	}{
		{name: "control characters", label: " Mega\tRAID\n\x00 ", fallback: "hba:sas:1234", want: "Mega RAID"},
		{name: "empty after sanitizing", label: "\t\r\n\x00", fallback: "hba:sas:1234", want: "hba:sas:1234"},
		{name: "fallback control characters", label: "", fallback: "hba:sas:\n1234", want: "hba:sas: 1234"},
		{name: "ASCII truncation", label: strings.Repeat("a", 96), fallback: "fallback", want: strings.Repeat("a", 95)},
		{name: "UTF-8 truncation keeps rune boundary", label: strings.Repeat("é", 48), fallback: "fallback", want: strings.Repeat("é", 47)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := sanitizeHWMonLabel(test.label, test.fallback); got != test.want {
				t.Fatalf("sanitizeHWMonLabel(%q) = %q, want %q", test.label, got, test.want)
			}
		})
	}
}

func TestMakeHWMonSamplesKeepsDiskLabelStableAcrossDeviceChanges(t *testing.T) {
	first, _ := makeHWMonSamples(sensors.Response{Disks: []sensors.Disk{{ID: "stable-id", Name: "disk1", Device: "sdb", Temp: 31}}})
	second, _ := makeHWMonSamples(sensors.Response{Disks: []sensors.Disk{{ID: "stable-id", Name: "disk1", Device: "sdc", Temp: 32}}})
	if got, want := first[0].sensor.label, "disk1"; got != want {
		t.Fatalf("first label = %q, want %q", got, want)
	}
	if got, want := second[0].sensor.label, "disk1"; got != want {
		t.Fatalf("second label = %q, want %q", got, want)
	}
	if !sameHWMonConfiguration(sensorsFromSamples(first), second) {
		t.Fatal("a device-name change for the same stable disk must not reconfigure hwmon")
	}
}
