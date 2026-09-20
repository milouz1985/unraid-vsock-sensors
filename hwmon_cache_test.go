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
		disks: hwmonInventory{sensors: []hwmonSensor{{
			id: "disk:serial", label: "disk1 (sda)",
		}}},
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
	root := t.TempDir()
	configRoot := filepath.Join(root, "config")
	deviceRoot := filepath.Join(root, "dev")
	if err := os.MkdirAll(deviceRoot, 0700); err != nil {
		t.Fatal(err)
	}
	prepareFakeHWMonKernel(t, configRoot, deviceRoot, "disk", []hwmonSample{
		hwmonTestSample("disk:serial", "disk1", hwmonFailsafeTemp),
	})
	prepareFakeHWMonKernel(t, configRoot, deviceRoot, "hba", nil)
	cache := filepath.Join(root, "inventory.json")
	data := `{"version":1,"disks":{"readings":[{"id":"disk:serial","label":"disk1"}]},"hbas":{"readings":[{"id":"invalid","label":"HBA"}]}}`
	if err := os.WriteFile(cache, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}

	publisher := &hwmonPublisher{cachePath: cache}
	err := publisher.restore(configRoot, deviceRoot)
	if err == nil || !strings.Contains(err.Error(), "restore HBA") {
		t.Fatalf("err=%v", err)
	}
	if publisher.disks.sensors == nil || len(publisher.disks.sensors) != 1 {
		t.Fatalf("restored disks = %#v", publisher.disks)
	}
}

func TestPublisherCachesChangedLabel(t *testing.T) {
	root := t.TempDir()
	configRoot := filepath.Join(root, "config")
	deviceRoot := filepath.Join(root, "dev")
	if err := os.MkdirAll(deviceRoot, 0700); err != nil {
		t.Fatal(err)
	}
	state := sensors.Response{
		Disks: []sensors.Disk{{ID: "serial", Name: "disk1", Device: "sdb", Temp: 35}},
		HBAs:  []sensors.HBA{},
	}
	disks, hbas := makeHWMonSamples(state)
	prepareFakeHWMonKernel(t, configRoot, deviceRoot, "disk", disks)
	prepareFakeHWMonKernel(t, configRoot, deviceRoot, "hba", hbas)
	publisher := &hwmonPublisher{
		cachePath: filepath.Join(root, "inventory.json"),
		disks: hwmonInventory{sensors: []hwmonSensor{{
			id: "disk:serial", label: "disk1 (sda)",
		}}},
		hbas: hwmonInventory{sensors: []hwmonSensor{}},
	}

	reconfigured, err := publisher.publish(configRoot, deviceRoot, state)
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
	root := t.TempDir()
	configRoot := filepath.Join(root, "config")
	deviceRoot := filepath.Join(root, "dev")
	if err := os.MkdirAll(deviceRoot, 0700); err != nil {
		t.Fatal(err)
	}
	state := sensors.Response{
		Disks: []sensors.Disk{{ID: "1", Name: "disk1", Device: "sda", Rotational: true, Temp: 34}},
		HBAs:  []sensors.HBA{},
	}
	disks, hbas := makeHWMonSamples(state)
	prepareFakeHWMonKernel(t, configRoot, deviceRoot, "disk", disks)
	prepareFakeHWMonKernel(t, configRoot, deviceRoot, "hba", hbas)
	blockingFile := filepath.Join(root, "not-a-directory")
	if err := os.WriteFile(blockingFile, nil, 0600); err != nil {
		t.Fatal(err)
	}
	publisher := &hwmonPublisher{cachePath: filepath.Join(blockingFile, "inventory.json")}
	reconfigured, err := publisher.publish(configRoot, deviceRoot, state)
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
