package main

import (
	"bytes"
	"context"
	"errors"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"unraid-vsock-sensors/internal/sensors"
)

func TestMakeHWMonReadings(t *testing.T) {
	state := sensors.Response{
		Disks: []sensors.Disk{
			{ID: "1", Name: "disk1", Device: "sda", Rotational: true, Temp: 34},
			{ID: "2", Name: "disk2", Device: "sdb", Rotational: true, Temp: 38},
			{ID: "3", Name: "cache", Device: "nvme0n1", Transport: "nvme", Temp: 45},
			{ID: "4", Name: "external", Device: "sdc", Transport: "usb", Rotational: true, Temp: 60},
		},
		HBAs: []sensors.HBA{{Name: "hba0", Temp: 51}},
	}

	disks, hbas := makeHWMonReadings(state)
	wantDisks := []hwmonReading{
		{id: "disk:group:hdd", label: "HDD maximum", temperature: 38, members: []string{"disk:1", "disk:2"}},
		{id: "disk:1", label: "disk1 (sda)", temperature: 34},
		{id: "disk:2", label: "disk2 (sdb)", temperature: 38},
		{id: "disk:3", label: "cache (nvme0n1)", temperature: 45},
	}
	wantHBAs := []hwmonReading{{id: "hba:hba0", label: "hba0", temperature: 51}}
	if !reflect.DeepEqual(disks, wantDisks) {
		t.Fatalf("disk readings = %#v, want %#v", disks, wantDisks)
	}
	if !reflect.DeepEqual(hbas, wantHBAs) {
		t.Fatalf("HBA readings = %#v, want %#v", hbas, wantHBAs)
	}
}

func TestGroupMaximumIgnoresUnavailableDisk(t *testing.T) {
	state := sensors.Response{Disks: []sensors.Disk{
		{ID: "1", Name: "disk1", Device: "sda", Rotational: true, Temp: 34},
		{ID: "2", Name: "disk2", Device: "sdb", Rotational: true, Unavailable: true},
		{ID: "3", Name: "cache1", Device: "nvme0n1", Transport: "nvme", Temp: 45},
		{ID: "4", Name: "cache2", Device: "nvme1n1", Transport: "nvme", Temp: 47},
	}}
	readings := makeDiskReadings(state)
	byID := make(map[string]hwmonReading, len(readings))
	for _, reading := range readings {
		byID[reading.id] = reading
	}
	if !byID["disk:2"].unavailable {
		t.Error("disk:2 should be unavailable")
	}
	for _, id := range []string{"disk:1", "disk:3", "disk:4", "disk:group:hdd", "disk:group:nvme"} {
		if byID[id].unavailable {
			t.Errorf("%s should remain available", id)
		}
	}
	if got := byID["disk:group:hdd"].temperature; got != 34 {
		t.Errorf("HDD maximum = %v, want available disk temperature 34", got)
	}
}

func TestPublisherSkipsUnavailableDiskButUpdatesItsGroup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "virt-temp")
	if err := os.WriteFile(path, nil, 0600); err != nil {
		t.Fatal(err)
	}
	inventory := hwmonInventory{}
	initial := makeDiskReadings(sensors.Response{Disks: []sensors.Disk{
		{ID: "1", Name: "disk1", Device: "sda", Rotational: true, Temp: 34},
		{ID: "2", Name: "disk2", Device: "sdb", Rotational: true, Temp: 38},
		{ID: "3", Name: "cache", Device: "nvme0n1", Transport: "nvme", Temp: 45},
	}})
	if _, err := publishHWMonFamily(path, "disk", &inventory, initial, false); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(path, 0); err != nil {
		t.Fatal(err)
	}
	current := makeDiskReadings(sensors.Response{Disks: []sensors.Disk{
		{ID: "1", Name: "disk1", Device: "sda", Rotational: true, Temp: 35},
		{ID: "2", Name: "disk2", Device: "sdb", Rotational: true, Unavailable: true},
		{ID: "3", Name: "cache", Device: "nvme0n1", Transport: "nvme", Temp: 46},
	}})
	if _, err := publishHWMonFamily(path, "disk", &inventory, current, false); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want := "disk:group:hdd\t35000\tHDD maximum\n" +
		"disk:1\t35000\tdisk1 (sda)\n" +
		"disk:3\t46000\tcache (nvme0n1)\n" +
		"commit\tdisk\n"
	if got := string(data); got != want {
		t.Fatalf("update = %q, want healthy channels and partial maximum %q", got, want)
	}
}

func TestGroupMaximumUnavailableWhenEveryMemberIsUnavailable(t *testing.T) {
	readings := makeDiskReadings(sensors.Response{Disks: []sensors.Disk{
		{ID: "1", Name: "disk1", Device: "sda", Rotational: true, Unavailable: true},
		{ID: "2", Name: "disk2", Device: "sdb", Rotational: true, Unavailable: true},
	}})
	for _, reading := range readings {
		if reading.id == "disk:group:hdd" && !reading.unavailable {
			t.Fatal("group maximum should be unavailable without any usable member")
		}
	}
}

func TestEncodeHWMonReadings(t *testing.T) {
	readings := []hwmonReading{
		{id: "disk:1", label: "disk1 (sda)", temperature: 34.125},
		{id: "disk:group:hdd", label: "HDD maximum", temperature: 38},
	}
	var output bytes.Buffer
	if err := encodeHWMonReadings(&output, "disk", "commit", readings); err != nil {
		t.Fatal(err)
	}
	want := "disk:1\t34125\tdisk1 (sda)\n" +
		"disk:group:hdd\t38000\tHDD maximum\n" +
		"commit\tdisk\n"
	if got := output.String(); got != want {
		t.Fatalf("encoded snapshot = %q, want %q", got, want)
	}
}

func TestEncodeHWMonReadingsRejectsInvalidFields(t *testing.T) {
	tests := []struct {
		name      string
		namespace string
		reading   hwmonReading
	}{
		{name: "namespace", namespace: "other", reading: hwmonReading{id: "other:1", label: "disk1", temperature: 30}},
		{name: "wrong prefix", namespace: "disk", reading: hwmonReading{id: "hba:0", label: "hba0", temperature: 30}},
		{name: "newline", namespace: "disk", reading: hwmonReading{id: "disk:1", label: "disk1\nbad", temperature: 30}},
		{name: "NaN", namespace: "disk", reading: hwmonReading{id: "disk:1", label: "disk1", temperature: math.NaN()}},
		{name: "huge", namespace: "disk", reading: hwmonReading{id: "disk:1", label: "disk1", temperature: 1e300}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := encodeHWMonReadings(&bytes.Buffer{}, test.namespace, "commit", []hwmonReading{test.reading}); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
}

func TestEncodeHWMonReadingsValidatesBeforeWriting(t *testing.T) {
	readings := []hwmonReading{
		{id: "disk:1", label: "disk1", temperature: 30},
		{id: "disk:2", label: "invalid\nlabel", temperature: 31},
	}
	var output bytes.Buffer
	if err := encodeHWMonReadings(&output, "disk", "commit", readings); err == nil {
		t.Fatal("expected validation error")
	}
	if output.Len() != 0 {
		t.Fatalf("validation wrote %q before returning an error", output.String())
	}
}

func TestEncodeHWMonReadingsRejectsDuplicateIDsBeforeWriting(t *testing.T) {
	readings := []hwmonReading{
		{id: "hba:serial:1234", label: "hba0", temperature: 50},
		{id: "hba:serial:1234", label: "hba1", temperature: 51},
	}
	var output bytes.Buffer
	if err := encodeHWMonReadings(&output, "hba", "commit", readings); err == nil {
		t.Fatal("expected duplicate ID error")
	}
	if output.Len() != 0 {
		t.Fatalf("validation wrote %q before returning an error", output.String())
	}
}

func TestPublishHWMonStateKeepsFamiliesIndependent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "virt-temp")
	if err := os.WriteFile(path, nil, 0600); err != nil {
		t.Fatal(err)
	}
	fetch := func(context.Context, uint32, uint32) (sensors.Response, error) {
		return sensors.Response{
			Error: "disks.ini failed",
			HBAs:  []sensors.HBA{{Name: "hba0", Temp: 51}},
		}, nil
	}
	_, err := (&hwmonPublisher{}).publish(context.Background(), 42, 19090, path, fetch)
	if err == nil {
		t.Fatal("expected disk error")
	}
	if message := err.Error(); !strings.Contains(message, "disks: disks.ini failed") {
		t.Fatalf("unexpected error %q", message)
	}
	data, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if got, want := string(data), "hba:hba0\t51000\thba0\nconfigure\thba\n"; got != want {
		t.Fatalf("HBA snapshot = %q, want %q", got, want)
	}
}

func TestPublisherReconfiguresChangedTopology(t *testing.T) {
	path := filepath.Join(t.TempDir(), "virt-temp")
	if err := os.WriteFile(path, nil, 0600); err != nil {
		t.Fatal(err)
	}
	publisher := &hwmonPublisher{}
	states := [][]hwmonReading{
		makeDiskReadings(sensors.Response{Disks: []sensors.Disk{
			{ID: "1", Name: "disk1", Device: "sda", Rotational: true, Temp: 34},
			{ID: "2", Name: "disk2", Device: "sdb", Rotational: true, Temp: 38},
		}}),
		makeDiskReadings(sensors.Response{Disks: []sensors.Disk{
			{ID: "1", Name: "disk1", Device: "sda", Rotational: true, Temp: 35},
		}}),
	}
	for index, readings := range states {
		if index > 0 {
			if err := os.Truncate(path, 0); err != nil {
				t.Fatal(err)
			}
		}
		_, _ = publishHWMonFamily(path, "disk", &publisher.disks, readings, false)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(data), "disk:1\t35000\tdisk1 (sda)\nconfigure\tdisk\n"; got != want {
		t.Fatalf("update = %q, want replacement inventory %q", got, want)
	}
}

func TestPublisherRestoresCachedInventoryAtFailsafe(t *testing.T) {
	directory := t.TempDir()
	device := filepath.Join(directory, "virt-temp")
	cache := filepath.Join(directory, "inventory.json")
	if err := os.WriteFile(device, nil, 0600); err != nil {
		t.Fatal(err)
	}
	publisher := &hwmonPublisher{
		cachePath: cache,
		disks: hwmonInventory{initialized: true, readings: []hwmonReading{
			{id: "disk:serial", label: "disk1 (sda)", temperature: 35},
		}},
	}
	if err := publisher.saveCache(); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(device, 0); err != nil {
		t.Fatal(err)
	}
	restored := &hwmonPublisher{cachePath: cache}
	if restoredCache, err := restored.restore(device); err != nil {
		t.Fatal(err)
	} else if !restoredCache {
		t.Fatal("cache was not reported as restored")
	}
	data, err := os.ReadFile(device)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(data), "disk:serial\t100000\tdisk1 (sda)\nconfigure\tdisk\n"; got != want {
		t.Fatalf("restored inventory = %q, want failsafe inventory %q", got, want)
	}
}

func TestParseRestartUnits(t *testing.T) {
	units, err := parseRestartUnits("coolercontrold.service, fan2go.service,coolercontrold.service")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"coolercontrold.service", "fan2go.service"}; !reflect.DeepEqual(units, want) {
		t.Fatalf("units = %#v, want %#v", units, want)
	}
	if _, err := parseRestartUnits("--no-block"); err == nil {
		t.Fatal("option-like unit must be rejected")
	}
}

func TestPublisherUsesStableIDWhenLabelChanges(t *testing.T) {
	path := filepath.Join(t.TempDir(), "virt-temp")
	if err := os.WriteFile(path, nil, 0600); err != nil {
		t.Fatal(err)
	}
	inventory := hwmonInventory{}
	initial := []hwmonReading{{id: "disk:serial", label: "disk1 (sda)", temperature: 34}}
	if _, err := publishHWMonFamily(path, "disk", &inventory, initial, false); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(path, 0); err != nil {
		t.Fatal(err)
	}
	changed := []hwmonReading{{id: "disk:serial", label: "disk1 (sdb)", temperature: 35}}
	if _, err := publishHWMonFamily(path, "disk", &inventory, changed, false); err != nil {
		t.Fatalf("a label change must not change sensor identity: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(data), "disk:serial\t35000\tdisk1 (sda)\ncommit\tdisk\n"; got != want {
		t.Fatalf("update = %q, want configured label %q", got, want)
	}
}

func TestPublisherWaitsForFirstNonEmptyInventory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "virt-temp")
	if err := os.WriteFile(path, nil, 0600); err != nil {
		t.Fatal(err)
	}
	inventory := hwmonInventory{}
	if _, err := publishHWMonFamily(path, "disk", &inventory, nil, false); err == nil {
		t.Fatal("an empty initial inventory should not be configured")
	}
	if inventory.initialized {
		t.Fatal("empty initial inventory was frozen")
	}
	readings := []hwmonReading{{id: "disk:serial", label: "disk1 (sda)", temperature: 35}}
	if _, err := publishHWMonFamily(path, "disk", &inventory, readings, false); err != nil {
		t.Fatal(err)
	}
	if !inventory.initialized {
		t.Fatal("non-empty inventory was not configured")
	}
}

func TestPublisherAllowsExplicitlyDisabledEmptyFamily(t *testing.T) {
	path := filepath.Join(t.TempDir(), "virt-temp")
	if err := os.WriteFile(path, nil, 0600); err != nil {
		t.Fatal(err)
	}
	inventory := hwmonInventory{}
	if _, err := publishHWMonFamily(path, "hba", &inventory, nil, true); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(data), "configure\thba\n"; got != want {
		t.Fatalf("configuration = %q, want %q", got, want)
	}
}

func makeDiskReadings(state sensors.Response) []hwmonReading {
	disks, _ := makeHWMonReadings(state)
	return disks
}

func TestPublishHWMonStateReturnsFetchError(t *testing.T) {
	want := errors.New("vsock failed")
	fetch := func(context.Context, uint32, uint32) (sensors.Response, error) {
		return sensors.Response{}, want
	}
	if _, err := (&hwmonPublisher{}).publish(context.Background(), 42, 19090, "/dev/null", fetch); !errors.Is(err, want) {
		t.Fatalf("got %v, want %v", err, want)
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
	fetch := func(context.Context, uint32, uint32) (sensors.Response, error) {
		return sensors.Response{
			Disks:       []sensors.Disk{{ID: "1", Name: "disk1", Device: "sda", Rotational: true, Temp: 34}},
			HBADisabled: true,
		}, nil
	}
	reconfigured, err := publisher.publish(context.Background(), 42, 19090, device, fetch)
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
