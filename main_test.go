package main

import (
	"bytes"
	"encoding/json"
	"net"
	"testing"
	"time"

	"unraid-vsock-sensors/internal/sensors"
)

func TestServerResponseMetadata(t *testing.T) {
	diskMessage, _, hbaMessage, _, heartbeat := collectorMessages(
		newDiskCollector("unused", time.Minute),
		newHBACollector(time.Minute, hbaModeDisabled),
	)

	if heartbeat.Version != version {
		t.Fatalf("got version %q, want %q", heartbeat.Version, version)
	}
	if !hbaMessage.HBADisabled {
		t.Fatal("disabled HBA collection was not reported")
	}
	if diskMessage.Type != sensors.MessageDisks || hbaMessage.Type != sensors.MessageHBAs ||
		heartbeat.Type != sensors.MessageHeartbeat {
		t.Fatalf("unexpected message types: %q, %q, %q", diskMessage.Type, hbaMessage.Type, heartbeat.Type)
	}
	if heartbeat.Disks != nil || heartbeat.HBAs != nil || heartbeat.Error != "" ||
		heartbeat.HBAError != "" || heartbeat.HBADisabled {
		t.Fatalf("heartbeat contains sensor data: %#v", heartbeat)
	}
}

func TestHandleReturnsLatestSnapshot(t *testing.T) {
	store := &snapshotStore{}
	if err := store.apply(sensors.Message{
		Type: sensors.MessageDisks,
		Response: sensors.Response{
			Version: "test-version",
			Disks:   []sensors.Disk{{ID: "serial", Name: "disk1", Temp: 37}},
		},
	}); err != nil {
		t.Fatal(err)
	}
	server, client := net.Pipe()
	done := make(chan struct{})
	go func() {
		handleSnapshotRequest(server, store)
		close(done)
	}()

	var response sensors.Response
	if err := json.NewDecoder(client).Decode(&response); err != nil {
		t.Fatal(err)
	}
	client.Close()
	<-done
	if response.Version != "test-version" || len(response.Disks) != 1 || response.Disks[0].Temp != 37 {
		t.Fatalf("unexpected response: %#v", response)
	}
}

func TestSnapshotStoreExpiresFamiliesIndependently(t *testing.T) {
	store := &snapshotStore{}
	if err := store.apply(sensors.Message{
		Type:     sensors.MessageDisks,
		Response: sensors.Response{Disks: []sensors.Disk{{ID: "disk", Temp: 37}}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.apply(sensors.Message{
		Type:     sensors.MessageHBAs,
		Response: sensors.Response{HBAs: []sensors.HBA{{ID: "hba", Temp: 48}}},
	}); err != nil {
		t.Fatal(err)
	}
	store.expireDisks()
	response := store.get()
	if response.Error == "" || len(response.Disks) != 1 {
		t.Fatalf("disk family did not expire: %#v", response)
	}
	if response.HBAError != "" || len(response.HBAs) != 1 {
		t.Fatalf("disk expiry changed HBA family: %#v", response)
	}

	store.expireHBAs()
	response = store.get()
	if response.HBAError == "" || len(response.HBAs) != 1 {
		t.Fatalf("HBA family did not expire: %#v", response)
	}
}

func TestSnapshotStoreHeartbeatPreservesFamilyState(t *testing.T) {
	store := &snapshotStore{}
	for _, message := range []sensors.Message{
		{Type: sensors.MessageDisks, Response: sensors.Response{
			Disks: []sensors.Disk{{ID: "disk", Temp: 37}}, Error: "disk failed",
		}},
		{Type: sensors.MessageHBAs, Response: sensors.Response{
			HBAs: []sensors.HBA{{ID: "hba", Temp: 48}}, HBADisabled: true, HBAError: "HBA failed",
		}},
		{Type: sensors.MessageHeartbeat, Response: sensors.Response{Version: "test"}},
	} {
		if err := store.apply(message); err != nil {
			t.Fatal(err)
		}
	}
	response := store.get()
	if response.Version != "test" || response.Error != "disk failed" ||
		response.HBAError != "HBA failed" || !response.HBADisabled {
		t.Fatalf("heartbeat changed family state: %#v", response)
	}
	if len(response.Disks) != 1 || len(response.HBAs) != 1 {
		t.Fatalf("heartbeat replaced family data: %#v", response)
	}
}

func TestDiskErrorDoesNotBlockHBASelector(t *testing.T) {
	r := sensors.Response{Error: "disks.ini failed", HBAs: []sensors.HBA{{ID: "sas:1234", Temp: 46}}}
	var out bytes.Buffer
	if err := writeResponse(&out, r, sensorTypeHBA, "sas:1234"); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); got != "46\n" {
		t.Fatalf("got %q, want HBA temperature", got)
	}
}

func TestDiskSelectorStillReturnsDiskError(t *testing.T) {
	r := sensors.Response{Error: "disks.ini failed", HBAs: []sensors.HBA{{ID: "sas:1234", Temp: 46}}}
	if err := writeResponse(&bytes.Buffer{}, r, sensorTypeDisk, "hdd"); err == nil || err.Error() != r.Error {
		t.Fatalf("got %v, want disk error", err)
	}
}

func TestHBASelectorRejectsCachedDataAfterAnError(t *testing.T) {
	r := sensors.Response{
		HBAs:     []sensors.HBA{{ID: "sas:1234", Temp: 46}},
		HBAError: "HBA snapshot TTL expired",
	}
	if err := writeResponse(&bytes.Buffer{}, r, sensorTypeHBA, "sas:1234"); err == nil ||
		err.Error() != "HBA temperature unavailable: HBA snapshot TTL expired" {
		t.Fatalf("got %v, want HBA expiry error", err)
	}
}

func TestDiskNamedLikeHBAIsSelectedAsDisk(t *testing.T) {
	r := sensors.Response{Disks: []sensors.Disk{{Name: "hba1", Temp: 38}}}
	var out bytes.Buffer
	if err := writeResponse(&out, r, sensorTypeDisk, "hba1"); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); got != "38\n" {
		t.Fatalf("got %q, want disk temperature", got)
	}
}

func TestDiskMaximumFailsSafeForUnavailableDisk(t *testing.T) {
	r := sensors.Response{Disks: []sensors.Disk{
		{Name: "disk1", Rotational: true, Temp: 38},
		{Name: "disk2", Rotational: true, Unavailable: true},
	}}
	var out bytes.Buffer
	if err := writeResponse(&out, r, sensorTypeDisk, "hdd"); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); got != "100\n" {
		t.Fatalf("got %q, want group failsafe", got)
	}
}

func TestWriteResponseSelectors(t *testing.T) {
	response := sensors.Response{
		Disks: []sensors.Disk{
			{Name: "disk1", Device: "sdb", Rotational: true, Temp: 35},
			{Name: "disk2", Device: "sdc", Rotational: true, Temp: 40},
			{Name: "cache", Device: "sdd", Transport: "ata", Temp: 44.5},
			{Name: "fast", Device: "nvme0n1", Transport: "nvme", Temp: 48},
		},
		HBAs: []sensors.HBA{{ID: "sas:1234", Temp: 49}, {ID: "pci:0000:06:10.0", Temp: 55}},
	}
	tests := []struct {
		name     string
		kind     sensorType
		selector string
		want     string
	}{
		{name: "HDD maximum", kind: sensorTypeDisk, selector: "hdd", want: "40\n"},
		{name: "SSD maximum", kind: sensorTypeDisk, selector: "ssd", want: "44.5\n"},
		{name: "NVMe maximum", kind: sensorTypeDisk, selector: "nvme", want: "48\n"},
		{name: "all disks", kind: sensorTypeDisk, selector: "all", want: "48\n"},
		{name: "disk name case insensitive", kind: sensorTypeDisk, selector: "DISK1", want: "35\n"},
		{name: "device case insensitive", kind: sensorTypeDisk, selector: "SDD", want: "44.5\n"},
		{name: "all HBAs", kind: sensorTypeHBA, selector: "all", want: "55\n"},
		{name: "HBA ID case insensitive", kind: sensorTypeHBA, selector: "SAS:1234", want: "49\n"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var out bytes.Buffer
			if err := writeResponse(&out, response, test.kind, test.selector); err != nil {
				t.Fatal(err)
			}
			if got := out.String(); got != test.want {
				t.Fatalf("got %q, want %q", got, test.want)
			}
		})
	}
}

func TestWriteResponseRejectsUnavailableSelector(t *testing.T) {
	for _, test := range []struct {
		name     string
		response sensors.Response
		kind     sensorType
		selector string
		want     string
	}{
		{name: "unknown disk", kind: sensorTypeDisk, selector: "missing", want: `no available temperature for selector "missing"`},
		{name: "unknown HBA", kind: sensorTypeHBA, selector: "sas:missing", want: `no available temperature for selector "sas:missing"`},
		{name: "HBA collection error", response: sensors.Response{HBAError: "storcli failed"}, kind: sensorTypeHBA, selector: "all", want: "HBA temperature unavailable: storcli failed"},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := writeResponse(&bytes.Buffer{}, test.response, test.kind, test.selector)
			if err == nil || err.Error() != test.want {
				t.Fatalf("got %v, want %q", err, test.want)
			}
		})
	}
}

func TestGetRequiresExplicitSensorTypeAndSelector(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "missing both", want: "a sensor type and selector are required (for example: disk hdd or hba all)"},
		{name: "missing type", args: []string{"hdd"}, want: "a sensor type and selector are required (for example: disk hdd or hba all)"},
		{name: "unknown type", args: []string{"fan", "all"}, want: `unknown sensor type "fan" (expected disk or hba)`},
		{name: "JSON with arguments", args: []string{"--json", "disk", "all"}, want: "--json does not accept a sensor type or selector"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := get(test.args)
			if err == nil || err.Error() != test.want {
				t.Fatalf("got %v, want %q", err, test.want)
			}
		})
	}
}
