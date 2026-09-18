// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"unraid-vsock-sensors/internal/sensors"
)

func TestPublisherSavesHWMonInventoryCache(t *testing.T) {
	directory := t.TempDir()
	cache := filepath.Join(directory, "inventory.json")
	publisher := &hwmonPublisher{
		cachePath: cache,
		disks: hwmonInventory{sensors: []hwmonSensor{
			{id: "disk:serial", label: "disk1 (sda)"},
		}},
	}
	if err := publisher.saveCache(); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(cache); err != nil {
		t.Fatal(err)
	} else if mode := info.Mode().Perm(); mode != 0600 {
		t.Fatalf("cache mode = %04o, want 0600", mode)
	}
	data, err := os.ReadFile(cache)
	if err != nil {
		t.Fatal(err)
	}
	want := "{\n" +
		"  \"version\": 1,\n" +
		"  \"disks\": {\n" +
		"    \"readings\": [\n" +
		"      {\n" +
		"        \"id\": \"disk:serial\",\n" +
		"        \"label\": \"disk1 (sda)\"\n" +
		"      }\n" +
		"    ]\n" +
		"  }\n" +
		"}\n"
	if got := string(data); got != want {
		t.Fatalf("cache = %q, want %q", got, want)
	}
}

func TestLoadHWMonCacheSupportsRetiredFieldsAtFailsafe(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "inventory.json")
	legacy := `{"version":1,"disks":{"readings":[{"id":"disk:serial","label":"disk1 (sda)","members":["disk:serial"]}]}}`
	if err := os.WriteFile(cache, []byte(legacy), 0600); err != nil {
		t.Fatal(err)
	}
	cached, err := loadHWMonCache(cache)
	if err != nil {
		t.Fatal(err)
	}
	if cached == nil || cached.Disks == nil {
		t.Fatalf("loaded cache = %#v, want disk inventory", cached)
	}
	want := []hwmonSample{hwmonTestSample("disk:serial", "disk1 (sda)", hwmonFailsafeTemp)}
	if got := samplesFromCache(cached.Disks.Sensors); !reflect.DeepEqual(got, want) {
		t.Fatalf("cached samples = %#v, want failsafe samples %#v", got, want)
	}
}

func TestPublisherReportsPartialCacheRestore(t *testing.T) {
	directory := t.TempDir()
	device := filepath.Join(directory, "virt-temp")
	cache := filepath.Join(directory, "inventory.json")
	if err := os.WriteFile(device, nil, 0600); err != nil {
		t.Fatal(err)
	}
	data := `{"version":1,"disks":{"readings":[{"id":"disk:serial","label":"disk1"}]},"hbas":{"readings":[{"id":"invalid","label":"HBA"}]}}`
	if err := os.WriteFile(cache, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}

	err := (&hwmonPublisher{cachePath: cache}).restore(device)
	if err == nil || !strings.Contains(err.Error(), "restore HBA") {
		t.Fatalf("err=%v", err)
	}
	contents, readErr := os.ReadFile(device)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if got, want := string(contents), "sample\tdisk:serial\t100000\tdisk1\nconfigure\tdisk\n"; got != want {
		t.Fatalf("restored disk inventory = %q, want %q", got, want)
	}
}

func TestPublisherContinuesCacheRestoreAfterDiskFailure(t *testing.T) {
	directory := t.TempDir()
	device := filepath.Join(directory, "virt-temp")
	cache := filepath.Join(directory, "inventory.json")
	if err := os.WriteFile(device, nil, 0600); err != nil {
		t.Fatal(err)
	}
	data := `{"version":1,"disks":{"readings":[{"id":"invalid","label":"disk1"}]},"hbas":{"readings":[{"id":"hba:sas:1234","label":"SAS3008"}]}}`
	if err := os.WriteFile(cache, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}

	err := (&hwmonPublisher{cachePath: cache}).restore(device)
	if err == nil || !strings.Contains(err.Error(), "restore disks") {
		t.Fatalf("err=%v", err)
	}
	contents, readErr := os.ReadFile(device)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if got, want := string(contents), "sample\thba:sas:1234\t100000\tSAS3008\nconfigure\thba\n"; got != want {
		t.Fatalf("restored HBA inventory = %q, want %q", got, want)
	}
}

func TestPublisherCachesChangedLabel(t *testing.T) {
	directory := t.TempDir()
	device := filepath.Join(directory, "virt-temp")
	if err := os.WriteFile(device, nil, 0600); err != nil {
		t.Fatal(err)
	}
	publisher := &hwmonPublisher{
		cachePath: filepath.Join(directory, "inventory.json"),
		disks: hwmonInventory{sensors: []hwmonSensor{
			{id: "disk:serial", label: "disk1 (sda)"},
		}},
		hbas: hwmonInventory{sensors: []hwmonSensor{}},
	}
	state := sensors.Response{
		Disks: []sensors.Disk{{ID: "serial", Name: "disk1", Device: "sdb", Temp: 35}},
		HBAs:  []sensors.HBA{},
	}

	reconfigured, err := publisher.publish(device, state)
	if err != nil {
		t.Fatal(err)
	}
	if !reconfigured {
		t.Fatal("a label change must be reported as a reconfiguration")
	}
	data, err := os.ReadFile(publisher.cachePath)
	if err != nil {
		t.Fatal(err)
	}
	var cached cachedHWMonInventory
	if err := json.Unmarshal(data, &cached); err != nil {
		t.Fatal(err)
	}
	if cached.Disks == nil || len(cached.Disks.Sensors) != 1 {
		t.Fatalf("cached disks = %#v, want one sensor", cached.Disks)
	}
	if got, want := cached.Disks.Sensors[0].Label, "disk1"; got != want {
		t.Fatalf("cached label = %q, want %q", got, want)
	}
}

func TestPublisherReportsReconfigurationWhenCacheSaveFails(t *testing.T) {
	directory := t.TempDir()
	device := filepath.Join(directory, "virt-temp")
	blockingFile := filepath.Join(directory, "not-a-directory")
	if err := os.WriteFile(device, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(blockingFile, nil, 0600); err != nil {
		t.Fatal(err)
	}
	publisher := &hwmonPublisher{cachePath: filepath.Join(blockingFile, "inventory.json")}
	state := sensors.Response{
		Disks: []sensors.Disk{{ID: "1", Name: "disk1", Device: "sda", Rotational: true, Temp: 34}},
		HBAs:  []sensors.HBA{},
	}
	reconfigured, err := publisher.publish(device, state)
	if !reconfigured {
		t.Fatal("kernel reconfiguration must be reported even when the cache cannot be saved")
	}
	if err == nil || !strings.Contains(err.Error(), "save hwmon inventory cache") {
		t.Fatalf("error = %v, want cache save failure", err)
	}
	if !publisher.cacheDirty {
		t.Fatal("failed cache save must remain pending for the next cycle")
	}
}
