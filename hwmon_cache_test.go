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

func TestConsumerRestartWaitsForPendingCacheReconciliation(t *testing.T) {
	root := t.TempDir()
	configRoot := filepath.Join(root, "config")
	deviceRoot := filepath.Join(root, "dev")
	cachedDisks := []cachedHWMonSensor{{ID: "disk:cached", Label: "Cached disk"}}
	prepareFakeHWMonKernel(t, configRoot, deviceRoot, "disk", samplesFromCache(cachedDisks))
	if err := os.MkdirAll(filepath.Join(configRoot, "hba"), 0700); err != nil {
		t.Fatal(err)
	}
	cache := filepath.Join(root, "inventory.json")
	data := `{"version":1,"disks":{"readings":[{"id":"disk:cached","label":"Cached disk"}]},"hbas":{"readings":[{"id":"hba:cached","label":"Cached HBA"}]}}`
	if err := os.WriteFile(cache, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}

	publisher := &hwmonPublisher{cachePath: cache}
	if err := publisher.restore(configRoot, deviceRoot); err == nil || !strings.Contains(err.Error(), "restore HBA") {
		t.Fatalf("cache restore error = %v, want partial HBA failure", err)
	}
	if publisher.disks.sensors == nil {
		t.Fatal("successful disk cache restore did not initialize the family")
	}
	if !publisher.hbas.needsReconcile {
		t.Fatal("failed HBA cache restore did not remain pending")
	}

	state := sensors.Response{
		Disks:    []sensors.Disk{{ID: "cached", Name: "Cached disk", Temp: 35}},
		HBAError: "temporarily unavailable",
	}
	reconfigured, err := publisher.publish(configRoot, deviceRoot, state)
	if reconfigured {
		t.Fatal("snapshot with pending HBA reconciliation reported a reconfiguration")
	}
	if err == nil || !strings.Contains(err.Error(), "temporarily unavailable") {
		t.Fatalf("publish error = %v, want unavailable HBA", err)
	}
	if !publisher.reconciliationPending() {
		t.Fatal("publisher did not report the pending HBA reconciliation")
	}
	// A pending reconciliation also abandons any previously scheduled restart
	// retry when its timer expires.
	// A decoded snapshot still counts as the first guest snapshot when one of
	// its collectors reports an error.
	if publisher.shouldRestartConsumers(reconfigured, true) {
		t.Fatal("first guest snapshot authorized consumer restart during pending HBA reconciliation")
	}
	if publisher.shouldRestartConsumers(true, false) {
		t.Fatal("reconfigured bypassed the pending-family restart barrier")
	}

	initializedPublisher := &hwmonPublisher{disks: hwmonInventory{sensors: []hwmonSensor{
		{id: "disk:cached", label: "Cached disk"},
	}}}
	if !initializedPublisher.shouldRestartConsumers(false, true) {
		t.Fatal("first guest snapshot did not restart consumers after a complete cache restore")
	}

	hbas := []sensors.HBA{{ID: "cached", Model: "Cached HBA", Temp: 50}}
	_, hbaSamples := makeHWMonSamples(sensors.Response{HBAs: hbas})
	prepareFakeHWMonKernel(t, configRoot, deviceRoot, "hba", hbaSamples)
	state.HBAError = ""
	state.HBAs = hbas
	reconfigured, err = publisher.publish(configRoot, deviceRoot, state)
	if err != nil {
		t.Fatal(err)
	}
	if !reconfigured || publisher.reconciliationPending() {
		t.Fatalf("successful HBA retry: reconfigured=%v pending=%v, want true/false",
			reconfigured, publisher.reconciliationPending())
	}
	if !publisher.shouldRestartConsumers(reconfigured, false) {
		t.Fatal("successful HBA reconciliation did not rearm consumer restart")
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

func TestPublisherDefersReconfigurationWhileFamilyNeedsReconcile(t *testing.T) {
	tests := []struct {
		name             string
		failedNamespace  string
		failedErrorLabel string
	}{
		{name: "disk fails", failedNamespace: "disk", failedErrorLabel: "disks:"},
		{name: "HBA fails", failedNamespace: "hba", failedErrorLabel: "HBA:"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			configRoot := filepath.Join(root, "config")
			deviceRoot := filepath.Join(root, "dev")
			state := sensors.Response{
				Disks: []sensors.Disk{{ID: "new", Name: "disk1", Temp: 35}},
				HBAs:  []sensors.HBA{{ID: "new", Model: "New HBA", Temp: 50}},
			}
			disks, hbas := makeHWMonSamples(state)
			lastValidDisks := []hwmonSensor{{id: "disk:old", label: "Old disk"}}
			lastValidHBAs := []hwmonSensor{{id: "hba:old", label: "Old HBA"}}
			publisher := &hwmonPublisher{
				cachePath: filepath.Join(root, "inventory.json"),
				disks:     hwmonInventory{sensors: lastValidDisks},
				hbas:      hwmonInventory{sensors: lastValidHBAs},
			}

			var failedReadings []hwmonSample
			if test.failedNamespace == "disk" {
				failedReadings = disks
				if err := os.MkdirAll(filepath.Join(configRoot, "disk"), 0700); err != nil {
					t.Fatal(err)
				}
				prepareFakeHWMonKernel(t, configRoot, deviceRoot, "hba", hbas)
			} else {
				failedReadings = hbas
				if err := os.MkdirAll(filepath.Join(configRoot, "hba"), 0700); err != nil {
					t.Fatal(err)
				}
				prepareFakeHWMonKernel(t, configRoot, deviceRoot, "disk", disks)
			}

			reconfigured, err := publisher.publish(configRoot, deviceRoot, state)
			if reconfigured {
				t.Fatal("partial reconciliation authorized consumer restart")
			}
			if err == nil || !strings.Contains(err.Error(), test.failedErrorLabel) {
				t.Fatalf("publish error = %v, want %s reconciliation failure", err, test.failedNamespace)
			}
			if test.failedNamespace == "disk" && !publisher.disks.needsReconcile ||
				test.failedNamespace == "hba" && !publisher.hbas.needsReconcile {
				t.Fatalf("failed %s reconciliation did not remain pending", test.failedNamespace)
			}

			data, err := os.ReadFile(publisher.cachePath)
			if err != nil {
				t.Fatal(err)
			}
			var cached cachedHWMonInventory
			if err := json.Unmarshal(data, &cached); err != nil {
				t.Fatal(err)
			}
			wantDisks := sensorsToCache(sensorsFromSamples(disks))
			wantHBAs := sensorsToCache(sensorsFromSamples(hbas))
			if test.failedNamespace == "disk" {
				wantDisks = sensorsToCache(lastValidDisks)
			} else {
				wantHBAs = sensorsToCache(lastValidHBAs)
			}
			if cached.Disks == nil || !reflect.DeepEqual(cached.Disks.Sensors, wantDisks) {
				t.Fatalf("cached disks = %#v, want %#v", cached.Disks, wantDisks)
			}
			if cached.HBAs == nil || !reflect.DeepEqual(cached.HBAs.Sensors, wantHBAs) {
				t.Fatalf("cached HBAs = %#v, want %#v", cached.HBAs, wantHBAs)
			}

			prepareFakeHWMonKernel(t, configRoot, deviceRoot, test.failedNamespace, failedReadings)
			reconfigured, err = publisher.publish(configRoot, deviceRoot, state)
			if err != nil {
				t.Fatal(err)
			}
			if !reconfigured {
				t.Fatal("successful retry did not report the deferred reconfiguration")
			}
			if publisher.disks.needsReconcile || publisher.hbas.needsReconcile {
				t.Fatalf("successful retry left reconciliation pending: disks=%v HBA=%v",
					publisher.disks.needsReconcile, publisher.hbas.needsReconcile)
			}
		})
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
