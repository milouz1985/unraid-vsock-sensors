// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type diskTestEnvironment struct {
	paths diskDataPaths
	now   time.Time
}

func newDiskTestEnvironment(t *testing.T, pollAttributes string) *diskTestEnvironment {
	t.Helper()
	root := t.TempDir()
	environment := &diskTestEnvironment{
		paths: diskDataPaths{
			disksINI: filepath.Join(root, "disks.ini"), devsINI: filepath.Join(root, "devs.ini"),
			varINI:       filepath.Join(root, "var.ini"),
			sysBlockRoot: filepath.Join(root, "class", "block"),
			policyFile:   filepath.Join(root, "disk-policies.json"),
		},
		now: time.Unix(1_800_000_000, 0),
	}
	environment.write(t, environment.paths.disksINI, "[flash]\ndevice=sdz\nrotational=0\nspundown=0\n")
	environment.write(t, environment.paths.devsINI, "")
	environment.write(t, environment.paths.varINI, "poll_attributes=\""+pollAttributes+"\"\n")
	return environment
}

func (environment *diskTestEnvironment) write(t *testing.T, path, data string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
}

func (environment *diskTestEnvironment) collector() *diskCollector {
	collector := newDiskCollector(environment.paths)
	collector.now = func() time.Time { return environment.now }
	return collector
}

func (c *diskCollector) refresh() {
	c.refreshWithContext(context.Background())
}

func requireSingleDisk(t *testing.T, collector *diskCollector) sensorsDisk {
	t.Helper()
	readings, err := collector.snapshot()
	if err != nil || len(readings) != 1 {
		t.Fatalf("snapshot = %#v, %v; want one disk", readings, err)
	}
	return sensorsDisk{readings[0].ID, readings[0].Name, readings[0].Device, readings[0].Temp, readings[0].Unavailable}
}

// sensorsDisk keeps assertions concise without hiding the public snapshot fields.
type sensorsDisk struct {
	id, name, device string
	temp             float64
	unavailable      bool
}

func addFakeBlockDevice(t *testing.T, blockRoot, device string, usb bool) {
	t.Helper()
	root := filepath.Clean(filepath.Join(blockRoot, "..", ".."))
	busRoot := filepath.Join(root, "bus")
	for _, bus := range []string{"pci", "scsi", "usb"} {
		if err := os.MkdirAll(filepath.Join(busRoot, bus), 0700); err != nil {
			t.Fatal(err)
		}
	}
	physical := filepath.Join(root, "devices", "pci0000:00", "0000:00:01.0", device, "host6", "target6:0:0", "6:0:0:0")
	if usb {
		physical = filepath.Join(root, "devices", "pci0000:00", "usb2", "2-1", "2-1:1.0", device, "host6", "target6:0:0", "6:0:0:0")
	}
	if err := os.MkdirAll(physical, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(busRoot, "scsi"), filepath.Join(physical, "subsystem")); err != nil {
		t.Fatal(err)
	}
	busNode := filepath.Join(root, "devices", "pci0000:00", "0000:00:01.0")
	bus := "pci"
	if usb {
		busNode = filepath.Join(root, "devices", "pci0000:00", "usb2", "2-1", "2-1:1.0")
		bus = "usb"
	}
	if err := os.Symlink(filepath.Join(busRoot, bus), filepath.Join(busNode, "subsystem")); err != nil && !os.IsExist(err) {
		t.Fatal(err)
	}
	classNode := filepath.Join(blockRoot, device)
	if err := os.MkdirAll(classNode, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(physical, filepath.Join(classNode, "device")); err != nil {
		t.Fatal(err)
	}
}
