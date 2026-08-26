package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"unraid-vsock-sensors/internal/sensors"
)

func TestPublishHDDMaximum(t *testing.T) {
	path := filepath.Join(t.TempDir(), "temp1_input")
	state := sensors.Response{Disks: []sensors.Disk{
		{Name: "disk1", Rotational: true, Temp: 34},
		{Name: "disk2", Rotational: true, Temp: 38.5},
		{Name: "external", Rotational: true, Transport: "usb", Temp: 60},
		{Name: "cache", Transport: "nvme", Temp: 48},
	}}
	fetch := func(context.Context, uint32, uint32) (sensors.Response, error) {
		return state, nil
	}

	if err := publishHDDMaximum(context.Background(), 42, 19090, path, fetch); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(data); got != "38500\n" {
		t.Fatalf("got %q, want 38500 milli-degrees", got)
	}
}

func TestPublishHDDMaximumLeavesWatchdogInChargeOnError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "temp1_input")
	if err := os.WriteFile(path, []byte("42000\n"), 0600); err != nil {
		t.Fatal(err)
	}
	fetch := func(context.Context, uint32, uint32) (sensors.Response, error) {
		return sensors.Response{}, errors.New("vsock failed")
	}

	if err := publishHDDMaximum(context.Background(), 42, 19090, path, fetch); err == nil {
		t.Fatal("expected fetch error")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(data); got != "42000\n" {
		t.Fatalf("failed update changed temperature to %q", got)
	}
}
