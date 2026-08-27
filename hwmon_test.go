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
	err := (&hwmonPublisher{}).publish(context.Background(), 42, 19090, path, fetch)
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

func TestPublisherKeepsMissingSensorAndGroupStale(t *testing.T) {
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
		_ = publishHWMonFamily(path, "disk", &publisher.disks, readings)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(data), "disk:1\t35000\tdisk1 (sda)\ncommit\tdisk\n"; got != want {
		t.Fatalf("update = %q, want only the available individual sensor %q", got, want)
	}
}

func TestPublisherUsesStableIDWhenLabelChanges(t *testing.T) {
	path := filepath.Join(t.TempDir(), "virt-temp")
	if err := os.WriteFile(path, nil, 0600); err != nil {
		t.Fatal(err)
	}
	inventory := hwmonInventory{}
	initial := []hwmonReading{{id: "disk:serial", label: "disk1 (sda)", temperature: 34}}
	if err := publishHWMonFamily(path, "disk", &inventory, initial); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(path, 0); err != nil {
		t.Fatal(err)
	}
	changed := []hwmonReading{{id: "disk:serial", label: "disk1 (sdb)", temperature: 35}}
	if err := publishHWMonFamily(path, "disk", &inventory, changed); err != nil {
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

func makeDiskReadings(state sensors.Response) []hwmonReading {
	disks, _ := makeHWMonReadings(state)
	return disks
}

func TestPublishHWMonStateReturnsFetchError(t *testing.T) {
	want := errors.New("vsock failed")
	fetch := func(context.Context, uint32, uint32) (sensors.Response, error) {
		return sensors.Response{}, want
	}
	if err := (&hwmonPublisher{}).publish(context.Background(), 42, 19090, "/dev/null", fetch); !errors.Is(err, want) {
		t.Fatalf("got %v, want %v", err, want)
	}
}
