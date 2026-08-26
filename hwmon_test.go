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

func TestFindHWMon(t *testing.T) {
	root := t.TempDir()
	for directory, name := range map[string]string{
		"hwmon0": "coretemp",
		"hwmon3": virtTempName,
	} {
		path := filepath.Join(root, directory)
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(path, "name"), []byte(name+"\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}

	got, err := findHWMon(root, virtTempName)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(root, "hwmon3"); got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestHWMonTargetRediscoversAfterModuleReload(t *testing.T) {
	paths := []string{"/sys/class/hwmon/hwmon3", "/sys/class/hwmon/hwmon4"}
	target := newHWMonTarget()
	target.find = func(string) (string, error) {
		path := paths[0]
		paths = paths[1:]
		return path, nil
	}

	first, err := target.resolve()
	if err != nil {
		t.Fatal(err)
	}
	if first != "/sys/class/hwmon/hwmon3/temp1_input" {
		t.Fatalf("unexpected first path %q", first)
	}
	target.handleWriteError(os.ErrNotExist)
	second, err := target.resolve()
	if err != nil {
		t.Fatal(err)
	}
	if second != "/sys/class/hwmon/hwmon4/temp1_input" {
		t.Fatalf("unexpected rediscovered path %q", second)
	}
}
